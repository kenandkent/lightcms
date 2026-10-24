package staticdelivery

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/metavert-io/lightcms/v7/internal/product/artifacts"
	"github.com/metavert-io/lightcms/v7/internal/product/jobs"
	"net/url"
	"sort"
	"time"

	ex "github.com/metavert-io/lightcms/v7/internal/product/staticexport"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type CommandOptions struct {
	InspectTarget        func(context.Context, Target) (RemoteState, error)
	VerifyRecovery       func(context.Context, ReleaseManifest) error
	CheckCandidateOrigin func(context.Context, Target) error
	Admission            *artifacts.Admission
	Source               ex.Source
	Exports              *ex.Service
	Store                ReleaseStore
	Accounts             *AccountService
	Authorize            func(context.Context, ActorSnapshot, bool, bool) error
	Defaults             func(context.Context, string) (ex.Resource, ex.Resource, error)
}
type CommandService struct {
	previewRunner *jobs.Runner
	repo          *Repository
	options       CommandOptions
	store         ReleaseStore
}

func NewCommandService(r *Repository, o CommandOptions) *CommandService {
	if summaries, ok := o.Store.(interface {
		Summary(context.Context, primitive.ObjectID) (ReleaseManifest, error)
	}); ok && r != nil {
		r.summaries = summaries
	}
	if refs, ok := o.Store.(releaseReferences); ok && r != nil {
		r.references = refs
	}
	return &CommandService{repo: r, options: o, store: o.Store}
}

type ExportRecord struct {
	Record      `bson:",inline"`
	OperationID primitive.ObjectID `bson:"operation_id" json:"operation_id"`
	JobID       primitive.ObjectID `bson:"job_id" json:"job_id"`
	Owner       string             `bson:"owner" json:"-"`
	State       string             `bson:"state" json:"state"`
	Frozen      ex.FrozenExport    `bson:"frozen" json:"frozen"`
	ErrorCode   string             `bson:"error_code" json:"error_code,omitempty"`
}
type DeployRequest struct {
	QueueMode              string               `json:"queue_mode,omitempty"`
	Operation              string               `json:"operation,omitempty"`
	Paths                  []string             `json:"paths,omitempty"`
	SourceReleaseID        string               `json:"source_release_id,omitempty"`
	NewOrigin              string               `json:"new_origin,omitempty"`
	NewEnvironment         string               `json:"new_environment,omitempty"`
	ExportIDs              []primitive.ObjectID `json:"export_ids"`
	ExpectedRevision       int64                `json:"expected_revision"`
	ExpectedConfigRevision int64                `json:"expected_config_revision"`
	ConfirmationDigest     string               `json:"confirmation_digest,omitempty"`
	ManualScriptReview     bool                 `json:"manual_script_review"`
}
type PageChange struct {
	Path          string             `json:"path"`
	Action        string             `json:"action"`
	PublicationID primitive.ObjectID `json:"publication_id"`
	URL           string             `json:"url"`
}
type DeploymentPreview struct {
	ExpectedPaths  map[string]string  `bson:"expected_paths,omitempty" json:"expected_paths,omitempty"`
	DeliveryMode   string             `bson:"delivery_mode,omitempty" json:"delivery_mode,omitempty"`
	Digest         string             `bson:"_id" json:"confirmation_digest"`
	TargetID       primitive.ObjectID `bson:"target_id" json:"target_id"`
	ReleaseID      primitive.ObjectID `bson:"release_id" json:"-"`
	Owner          string             `bson:"owner" json:"-"`
	Request        DeployRequest      `bson:"request" json:"request"`
	Changes        []PageChange       `bson:"changes" json:"changes"`
	PreservedPages int                `bson:"preserved_pages" json:"preserved_pages"`
	Diagnostics    []ex.Diagnostic    `bson:"diagnostics" json:"diagnostics"`
	ExpiresAt      time.Time          `bson:"expires_at" json:"expires_at"`
}
type exportExecution struct {
	Frozen ex.FrozenExport
	Actor  ActorSnapshot
}
type deployExecution struct {
	ExpectedPaths        map[string]string
	ReleaseID            primitive.ObjectID
	CredentialRef        primitive.ObjectID
	Actor                ActorSnapshot
	CandidateOrigin      string
	CandidateEnvironment string
}

func (s *CommandService) authorize(ctx context.Context, a ActorSnapshot, publish, recovery bool) error {
	if s == nil || s.repo == nil || s.options.Authorize == nil {
		return failure(CodeModuleUnavailable, "static delivery unavailable", nil)
	}
	if a.Owner == "" || a.UserID == "" {
		return failure(CodePermissionDenied, "authenticated actor required", nil)
	}
	return s.options.Authorize(ctx, a, publish, recovery)
}
func (s *CommandService) SubmitExport(ctx context.Context, a ActorSnapshot, key string, items []ex.Item) (Job, error) {
	if e := s.authorize(ctx, a, false, false); e != nil {
		return Job{}, e
	}
	if s.options.Source == nil || s.options.Exports == nil {
		return Job{}, failure(CodeModuleUnavailable, "export service unavailable", nil)
	}
	if len(items) < 1 || len(items) > 100 {
		return Job{}, failure(CodeInvalidRequest, "select 1 to 100 reports", nil)
	}
	seen := map[primitive.ObjectID]bool{}
	for _, i := range items {
		if i.ContentID.IsZero() || seen[i.ContentID] {
			return Job{}, failure(CodeInvalidRequest, "duplicate or empty content ID", nil)
		}
		seen[i.ContentID] = true
	}
	body, _ := json.Marshal(struct {
		Items []ex.Item `json:"items"`
	}{items})
	op, _, e := s.repo.ReserveOperation(ctx, OperationRequest{Owner: a.Owner, Action: "export", Key: key, Body: body})
	if e != nil {
		return Job{}, e
	}
	if len(op.Snapshot) == 0 {
		frozen := ex.FrozenExport{ID: op.ExportID, OperationID: op.ID, CapturedAt: op.CreatedAt}
		for _, i := range items {
			p, e := s.options.Source.Resolve(ctx, i)
			if e != nil {
				return Job{}, e
			}
			frozen.Pages = append(frozen.Pages, p)
		}
		data, _ := bson.Marshal(exportExecution{frozen, a})
		op, e = s.repo.FreezeOperation(ctx, op.ID, data)
		if e != nil {
			return Job{}, e
		}
	}
	var x exportExecution
	if bson.Unmarshal(op.Snapshot, &x) != nil {
		return Job{}, failure(CodeInternal, "invalid frozen export operation", nil)
	}
	j := Job{Record: newRecord(), OperationID: op.ID, Kind: "export", Attempt: 1, State: JobQueued, ExportID: op.ExportID, Actor: x.Actor}
	j.ID = op.JobID
	record := ExportRecord{Record: newRecord(), OperationID: op.ID, JobID: j.ID, Owner: a.Owner, State: "queued", Frozen: x.Frozen}
	record.ID = op.ExportID
	e = s.repo.db.WithTransaction(ctx, func(sc mongo.SessionContext) error {
		if _, e := s.repo.collection("exports").UpdateOne(sc, bson.M{"_id": record.ID}, bson.M{"$setOnInsert": record}, options.Update().SetUpsert(true)); e != nil {
			return e
		}
		_, e := s.repo.collection("jobs").UpdateOne(sc, bson.M{"_id": j.ID}, bson.M{"$setOnInsert": j}, options.Update().SetUpsert(true))
		return e
	})
	if e != nil {
		return Job{}, e
	}
	return s.GetJob(ctx, a, j.ID)
}
func (s *CommandService) GetExport(ctx context.Context, a ActorSnapshot, id primitive.ObjectID) (ExportRecord, error) {
	if e := s.authorize(ctx, a, false, false); e != nil {
		return ExportRecord{}, e
	}
	var x ExportRecord
	e := s.repo.collection("exports").FindOne(ctx, bson.M{"_id": id}).Decode(&x)
	if errors.Is(e, mongo.ErrNoDocuments) {
		e = failure(CodeNotFound, "export not found", nil)
	}
	if e == nil && x.Owner != a.Owner && a.Role != "admin" {
		e = failure(CodeNotFound, "export not found", nil)
	}
	return x, e
}
func (s *CommandService) GetJob(ctx context.Context, a ActorSnapshot, id primitive.ObjectID) (Job, error) {
	var j Job
	e := s.repo.collection("jobs").FindOne(ctx, bson.M{"_id": id}).Decode(&j)
	if errors.Is(e, mongo.ErrNoDocuments) {
		return j, failure(CodeNotFound, "job not found", nil)
	}
	if e != nil {
		return j, e
	}
	if e = s.authorize(ctx, a, j.Kind != "export", false); e != nil {
		return Job{}, e
	}
	if j.Actor.Owner != a.Owner && a.Role != "admin" {
		return Job{}, failure(CodeNotFound, "job not found", nil)
	}
	return j, nil
}
func (s *CommandService) availableTarget(ctx context.Context, id primitive.ObjectID, req DeployRequest) (Target, error) {
	t, e := s.repo.GetTarget(ctx, id)
	if e != nil {
		return t, e
	}
	if t.Status != "ready" {
		return t, failure(CodeTargetNotReady, "Worker target is not ready", nil)
	}
	if t.CurrentRevision != req.ExpectedRevision || t.ConfigRevision != req.ExpectedConfigRevision {
		return t, failure(CodeRevisionConflict, "target changed; preview again", nil)
	}
	if !t.ActiveJobID.IsZero() && req.QueueMode != queueMerge {
		return t, failure(CodeTargetBusy, "target has an unfinished deployment", nil)
	}
	a, e := s.options.Accounts.Get(ctx, t.AccountRef)
	if e != nil {
		return t, e
	}
	if a.Status != "enabled" || !a.Verification.Readable {
		return t, failure(CodeAccountDisabled, "account must be enabled and verified", nil)
	}
	return t, nil
}
func (s *CommandService) PreviewDeployment(ctx context.Context, a ActorSnapshot, targetID primitive.ObjectID, req DeployRequest) (DeploymentPreview, error) {
	var out DeploymentPreview
	if e := s.authorize(ctx, a, true, false); e != nil {
		return out, e
	}
	if s.store == nil || s.options.Exports == nil || s.options.Accounts == nil {
		return out, failure(CodeModuleUnavailable, "deployment unavailable", nil)
	}
	if err := validateDeployment(req); err != nil {
		return out, err
	}
	if (operation(req) == "change_origin" || operation(req) == "change_environment") && a.Role != "admin" {
		return out, failure(CodePermissionDenied, "target configuration changes require administrator access", nil)
	}
	t, e := s.availableTarget(ctx, targetID, req)
	if e != nil {
		return out, e
	}
	if s.options.Admission != nil {
		historical, _ := primitive.ObjectIDFromHex(req.SourceReleaseID)
		amount, err := releaseReservation(ctx, s.store, t.CurrentReleaseID, historical)
		if err != nil {
			return out, err
		}
		release, err := s.options.Admission.Acquire(ctx, amount)
		if err != nil {
			return out, failure(CodeAssetLimitExceeded, "release preparation exceeds or is waiting for configured memory capacity", err)
		}
		defer release()
	}
	var current *ReleaseManifest
	if !t.CurrentReleaseID.IsZero() {
		r, e := s.store.Load(ctx, t.CurrentReleaseID)
		if e != nil {
			return out, e
		}
		current = &r
	}
	pages := []CapturedPage{}
	var captureBytes int64
	seen := map[primitive.ObjectID]bool{}
	for _, id := range req.ExportIDs {
		if seen[id] {
			return out, failure(CodeInvalidRequest, "duplicate export", nil)
		}
		seen[id] = true
		record, e := s.GetExport(ctx, a, id)
		if e != nil {
			return out, e
		}
		if record.State != "ready" {
			return out, failure(CodeArtifactUnavailable, "export is not ready", nil)
		}
		docs, e := s.options.Exports.Documents(ctx, id)
		if e != nil {
			return out, e
		}
		for _, d := range docs {
			captureBytes += ex.DocumentBytes(d)
			if captureBytes > ex.MaxCaptureBytes {
				return out, failure(CodeAssetLimitExceeded, "selected exports exceed the 128 MiB source budget", nil)
			}
			pages = append(pages, CapturedPage{ExportID: id, Document: d})
		}
	}
	input := BuildInput{ID: primitive.NewObjectID(), Target: t, Current: current, Pages: pages, Defaults: s.options.Defaults}
	if operation(req) != "publish" {
		var historical *ReleaseManifest
		if req.SourceReleaseID != "" {
			id, _ := primitive.ObjectIDFromHex(req.SourceReleaseID)
			saved, err := s.store.Load(ctx, id)
			if err != nil {
				return out, err
			}
			historical = &saved
		}
		input, e = lifecycleBuildInput(t, req, current, historical)
		if e != nil {
			return out, e
		}
		input.Defaults = s.options.Defaults
		if operation(req) == "change_origin" {
			if s.options.CheckCandidateOrigin == nil {
				return out, failure(CodeModuleUnavailable, "origin migration verification unavailable", nil)
			}
			if e = s.options.CheckCandidateOrigin(ctx, input.Target); e != nil {
				return out, e
			}
		}
	}
	manifest, e := BuildRelease(ctx, input)
	if e != nil {
		return out, e
	}
	if _, e = ReleaseModules(manifest); e != nil {
		return out, e
	}
	if len(manifest.Files) > t.AssetLimit && t.AssetLimit > 0 {
		return out, failure(CodeAssetLimitExceeded, "full release exceeds target file limit", nil)
	}
	changes, removed := lifecycleChanges(current, manifest)
	manifest.RemovedPaths = removed
	if e = s.store.Save(ctx, manifest); e != nil {
		return out, e
	}
	req.ConfirmationDigest = ""
	out = DeploymentPreview{TargetID: t.ID, ReleaseID: manifest.ID, Owner: a.Owner, Request: req, ExpiresAt: time.Now().UTC().Add(time.Hour), Diagnostics: manifest.Diagnostics}
	changed := map[string]bool{}
	for _, p := range pages {
		path := p.Document.Source.FullPath
		if changed[path] {
			return out, failure(CodePathConflict, "multiple selected pages use the same path", nil)
		}
		changed[path] = true
		action := "add"
		if current != nil {
			if _, ok := current.Pages[path]; ok {
				action = "replace"
			}
		}
		out.Changes = append(out.Changes, PageChange{Path: path, Action: action, PublicationID: p.Document.Source.PublicationID, URL: t.Origin + (&url.URL{Path: path}).EscapedPath()})
	}
	out.PreservedPages = len(manifest.Pages) - len(changed)
	if req.QueueMode == queueMerge {
		out.ExpectedPaths = map[string]string{}
		for path := range changed {
			value := ""
			if current != nil {
				if page, ok := current.Pages[path]; ok {
					value = pageExpectation(page)
				}
			}
			out.ExpectedPaths[path] = value
		}
	}
	if operation(req) != "publish" {
		out.Changes = changes
		replaced := 0
		for _, change := range changes {
			if change.Action != "remove" {
				replaced++
			}
		}
		out.PreservedPages = len(manifest.Pages) - replaced
	}
	out.DeliveryMode = manifest.DeliveryMode
	sort.Slice(out.Changes, func(i, j int) bool { return out.Changes[i].Path < out.Changes[j].Path })
	raw, _ := json.Marshal(out)
	out.Digest = releaseHash(raw)
	_, e = s.repo.collection("previews").InsertOne(ctx, out)
	return out, e
}
func (s *CommandService) SubmitDeployment(ctx context.Context, a ActorSnapshot, key string, targetID primitive.ObjectID, req DeployRequest) (Job, error) {
	if e := s.authorize(ctx, a, true, false); e != nil {
		return Job{}, e
	}
	if (operation(req) == "change_origin" || operation(req) == "change_environment") && a.Role != "admin" {
		return Job{}, failure(CodePermissionDenied, "target configuration changes require administrator permission", nil)
	}
	if req.ConfirmationDigest == "" {
		return Job{}, failure(CodeConfirmationRequired, "deployment preview confirmation required", nil)
	}
	body, _ := json.Marshal(req)
	op, _, e := s.repo.ReserveOperation(ctx, OperationRequest{Owner: a.Owner, Action: "deploy", TargetScope: targetID.Hex(), Key: key, Body: body})
	if e != nil {
		return Job{}, e
	}
	var x deployExecution
	if len(op.Snapshot) == 0 {
		var p DeploymentPreview
		e = s.repo.collection("previews").FindOne(ctx, bson.M{"_id": req.ConfirmationDigest, "owner": a.Owner, "target_id": targetID}).Decode(&p)
		if e != nil {
			return Job{}, failure(CodeConfirmationRequired, "preview not found; preview again", nil)
		}
		original := req
		original.ConfirmationDigest = ""
		before, _ := json.Marshal(p.Request)
		after, _ := json.Marshal(original)
		if string(before) != string(after) || time.Now().After(p.ExpiresAt) {
			return Job{}, failure(CodeConfirmationRequired, "preview changed or expired", nil)
		}
		target, e := s.availableTarget(ctx, targetID, req)
		if e != nil {
			return Job{}, e
		}
		manifest, e := s.releaseIdentity(ctx, p.ReleaseID)
		if e != nil {
			return Job{}, e
		}
		expectedOrigin, expectedEnvironment, expectedConfig := target.Origin, target.Environment, target.ConfigRevision
		candidateOrigin, candidateEnvironment := "", ""
		if operation(req) == "change_origin" {
			expectedOrigin, e = NormalizeOrigin(req.NewOrigin)
			if e != nil {
				return Job{}, e
			}
			candidateOrigin = expectedOrigin
			expectedConfig++
		}
		if operation(req) == "change_environment" {
			expectedEnvironment = req.NewEnvironment
			candidateEnvironment = expectedEnvironment
			expectedConfig++
		}
		if manifest.TargetID != target.ID || manifest.Origin != expectedOrigin || manifest.Environment != expectedEnvironment || manifest.BaseRevision != target.CurrentRevision || manifest.ConfigRevision != expectedConfig {
			return Job{}, failure(CodeRevisionConflict, "frozen release does not match target", nil)
		}
		account, e := s.options.Accounts.Get(ctx, target.AccountRef)
		if e != nil {
			return Job{}, e
		}
		var c Credential
		if e = s.repo.collection("credentials").FindOne(ctx, bson.M{"account_ref": account.ID, "revision": account.CredentialRevision}).Decode(&c); e != nil {
			return Job{}, failure(CodeCredentialUnavailable, "credential missing", nil)
		}
		data, _ := bson.Marshal(deployExecution{ExpectedPaths: p.ExpectedPaths, ReleaseID: p.ReleaseID, CredentialRef: c.ID, Actor: a, CandidateOrigin: candidateOrigin, CandidateEnvironment: candidateEnvironment})
		op, e = s.repo.FreezeOperation(ctx, op.ID, data)
		if e != nil {
			return Job{}, e
		}
	}
	if bson.Unmarshal(op.Snapshot, &x) != nil {
		return Job{}, failure(CodeInternal, "invalid deployment execution snapshot", nil)
	}
	j := Job{Record: newRecord(), OperationID: op.ID, Kind: "deploy", State: JobQueued, Attempt: 1, TargetID: targetID, ReleaseID: x.ReleaseID, ExportIDs: req.ExportIDs, ExpectedRevision: req.ExpectedRevision, ConfigRevision: req.ExpectedConfigRevision, CredentialRef: x.CredentialRef, Actor: x.Actor}
	j.ID = op.JobID
	j.Operation = req.Operation
	j.CandidateOrigin = x.CandidateOrigin
	j.CandidateEnvironment = x.CandidateEnvironment
	j.QueueMode = req.QueueMode
	j.ExpectedPaths = x.ExpectedPaths
	j.ConfirmedReleaseID = x.ReleaseID
	j.ConfirmedRevision = req.ExpectedRevision
	e = s.repo.db.WithTransaction(ctx, func(sc mongo.SessionContext) error {
		if e := s.repo.collection("jobs").FindOne(sc, bson.M{"_id": j.ID}).Err(); e == nil {
			return nil
		} else if !errors.Is(e, mongo.ErrNoDocuments) {
			return e
		}
		if e := s.repo.touchManagementRecord(sc, "targets", j.TargetID); e != nil {
			return e
		}
		if j.QueueMode != queueMerge {
			if e := s.repo.ClaimTarget(sc, j.TargetID, j.ID, j.ExpectedRevision); e != nil {
				return e
			}
		}
		if s.repo.references != nil {
			if e := s.repo.references.ReferenceRelease(sc, "delivery-job:"+j.ID.Hex(), j.ReleaseID, nil); e != nil {
				return e
			}
		}
		if _, e := s.repo.collection("releases").InsertOne(sc, bson.M{"_id": j.ReleaseID, "job_id": j.ID, "target_id": targetID, "state": "prepared", "created_at": j.CreatedAt}); e != nil {
			return e
		}
		_, e := s.repo.collection("jobs").InsertOne(sc, j)
		return e
	})
	if e != nil {
		return Job{}, e
	}
	return s.GetJob(ctx, a, j.ID)
}

func (s *CommandService) NewRunner(d RunnerDependencies) *Runner {
	d.Store = s.store
	d.Admission = s.options.Admission
	d.Authorize = func(ctx context.Context, j Job, recovery bool) error {
		return s.authorize(ctx, j.Actor, j.Kind != "export", recovery)
	}
	d.Export = func(ctx context.Context, j Job) error {
		var record ExportRecord
		if e := s.repo.collection("exports").FindOne(ctx, bson.M{"_id": j.ExportID}).Decode(&record); e != nil {
			return e
		}
		_, e := s.repo.collection("exports").UpdateOne(ctx, bson.M{"_id": j.ExportID}, bson.M{"$set": bson.M{"state": "building"}})
		if e != nil {
			return e
		}
		_, e = s.options.Exports.Build(ctx, record.Frozen)
		state := "ready"
		if e != nil {
			state = "failed"
		}
		_, writeErr := s.repo.collection("exports").UpdateOne(ctx, bson.M{"_id": j.ExportID}, bson.M{"$set": bson.M{"state": state, "error_code": CodeOf(e), "updated_at": time.Now().UTC()}})
		if writeErr != nil {
			return writeErr
		}
		return e
	}
	d.Prepare = func(ctx context.Context, j Job, t Target) (ReleaseManifest, error) {
		return ReleaseManifest{}, failure(CodeArtifactUnavailable, "confirmed release is missing; preview again", nil)
	}
	return NewRunner(s.repo, d)
}

type releaseIdentity struct {
	TargetID                     primitive.ObjectID
	Origin                       string
	Environment                  string
	BaseRevision, ConfigRevision int64
}

func (s *CommandService) releaseIdentity(ctx context.Context, id primitive.ObjectID) (releaseIdentity, error) {
	var manifest ReleaseManifest
	var err error
	if summaries, ok := s.store.(interface {
		Summary(context.Context, primitive.ObjectID) (ReleaseManifest, error)
	}); ok {
		manifest, err = summaries.Summary(ctx, id)
	} else {
		manifest, err = s.store.Load(ctx, id)
	}
	return releaseIdentity{TargetID: manifest.TargetID, Origin: manifest.Origin, Environment: manifest.Environment, BaseRevision: manifest.BaseRevision, ConfigRevision: manifest.ConfigRevision}, err
}

// Template Upgrade Preview (read-only) + durable Upgrade Job.
//
// Never silently regenerate live pages: Preview lists what WOULD change
// without writing anything; the Job runs only on explicit Start, processes
// each content via PublicationService.Publish (one new Publication per page,
// never GenerateStaticPage overwrites), and records per-item outcomes with
// retry/resume. Job state is durable in template_upgrade_jobs.
package generation

import (
	"context"
	"fmt"
	"time"

	"github.com/metavert-io/lightcms/v7/internal/product/idempotency"
	"github.com/metavert-io/lightcms/v7/internal/product/publication"
	"github.com/metavert-io/lightcms/v7/internal/product/templatecontract"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Upgrade collections.
const CollectionUpgradeJobs = "template_upgrade_jobs"

// UpgradeItemStatus tracks one page inside a job.
type UpgradeItemStatus string

const (
	UpgradeItemPending UpgradeItemStatus = "pending"
	UpgradeItemDone    UpgradeItemStatus = "done"
	UpgradeItemFailed  UpgradeItemStatus = "failed"
	UpgradeItemSkipped UpgradeItemStatus = "skipped"
)

// UpgradeJobStatus tracks the job itself.
type UpgradeJobStatus string

const (
	UpgradeJobRunning   UpgradeJobStatus = "running"
	UpgradeJobCompleted UpgradeJobStatus = "completed"
	UpgradeJobPartial   UpgradeJobStatus = "partial"
)

// UpgradePreviewItem is one page that references the template.
type UpgradePreviewItem struct {
	ActivePublicationID primitive.ObjectID `json:"active_publication_id"`
	ContentID           string             `json:"content_id"`
	FullPath            string             `json:"full_path"`
	CurrentVersion      int64              `json:"current_version"`
	HasActive           bool               `json:"has_active"`
	ActiveTemplateV     int64              `json:"active_template_version"`
	TargetTemplateV     int64              `json:"target_template_version"`
	WouldRepublish      bool               `json:"would_republish"`
	ValidationErrors    int                `json:"validation_errors"`
}

// UpgradePreview is the read-only result: no Publication, no file, no job row.
type UpgradePreview struct {
	TemplateID      primitive.ObjectID   `json:"template_id"`
	TargetVersionID primitive.ObjectID   `json:"target_version_id"`
	Template        string               `json:"template"`
	FromVersion     int64                `json:"from_version"`
	ToVersion       int64                `json:"to_version"`
	TotalPages      int                  `json:"total_pages"`
	WouldRepublish  int                  `json:"would_republish"`
	Items           []UpgradePreviewItem `json:"items"`
}

// UpgradeJob is the durable record.
type UpgradeJob struct {
	ID              primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	TemplateID      primitive.ObjectID `bson:"template_id" json:"template_id"`
	TemplateSlug    string             `bson:"template_slug" json:"template_slug"`
	FromVersion     int64              `bson:"from_version" json:"from_version"`
	ToVersion       int64              `bson:"to_version" json:"to_version"`
	TargetVersionID primitive.ObjectID `bson:"target_version_id" json:"target_version_id"`
	Status          UpgradeJobStatus   `bson:"status" json:"status"`
	Items           []UpgradeJobItem   `bson:"items" json:"items"`
	CreatedBy       string             `bson:"created_by" json:"created_by"`
	CreatedAt       time.Time          `bson:"created_at" json:"created_at"`
	UpdatedAt       time.Time          `bson:"updated_at" json:"updated_at"`
}

// UpgradeJobItem is one page's outcome.
type UpgradeJobItem struct {
	ExpectedActiveID primitive.ObjectID  `bson:"expected_active_id" json:"expected_active_id"`
	ContentVersion   int64               `bson:"content_version" json:"content_version"`
	ContentID        primitive.ObjectID  `bson:"content_id" json:"content_id"`
	FullPath         string              `bson:"full_path" json:"full_path"`
	Status           UpgradeItemStatus   `bson:"status" json:"status"`
	Attempts         int                 `bson:"attempts" json:"attempts"`
	PublicationID    *primitive.ObjectID `bson:"publication_id,omitempty" json:"publication_id,omitempty"`
	Error            string              `bson:"error,omitempty" json:"error,omitempty"`
}

// PreviewUpgrade lists pages referencing slug at their frozen versions.
// Read-only: no writes of any kind.
func (s *Service) PreviewUpgrade(ctx context.Context, actor Actor, slug string) (UpgradePreview, error) {
	var zero UpgradePreview
	if !actor.Authenticated {
		return zero, genErr(CodeUnauthenticated, "authentication is required", nil)
	}
	if !actor.Can(ScopeTemplateView) {
		return zero, genErr(CodePermissionDenied, "missing required scope template.view", nil)
	}
	tpl, err := s.templatesRepo().FindTemplateBySlug(ctx, slug)
	if err != nil {
		if templatecontract.CodeOf(err) == templatecontract.CodeNotFound {
			return zero, genErr(CodeTemplateNotFound, fmt.Sprintf("template %q not found", slug), err)
		}
		return zero, genErr(CodeInternal, "load template", err)
	}
	cur, err := s.templatesRepo().FindVersion(ctx, tpl.ID, tpl.CurrentVersion)
	if err != nil {
		return zero, genErr(CodeTemplateVersionNotFound, "current template version not found", err)
	}
	// Every live page participates. A silent cap would make a completed job
	// falsely certify a partial upgrade.
	cur2, err := s.db.Collection("content").Find(ctx,
		bson.M{"template_id": tpl.ID, "path_scope": "live", "path_active": true},
		options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}).SetProjection(bson.M{"_id": 1, "full_path": 1, "current_version": 1, "data": 1}))
	if err != nil {
		return zero, genErr(CodeInternal, "list template pages", err)
	}
	defer cur2.Close(ctx)
	var out UpgradePreview
	out.TemplateID, out.TargetVersionID = tpl.ID, cur.ID
	out.Template = slug
	out.ToVersion = cur.Version
	// FromVersion: minimum active template version across pages (best effort).
	out.FromVersion = cur.Version
	type row struct {
		ID             primitive.ObjectID `bson:"_id"`
		FullPath       string             `bson:"full_path"`
		CurrentVersion int64              `bson:"current_version"`
		Data           map[string]any     `bson:"data"`
	}
	var rows []row
	if err := cur2.All(ctx, &rows); err != nil {
		return zero, genErr(CodeInternal, "decode template pages", err)
	}
	for _, r := range rows {
		var hasActive bool
		var activeV int64
		var activeID primitive.ObjectID
		if s.pubRepo != nil {
			a, err := s.pubRepo.GetActive(ctx, r.ID)
			if err != nil {
				return zero, genErr(CodeInternal, "read active publication", err)
			}
			if a != nil {
				activeID = a.ID
				hasActive = true
				activeV = a.TemplateVersion
				if activeV < out.FromVersion {
					out.FromVersion = activeV
				}
			}
		}
		errs, _ := templatecontract.ValidateData(cur, r.Data)
		would := hasActive && activeV != cur.Version && len(errs) == 0
		out.Items = append(out.Items, UpgradePreviewItem{
			ActivePublicationID: activeID,
			ContentID:           r.ID.Hex(), FullPath: r.FullPath, CurrentVersion: r.CurrentVersion,
			HasActive: hasActive, ActiveTemplateV: activeV, TargetTemplateV: cur.Version,
			WouldRepublish: would, ValidationErrors: len(errs),
		})
		if would {
			out.WouldRepublish++
		}
	}
	out.TotalPages = len(out.Items)
	return out, nil
}

// StartUpgradeJob creates the durable job (no live pages touched yet).
func (s *Service) StartUpgradeJob(ctx context.Context, actor Actor, slug string) (UpgradeJob, error) {
	var zero UpgradeJob
	if !actor.Authenticated {
		return zero, genErr(CodeUnauthenticated, "authentication is required", nil)
	}
	if !actor.IsAdmin || !actor.Can(ScopeTemplateEdit) {
		return zero, genErr(CodePermissionDenied, "upgrade jobs require an admin with template.edit", nil)
	}
	preview, err := s.PreviewUpgrade(ctx, Actor{
		Authenticated: true, IsAdmin: actor.IsAdmin, Role: actor.Role, Scopes: []string{},
		Email: actor.Email, ID: actor.ID,
	}, slug)
	if err != nil {
		return zero, err
	}
	tpl, err := s.templatesRepo().FindTemplateBySlug(ctx, slug)
	if err != nil {
		return zero, genErr(CodeTemplateNotFound, "template not found", err)
	}
	cur, err := s.templatesRepo().FindVersion(ctx, tpl.ID, tpl.CurrentVersion)
	if err != nil {
		return zero, genErr(CodeTemplateVersionNotFound, "current version not found", err)
	}
	if tpl.ID != preview.TemplateID || cur.ID != preview.TargetVersionID {
		return zero, genErr(CodeUpgradeJobConflict, "template changed during upgrade planning; preview again", nil)
	}
	items := make([]UpgradeJobItem, 0, len(preview.Items))
	for _, it := range preview.Items {
		cid, _ := primitive.ObjectIDFromHex(it.ContentID)
		st := UpgradeItemPending
		if !it.WouldRepublish {
			st = UpgradeItemSkipped
		}
		items = append(items, UpgradeJobItem{ContentID: cid, FullPath: it.FullPath, Status: st, ExpectedActiveID: it.ActivePublicationID, ContentVersion: it.CurrentVersion})
	}
	now := s.now()
	job := UpgradeJob{
		TemplateID: tpl.ID, TemplateSlug: slug,
		FromVersion: preview.FromVersion, ToVersion: cur.Version, TargetVersionID: cur.ID,
		Status: UpgradeJobRunning, Items: items, CreatedBy: actor.Email,
		CreatedAt: now, UpdatedAt: now,
	}
	res, err := s.db.Collection(CollectionUpgradeJobs).InsertOne(ctx, job)
	if err != nil {
		return zero, genErr(CodeInternal, "create upgrade job", err)
	}
	job.ID = res.InsertedID.(primitive.ObjectID)
	s.auditf(ctx, "template.upgrade.job.create", map[string]any{
		"job_id": job.ID.Hex(), "template": slug, "to_version": cur.Version,
	})
	return job, nil
}

// GetUpgradeJob loads a job.
func (s *Service) GetUpgradeJob(ctx context.Context, actor Actor, jobID primitive.ObjectID) (UpgradeJob, error) {
	var zero UpgradeJob
	if !actor.Authenticated {
		return zero, genErr(CodeUnauthenticated, "authentication is required", nil)
	}
	if !actor.Can(ScopeTemplateView) {
		return zero, genErr(CodePermissionDenied, "missing required scope template.view", nil)
	}
	var job UpgradeJob
	if err := s.db.FindOne(ctx, CollectionUpgradeJobs, bson.M{"_id": jobID}, &job); err != nil {
		if err == mongo.ErrNoDocuments {
			return zero, genErr(CodeUpgradeJobNotFound, "upgrade job not found", err)
		}
		return zero, genErr(CodeInternal, "load upgrade job", err)
	}
	return job, nil
}

// RunUpgradeJob processes pending/failed items via PublicationService.Publish.
// Each page mints its own Publication; failures are recorded per item and the
// job stays resumable (retry = call again). Never silently regenerates: only
// items the caller explicitly runs are published, and the caller must hold
// content.publish (checked once up front).
func (s *Service) RunUpgradeJob(ctx context.Context, actor Actor, jobID primitive.ObjectID) (UpgradeJob, error) {
	if s.workflow != nil {
		return s.waitUpgradeRun(ctx, actor, jobID)
	}
	return s.runUpgradeJob(ctx, actor, jobID)
}

func (s *Service) runUpgradeJob(ctx context.Context, actor Actor, jobID primitive.ObjectID) (UpgradeJob, error) {
	var zero UpgradeJob
	if !actor.Authenticated {
		return zero, genErr(CodeUnauthenticated, "authentication is required", nil)
	}
	if !actor.Can(ScopeContentPublish) || !actor.Can(ScopeContentEdit) {
		return zero, genErr(CodePermissionDenied, "upgrade run requires content.edit + content.publish", nil)
	}
	job, err := s.GetUpgradeJob(ctx, actor, jobID)
	if err != nil {
		return zero, err
	}
	if s.pubs == nil {
		return zero, genErr(CodeInternal, "publication service is not wired", nil)
	}
	// Bound each run below its lease. A crashed runner is resumable, while
	// concurrent callers cannot overwrite each other's item checkpoints.
	ctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	owner := primitive.NewObjectID().Hex()
	now := time.Now().UTC()
	err = s.db.Collection(CollectionUpgradeJobs).FindOneAndUpdate(ctx,
		bson.M{"_id": job.ID, "$or": bson.A{bson.M{"lease_until": bson.M{"$lte": now}}, bson.M{"lease_until": bson.M{"$exists": false}}}},
		bson.M{"$set": bson.M{"lease_owner": owner, "lease_until": now.Add(5 * time.Minute)}},
		options.FindOneAndUpdate().SetReturnDocument(options.After)).Decode(&job)
	if err != nil {
		return zero, genErr(CodeUpgradeJobConflict, "upgrade job is already running or unavailable", err)
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer stop()
		_, _ = s.db.Collection(CollectionUpgradeJobs).UpdateOne(cleanup, bson.M{"_id": job.ID, "lease_owner": owner}, bson.M{"$unset": bson.M{"lease_owner": "", "lease_until": ""}})
	}()
	persist := func() error {
		job.UpdatedAt = s.now()
		res, e := s.db.Collection(CollectionUpgradeJobs).UpdateOne(ctx, bson.M{"_id": job.ID, "lease_owner": owner}, bson.M{"$set": bson.M{"items": job.Items, "status": job.Status, "updated_at": job.UpdatedAt}})
		if e != nil {
			return genErr(CodeInternal, "save upgrade progress", e)
		}
		if res.MatchedCount != 1 {
			return genErr(CodeUpgradeJobConflict, "upgrade lease changed", nil)
		}
		return nil
	}
	for i := range job.Items {
		if s.workflowAuth != nil {
			if err := s.workflowAuth(ctx, actor); err != nil {
				return job, err
			}
		}
		it := &job.Items[i]
		if it.Status == UpgradeItemDone || it.Status == UpgradeItemSkipped {
			continue
		}
		// Old jobs without frozen baselines must be recreated explicitly. Never
		// infer fresh publishing intent from today's mutable content.
		if it.ExpectedActiveID.IsZero() || it.ContentVersion <= 0 {
			it.Status = UpgradeItemFailed
			it.Error = "upgrade baseline unavailable; create a new preview and job"
			it.Attempts++
			if err := persist(); err != nil {
				return job, err
			}
			continue
		}
		res, perr := s.publishUpgradeItem(ctx, actor, job, it.ContentID, it.ContentVersion, job.TargetVersionID, &it.ExpectedActiveID)
		it.Attempts++
		if perr != nil {
			it.Status = UpgradeItemFailed
			it.Error = perr.Error()
			if err := persist(); err != nil {
				return job, err
			}
			continue
		}
		pid := res.PublicationID
		it.PublicationID = &pid
		it.Status = UpgradeItemDone
		it.Error = ""
		if err := persist(); err != nil {
			return job, err
		}
	}
	// Recompute job status.
	pending, failed := 0, 0
	for _, it := range job.Items {
		switch it.Status {
		case UpgradeItemPending, UpgradeItemFailed:
			if it.Status == UpgradeItemFailed {
				failed++
			} else {
				pending++
			}
		}
	}
	// Failed items stay failed-but-retryable (Run again resumes them).
	if pending > 0 || failed > 0 {
		if failed > 0 && pending == 0 {
			job.Status = UpgradeJobPartial
		} else {
			job.Status = UpgradeJobRunning
		}
	} else {
		job.Status = UpgradeJobCompleted
	}
	if err := persist(); err != nil {
		return job, err
	}
	s.auditf(ctx, "template.upgrade.job.run", map[string]any{
		"job_id": job.ID.Hex(), "status": string(job.Status),
	})
	return job, nil
}

// publishUpgradeItem publishes one upgrade-job page through the saga under
// a stable per-item operation key (Task 16D: upgrade/<jobID>/<contentID>).
// A job resume after a crash replays the cached completion instead of
// minting a duplicate Publication/outbox row; a terminal pre-activation
// failure gets a new attempt (new Publication ID) on resume. Without a
// wired idempotency service the publish is unkeyed (legacy behavior).
func (s *Service) publishUpgradeItem(ctx context.Context, actor Actor, job UpgradeJob, contentID primitive.ObjectID, cv int64, targetVersionID primitive.ObjectID, expected *primitive.ObjectID) (publication.PublicationResult, error) {
	req := publication.PublishRequest{
		ExpectedTemplateID: job.TemplateID,
		ContentID:          contentID, ContentVersion: cv,
		TemplateVersionID: targetVersionID, ExpectedActiveID: expected,
		Reason: "template upgrade job " + job.ID.Hex(),
		// Lane 2B: thread caller attribution into the minted record.
		Actor: actorKind(actor), Via: actor.Via, AgentSession: actor.AgentSession,
		AuthorIsAdmin: actor.IsAdmin,
	}
	if s.idem == nil {
		return s.pubs.Publish(ctx, req)
	}
	owner := actor.Owner()
	path := "/internal/upgrade-jobs/" + job.ID.Hex() + "/publish"
	key := "upgrade/" + job.ID.Hex() + "/" + contentID.Hex()
	op, berr := s.beginOrResume(ctx, owner, "POST", path, key, nil)
	if berr != nil {
		return publication.PublicationResult{}, mapIdemBeginErr(berr)
	}
	ctx = idempotency.WithLeaseGeneration(ctx, op.LeaseGeneration)
	if op.Replay {
		// Resume path returns the frozen identifiers only: the job loop
		// records PublicationID per item (full result fields are not
		// needed — the canonical bytes/URL are readable from the live
		// publication record). Deliberately minimal, not lossy.
		if hex, ok := op.Response["publication_id"].(string); ok && hex != "" {
			if pid, perr := primitive.ObjectIDFromHex(hex); perr == nil {
				return publication.PublicationResult{PublicationID: pid, ContentID: contentID}, nil
			}
		}
		return publication.PublicationResult{}, genErr(CodeInternal, "idempotent replay without a cached publication", nil)
	}
	req.IdempotencyRecord = &op.ID
	res, perr := s.pubs.Publish(ctx, req)
	if perr != nil {
		code := mapSagaCode(perr)
		if code == "" {
			code = string(publication.CodeActivateFailed)
		}
		// Only the saga knows whether activation is provably absent.
		// Releasing a retryable lease must not terminalize an unknown commit.
		_, _ = s.idem.Complete(ctx, op.ID, op.Attempt, 503, map[string]any{"error_code": code}, false)
		return publication.PublicationResult{}, mapSagaErr(perr)
	}
	_, _ = s.idem.Complete(ctx, op.ID, op.Attempt, 200,
		map[string]any{"publication_id": res.PublicationID.Hex(), "content_id": contentID.Hex()}, false)
	return res, nil
}

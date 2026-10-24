package staticdelivery

import (
	"context"
	"net/url"
	"regexp"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type RemoteFactory func(token, marker string) Remote
type SubdomainEnabler func(context.Context, string, Target) error
type OriginChecker func(context.Context, Target, RemoteState) error
type TargetService struct {
	repo            *Repository
	accounts        *AccountService
	installation    string
	remote          RemoteFactory
	enableSubdomain SubdomainEnabler
	checkOrigin     OriginChecker
}

func NewTargetService(r *Repository, a *AccountService, installation string, f RemoteFactory, enable SubdomainEnabler, check OriginChecker) *TargetService {
	return &TargetService{r, a, installation, f, enable, check}
}

var cfWorkerName = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

func NormalizeOrigin(raw string) (string, error) {
	u, e := url.Parse(raw)
	if e != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || u.Opaque != "" || u.Port() != "" && u.Port() != "443" {
		return "", failure(CodeInvalidRequest, "public origin must be a bare HTTPS origin", nil)
	}
	host := strings.ToLower(u.Hostname())
	if strings.HasSuffix(host, ".") || strings.ContainsAny(host, "\\ \x00") {
		return "", failure(CodeInvalidRequest, "invalid origin hostname", nil)
	}
	return "https://" + host, nil
}
func (s *TargetService) Create(ctx context.Context, account primitive.ObjectID, name, worker, environment, origin string) (Target, error) {
	a, e := s.accounts.Get(ctx, account)
	if e != nil {
		return Target{}, e
	}
	if a.Status == "archived" {
		return Target{}, failure(CodeAccountDisabled, "account is archived", nil)
	}
	if strings.TrimSpace(name) == "" || len(name) > 200 || !cfWorkerName.MatchString(worker) || (environment != "test" && environment != "production") || s.installation == "" {
		return Target{}, failure(CodeInvalidRequest, "invalid Worker target", nil)
	}
	if origin != "" {
		origin, e = NormalizeOrigin(origin)
		if e != nil {
			return Target{}, e
		}
	}
	target := Target{Record: newRecord(), AccountRef: account, AccountID: a.AccountID, Name: strings.TrimSpace(name), WorkerName: worker, Environment: environment, Origin: origin, ConfigRevision: 1, Status: "draft", InstallationID: s.installation, AssetLimit: 20000}
	_, e = s.repo.collection("targets").InsertOne(ctx, target)
	if mongo.IsDuplicateKeyError(e) {
		return Target{}, failure(CodeTargetExists, "Worker target already exists; restore the existing target", nil)
	}
	return target, e
}
func (s *TargetService) List(ctx context.Context) ([]Target, error) {
	cur, e := s.repo.collection("targets").Find(ctx, bson.M{}, options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}}).SetLimit(1000))
	if e != nil {
		return nil, e
	}
	defer cur.Close(ctx)
	out := []Target{}
	e = cur.All(ctx, &out)
	return out, e
}
func (s *TargetService) remoteFor(ctx context.Context, t Target, revision int64) (Remote, error) {
	if s.remote == nil {
		return nil, failure(CodeModuleUnavailable, "Cloudflare client unavailable", nil)
	}
	raw, e := s.accounts.Token(ctx, t.AccountRef, revision)
	if e != nil {
		return nil, e
	}
	return s.remote(string(raw), OwnershipMarker{Protocol: 1, InstallationID: t.InstallationID, TargetID: t.ID.Hex()}.String()), nil
}
func ownsTarget(t Target, state RemoteState) bool {
	return state.Exists && state.SingleVersion && state.InstallationID == t.InstallationID && state.TargetID == t.ID.Hex()
}
func (s *TargetService) Verify(ctx context.Context, id primitive.ObjectID) (RemoteState, error) {
	t, e := s.repo.GetTarget(ctx, id)
	if e != nil {
		return RemoteState{}, e
	}
	a, e := s.accounts.Get(ctx, t.AccountRef)
	if e != nil {
		return RemoteState{}, e
	}
	revision := a.CredentialRevision
	if t.Status == "initializing" {
		revision = t.InitializationCredentialRevision
	}
	remote, e := s.remoteFor(ctx, t, revision)
	if e != nil {
		return RemoteState{}, e
	}
	state, e := remote.Inspect(ctx, t)
	if e != nil {
		return state, e
	}
	if !ownsTarget(t, state) {
		return state, failure(CodeUnmanagedWorker, "Worker ownership or active version does not match", nil)
	}
	if t.RemoteVersionID != "" && !matchesRemote(t, state, t.RemoteVersionID) {
		return state, failure(CodeRemoteDrift, "Worker version changed outside this application", nil)
	}
	if s.checkOrigin == nil {
		return state, failure(CodeOriginNotReady, "public origin has not been verified", nil)
	}
	if e = s.checkOrigin(ctx, t, state); e != nil {
		return state, e
	}
	return state, nil
}
func (s *TargetService) Initialize(ctx context.Context, id primitive.ObjectID) (Target, error) {
	t, e := s.repo.GetTarget(ctx, id)
	if e != nil {
		return t, e
	}
	a, e := s.accounts.Get(ctx, t.AccountRef)
	if e != nil {
		return t, e
	}
	if a.Status != "enabled" {
		return t, failure(CodeAccountDisabled, "account is disabled", nil)
	}
	if t.Status != "draft" && t.Status != "initializing" {
		return t, failure(CodeTargetNotReady, "target is not available for initialization", nil)
	}
	if !a.Verification.Readable {
		return t, failure(CodeCFPermission, "verify account access first", nil)
	}
	if t.Origin == "" {
		if a.Verification.Subdomain == "" {
			return t, failure(CodeOriginNotReady, "enable the account workers.dev subdomain in Cloudflare first", nil)
		}
		if strings.ContainsAny(a.Verification.Subdomain, "./\\ ") {
			return t, failure(CodeOriginNotReady, "invalid workers.dev subdomain", nil)
		}
		t.Origin = "https://" + t.WorkerName + "." + a.Verification.Subdomain + ".workers.dev"
	}
	fresh := t.Status == "draft"
	revision := t.InitializationCredentialRevision
	if fresh {
		revision = a.CredentialRevision
	}
	remote, e := s.remoteFor(ctx, t, revision)
	if e != nil {
		return t, e
	}
	if fresh {
		state, err := remote.Inspect(ctx, t)
		if err != nil {
			return t, err
		}
		if state.Exists {
			return t, failure(CodeUnmanagedWorker, "existing Worker cannot be initialized or overwritten", nil)
		}
		// Account guard is updated in the same transaction: concurrent disable/archive
		// or rotation conflicts instead of silently changing the initialization identity.
		e = s.repo.db.WithTransaction(ctx, func(sc mongo.SessionContext) error {
			res, e := s.repo.collection("accounts").UpdateOne(sc, bson.M{"_id": a.ID, "status": "enabled", "credential_revision": revision}, bson.M{"$set": bson.M{"updated_at": time.Now().UTC()}})
			if e != nil {
				return e
			}
			if res.MatchedCount != 1 {
				return failure(CodeAccountDisabled, "account changed before initialization", nil)
			}
			res, e = s.repo.collection("targets").UpdateOne(sc, bson.M{"_id": id, "status": "draft", "config_revision": t.ConfigRevision}, bson.M{"$set": bson.M{"status": "initializing", "public_origin": t.Origin, "initialization_credential_revision": revision, "updated_at": time.Now().UTC()}})
			if e == nil && res.MatchedCount != 1 {
				return failure(CodeTargetBusy, "initialization already started", nil)
			}
			return e
		})
		if e != nil {
			return t, e
		}
		t.Status = "initializing"
		t.InitializationCredentialRevision = revision
		state, e = remote.Inspect(ctx, t)
		if e != nil {
			return t, e
		}
		if state.Exists {
			return t, failure(CodeUnmanagedWorker, "Worker appeared during initialization; refusing overwrite", nil)
		}
		config, e := nativeAssetConfig(ReleaseManifest{DeliveryMode: AssetsOnlyV1, Environment: t.Environment})
		if e != nil {
			return t, e
		}
		modules := []Module{
			{Name: "delivery-health.json", MIME: "application/json", Data: staticHealth(t.InstallationID, t.ID.Hex(), "")},
			{Name: "delivery-assets-only.json", MIME: "application/json", Data: config},
		}
		if _, e = remote.Initialize(ctx, t, modules); e != nil {
			return t, e
		}
	}
	// Recovery NEVER repeats PUT, even if an initializing Worker is absent.
	state, e := remote.Inspect(ctx, t)
	if e != nil {
		return t, e
	}
	if !ownsTarget(t, state) {
		return t, failure(CodeTargetNotReady, "initialization remains uncertain; inspect remote Worker before retrying", nil)
	}
	a, e = s.accounts.Get(ctx, t.AccountRef)
	if e != nil {
		return t, e
	}
	if a.Status != "enabled" {
		return t, failure(CodeAccountDisabled, "account disabled; initialization can only be inspected", nil)
	}
	if strings.HasSuffix(t.Origin, ".workers.dev") {
		if s.enableSubdomain == nil {
			return t, failure(CodeModuleUnavailable, "workers.dev initialization unavailable", nil)
		}
		raw, e := s.accounts.Token(ctx, a.ID, revision)
		if e != nil {
			return t, e
		}
		if e = s.enableSubdomain(ctx, string(raw), t); e != nil {
			return t, e
		}
	}
	if s.checkOrigin == nil {
		return t, failure(CodeOriginNotReady, "public origin check unavailable", nil)
	}
	if e = s.checkOrigin(ctx, t, state); e != nil {
		return t, e
	}
	e = s.repo.db.WithTransaction(ctx, func(sc mongo.SessionContext) error {
		set := bson.M{"updated_at": time.Now().UTC()}
		if a.CredentialRevision == revision {
			set["verification.write_verified"] = true
		}
		res, e := s.repo.collection("accounts").UpdateOne(sc, bson.M{"_id": a.ID, "status": "enabled", "credential_revision": a.CredentialRevision}, bson.M{"$set": set})
		if e != nil {
			return e
		}
		if res.MatchedCount != 1 {
			return failure(CodeAccountDisabled, "account changed during initialization verification", nil)
		}
		res, e = s.repo.collection("targets").UpdateOne(sc, bson.M{"_id": id, "status": "initializing", "initialization_credential_revision": revision}, bson.M{"$set": bson.M{"status": "ready", "remote_version_id": state.VersionID, "remote_deployment_id": state.DeploymentID, "remote_settings_hash": state.SettingsHash, "remote_code_hash": state.CodeHash, "updated_at": time.Now().UTC()}})
		if e == nil && res.MatchedCount != 1 {
			return failure(CodeRevisionConflict, "target changed during initialization", nil)
		}
		return e
	})
	if e != nil {
		return t, e
	}
	return s.repo.GetTarget(ctx, id)
}
func (s *TargetService) SetEnabled(ctx context.Context, id primitive.ObjectID, enabled bool) error {
	t, e := s.repo.GetTarget(ctx, id)
	if e != nil {
		return e
	}
	if t.Status == "archived" || t.Status == "initializing" {
		return failure(CodeTargetNotReady, "target cannot change enabled state", nil)
	}
	state := "disabled"
	if enabled {
		if t.Status != "disabled" {
			return failure(CodeTargetNotReady, "target is not disabled", nil)
		}
		a, e := s.accounts.Get(ctx, t.AccountRef)
		if e != nil {
			return e
		}
		if a.Status != "enabled" {
			return failure(CodeAccountDisabled, "account is disabled", nil)
		}
		if _, e = s.Verify(ctx, id); e != nil {
			return e
		}
		state = "ready"
	}
	res, e := s.repo.collection("targets").UpdateOne(ctx, bson.M{"_id": id, "status": t.Status, "config_revision": t.ConfigRevision}, bson.M{"$set": bson.M{"status": state, "updated_at": time.Now().UTC()}, "$inc": bson.M{"config_revision": int64(1)}})
	if e == nil && res.MatchedCount != 1 {
		return failure(CodeRevisionConflict, "target changed", nil)
	}
	return e
}
func (s *TargetService) Archive(ctx context.Context, id primitive.ObjectID) error {
	t, e := s.repo.GetTarget(ctx, id)
	if e != nil {
		return e
	}
	if !t.ActiveJobID.IsZero() || t.Status == "initializing" {
		return failure(CodeTargetBusy, "target has an active operation", nil)
	}
	res, e := s.repo.collection("targets").UpdateOne(ctx, bson.M{"_id": id, "status": t.Status, "active_job_id": primitive.NilObjectID}, bson.M{"$set": bson.M{"status": "archived", "updated_at": time.Now().UTC()}, "$inc": bson.M{"config_revision": int64(1)}})
	if e == nil && res.MatchedCount != 1 {
		return failure(CodeTargetBusy, "target changed", nil)
	}
	return e
}
func (s *TargetService) Restore(ctx context.Context, id primitive.ObjectID) error {
	res, e := s.repo.collection("targets").UpdateOne(ctx, bson.M{"_id": id, "status": "archived"}, bson.M{"$set": bson.M{"status": "disabled", "updated_at": time.Now().UTC()}, "$inc": bson.M{"config_revision": int64(1)}})
	if e == nil && res.MatchedCount != 1 {
		return failure(CodeRevisionConflict, "target is not archived", nil)
	}
	return e
}

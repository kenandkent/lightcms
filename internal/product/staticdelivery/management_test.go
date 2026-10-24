package staticdelivery

import (
	"context"
	"errors"
	"github.com/metavert-io/lightcms/v7/internal/product/jobs"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func managementFixture(t *testing.T) (*Repository, context.Context, *AccountService, *TargetService, Account, Target) {
	t.Helper()
	r, ctx := connectDeliveryTestDB(t)
	accounts := NewAccountService(r, nil, func(context.Context, string, string, string) (ConnectionCheck, error) {
		t.Fatal("local configuration management must not call Cloudflare")
		return ConnectionCheck{}, nil
	})
	a, err := accounts.Create(ctx, "Account", "0123456789abcdef0123456789abcdef", "private-token", "")
	if err != nil {
		t.Fatal(err)
	}
	targets := NewTargetService(r, accounts, "management-installation", nil, nil, nil)
	target, err := targets.Create(ctx, a.ID, "Report site", "report-site", "test", "https://reports.example.com")
	if err != nil {
		t.Fatal(err)
	}
	return r, ctx, accounts, targets, a, target
}

func TestManagementAccountDeleteRemovesOnlyLocalConfiguration(t *testing.T) {
	r, ctx, accounts, _, a, target := managementFixture(t)
	other, err := accounts.Create(ctx, "Other", "abcdef0123456789abcdef0123456789", "other-token", "")
	if err != nil {
		t.Fatal(err)
	}
	job := Job{Record: newRecord(), OperationID: primitive.NewObjectID(), Kind: "deploy", State: JobSucceeded, TargetID: target.ID, Attempt: 1}
	if _, err := r.collection("jobs").InsertOne(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := accounts.Delete(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"accounts", "targets", "credentials"} {
		filter := bson.M{"account_ref": a.ID}
		if c == "accounts" {
			filter = bson.M{"_id": a.ID}
		}
		if n, err := r.collection(c).CountDocuments(ctx, filter); err != nil || n != 0 {
			t.Fatalf("%s still contains deleted configuration: %d %v", c, n, err)
		}
	}
	if _, err := accounts.Get(ctx, other.ID); err != nil {
		t.Fatal("unrelated account deleted", err)
	}
	if n, _ := r.collection("jobs").CountDocuments(ctx, bson.M{"_id": job.ID}); n != 1 {
		t.Fatal("historical task deleted")
	}
	if _, err := accounts.Token(ctx, a.ID, 1); CodeOf(err) != CodeCredentialUnavailable {
		t.Fatal("deleted token still readable", err)
	}
}

func TestManagementRejectsBusyConfigurationMutations(t *testing.T) {
	for _, busy := range []string{"active", "initializing", "queued", "activation_unknown", "recovery"} {
		t.Run(busy, func(t *testing.T) {
			r, ctx, accounts, targets, a, target := managementFixture(t)
			switch busy {
			case "active":
				_, _ = r.collection("targets").UpdateOne(ctx, bson.M{"_id": target.ID}, bson.M{"$set": bson.M{"active_job_id": primitive.NewObjectID()}})
			case "initializing":
				_, _ = r.collection("targets").UpdateOne(ctx, bson.M{"_id": target.ID}, bson.M{"$set": bson.M{"status": "initializing"}})
			case "recovery":
				_, _ = r.db.Collection("workflow_jobs").InsertOne(ctx, bson.M{"_id": primitive.NewObjectID(), "kind": "delivery_recovery", "state": "queued", "input_ref": target.ID.Hex()})
			default:
				_, _ = r.collection("jobs").InsertOne(ctx, Job{Record: newRecord(), OperationID: primitive.NewObjectID(), Kind: "deploy", State: JobState(busy), TargetID: target.ID, Attempt: 1})
			}
			if err := targets.Delete(ctx, target.ID, a.ID); CodeOf(err) != CodeTargetBusy {
				t.Fatalf("busy target deleted: %v", err)
			}
			if err := accounts.Delete(ctx, a.ID); CodeOf(err) != CodeTargetBusy {
				t.Fatalf("busy account deleted: %v", err)
			}
			if _, err := targets.Update(ctx, target.ID, TargetUpdate{AccountRef: a.ID, ExpectedConfigRevision: 1, Name: "New", WorkerName: target.WorkerName, Environment: target.Environment, Origin: target.Origin}); CodeOf(err) != CodeTargetBusy {
				t.Fatalf("busy target edited: %v", err)
			}
			if _, err := r.GetTarget(ctx, target.ID); err != nil {
				t.Fatal("rejected deletion changed target", err)
			}
		})
	}
}

func TestManagementWorkerUpdateScopesAndPinsConfiguration(t *testing.T) {
	r, ctx, _, targets, a, target := managementFixture(t)
	update := TargetUpdate{AccountRef: a.ID, ExpectedConfigRevision: 1, Name: "Renamed", WorkerName: "new-worker", Environment: "production", Origin: "https://NEW.example.com/"}
	wrong := update
	wrong.AccountRef = primitive.NewObjectID()
	if _, err := targets.Update(ctx, target.ID, wrong); CodeOf(err) != CodeNotFound {
		t.Fatal("cross-account update allowed", err)
	}
	if err := targets.Delete(ctx, target.ID, wrong.AccountRef); CodeOf(err) != CodeNotFound {
		t.Fatal("cross-account delete allowed", err)
	}
	stale := update
	stale.ExpectedConfigRevision = 99
	if _, err := targets.Update(ctx, target.ID, stale); CodeOf(err) != CodeRevisionConflict {
		t.Fatal("stale update allowed", err)
	}
	changed, err := targets.Update(ctx, target.ID, update)
	if err != nil || changed.ConfigRevision != 2 || changed.WorkerName != "new-worker" || changed.Origin != "https://new.example.com" {
		t.Fatalf("draft not updated: %+v %v", changed, err)
	}
	_, _ = r.collection("targets").UpdateOne(ctx, bson.M{"_id": target.ID}, bson.M{"$set": bson.M{"status": "ready"}})
	update.ExpectedConfigRevision = 2
	update.ExpectedManagementRevision = changed.ManagementRevision
	update.Origin = changed.Origin
	update.WorkerName = "another-name"
	if _, err := targets.Update(ctx, target.ID, update); CodeOf(err) != CodeInvalidRequest {
		t.Fatal("initialized Worker renamed", err)
	}
	update.WorkerName = changed.WorkerName
	update.Origin = "https://second.example.com"
	changed, err = targets.Update(ctx, target.ID, update)
	if err != nil || changed.Status != "disabled" {
		t.Fatalf("changed origin did not require verification: %+v %v", changed, err)
	}
	_, _ = r.collection("targets").UpdateOne(ctx, bson.M{"_id": target.ID}, bson.M{"$set": bson.M{"current_revision": int64(1), "current_release_id": primitive.NewObjectID()}})
	update.ExpectedConfigRevision = 3
	update.ExpectedManagementRevision = changed.ManagementRevision
	update.Origin = "https://published.example.com"
	if _, err := targets.Update(ctx, target.ID, update); CodeOf(err) != CodeInvalidRequest {
		t.Fatal("published configuration changed without deployment", err)
	}
	update.Origin = changed.Origin
	update.Name = "Only the label"
	if updated, err := targets.Update(ctx, target.ID, update); err != nil || updated.Name != update.Name {
		t.Fatal("safe label edit rejected", err)
	}
	if err := targets.Delete(ctx, target.ID, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.GetTarget(ctx, target.ID); CodeOf(err) != CodeNotFound {
		t.Fatal("local target not removed", err)
	}
	if _, err := targets.accounts.Get(ctx, a.ID); err != nil {
		t.Fatal("target deletion removed account", err)
	}
}

func TestManagementConcurrentAccountRemovalCannotLeaveWorkerOrphans(t *testing.T) {
	r, ctx, accounts, targets, a, _ := managementFixture(t)
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	var deletion, creation error
	go func() { defer wg.Done(); <-start; deletion = accounts.Delete(ctx, a.ID) }()
	go func() {
		defer wg.Done()
		<-start
		_, creation = targets.Create(ctx, a.ID, "Concurrent", "concurrent-worker", "test", "")
	}()
	close(start)
	wg.Wait()
	if deletion != nil {
		t.Fatal(deletion)
	}
	if creation != nil && CodeOf(creation) != CodeNotFound {
		t.Fatal(creation)
	}
	if n, err := r.collection("targets").CountDocuments(ctx, bson.M{"account_ref": a.ID}); err != nil || n != 0 {
		t.Fatalf("orphan Worker: %d %v", n, err)
	}
}

func TestManagementWorkflowAdmissionAndDeletionShareTargetFence(t *testing.T) {
	r, ctx, accounts, targets, a, target := managementFixture(t)
	commands := NewCommandService(r, CommandOptions{})
	commands.previewRunner = jobs.NewRunner(jobs.NewRepository(r.db, 10), 1)
	_, err := commands.submitManagedWorkflow(ctx, target.ID, jobs.Submission{Kind: "delivery_preview", Actor: "owner", Key: "preview", InputRef: target.ID.Hex(), Items: []jobs.Input{{Key: "prepare", Data: bson.M{"fixture": true}}}})
	if err != nil {
		t.Fatal(err)
	}
	if err = accounts.Delete(ctx, a.ID); CodeOf(err) != CodeTargetBusy {
		t.Fatal("queued workflow did not protect account", err)
	}
	if err = targets.Delete(ctx, target.ID, a.ID); CodeOf(err) != CodeTargetBusy {
		t.Fatal("queued workflow did not protect Worker", err)
	}
	_, _ = r.db.Collection("workflow_jobs").UpdateMany(ctx, bson.M{"input_ref": target.ID.Hex()}, bson.M{"$set": bson.M{"state": "failed"}})
	if err = targets.Delete(ctx, target.ID, a.ID); err != nil {
		t.Fatal(err)
	}
	_, err = commands.submitManagedWorkflow(ctx, target.ID, jobs.Submission{Kind: "delivery_preview", Actor: "owner", Key: "after-delete", InputRef: target.ID.Hex(), Items: []jobs.Input{{Key: "prepare"}}})
	if CodeOf(err) != CodeNotFound {
		t.Fatal("workflow admitted after deletion", err)
	}
}

type managementReferences struct {
	err    error
	owners []string
}

func (r *managementReferences) ReferenceRelease(context.Context, string, primitive.ObjectID, *time.Time) error {
	return nil
}
func (r *managementReferences) ReleaseOwner(_ context.Context, owner string) error {
	r.owners = append(r.owners, owner)
	return r.err
}

func TestManagementDeletionReleasesOnlyRemovedCurrentReferences(t *testing.T) {
	r, ctx, accounts, targets, a, target := managementFixture(t)
	refs := &managementReferences{err: errors.New("reference storage unavailable")}
	r.references = refs
	if err := targets.Delete(ctx, target.ID, a.ID); err == nil {
		t.Fatal("configuration deleted without releasing its current reference")
	}
	if _, err := r.GetTarget(ctx, target.ID); err != nil {
		t.Fatal("failed deletion was not atomic", err)
	}
	refs.err = nil
	refs.owners = nil
	if err := accounts.Delete(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if len(refs.owners) != 1 || refs.owners[0] != "delivery-current:"+target.ID.Hex() {
		t.Fatalf("unrelated references released: %+v", refs.owners)
	}
}

func TestManagementDuplicateWorkerEditReturnsConflict(t *testing.T) {
	_, ctx, _, targets, a, target := managementFixture(t)
	other, err := targets.Create(ctx, a.ID, "Other", "other-worker", "test", "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = targets.Update(ctx, target.ID, TargetUpdate{AccountRef: a.ID, ExpectedConfigRevision: 1, Name: "Duplicate", WorkerName: other.WorkerName, Environment: target.Environment, Origin: target.Origin})
	if CodeOf(err) != CodeTargetExists {
		t.Fatal("duplicate Worker edit not reported as conflict", err)
	}
}

func TestManagementDisplayNamePreservesKnownReleaseRecovery(t *testing.T) {
	r, ctx, accounts, targets, a, target := managementFixture(t)
	target.Status = "drifted"
	manifest, err := BuildRelease(ctx, BuildInput{ID: primitive.NewObjectID(), Target: target, Pages: []CapturedPage{releasePage(primitive.NewObjectID(), "/known")}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.collection("targets").UpdateOne(ctx, bson.M{"_id": target.ID}, bson.M{"$set": bson.M{"status": "drifted"}}); err != nil {
		t.Fatal(err)
	}
	if _, err = r.collection("jobs").InsertOne(ctx, Job{Record: newRecord(), OperationID: primitive.NewObjectID(), Kind: "deploy", Attempt: 1, TargetID: target.ID, ReleaseID: manifest.ID, State: JobSucceeded, Checkpoint: Checkpoint{RemoteVersionID: "known"}}); err != nil {
		t.Fatal(err)
	}
	state := RemoteState{Exists: true, SingleVersion: true, InstallationID: target.InstallationID, TargetID: target.ID.Hex(), ReleaseID: manifest.ID.Hex(), VersionID: "known"}
	service := NewCommandService(r, CommandOptions{Accounts: accounts, Store: &memoryReleaseStore{manifest: manifest}, Authorize: func(context.Context, ActorSnapshot, bool, bool) error { return nil }, InspectTarget: func(context.Context, Target) (RemoteState, error) { return state, nil }, VerifyRecovery: func(context.Context, ReleaseManifest) error { return nil }})
	request := TargetUpdate{AccountRef: a.ID, ExpectedConfigRevision: target.ConfigRevision, Name: "A new local label", WorkerName: target.WorkerName, Environment: target.Environment, Origin: target.Origin}
	if _, err = targets.Update(ctx, target.ID, request); err != nil {
		t.Fatal(err)
	}
	if _, err = service.PreviewRecovery(ctx, ActorSnapshot{Owner: "admin", UserID: "admin", Role: "admin"}, target.ID); err != nil {
		t.Fatal("display name made a known release unrecoverable", err)
	}
	if _, err = targets.Update(ctx, target.ID, request); CodeOf(err) != CodeRevisionConflict {
		t.Fatal("old local editing version accepted", err)
	}
}

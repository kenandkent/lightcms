package staticdelivery

import (
	"context"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"testing"
	"time"
)

func TestReviewRecoveryMarksForeignVersionDrifted(t *testing.T) {
	r, ctx := connectDeliveryTestDB(t)
	target := releaseTarget()
	target.Status = "ready"
	now := time.Now().UTC()
	job := Job{Record: newRecord(), OperationID: primitive.NewObjectID(), State: JobActivationUnknown, TargetID: target.ID, ReleaseID: primitive.NewObjectID(), ConfigRevision: 1, Checkpoint: Checkpoint{ActivationIntentAt: &now, PreviousVersionID: "old", RemoteVersionID: "candidate"}}
	target.ActiveJobID = job.ID
	r.collection("targets").InsertOne(ctx, target)
	r.collection("jobs").InsertOne(ctx, job)
	f := &runnerRemote{targetRemote: targetRemote{state: RemoteState{Exists: true, SingleVersion: true, InstallationID: target.InstallationID, TargetID: target.ID.Hex(), VersionID: "foreign"}}}
	runner := NewRunner(r, RunnerDependencies{Owner: "review", Authorize: func(context.Context, Job, bool) error { return nil }, Remote: func(context.Context, Job, Target) (Remote, error) { return f, nil }})
	if err := runner.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	latest, err := r.GetTarget(ctx, target.ID)
	if err != nil || latest.Status != "drifted" || latest.ActiveJobID != job.ID || f.activateCalls != 0 {
		t.Fatalf("foreign version not safely quarantined: target=%+v activate=%d err=%v", latest, f.activateCalls, err)
	}
}

func TestReviewMissingStoreBlocksBeforeUpload(t *testing.T) {
	r, ctx := connectDeliveryTestDB(t)
	target := releaseTarget()
	target.Status = "ready"
	target.RemoteVersionID = "old"
	job := Job{Record: newRecord(), OperationID: primitive.NewObjectID(), State: JobUploading, TargetID: target.ID, ReleaseID: primitive.NewObjectID(), ConfigRevision: 1}
	target.ActiveJobID = job.ID
	r.collection("targets").InsertOne(ctx, target)
	r.collection("jobs").InsertOne(ctx, job)
	f := &runnerRemote{targetRemote: targetRemote{state: RemoteState{Exists: true, SingleVersion: true, InstallationID: target.InstallationID, TargetID: target.ID.Hex(), VersionID: "old"}}}
	runner := NewRunner(r, RunnerDependencies{Owner: "review", Authorize: func(context.Context, Job, bool) error { return nil }, Remote: func(context.Context, Job, Target) (Remote, error) { return f, nil }})
	if err := runner.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var got Job
	if err := r.collection("jobs").FindOne(ctx, bson.M{"_id": job.ID}).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.State != JobFailed || got.ErrorCode != CodeModuleUnavailable {
		t.Fatalf("unavailable store did not block safely: %+v", got)
	}
}

func TestReviewRunnerBlocksSettingsDriftBeforeUpload(t *testing.T) {
	r, ctx := connectDeliveryTestDB(t)
	target := releaseTarget()
	target.Status = "ready"
	target.RemoteVersionID = "old"
	target.RemoteSettingsHash = "expected"
	job := Job{Record: newRecord(), OperationID: primitive.NewObjectID(), State: JobUploading, TargetID: target.ID, ReleaseID: primitive.NewObjectID(), ConfigRevision: 1}
	target.ActiveJobID = job.ID
	r.collection("targets").InsertOne(ctx, target)
	r.collection("jobs").InsertOne(ctx, job)
	f := &runnerRemote{targetRemote: targetRemote{state: RemoteState{Exists: true, SingleVersion: true, InstallationID: target.InstallationID, TargetID: target.ID.Hex(), VersionID: "old", SettingsHash: "unexpected"}}}
	runner := NewRunner(r, RunnerDependencies{Owner: "review", Authorize: func(context.Context, Job, bool) error { return nil }, Remote: func(context.Context, Job, Target) (Remote, error) { return f, nil }, Store: &memoryReleaseStore{}})
	if e := runner.RunOnce(ctx); e != nil {
		t.Fatal(e)
	}
	var got Job
	r.collection("jobs").FindOne(ctx, bson.M{"_id": job.ID}).Decode(&got)
	latest, _ := r.GetTarget(ctx, target.ID)
	if got.State != JobFailed || got.ErrorCode != CodeRemoteDrift || latest.Status != "drifted" {
		t.Fatalf("drift ignored: job=%s error=%s target=%s", got.State, got.ErrorCode, latest.Status)
	}
}

type disableOnInspection struct {
	runnerRemote
	hook func()
}

func (f *disableOnInspection) Inspect(ctx context.Context, t Target) (RemoteState, error) {
	f.hook()
	return f.state, nil
}
func TestReviewDisabledAccountCannotCrossActivationIntent(t *testing.T) {
	r, ctx := connectDeliveryTestDB(t)
	account := Account{Record: newRecord(), Status: "enabled", CredentialRevision: 1}
	target := releaseTarget()
	target.Status = "ready"
	target.AccountRef = account.ID
	target.RemoteVersionID = "old"
	job := Job{Record: newRecord(), OperationID: primitive.NewObjectID(), State: JobVersionReady, TargetID: target.ID, ReleaseID: primitive.NewObjectID(), ConfigRevision: 1, Checkpoint: Checkpoint{PreviousVersionID: "old", RemoteVersionID: "candidate"}}
	target.ActiveJobID = job.ID
	r.collection("accounts").InsertOne(ctx, account)
	r.collection("targets").InsertOne(ctx, target)
	r.collection("jobs").InsertOne(ctx, job)
	f := &disableOnInspection{runnerRemote: runnerRemote{targetRemote: targetRemote{state: RemoteState{Exists: true, SingleVersion: true, InstallationID: target.InstallationID, TargetID: target.ID.Hex(), VersionID: "old"}}}, hook: func() {
		r.collection("accounts").UpdateOne(ctx, bson.M{"_id": account.ID}, bson.M{"$set": bson.M{"status": "disabled"}})
	}}
	manifest, err := BuildRelease(ctx, BuildInput{ID: job.ReleaseID, Target: target})
	if err != nil {
		t.Fatal(err)
	}
	runner := NewRunner(r, RunnerDependencies{Owner: "review", Store: &memoryReleaseStore{manifest: manifest}, Authorize: func(context.Context, Job, bool) error { return nil }, Remote: func(context.Context, Job, Target) (Remote, error) { return f, nil }})
	if e := runner.RunOnce(ctx); e != nil {
		t.Fatal(e)
	}
	var got Job
	r.collection("jobs").FindOne(ctx, bson.M{"_id": job.ID}).Decode(&got)
	if f.activateCalls != 0 || got.Checkpoint.ActivationIntentAt != nil || got.State != JobFailed {
		t.Fatalf("disabled account activated: calls=%d state=%s intent=%v", f.activateCalls, got.State, got.Checkpoint.ActivationIntentAt)
	}
}

type corruptReleaseStore struct{}

func (corruptReleaseStore) Load(context.Context, primitive.ObjectID) (ReleaseManifest, error) {
	return ReleaseManifest{}, failure(CodeArtifactUnavailable, "corrupted", nil)
}
func (corruptReleaseStore) Save(context.Context, ReleaseManifest) error { return nil }
func TestReviewCorruptPreparedReleaseIsNotRebuilt(t *testing.T) {
	r, ctx := connectDeliveryTestDB(t)
	target := releaseTarget()
	target.Status = "ready"
	job := Job{Record: newRecord(), OperationID: primitive.NewObjectID(), State: JobPreparing, TargetID: target.ID, ReleaseID: primitive.NewObjectID(), ConfigRevision: 1}
	target.ActiveJobID = job.ID
	r.collection("targets").InsertOne(ctx, target)
	r.collection("jobs").InsertOne(ctx, job)
	prepared := 0
	runner := NewRunner(r, RunnerDependencies{Owner: "review", Store: corruptReleaseStore{}, Authorize: func(context.Context, Job, bool) error { return nil }, Remote: func(context.Context, Job, Target) (Remote, error) { return &runnerRemote{}, nil }, Prepare: func(context.Context, Job, Target) (ReleaseManifest, error) { prepared++; return ReleaseManifest{}, nil }})
	if e := runner.RunOnce(ctx); e != nil {
		t.Fatal(e)
	}
	if prepared != 0 {
		t.Fatal("corrupted immutable release was silently reconstructed")
	}
}

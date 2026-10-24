package staticdelivery

import (
	"context"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"os"
	"testing"
	"time"
)

type runnerRemote struct {
	targetRemote
	activateCalls int
	activationErr error
	onActivate    func()
	releaseID     string
}

func (f *runnerRemote) UploadAssets(context.Context, Target, []UploadFile) (CompletionToken, error) {
	return "ephemeral-jwt", nil
}
func (f *runnerRemote) UploadVersion(context.Context, Target, CompletionToken, []Module) (RemoteVersion, error) {
	return RemoteVersion{ID: "new-version"}, nil
}
func (f *runnerRemote) Activate(_ context.Context, t Target, v string) (RemoteDeployment, error) {
	f.activateCalls++
	if f.onActivate != nil {
		f.onActivate()
	}
	f.state = RemoteState{Exists: true, SingleVersion: true, InstallationID: t.InstallationID, TargetID: t.ID.Hex(), ReleaseID: f.releaseID, VersionID: v, DeploymentID: "new-deployment"}
	if f.activationErr != nil {
		return RemoteDeployment{}, f.activationErr
	}
	return RemoteDeployment{ID: "new-deployment", VersionID: v}, nil
}
func TestRunnerPersistsIntentAndCommitsVerifiedRelease(t *testing.T) {
	r, ctx := connectDeliveryTestDB(t)
	target := releaseTarget()
	target.Status = "ready"
	target.RemoteVersionID = "old-version"
	target.AccountRef = primitive.NewObjectID()
	account := Account{Record: newRecord(), Status: "enabled", CredentialRevision: 1}
	account.ID = target.AccountRef
	if _, err := r.collection("accounts").InsertOne(ctx, account); err != nil {
		t.Fatal(err)
	}
	job := Job{Record: newRecord(), OperationID: primitive.NewObjectID(), State: JobQueued, Kind: "deploy", TargetID: target.ID, ReleaseID: primitive.NewObjectID(), ConfigRevision: target.ConfigRevision}
	_, e := r.collection("targets").InsertOne(ctx, target)
	if e != nil {
		t.Fatal(e)
	}
	r.collection("jobs").InsertOne(ctx, job)
	manifest, e := BuildRelease(ctx, BuildInput{ID: job.ReleaseID, Target: target, Pages: []CapturedPage{releasePage(primitive.NewObjectID(), "/a")}})
	if e != nil {
		t.Fatal(e)
	}
	f := &runnerRemote{targetRemote: targetRemote{state: RemoteState{Exists: true, SingleVersion: true, InstallationID: target.InstallationID, TargetID: target.ID.Hex(), VersionID: "old-version"}}, releaseID: job.ReleaseID.Hex()}
	store := &memoryReleaseStore{}
	runner := NewRunner(r, RunnerDependencies{Owner: "worker-1", Lease: time.Minute, Authorize: func(context.Context, Job, bool) error { return nil }, Remote: func(context.Context, Job, Target) (Remote, error) { return f, nil }, Prepare: func(context.Context, Job, Target) (ReleaseManifest, error) { return manifest, nil }, Store: store, Verify: func(context.Context, ReleaseManifest) error { return nil }})
	f.onActivate = func() {
		var persisted Job
		r.collection("jobs").FindOne(ctx, bson.M{"_id": job.ID}).Decode(&persisted)
		if persisted.State != JobActivating || persisted.Checkpoint.ActivationIntentAt == nil || persisted.Checkpoint.RemoteVersionID != "new-version" {
			t.Error("activation before durable intent", persisted.State)
		}
	}
	if e = runner.RunOnce(ctx); e != nil {
		t.Fatal(e)
	}
	var done Job
	r.collection("jobs").FindOne(ctx, bson.M{"_id": job.ID}).Decode(&done)
	latest, _ := r.GetTarget(ctx, target.ID)
	if done.State != JobSucceeded || latest.CurrentReleaseID != job.ReleaseID || !latest.ActiveJobID.IsZero() || latest.CurrentRevision != 1 || latest.RemoteVersionID != "new-version" || f.activateCalls != 1 {
		t.Fatalf("job=%+v target=%+v calls=%d", done, latest, f.activateCalls)
	}
	if e = runner.RunOnce(ctx); e != nil || f.activateCalls != 1 {
		t.Fatal("repeat activation", e)
	}
}

type memoryReleaseStore struct{ manifest ReleaseManifest }

func (s *memoryReleaseStore) Save(_ context.Context, r ReleaseManifest) error {
	s.manifest = r
	return nil
}
func (s *memoryReleaseStore) Load(context.Context, primitive.ObjectID) (ReleaseManifest, error) {
	if s.manifest.ID.IsZero() {
		return ReleaseManifest{}, os.ErrNotExist
	}
	return s.manifest, nil
}

func TestActivationUnknownTakeoverNeverRepeatsPOST(t *testing.T) {
	r, ctx := connectDeliveryTestDB(t)
	target := releaseTarget()
	target.Status = "ready"
	job := Job{Record: newRecord(), OperationID: primitive.NewObjectID(), State: JobActivationUnknown, TargetID: target.ID, ReleaseID: primitive.NewObjectID(), ConfigRevision: 1, Checkpoint: Checkpoint{ActivationIntentAt: func() *time.Time { now := time.Now().UTC(); return &now }(), RemoteVersionID: "candidate", PreviousVersionID: "old"}}
	target.ActiveJobID = job.ID
	r.collection("targets").InsertOne(ctx, target)
	r.collection("jobs").InsertOne(ctx, job)
	f := &runnerRemote{targetRemote: targetRemote{state: RemoteState{Exists: true, SingleVersion: true, InstallationID: target.InstallationID, TargetID: target.ID.Hex(), VersionID: "old"}}}
	mutations := 0
	runner := NewRunner(r, RunnerDependencies{Owner: "new-worker", Authorize: func(_ context.Context, _ Job, readOnly bool) error {
		if !readOnly {
			mutations++
		}
		return nil
	}, Remote: func(context.Context, Job, Target) (Remote, error) { return f, nil }, Store: &memoryReleaseStore{}})
	if e := runner.RunOnce(ctx); e != nil {
		t.Fatal(e)
	}
	var got Job
	r.collection("jobs").FindOne(ctx, bson.M{"_id": job.ID}).Decode(&got)
	latest, _ := r.GetTarget(ctx, target.ID)
	if got.State != JobActivationUnknown || latest.ActiveJobID != job.ID || f.activateCalls != 0 || mutations != 0 {
		t.Fatalf("unknown released/reposted %+v calls=%d", got, f.activateCalls)
	}
	if e := r.CancelJob(ctx, job.ID); CodeOf(e) != CodeActivationUnknown {
		t.Fatal("cancel crossed intent", e)
	}
}
func TestCancelBeforeIntentReleasesTarget(t *testing.T) {
	r, ctx := connectDeliveryTestDB(t)
	target := releaseTarget()
	target.Status = "ready"
	job := Job{Record: newRecord(), OperationID: primitive.NewObjectID(), State: JobUploading, TargetID: target.ID}
	target.ActiveJobID = job.ID
	r.collection("targets").InsertOne(ctx, target)
	r.collection("jobs").InsertOne(ctx, job)
	if e := r.CancelJob(ctx, job.ID); e != nil {
		t.Fatal(e)
	}
	latest, _ := r.GetTarget(ctx, target.ID)
	var got Job
	r.collection("jobs").FindOne(ctx, bson.M{"_id": job.ID}).Decode(&got)
	if got.State != JobCancelled || !latest.ActiveJobID.IsZero() {
		t.Fatal("cancellation not atomic")
	}
}

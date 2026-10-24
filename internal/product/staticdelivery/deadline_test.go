package staticdelivery

import (
	"context"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"testing"
	"time"
)

func TestDeploymentAgeExpiresBeforeAnyRemoteCall(t *testing.T) {
	repo, ctx := connectDeliveryTestDB(t)
	target := releaseTarget()
	target.Status = "ready"
	job := Job{Record: newRecord(), Kind: "deploy", State: JobQueued, TargetID: target.ID, ConfigRevision: target.ConfigRevision, ReleaseID: primitive.NewObjectID()}
	job.CreatedAt = time.Now().Add(-6 * time.Minute)
	target.ActiveJobID = job.ID
	repo.collection("targets").InsertOne(ctx, target)
	repo.collection("jobs").InsertOne(ctx, job)
	calls := 0
	runner := NewRunner(repo, RunnerDependencies{Owner: "deadline", Authorize: func(context.Context, Job, bool) error { return nil }, Remote: func(context.Context, Job, Target) (Remote, error) { calls++; return &runnerRemote{}, nil }})
	if err := runner.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var got Job
	repo.collection("jobs").FindOne(ctx, bson.M{"_id": job.ID}).Decode(&got)
	latest, _ := repo.GetTarget(ctx, target.ID)
	if got.State != JobFailed || calls != 0 || !latest.ActiveJobID.IsZero() {
		t.Fatalf("expired job state=%s calls=%d owner=%s", got.State, calls, latest.ActiveJobID)
	}
}

func TestExpiredActivationOnlyReconcilesAndKeepsTarget(t *testing.T) {
	repo, ctx := connectDeliveryTestDB(t)
	target := releaseTarget()
	target.Status = "ready"
	target.RemoteVersionID = "old"
	now := time.Now().Add(-6 * time.Minute)
	job := Job{Record: newRecord(), Kind: "deploy", State: JobActivationUnknown, TargetID: target.ID, ReleaseID: primitive.NewObjectID(), ConfigRevision: target.ConfigRevision, Checkpoint: Checkpoint{ActivationIntentAt: &now, PreviousVersionID: "old", RemoteVersionID: "candidate"}}
	job.CreatedAt = now
	target.ActiveJobID = job.ID
	repo.collection("targets").InsertOne(ctx, target)
	repo.collection("jobs").InsertOne(ctx, job)
	remote := &runnerRemote{targetRemote: targetRemote{state: RemoteState{Exists: true, SingleVersion: true, InstallationID: target.InstallationID, TargetID: target.ID.Hex(), VersionID: "old"}}}
	runner := NewRunner(repo, RunnerDependencies{Owner: "recovery", Authorize: func(_ context.Context, _ Job, recovery bool) error {
		if !recovery {
			t.Error("expired activation attempted mutation authorization")
		}
		return nil
	}, Remote: func(context.Context, Job, Target) (Remote, error) { return remote, nil }})
	if err := runner.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var got Job
	repo.collection("jobs").FindOne(ctx, bson.M{"_id": job.ID}).Decode(&got)
	latest, _ := repo.GetTarget(ctx, target.ID)
	if got.State != JobActivationUnknown || !got.Checkpoint.DeadlineExceeded || latest.ActiveJobID != job.ID || remote.activateCalls != 0 {
		t.Fatalf("unsafe expired recovery: %+v", got)
	}
}

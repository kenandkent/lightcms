package staticdelivery

import (
	"context"
	cf "github.com/metavert-io/lightcms/v7/internal/product/staticdelivery/cloudflare"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"testing"
)

type rejectedActivationRemote struct{ runnerRemote }

func (r *rejectedActivationRemote) Activate(context.Context, Target, string) (RemoteDeployment, error) {
	r.activateCalls++
	return RemoteDeployment{}, &cf.APIError{Status: 400, Detail: "invalid deployment", Uncertain: false}
}
func TestDefiniteActivationRejectionFailsAndReleasesTarget(t *testing.T) {
	repo, ctx := connectDeliveryTestDB(t)
	target := releaseTarget()
	target.Status = "ready"
	target.RemoteVersionID = "old"
	target.AccountRef = primitive.NewObjectID()
	account := Account{Record: newRecord(), Status: "enabled", CredentialRevision: 1}
	account.ID = target.AccountRef
	if _, err := repo.collection("accounts").InsertOne(ctx, account); err != nil {
		t.Fatal(err)
	}
	job := Job{Record: newRecord(), Kind: "deploy", State: JobVersionReady, TargetID: target.ID, ReleaseID: primitive.NewObjectID(), ConfigRevision: target.ConfigRevision, Checkpoint: Checkpoint{PreviousVersionID: "old", RemoteVersionID: "candidate"}}
	target.ActiveJobID = job.ID
	if _, err := repo.collection("targets").InsertOne(ctx, target); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.collection("jobs").InsertOne(ctx, job); err != nil {
		t.Fatal(err)
	}
	remote := &rejectedActivationRemote{runnerRemote: runnerRemote{targetRemote: targetRemote{state: RemoteState{Exists: true, SingleVersion: true, InstallationID: target.InstallationID, TargetID: target.ID.Hex(), VersionID: "old"}}}}
	runner := NewRunner(repo, RunnerDependencies{Owner: "rejection", Store: &memoryReleaseStore{}, Authorize: func(context.Context, Job, bool) error { return nil }, Remote: func(context.Context, Job, Target) (Remote, error) { return remote, nil }})
	if err := runner.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var got Job
	if err := repo.collection("jobs").FindOne(ctx, bson.M{"_id": job.ID}).Decode(&got); err != nil {
		t.Fatal(err)
	}
	latest, err := repo.GetTarget(ctx, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != JobFailed || !latest.ActiveJobID.IsZero() || remote.activateCalls != 1 {
		t.Fatalf("definite rejection not released: state=%s target=%s", got.State, latest.ActiveJobID)
	}
}

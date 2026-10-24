package staticdelivery

import (
	"context"
	"github.com/metavert-io/lightcms/v7/internal/product/artifacts"
	"github.com/metavert-io/lightcms/v7/internal/product/jobs"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"testing"
)

func TestRecoveryAdoptsOnlyVerifiedKnownReleaseWithoutActiveJob(t *testing.T) {
	repo, ctx := connectDeliveryTestDB(t)
	target := releaseTarget()
	target.Status = "drifted"
	manifest, err := BuildRelease(ctx, BuildInput{ID: primitive.NewObjectID(), Target: target, Pages: []CapturedPage{releasePage(primitive.NewObjectID(), "/known")}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = repo.collection("targets").InsertOne(ctx, target); err != nil {
		t.Fatal(err)
	}
	state := RemoteState{Exists: true, SingleVersion: true, InstallationID: target.InstallationID, TargetID: target.ID.Hex(), ReleaseID: manifest.ID.Hex(), VersionID: "known-version"}
	verified := 0
	// 812 MiB estimate fits; the old hard-coded 1 GiB request would reject it.
	store := &sizedReleaseStore{memoryReleaseStore: memoryReleaseStore{manifest: manifest}}
	svc := NewCommandService(repo, CommandOptions{Store: store, Admission: artifacts.NewAdmission(900 << 20), Authorize: func(context.Context, ActorSnapshot, bool, bool) error { return nil }, InspectTarget: func(context.Context, Target) (RemoteState, error) { return state, nil }, VerifyRecovery: func(context.Context, ReleaseManifest) error { verified++; return nil }})
	actor := ActorSnapshot{Owner: "admin", UserID: "admin", Role: "admin"}
	if _, err = svc.PreviewRecovery(ctx, actor, target.ID); CodeOf(err) != CodeRemoteDrift {
		t.Fatalf("unrecorded version accepted: %v", err)
	}
	known := Job{Record: newRecord(), TargetID: target.ID, ReleaseID: manifest.ID, State: JobSucceeded, Checkpoint: Checkpoint{RemoteVersionID: "known-version"}}
	if _, err = repo.collection("jobs").InsertOne(ctx, known); err != nil {
		t.Fatal(err)
	}
	proof, err := svc.PreviewRecovery(ctx, actor, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	busy := primitive.NewObjectID()
	if _, err = repo.collection("targets").UpdateOne(ctx, bson.M{"_id": target.ID}, bson.M{"$set": bson.M{"active_job_id": busy}}); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.ConfirmRecovery(ctx, actor, target.ID, proof.Digest, true); CodeOf(err) != CodeTargetBusy {
		t.Fatalf("unknown active job overwritten: %v", err)
	}
	if _, err = repo.collection("targets").UpdateOne(ctx, bson.M{"_id": target.ID}, bson.M{"$set": bson.M{"active_job_id": primitive.NilObjectID}}); err != nil {
		t.Fatal(err)
	}
	recovered, err := svc.ConfirmRecovery(ctx, actor, target.ID, proof.Digest, true)
	if err != nil {
		t.Fatal(err)
	}
	latest, err := repo.GetTarget(ctx, target.ID)
	if err != nil || latest.CurrentReleaseID != manifest.ID || latest.Status != "ready" || latest.CurrentRevision != 1 || verified != 1 {
		t.Fatalf("recovery mismatch %+v %v verified=%d", latest, err, verified)
	}
	replay, err := svc.ConfirmRecovery(ctx, actor, target.ID, proof.Digest, true)
	if err != nil || replay.ID != recovered.ID || verified != 1 {
		t.Fatalf("recovery replay was not idempotent: %v", err)
	}
	preparer := jobs.NewRunner(jobs.NewRepository(repo.db, 10), 1)
	svc.EnablePreviewJobs(preparer)
	if _, err := svc.GetRecoveryJob(ctx, actor, primitive.NewObjectID()); CodeOf(err) != CodeNotFound {
		t.Fatalf("missing recovery returned wrong code: %v", err)
	}
	proof, err = svc.PreviewRecovery(ctx, actor, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	queued, err := svc.SubmitRecovery(ctx, actor, target.ID, proof.Digest, true)
	if err != nil || queued.State != "queued" || verified != 1 {
		t.Fatalf("recovery performed verification in HTTP request: %+v %v", queued, err)
	}
	if err := preparer.RunOnce(ctx, "delivery_recovery"); err != nil {
		t.Fatal(err)
	}
	finished, err := svc.GetRecoveryJob(ctx, actor, queued.JobID)
	if err != nil || finished.State != "succeeded" || finished.DeliveryJobID.IsZero() || verified != 2 {
		t.Fatalf("durable recovery failed: %+v %v", finished, err)
	}
}

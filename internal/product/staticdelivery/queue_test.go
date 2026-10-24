package staticdelivery

import (
	"context"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"path/filepath"
	"testing"
	"time"
)

func TestQueuedClaimRebasesOnlyAfterPreviousTargetOwnerReleases(t *testing.T) {
	repo, ctx := connectDeliveryTestDB(t)
	root := t.TempDir()
	store := NewFileReleaseStore(Config{ProjectRoot: root, Root: filepath.Join(root, "private")})
	target := releaseTarget()
	target.Status = "ready"
	confirmed, err := BuildRelease(ctx, BuildInput{ID: primitive.NewObjectID(), Target: target, Pages: []CapturedPage{releasePage(primitive.NewObjectID(), "/queued")}})
	if err != nil {
		t.Fatal(err)
	}
	current, err := BuildRelease(ctx, BuildInput{ID: primitive.NewObjectID(), Target: target, Pages: []CapturedPage{releasePage(primitive.NewObjectID(), "/other")}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(ctx, confirmed); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(ctx, current); err != nil {
		t.Fatal(err)
	}
	target.CurrentRevision = 1
	target.CurrentReleaseID = current.ID
	target.ActiveJobID = primitive.NewObjectID()
	if _, err := repo.collection("targets").InsertOne(ctx, target); err != nil {
		t.Fatal(err)
	}
	j := Job{Record: newRecord(), State: JobQueued, Kind: "deploy", QueueMode: queueMerge, TargetID: target.ID, ReleaseID: confirmed.ID, ConfirmedReleaseID: confirmed.ID, ExpectedRevision: 0, ConfigRevision: target.ConfigRevision, ExpectedPaths: map[string]string{"/queued": ""}, LeaseOwner: "queue-worker", LeaseUntil: time.Now().Add(time.Minute)}
	if _, err := repo.collection("jobs").InsertOne(ctx, j); err != nil {
		t.Fatal(err)
	}
	runner := NewRunner(repo, RunnerDependencies{Owner: "queue-worker", Store: store})
	if claimed, err := runner.claimQueuedRelease(ctx, &j, target); err != nil || claimed {
		t.Fatalf("queue overtook prior target owner: %v %v", claimed, err)
	}
	if _, err := repo.collection("targets").UpdateOne(ctx, bson.M{"_id": target.ID}, bson.M{"$set": bson.M{"active_job_id": primitive.NilObjectID}}); err != nil {
		t.Fatal(err)
	}
	target, err = repo.GetTarget(ctx, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := runner.claimQueuedRelease(ctx, &j, target); err != nil || !claimed {
		t.Fatalf("queue failed to claim new baseline: %v %v", claimed, err)
	}
	next, err := store.Load(ctx, j.ReleaseID)
	if err != nil || len(next.Pages) != 2 || next.Pages["/other"].SHA256 != current.Pages["/other"].SHA256 {
		t.Fatalf("rebase lost prior release: %+v %v", next, err)
	}
	head, err := repo.GetTarget(ctx, target.ID)
	if err != nil || head.ActiveJobID != j.ID || head.CurrentReleaseID != current.ID || j.ExpectedRevision != 1 {
		t.Fatalf("queue committed head before activation: %+v %v", head, err)
	}
}

func TestQueuedReleasePreservesUnrelatedPagesAndRejectsConflict(t *testing.T) {
	ctx := context.Background()
	target := releaseTarget()
	confirmed, err := BuildRelease(ctx, BuildInput{ID: primitive.NewObjectID(), Target: target, Pages: []CapturedPage{releasePage(primitive.NewObjectID(), "/queued")}})
	if err != nil {
		t.Fatal(err)
	}
	current, err := BuildRelease(ctx, BuildInput{ID: primitive.NewObjectID(), Target: target, Pages: []CapturedPage{releasePage(primitive.NewObjectID(), "/other")}})
	if err != nil {
		t.Fatal(err)
	}
	target.CurrentRevision = 1
	next, err := rebaseQueuedRelease(ctx, target, confirmed, &current, map[string]string{"/queued": ""})
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Pages) != 2 || next.Pages["/other"].SHA256 != current.Pages["/other"].SHA256 || next.Pages["/queued"].PublicationID != confirmed.Pages["/queued"].PublicationID {
		t.Fatal("queued rebase lost an unrelated page or changed selected input")
	}
	conflict, err := BuildRelease(ctx, BuildInput{ID: primitive.NewObjectID(), Target: target, Current: &current, Pages: []CapturedPage{releasePage(primitive.NewObjectID(), "/queued")}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rebaseQueuedRelease(ctx, target, confirmed, &conflict, map[string]string{"/queued": ""}); CodeOf(err) != CodeRevisionConflict {
		t.Fatalf("same-path conflict silently rebased: %v", err)
	}
}

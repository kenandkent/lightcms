package generation

import (
	"context"
	"fmt"
	"github.com/metavert-io/lightcms/v7/internal/product/jobs"
	"github.com/metavert-io/lightcms/v7/internal/testutil"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"testing"
)

func TestDurableUpgradeRequiresFreshAuthorization(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	ctx := context.Background()
	for _, name := range []string{"workflow_jobs", "workflow_items", "workflow_capacity", "workflow_events", CollectionUpgradeJobs} {
		defer db.Collection(name).Drop(ctx)
	}
	service := NewService(db, Options{})
	runner := jobs.NewRunner(jobs.NewRepository(db, 10), 1)
	service.EnableDurableUpgrades(runner, func(context.Context, Actor) error { return genErr(CodePermissionDenied, "revoked", nil) })
	actor := Actor{ID: primitive.NewObjectID().Hex(), Authenticated: true, Role: "admin", IsAdmin: true}
	if _, err := service.SubmitUpgradeRun(ctx, actor, primitive.NewObjectID()); CodeOf(err) != CodePermissionDenied {
		t.Fatalf("revoked caller submitted upgrade: %v", err)
	}
}

func TestDurableUpgradeRevocationFinalizesLegacyStatus(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	ctx := context.Background()
	s := NewService(db, Options{})
	runner := jobs.NewRunner(jobs.NewRepository(db, 10), 1)
	revoked := false
	s.EnableDurableUpgrades(runner, func(context.Context, Actor) error {
		if revoked {
			return fmt.Errorf("revoked")
		}
		return nil
	})
	id := primitive.NewObjectID()
	if _, err := db.InsertOne(ctx, CollectionUpgradeJobs, UpgradeJob{ID: id, Status: UpgradeJobRunning}); err != nil {
		t.Fatal(err)
	}
	actor := Actor{ID: primitive.NewObjectID().Hex(), Authenticated: true, Role: "admin", IsAdmin: true}
	job, err := s.SubmitUpgradeRun(ctx, actor, id)
	if err != nil {
		t.Fatal(err)
	}
	revoked = true
	if err := runner.RunOnce(ctx, "template_upgrade"); err != nil {
		t.Fatal(err)
	}
	var old UpgradeJob
	if err := db.FindOne(ctx, CollectionUpgradeJobs, bson.M{"_id": id}, &old); err != nil {
		t.Fatal(err)
	}
	current, err := runner.Repo.Get(ctx, job.ID)
	if err != nil || current.State != "failed" || old.Status != UpgradeJobPartial {
		t.Fatalf("terminal workflow left legacy running: %+v %+v %v", current, old, err)
	}
}

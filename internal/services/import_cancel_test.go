package services

import (
	"context"
	"testing"

	"github.com/metavert-io/lightcms/v7/internal/models"
	"github.com/metavert-io/lightcms/v7/internal/testutil"
	"go.mongodb.org/mongo-driver/bson"
)

func TestImportCancelledCannotFinalizeDone(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	ctx := context.Background()
	s := NewImportService(db, NewContentService(db))
	id, err := db.InsertOne(ctx, "import_jobs", bson.M{"status": models.ImportStatusRunning})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CancelJob(ctx, id); err != nil {
		t.Fatal(err)
	}
	s.finalizeJob(ctx, id, models.ImportStatusDone, "", 3, 1, 0, 0)
	var job models.ImportJob
	if err := db.FindOne(ctx, "import_jobs", bson.M{"_id": id}, &job); err != nil {
		t.Fatal(err)
	}
	if job.Status != models.ImportStatusCancelled {
		t.Fatalf("cancel overwritten by %s", job.Status)
	}
	if err := s.writeContent(ctx, id, &models.Content{Title: "Late", Slug: "late"}, true); err == nil {
		t.Fatal("cancelled job wrote a new item")
	}
	if n, err := db.Count(ctx, "content", bson.M{"slug": "late"}); err != nil || n != 0 {
		t.Fatalf("late item exists: %d %v", n, err)
	}
}

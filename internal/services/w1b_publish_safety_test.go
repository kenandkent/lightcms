package services

// Lane 1B (publish-path safety) regression tests:
//  1. scheduler runOnce must skip fork rows and pending-approval rows.
//  2. GetContentByPath must never return a fork row (UpsertContent must
//     update the live page, never the sandbox copy).

import (
	"context"
	"testing"
	"time"

	"github.com/jonradoff/lightcms/v7/internal/models"
	"github.com/jonradoff/lightcms/v7/internal/testutil"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// seedDueRow inserts a minimal due (publish_at in the past, unpublished)
// content row with extra fields merged in. Raw docs omit path_active, so the
// partial UNIQUE(canonical_full_path, path_scope) index never conflicts.
func seedDueRow(t *testing.T, db interface {
	InsertOne(context.Context, string, interface{}) (primitive.ObjectID, error)
}, ctx context.Context, fullPath string, extra bson.M) primitive.ObjectID {
	t.Helper()
	doc := bson.M{
		"template_name": "Test Template",
		"title":         "due " + fullPath,
		"slug":          fullPath,
		"full_path":     fullPath,
		"published":     false,
		"publish_at":    time.Now().Add(-time.Minute),
		"deleted":       false,
		"created_at":    time.Now(),
		"updated_at":    time.Now(),
	}
	for k, v := range extra {
		doc[k] = v
	}
	id, err := db.InsertOne(ctx, "content", doc)
	if err != nil {
		t.Fatalf("seed due row %s: %v", fullPath, err)
	}
	return id
}

func TestScheduler_SkipsForkAndUnapproved(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	cs := NewContentService(db)
	sched := NewSchedulerService(db, cs)
	ctx := context.Background()

	tmplID := createTestTemplate(t, cs)

	// Control: due live draft created through the service (full shape).
	live := &models.Content{
		TemplateID: tmplID, TemplateName: "Test Template",
		Title: "W1B Live Due", Slug: "w1b-live-due",
		Data: map[string]interface{}{"content": "<p>live</p>"},
	}
	if err := cs.CreateContent(ctx, live); err != nil {
		t.Fatalf("CreateContent: %v", err)
	}
	if err := db.UpdateOne(ctx, "content", bson.M{"_id": live.ID},
		bson.M{"$set": bson.M{"publish_at": time.Now().Add(-time.Minute)}}); err != nil {
		t.Fatalf("set publish_at: %v", err)
	}

	// Fork draft due now: must NOT go live.
	forkID := primitive.NewObjectID()
	forkRow := seedDueRow(t, db, ctx, "/w1b-fork-due", bson.M{"fork_id": forkID})

	// Pending-approval submission due now: must NOT go live.
	pendingRow := seedDueRow(t, db, ctx, "/w1b-pending-due", bson.M{"pending_approval": true})

	// Future-scheduled live draft: must stay unpublished (control negative).
	futureRow := seedDueRow(t, db, ctx, "/w1b-future", bson.M{"publish_at": time.Now().Add(time.Hour)})

	sched.runOnce(ctx)

	gotLive, err := cs.GetContent(ctx, live.ID)
	if err != nil {
		t.Fatalf("GetContent live: %v", err)
	}
	if !gotLive.Published {
		t.Error("expected due live draft to be published by scheduler")
	}

	for name, id := range map[string]primitive.ObjectID{
		"fork": forkRow, "pending-approval": pendingRow, "future": futureRow,
	} {
		got, err := cs.GetContent(ctx, id)
		if err != nil {
			t.Fatalf("GetContent %s: %v", name, err)
		}
		if got.Published {
			t.Errorf("scheduler published %s row %s — must stay unpublished", name, id.Hex())
		}
	}
}

func TestGetContentByPath_NeverReturnsFork(t *testing.T) {
	svc, cleanup := newTestContentService(t)
	defer cleanup()
	ctx := context.Background()
	tmplID := createTestTemplate(t, svc)

	live := &models.Content{
		TemplateID: tmplID, TemplateName: "Test Template",
		Title: "W1B Live Original", Slug: "w1b-clash",
		Data: map[string]interface{}{"content": "<p>live</p>"},
	}
	if err := svc.CreateContent(ctx, live); err != nil {
		t.Fatalf("CreateContent: %v", err)
	}

	// Real fork copy sharing the live full_path.
	fs := NewForkService(svc.db, svc)
	forkRec, err := fs.Create(ctx, "w1b-fork", "", primitive.NewObjectID(), "w1b@test")
	if err != nil {
		t.Fatalf("fork create: %v", err)
	}
	forkCopy, err := fs.ForkPage(ctx, forkRec.ID, live.ID)
	if err != nil {
		t.Fatalf("ForkPage: %v", err)
	}
	if err := svc.db.UpdateOne(ctx, "content", bson.M{"_id": forkCopy.ID},
		bson.M{"$set": bson.M{"title": "W1B Fork Edit"}}); err != nil {
		t.Fatalf("edit fork copy: %v", err)
	}

	// Exact lookup must return the live page, never the fork copy.
	got, err := svc.GetContentByPath(ctx, "/w1b-clash")
	if err != nil {
		t.Fatalf("GetContentByPath: %v", err)
	}
	if got.ForkID != nil {
		t.Errorf("GetContentByPath returned a fork row (fork_id set)")
	}
	if got.Title != "W1B Live Original" {
		t.Errorf("expected live title, got %q", got.Title)
	}

	// Case-insensitive fallback must also skip the fork copy.
	gotCI, err := svc.GetContentByPath(ctx, "/W1B-CLASH")
	if err != nil {
		t.Fatalf("GetContentByPath case-variant: %v", err)
	}
	if gotCI.ForkID != nil || gotCI.Title != "W1B Live Original" {
		t.Errorf("case-insensitive lookup returned fork row: %+v", gotCI)
	}

	// Upsert at the clashed path must update live, leaving the fork copy alone.
	upsert := &models.Content{
		TemplateID: tmplID, TemplateName: "Test Template",
		Title: "W1B Live Updated", Slug: "w1b-clash",
		Data: map[string]interface{}{"content": "<p>updated</p>"},
	}
	created, err := svc.UpsertContent(ctx, upsert, "w1b upsert")
	if err != nil {
		t.Fatalf("UpsertContent: %v", err)
	}
	if created {
		t.Error("UpsertContent reported created, want updated (live page exists)")
	}
	afterLive, _ := svc.GetContent(ctx, live.ID)
	if afterLive.Title != "W1B Live Updated" {
		t.Errorf("live page not updated, title=%q", afterLive.Title)
	}
	afterFork, _ := svc.GetContent(ctx, forkCopy.ID)
	if afterFork.Title != "W1B Fork Edit" {
		t.Errorf("fork copy was modified by upsert, title=%q", afterFork.Title)
	}
}

func TestGetContentByPath_ForkOnlyIsNotFound(t *testing.T) {
	svc, cleanup := newTestContentService(t)
	defer cleanup()
	ctx := context.Background()
	tmplID := createTestTemplate(t, svc)

	live := &models.Content{
		TemplateID: tmplID, TemplateName: "Test Template",
		Title: "W1B Doomed", Slug: "w1b-fork-only",
		Data: map[string]interface{}{"content": "<p>x</p>"},
	}
	if err := svc.CreateContent(ctx, live); err != nil {
		t.Fatalf("CreateContent: %v", err)
	}
	fs := NewForkService(svc.db, svc)
	forkRec, err := fs.Create(ctx, "w1b-fork2", "", primitive.NewObjectID(), "w1b@test")
	if err != nil {
		t.Fatalf("fork create: %v", err)
	}
	if _, err := fs.ForkPage(ctx, forkRec.ID, live.ID); err != nil {
		t.Fatalf("ForkPage: %v", err)
	}
	// Remove the live page: only the fork copy remains at the path.
	if err := svc.DeleteContent(ctx, live.ID); err != nil {
		t.Fatalf("DeleteContent: %v", err)
	}

	if _, err := svc.GetContentByPath(ctx, "/w1b-fork-only"); err == nil {
		t.Error("GetContentByPath returned the fork copy after live delete, want not-found")
	}
	if _, err := svc.GetContentByPath(ctx, "/W1B-FORK-ONLY"); err == nil {
		t.Error("GetContentByPath case-variant returned the fork copy, want not-found")
	}
}

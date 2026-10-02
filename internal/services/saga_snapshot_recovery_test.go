package services

import (
	"bytes"
	"context"
	"testing"

	"github.com/jonradoff/lightcms/v7/internal/product/idempotency"
	"github.com/jonradoff/lightcms/v7/internal/testutil"
	"go.mongodb.org/mongo-driver/bson"
)

// A dependency edit between render and crash recovery must not change the
// frozen attempt; a new attempt must report the new dependency hash.
func TestFinalReviewSnapshotSurvivesDependencyEdit(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	ctx := context.Background()
	if err := db.EnsureProductIndexes(ctx); err != nil {
		t.Fatal(err)
	}
	defer db.Collection("snippets").DeleteMany(ctx, bson.M{"name": "frozen-review"})
	if _, err := db.Collection("snippets").InsertOne(ctx, bson.M{"name": "frozen-review", "html": "<b>Original</b>"}); err != nil {
		t.Fatal(err)
	}
	in := r02Input(true)
	in.Content.Data = map[string]any{"headline": "Literal", "body": "[[include:frozen-review]]"}
	idem, err := idempotency.NewService(db, idempotency.Options{})
	if err != nil {
		t.Fatal(err)
	}
	op, err := idem.Begin(ctx, "snapshot-review", "POST", "/render", "freeze", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx = idempotency.WithLeaseGeneration(ctx, op.LeaseGeneration)
	if err = idem.BindContentAndVersion(ctx, nil, op.ID, in.Content.ID, 1, in.Content.FullPath); err != nil {
		t.Fatal(err)
	}
	if err = idem.FreezeExecution(ctx, op.ID, op.Attempt, in.PublicationID, in.LogicalPublishedAt, in.Template.ID); err != nil {
		t.Fatal(err)
	}
	svc := NewContentService(db)
	first, err := svc.SagaSnapshotRender(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Collection("snippets").UpdateOne(ctx, bson.M{"name": "frozen-review"}, bson.M{"$set": bson.M{"html": "<b>Changed</b>"}}); err != nil {
		t.Fatal(err)
	}
	recovered, err := svc.SagaSnapshotRender(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.HTML, recovered.HTML) {
		t.Fatal("same attempt reread changed dependencies")
	}
	newInput := r02Input(true)
	newInput.Content.Data = in.Content.Data
	newRender, err := svc.SagaSnapshotRender(context.Background(), newInput)
	if err != nil {
		t.Fatal(err)
	}
	if first.RenderDependenciesHash == newRender.RenderDependenciesHash {
		t.Fatal("actual snippet edit not represented in dependency hash")
	}
}

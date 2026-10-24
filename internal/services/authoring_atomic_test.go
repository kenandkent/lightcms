package services

import (
	"context"
	"errors"
	"testing"

	"github.com/metavert-io/lightcms/v7/internal/models"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

func TestAuthoringAtomicHistoryFailure(t *testing.T) {
	s, cleanup := newTestContentService(t)
	defer cleanup()
	ctx := context.Background()
	c := &models.Content{Title: "Original", Slug: "atomic-original"}
	if err := s.CreateContent(ctx, c); err != nil {
		t.Fatal(err)
	}
	version := c.CurrentVersion
	s.db.SetFaultHook(func(op, collection string) error {
		if collection == "content_versions" && (op == "InsertOne" || op == "InsertMany") {
			return errors.New("history unavailable")
		}
		return nil
	})
	defer s.db.SetFaultHook(nil)
	c.Title = "Must not commit"
	if err := s.UpdateContent(ctx, c); err == nil {
		t.Fatal("expected history failure")
	}
	var stored models.Content
	if err := s.db.FindOne(ctx, "content", bson.M{"_id": c.ID}, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Title != "Original" || stored.CurrentVersion != version {
		t.Fatalf("partial update committed: %+v", stored)
	}
	created := &models.Content{Title: "Must not exist", Slug: "atomic-create"}
	if err := s.CreateContent(ctx, created); err == nil {
		t.Fatal("expected create failure")
	}
	if n, err := s.db.Count(ctx, "content", bson.M{"slug": created.Slug}); err != nil || n != 0 {
		t.Fatalf("partial create: count=%d error=%v", n, err)
	}
	results := s.BulkCreateContent(ctx, []*models.Content{{Title: "Bulk", Slug: "atomic-bulk"}}, "test")
	if results[0].Success {
		t.Fatal("bulk reported success without history")
	}
	if n, err := s.db.Count(ctx, "content", bson.M{"slug": "atomic-bulk"}); err != nil || n != 0 {
		t.Fatalf("partial bulk create: count=%d error=%v", n, err)
	}
}

func TestAuthoringAtomicOuterRollback(t *testing.T) {
	s, cleanup := newTestContentService(t)
	defer cleanup()
	ctx := context.Background()
	effects := 0
	err := s.db.WithTransaction(ctx, func(sc mongo.SessionContext) error {
		if err := s.CreateContent(sc, &models.Content{Title: "Nested", Slug: "nested"}); err != nil {
			return err
		}
		s.db.AfterCommit(sc, func(context.Context) { effects++ })
		return errors.New("outer operation failed")
	})
	if err == nil || effects != 0 {
		t.Fatalf("aborted transaction emitted effects: %v / %d", err, effects)
	}
	if n, err := s.db.Count(ctx, "content", bson.M{"slug": "nested"}); err != nil || n != 0 {
		t.Fatalf("outer rollback failed: %d %v", n, err)
	}
}

func TestAuthoringAtomicBulkPeerAndCompleteHistory(t *testing.T) {
	s, cleanup := newTestContentService(t)
	defer cleanup()
	ctx := context.Background()
	calls := 0
	s.db.SetFaultHook(func(op, collection string) error {
		if op == "InsertOne" && collection == "content_versions" {
			calls++
			if calls == 1 {
				return errors.New("first history failed")
			}
		}
		return nil
	})
	defer s.db.SetFaultHook(nil)
	items := []*models.Content{{Title: "Bad", Slug: "bad"}, {Title: "Good", Slug: "good", FolderPath: "/articles", MetaDescription: "Complete snapshot", UseTheme: true}}
	results := s.BulkCreateContent(ctx, items, "import")
	if results[0].Success || !results[1].Success {
		t.Fatalf("incorrect per-item outcomes: %+v", results)
	}
	var version models.ContentVersion
	if err := s.db.FindOne(ctx, "content_versions", bson.M{"content_id": items[1].ID}, &version); err != nil {
		t.Fatal(err)
	}
	if version.FullPath != "/articles/good" || !version.UseTheme || version.MetaDescription != "Complete snapshot" {
		t.Fatalf("incomplete history: %+v", version)
	}
}

func TestAuthoringAtomicRetryPreservesCAS(t *testing.T) {
	s, cleanup := newTestContentService(t)
	defer cleanup()
	ctx := context.Background()
	c := &models.Content{Title: "Original", Slug: "retry-cas"}
	if err := s.CreateContent(ctx, c); err != nil {
		t.Fatal(err)
	}
	injected := false
	s.db.SetFaultHook(func(op, collection string) error {
		if op == "UpdateOne" && collection == "content" && !injected {
			injected = true
			if _, err := s.db.Collection("content").UpdateOne(ctx, bson.M{"_id": c.ID}, bson.M{"$set": bson.M{"title": "Other writer", "current_version": 2}}); err != nil {
				return err
			}
			return mongo.CommandError{Code: 112, Message: "write conflict", Labels: []string{"TransientTransactionError"}}
		}
		return nil
	})
	defer s.db.SetFaultHook(nil)
	c.Title = "Stale edit"
	if err := s.UpdateContent(ctx, c); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("retry silently rebased stale edit: %v", err)
	}
	stored, err := s.GetContent(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Title != "Other writer" {
		t.Fatal("concurrent edit was overwritten")
	}
}

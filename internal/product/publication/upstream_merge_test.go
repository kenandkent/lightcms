package publication_test

import (
	"context"
	"testing"
	"time"

	"github.com/metavert-io/lightcms/v7/internal/models"
	"github.com/metavert-io/lightcms/v7/internal/product/publication"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestUpstreamHoldAndForkFlagsSurvivePublicationIntegration(t *testing.T) {
	s := newSagaSetup(t, nil, nil, publication.Options{})
	ctx := context.Background()
	tpl, tv := seedSagaTemplateVersion(t, s.db, "", true)
	id := seedSagaContent(t, s.db, tpl, "/merge-guards", 1, nil)
	for _, flag := range []bson.M{{"hold": true}, {"fork_id": primitive.NewObjectID()}} {
		if err := s.db.UpdateOne(ctx, "content", bson.M{"_id": id}, bson.M{"$set": flag}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.svc.Publish(ctx, publication.PublishRequest{ContentID: id, TemplateVersionID: tv}); publication.CodeOf(err) != publication.CodeConflict {
			t.Fatalf("flag %v allowed publication: %v", flag, err)
		}
		if active, err := s.repo.GetActive(ctx, id); err != nil || active != nil {
			t.Fatalf("rejected publication changed active state: %+v %v", active, err)
		}
		if err := s.db.UpdateOne(ctx, "content", bson.M{"_id": id}, bson.M{"$unset": bson.M{"hold": "", "fork_id": ""}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.svc.Publish(ctx, publication.PublishRequest{ContentID: id, TemplateVersionID: tv}); err != nil {
		t.Fatal("cleared flags did not restore publishing", err)
	}
}

func TestPublicationActivationPreservesSEODateForIdenticalBytes(t *testing.T) {
	db, repo := testRepo(t)
	ctx := context.Background()
	id := seedContent(t, db)
	first := stagedPub(id)
	if err := repo.InsertStaged(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := repo.ActivateCAS(ctx, id, first.ID, nil); err != nil {
		t.Fatal(err)
	}
	var content models.Content
	if err := db.FindOne(ctx, "content", bson.M{"_id": id}, &content); err != nil || content.ContentModifiedAt == nil {
		t.Fatalf("first publication did not stamp modification date: %v", err)
	}
	original := time.Unix(123, 0).UTC()
	if err := db.UpdateOne(ctx, "content", bson.M{"_id": id}, bson.M{"$set": bson.M{"content_modified_at": original}}); err != nil {
		t.Fatal(err)
	}
	same := stagedPub(id)
	same.ContentHash = first.ContentHash
	if err := repo.InsertStaged(ctx, same); err != nil {
		t.Fatal(err)
	}
	if err := repo.ActivateCAS(ctx, id, same.ID, &first.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.FindOne(ctx, "content", bson.M{"_id": id}, &content); err != nil || content.ContentModifiedAt == nil || !content.ContentModifiedAt.Equal(original) {
		t.Fatalf("identical bytes moved SEO date: %+v %v", content.ContentModifiedAt, err)
	}
	changed := stagedPub(id)
	changed.ContentHash = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	if err := repo.InsertStaged(ctx, changed); err != nil {
		t.Fatal(err)
	}
	if err := repo.ActivateCAS(ctx, id, changed.ID, &same.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.FindOne(ctx, "content", bson.M{"_id": id}, &content); err != nil || content.ContentModifiedAt == nil || !content.ContentModifiedAt.After(original) {
		t.Fatalf("changed bytes did not move SEO date: %+v %v", content.ContentModifiedAt, err)
	}
}

func TestHoldAlsoBlocksRollbackToRetainedPublication(t *testing.T) {
	s := newSagaSetup(t, nil, nil, publication.Options{})
	ctx := context.Background()
	tpl, tv := seedSagaTemplateVersion(t, s.db, "", true)
	id := seedSagaContent(t, s.db, tpl, "/rollback-held", 1, nil)
	first, err := s.svc.Publish(ctx, publication.PublishRequest{ContentID: id, TemplateVersionID: tv})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.svc.Unpublish(ctx, publication.UnpublishRequest{ContentID: id}); err != nil {
		t.Fatal(err)
	}
	if err := s.db.UpdateOne(ctx, "content", bson.M{"_id": id}, bson.M{"$set": bson.M{"hold": true}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.svc.Rollback(ctx, publication.RollbackRequest{ContentID: id, SourcePublicationID: first.PublicationID}); publication.CodeOf(err) != publication.CodeConflict {
		t.Fatalf("rollback bypassed hold: %v", err)
	}
	if active, err := s.repo.GetActive(ctx, id); err != nil || active != nil {
		t.Fatalf("held page became active: %+v %v", active, err)
	}
}

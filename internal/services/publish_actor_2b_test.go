package services

// Lane 2B Fix 4 (red, legacy path): ContentService.PublishContent and
// PublishInternal delegate to the saga with a bare {ContentID} request,
// dropping the caller provenance the middleware already stamped on ctx.
// The saga therefore mints actor-less Publication rows.

import (
	"context"
	"testing"

	"github.com/jonradoff/lightcms/v7/internal/models"
	"github.com/jonradoff/lightcms/v7/internal/product/publication"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

type actorRecordingPublisher struct {
	last publication.PublishRequest
}

func (f *actorRecordingPublisher) Publish(_ context.Context, req publication.PublishRequest) (publication.PublicationResult, error) {
	f.last = req
	return publication.PublicationResult{ContentID: req.ContentID}, nil
}

func (f *actorRecordingPublisher) Unpublish(_ context.Context, _ publication.UnpublishRequest) error {
	return nil
}

func TestPublishContent_ThreadsActorFromContext(t *testing.T) {
	svc, cleanup := newTestContentService(t)
	defer cleanup()
	defer SetPublicationPublisher(nil)
	fake := &actorRecordingPublisher{}
	SetPublicationPublisher(fake)

	ctx := context.Background()
	tmplID := createTestTemplate(t, svc)
	c := newTestContent(t, svc, tmplID)

	sessCtx := WithProvenance(WithEditorEmail(ctx, "ed@test.com"),
		Provenance{Actor: "agent", Via: "api", AgentSession: "sess-legacy-1"})
	if err := svc.PublishContent(sessCtx, c.ID); err != nil {
		t.Fatalf("PublishContent: %v", err)
	}
	if fake.last.ContentID != c.ID {
		t.Fatalf("saga request content = %s, want %s", fake.last.ContentID.Hex(), c.ID.Hex())
	}
	if fake.last.Actor != "agent" {
		t.Fatalf("saga request actor = %q, want %q", fake.last.Actor, "agent")
	}
	if fake.last.Via != "api" {
		t.Fatalf("saga request via = %q, want %q", fake.last.Via, "api")
	}
	if fake.last.AgentSession != "sess-legacy-1" {
		t.Fatalf("saga request session = %q, want %q", fake.last.AgentSession, "sess-legacy-1")
	}
}

func newTestContent(t *testing.T, svc *ContentService, tmplID primitive.ObjectID) *models.Content {
	t.Helper()
	c := &models.Content{
		TemplateID: tmplID, TemplateName: "Test Template",
		Title: "Actor Page", Slug: "actor-page",
		Data: map[string]interface{}{"content": "x"},
	}
	if err := svc.CreateContent(context.Background(), c); err != nil {
		t.Fatalf("seed content: %v", err)
	}
	return c
}

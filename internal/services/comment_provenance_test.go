package services

// Lane 2C fix 2 (red): CommentService.Create must refuse orphan comments
// (404-able ErrContentNotFound when the content row is missing) and must
// stamp provenance/session from the middleware context following the
// content version-row pattern (EditorEmail + Provenance, Actor "human"
// default).

import (
	"context"
	"errors"
	"testing"

	"github.com/jonradoff/lightcms/v7/internal/models"
	"github.com/jonradoff/lightcms/v7/internal/testutil"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

func seedCommentContent(t *testing.T, db interface {
	InsertOne(ctx context.Context, collection string, doc interface{}) (primitive.ObjectID, error)
}, ctx context.Context) primitive.ObjectID {
	t.Helper()
	id, err := db.InsertOne(ctx, "content", &models.Content{
		Title: "Comment Doc", Slug: "comment-doc", FullPath: "/comment-doc",
	})
	if err != nil {
		t.Fatalf("seed content: %v", err)
	}
	return id
}

func TestCommentService_CreateRequiresExistingContent(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()

	svc := NewCommentService(db)
	ctx := context.Background()
	_, err := svc.Create(ctx, primitive.NewObjectID(), primitive.NewObjectID(),
		"u@x.com", "User", "orphan comment", nil)
	if err == nil {
		t.Fatal("expected error for missing content, got nil")
	}
	if !errors.Is(err, ErrContentNotFound) {
		t.Fatalf("error = %v, want ErrContentNotFound", err)
	}
}

func TestCommentService_CreateStampsProvenance(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()

	svc := NewCommentService(db)
	ctx := context.Background()
	contentID := seedCommentContent(t, db, ctx)

	sessCtx := WithProvenance(WithEditorEmail(ctx, "bot@x.com"),
		Provenance{Actor: "agent", Via: "api", AgentSession: "agent-sess-1"})
	c, err := svc.Create(sessCtx, contentID, primitive.NewObjectID(),
		"bot@x.com", "Bot", "agent remark", nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if c.Actor != "agent" {
		t.Errorf("Actor = %q, want agent", c.Actor)
	}
	if c.Via != "api" {
		t.Errorf("Via = %q, want api", c.Via)
	}
	if c.AgentSession != "agent-sess-1" {
		t.Errorf("AgentSession = %q, want agent-sess-1", c.AgentSession)
	}
}

func TestCommentService_CreateDefaultsHumanProvenance(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()

	svc := NewCommentService(db)
	ctx := context.Background()
	contentID := seedCommentContent(t, db, ctx)

	c, err := svc.Create(ctx, contentID, primitive.NewObjectID(),
		"u@x.com", "User", "plain remark", nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if c.Actor != "human" {
		t.Errorf("Actor = %q, want human default", c.Actor)
	}
	if c.AgentSession != "" {
		t.Errorf("AgentSession = %q, want empty", c.AgentSession)
	}
}

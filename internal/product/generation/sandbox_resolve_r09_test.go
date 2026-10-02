package generation_test

// R09 regression: mode=sandbox binds to the caller's owned active fork via
// SandboxForkResolver — never to a client-supplied fork ID alone, and never
// without a session-bound active fork. Writes land only in the fork.

import (
	"context"
	"testing"
	"time"

	"github.com/jonradoff/lightcms/v7/internal/product/generation"
	"github.com/jonradoff/lightcms/v7/internal/services"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func seedR09Fork(t *testing.T, s *genSetup, uid primitive.ObjectID, session, status string) primitive.ObjectID {
	t.Helper()
	id := primitive.NewObjectID()
	if _, err := s.db.InsertOne(context.Background(), "content_forks", bson.M{
		"_id": id, "name": "r09", "status": status,
		"preview_token": "tok", "created_by": uid,
		"created_by_email": "e@x.com", "agent_session": session,
		"created_at": time.Now(),
	}); err != nil {
		t.Fatalf("seed fork: %v", err)
	}
	return id
}

func r09WiredGen(s *genSetup) *generation.Service {
	return generation.NewService(s.db, generation.Options{
		SandboxForkResolver: func(ctx context.Context, userID, session string) (*primitive.ObjectID, error) {
			return services.FindActiveSandboxFork(ctx, s.db, userID, session)
		},
	})
}

func TestSandboxResolverBindsOwnedFork(t *testing.T) {
	s := newGenSetup(t, generation.Options{})
	ctx := context.Background()
	s.seedTemplate(t, "r09-news", "", nil)

	uid := primitive.NewObjectID()
	forkID := seedR09Fork(t, s, uid, "sess-r09", "active")
	gen := r09WiredGen(s)

	actor := generation.Actor{
		Role: "admin", ID: uid.Hex(), Email: "admin@r09.test", Authenticated: true,
		IsAdmin: true, Scopes: []string{}, ActorKind: "agent", Via: "api",
		AgentSession: "sess-r09",
	}
	mkReq := func(slug string) generation.GenerateRequest {
		return generation.GenerateRequest{
			Template: "r09-news", Title: "R09 " + slug, Slug: slug, FolderPath: "/news",
			Mode: "sandbox", Data: map[string]any{"headline": "r09"},
		}
	}
	if _, err := gen.Generate(ctx, actor, mkReq("r09-page")); err != nil {
		t.Fatalf("sandbox with owned active fork: %v", err)
	}
	// The write landed ONLY in the fork: no live row at the path, one fork row.
	var live bson.M
	if err := s.db.FindOne(ctx, "content",
		bson.M{"full_path": "/news/r09-page", "fork_id": bson.M{"$exists": false}}, &live); err == nil {
		t.Fatal("sandbox write leaked to live content")
	}
	var forked bson.M
	if err := s.db.FindOne(ctx, "content",
		bson.M{"full_path": "/news/r09-page", "fork_id": forkID}, &forked); err != nil {
		t.Fatalf("sandbox write missing from fork: %v", err)
	}

	// Wrong session: no owned active fork resolves → denied, zero mutation.
	before, _ := s.db.Collection("content").CountDocuments(ctx, bson.M{})
	actor.AgentSession = "no-such-session"
	if _, err := gen.Generate(ctx, actor, mkReq("r09-denied")); err == nil ||
		generation.CodeOf(err) != generation.CodeAgentSandboxRequired {
		t.Fatalf("sandbox wrong session = %v, want AGENT_SANDBOX_REQUIRED", err)
	}
	// Spoofed fork ID: pre-set to another fork while owning forkID.
	other := seedR09Fork(t, s, uid, "sess-r09-other", "active")
	actor.AgentSession = "sess-r09"
	actor.SandboxForkID = &other
	if _, err := gen.Generate(ctx, actor, mkReq("r09-spoof")); err == nil ||
		generation.CodeOf(err) != generation.CodePermissionDenied {
		t.Fatalf("sandbox spoofed fork = %v, want PERMISSION_DENIED", err)
	}
	actor.SandboxForkID = nil
	// Merged fork: inactive → denied.
	seedR09Fork(t, s, uid, "sess-r09-merged", "merged")
	actor.AgentSession = "sess-r09-merged"
	if _, err := gen.Generate(ctx, actor, mkReq("r09-merged")); err == nil ||
		generation.CodeOf(err) != generation.CodeAgentSandboxRequired {
		t.Fatalf("sandbox merged fork = %v, want AGENT_SANDBOX_REQUIRED", err)
	}
	after, _ := s.db.Collection("content").CountDocuments(ctx, bson.M{})
	if after != before {
		t.Fatalf("denied sandbox writes mutated content: before=%d after=%d", before, after)
	}
}

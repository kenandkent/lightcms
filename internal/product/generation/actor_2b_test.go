package generation_test

// Lane 2B Fix 4 (red): the publish saga mints Publication records without
// actor attribution. Generation (and other callers) know the actor — it must
// be threaded through Publish/Rollback requests into the stored record.

import (
	"context"
	"testing"

	"github.com/jonradoff/lightcms/v7/internal/product/generation"
)

func TestPublishCarriesActorAttribution(t *testing.T) {
	s := newGenSetup(t, generation.Options{})
	ctx := context.Background()
	_, _, tv := s.seedTemplate(t, "financial-news", "", nil)

	actor := generation.Actor{
		Role: "admin",
		ID:   "agent-7", Email: "agent@example.com", Authenticated: true,
		IsAdmin: true, Scopes: []string{},
		ActorKind: "agent", Via: "api", AgentSession: "sess-2b-actor",
	}
	req := generation.GenerateRequest{
		Template: "financial-news", Title: "Actor Page", Slug: "actor-page", FolderPath: "/news",
		Mode: "publish", ExpectedTemplateVersion: &tv, Data: map[string]any{"headline": "attributed"},
	}
	pctx := pubCtx(actor, "k-actor-1", req, tv)
	res, err := s.gen.Generate(pctx, actor, req)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if res.PublicationID == nil {
		t.Fatal("publish must return a publication id")
	}
	active, err := s.repo.GetActive(ctx, mustOID(t, res.ID))
	if err != nil {
		t.Fatalf("GetActive: %v", err)
	}
	if active == nil {
		t.Fatal("no active publication")
	}
	if active.Actor != "agent" {
		t.Fatalf("publication actor = %q, want %q", active.Actor, "agent")
	}
	if active.Via != "api" {
		t.Fatalf("publication via = %q, want %q", active.Via, "api")
	}
	if active.AgentSession != "sess-2b-actor" {
		t.Fatalf("publication agent_session = %q, want %q", active.AgentSession, "sess-2b-actor")
	}
}

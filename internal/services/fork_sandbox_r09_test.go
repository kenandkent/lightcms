package services

// R09 regression: server-side agent-sandbox binding. Forks record the
// creating agent session; FindActiveSandboxFork resolves only an owned
// ACTIVE fork for (user, session) — merged/archived forks, other users'
// forks, and empty sessions never resolve (fail closed).

import (
	"context"
	"testing"
	"time"

	"github.com/jonradoff/lightcms/v7/internal/testutil"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestFindActiveSandboxFork(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	ctx := context.Background()

	uid := primitive.NewObjectID()
	other := primitive.NewObjectID()
	insert := func(uid primitive.ObjectID, session, status string) primitive.ObjectID {
		t.Helper()
		id := primitive.NewObjectID()
		if _, err := db.InsertOne(ctx, "content_forks", bson.M{
			"_id": id, "name": "sbx", "status": status,
			"preview_token": "tok", "created_by": uid,
			"created_by_email": "e@x.com", "agent_session": session,
			"created_at": time.Now(),
		}); err != nil {
			t.Fatalf("seed fork: %v", err)
		}
		t.Cleanup(func() {
			_, _ = db.Collection("content_forks").DeleteOne(context.Background(), bson.M{"_id": id})
		})
		return id
	}
	activeID := insert(uid, "sess-r09", "active")
	insert(uid, "sess-r09-merged", "merged")
	insert(other, "sess-r09", "active")

	got, err := FindActiveSandboxFork(ctx, db, uid.Hex(), "sess-r09")
	if err != nil {
		t.Fatalf("active fork: %v", err)
	}
	if *got != activeID {
		t.Fatalf("resolved %s, want %s", got.Hex(), activeID.Hex())
	}
	if _, err := FindActiveSandboxFork(ctx, db, uid.Hex(), "sess-r09-merged"); err == nil {
		t.Fatal("merged fork resolved, want error")
	}
	if _, err := FindActiveSandboxFork(ctx, db, uid.Hex(), "no-such-session"); err == nil {
		t.Fatal("unknown session resolved, want error")
	}
	if _, err := FindActiveSandboxFork(ctx, db, other.Hex(), "sess-r09-merged"); err == nil {
		t.Fatal("other user's fork resolved for wrong session, want error")
	}
	if _, err := FindActiveSandboxFork(ctx, db, uid.Hex(), ""); err == nil {
		t.Fatal("empty session resolved, want error")
	}
	if _, err := FindActiveSandboxFork(ctx, db, "not-hex", "sess-r09"); err == nil {
		t.Fatal("invalid user ID resolved, want error")
	}
}

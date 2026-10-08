package services

// Admin copy/reveal flow: sealed storage round-trips, permission gates,
// legacy keys fail closed, and rotation orphans old seals loudly.

import (
	"context"
	"errors"
	"testing"

	"github.com/jonradoff/lightcms/v7/internal/testutil"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestDeriveKeyEncryptionKey(t *testing.T) {
	a := DeriveKeyEncryptionKey("test-secret-min-32-chars-abcdef")
	b := DeriveKeyEncryptionKey("test-secret-min-32-chars-abcdef")
	if len(a) != 32 || string(a) != string(b) {
		t.Fatalf("derive must be deterministic 32 bytes, got %d", len(a))
	}
	if string(a) == string(DeriveKeyEncryptionKey("other-secret")) {
		t.Fatal("different secrets must derive different keys")
	}
}

func TestRevealAPIKeyRoundTrip(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	ctx := context.Background()
	svc := NewAPIKeyService(db)
	svc.SetEncryptionKey(DeriveKeyEncryptionKey("test-secret-min-32-chars-abcdef"))

	uid := primitive.NewObjectID()
	raw, key, err := svc.CreateAPIKeyForUser(ctx, "reveal-me", "d", &uid)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if key.KeyCiphertext == "" {
		t.Fatal("sealed copy must be stored when encryption is wired")
	}
	// Owner reveals.
	if got, err := svc.RevealAPIKey(ctx, key.ID, uid.Hex(), false); err != nil || got != raw {
		t.Fatalf("owner reveal = %q, %v (want exact raw key)", got, err)
	}
	// Admin (non-owner) reveals.
	if got, err := svc.RevealAPIKey(ctx, key.ID, primitive.NewObjectID().Hex(), true); err != nil || got != raw {
		t.Fatalf("admin reveal = %q, %v", got, err)
	}
	// Stranger denied.
	if _, err := svc.RevealAPIKey(ctx, key.ID, primitive.NewObjectID().Hex(), false); !errors.Is(err, ErrAPIKeyForbidden) {
		t.Fatalf("stranger reveal = %v, want ErrAPIKeyForbidden", err)
	}
	// Missing key.
	if _, err := svc.RevealAPIKey(ctx, primitive.NewObjectID(), uid.Hex(), false); !errors.Is(err, ErrAPIKeyNotFound) {
		t.Fatalf("missing reveal = %v, want ErrAPIKeyNotFound", err)
	}
}

func TestRevealAPIKeyLegacyAndRotation(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	ctx := context.Background()

	// Legacy: created with no encryption wired → fail closed with guidance.
	plain := NewAPIKeyService(db)
	uid := primitive.NewObjectID()
	_, key, err := plain.CreateAPIKeyForUser(ctx, "legacy", "d", &uid)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := plain.RevealAPIKey(ctx, key.ID, uid.Hex(), false); !errors.Is(err, ErrAPIKeyNotRevealable) {
		t.Fatalf("legacy reveal = %v, want ErrAPIKeyNotRevealable", err)
	}

	// Rotation: a different server secret cannot open the old seal.
	a := NewAPIKeyService(db)
	a.SetEncryptionKey(DeriveKeyEncryptionKey("secret-A-min-32-chars-xxxxxxxx"))
	_, keyA, err := a.CreateAPIKeyForUser(ctx, "rot", "d", &uid)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	b := NewAPIKeyService(db)
	b.SetEncryptionKey(DeriveKeyEncryptionKey("secret-B-min-32-chars-xxxxxxxx"))
	if _, err := b.RevealAPIKey(ctx, keyA.ID, uid.Hex(), false); err == nil {
		t.Fatal("rotated secret must not open the old seal")
	}
	// Same secret reopens fine.
	a2 := NewAPIKeyService(db)
	a2.SetEncryptionKey(DeriveKeyEncryptionKey("secret-A-min-32-chars-xxxxxxxx"))
	if _, err := a2.RevealAPIKey(ctx, keyA.ID, uid.Hex(), false); err != nil {
		t.Fatalf("same-secret reopen: %v", err)
	}
}

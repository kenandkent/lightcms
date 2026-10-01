package services

// Lane 1C (chain atomicity) red-green tests: search index staleness.
//
// Scope: triggerEmbedding was unbounded fire-and-forget with no version pin —
// rapid updates let older text overwrite newer, and a 2000-page bulk fired
// 2000 concurrent provider calls. The fix pins content_version on the async
// job (a stale job's write is skipped once a newer version is indexed) and
// bounds concurrency with embeddingMaxInFlight (documented on the constant).
//
// llms-full.txt note: ServeLlmsFullTxt (internal/handlers/llms.go, Lane B
// owned — NOT modified here) renders the stored plain_text field verbatim.
// These tests prove plain_text converges to the latest version's text after
// out-of-order embed jobs, which is exactly the "llms-full.txt eventually
// consistent" property at the data layer.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jonradoff/lightcms/v7/internal/database"
	"github.com/jonradoff/lightcms/v7/internal/models"
	"github.com/jonradoff/lightcms/v7/internal/testutil"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// seedPinnedContent inserts a content doc with an explicit CurrentVersion for
// pin tests.
func seedPinnedContent(t *testing.T, db *database.DB, title, body string, version int64) primitive.ObjectID {
	t.Helper()
	id := primitive.NewObjectID()
	now := time.Now().Add(-time.Hour)
	c := &models.Content{
		ID: id, Title: title, Slug: "pin-" + id.Hex()[:8], FullPath: "/pin-" + id.Hex()[:8],
		Published: true, CurrentVersion: version,
		CreatedAt: now, UpdatedAt: now,
		Data: map[string]interface{}{"body": fmt.Sprintf("<p>%s</p>", body)},
	}
	if _, err := db.InsertOne(context.Background(), "content", c); err != nil {
		t.Fatalf("seedPinnedContent: %v", err)
	}
	return id
}

func loadPinnedContent(t *testing.T, svc *SearchService, id primitive.ObjectID) models.Content {
	t.Helper()
	var stored models.Content
	if err := svc.db.FindOne(context.Background(), "content", bson.M{"_id": id}, &stored); err != nil {
		t.Fatalf("reload pinned content: %v", err)
	}
	return stored
}

// setPinnedLiveText rewrites the live doc to a new version + body (what
// ContentService.UpdateContent does, minus the service wiring).
func setPinnedLiveText(t *testing.T, svc *SearchService, id primitive.ObjectID, version int64, body string) {
	t.Helper()
	if err := svc.db.UpdateOne(context.Background(), "content", bson.M{"_id": id}, bson.M{"$set": bson.M{
		"current_version": version,
		"data":            map[string]interface{}{"body": fmt.Sprintf("<p>%s</p>", body)},
		"updated_at":      time.Now(),
	}}); err != nil {
		t.Fatalf("setPinnedLiveText: %v", err)
	}
}

// TestEmbedPin_OutOfOrderJobsConvergeToLatest: a stale (older-version) embed
// job never overwrites a newer version's index. Jobs run newest-last here to
// simulate delayed older jobs finishing after newer ones.
func TestEmbedPin_OutOfOrderJobsConvergeToLatest(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	ctx := context.Background()

	fake, _ := newFakeOllama(t)
	t.Setenv("LIGHTCMS_EMBEDDINGS_PROVIDER", "ollama")
	t.Setenv("OLLAMA_URL", fake.URL)
	svc := NewSearchService(db, "")

	id := seedPinnedContent(t, db, "Pin Doc", "alpha-text-aaa111", 1)

	// v1 job wins first.
	if won, err := svc.UpdateContentEmbeddingForVersion(ctx, id, 1); err != nil || !won {
		t.Fatalf("v1 embed = (%v, %v), want (true, nil)", won, err)
	}
	if got := loadPinnedContent(t, svc, id); got.EmbeddingVersion != 1 {
		t.Fatalf("embedding_version = %d, want 1", got.EmbeddingVersion)
	}

	// Live advances to v2; v2 job wins.
	setPinnedLiveText(t, svc, id, 2, "beta-text-bbb222")
	if won, err := svc.UpdateContentEmbeddingForVersion(ctx, id, 2); err != nil || !won {
		t.Fatalf("v2 embed = (%v, %v), want (true, nil)", won, err)
	}
	if got := loadPinnedContent(t, svc, id); got.EmbeddingVersion != 2 {
		t.Fatalf("embedding_version = %d, want 2", got.EmbeddingVersion)
	}

	// Delayed stale v1 job (read old era, generated slowly): must skip, and
	// the stored text must stay on the v2 text.
	if won, err := svc.UpdateContentEmbeddingForVersion(ctx, id, 1); err != nil || won {
		t.Fatalf("stale v1 embed = (%v, %v), want (false, nil)", won, err)
	}
	stored := loadPinnedContent(t, svc, id)
	if stored.EmbeddingVersion != 2 {
		t.Errorf("embedding_version = %d, want 2 (stale job regressed the pin)", stored.EmbeddingVersion)
	}
	if !strings.Contains(stored.PlainText, "beta-text-bbb222") || strings.Contains(stored.PlainText, "alpha-text-aaa111") {
		t.Errorf("plain_text = %q, want v2 text only (older text overwrote newer)", stored.PlainText)
	}

	// The conditional-write guard itself: a stale-era write carrying stale
	// text is rejected even when it reaches the store layer (covers the race
	// where the stored version advanced DURING generation).
	if won, err := svc.storeEmbeddingIfNewer(ctx, id, 1, []float32{9, 9, 9}, "stale-text-should-never-land"); err != nil || won {
		t.Fatalf("stale store = (%v, %v), want (false, nil)", won, err)
	}
	if got := loadPinnedContent(t, svc, id); got.PlainText != stored.PlainText {
		t.Errorf("stale store overwrote plain_text: %q", got.PlainText)
	}
}

// TestEmbedPin_LlmsFullTextEventuallyConsistent: after rapid versions with
// jobs completing out of order, the stored plain_text — the exact field
// ServeLlmsFullTxt projects and renders — equals the latest version's text.
func TestEmbedPin_LlmsFullTextEventuallyConsistent(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	ctx := context.Background()

	fake, _ := newFakeOllama(t)
	t.Setenv("LIGHTCMS_EMBEDDINGS_PROVIDER", "ollama")
	t.Setenv("OLLAMA_URL", fake.URL)
	svc := NewSearchService(db, "")

	id := seedPinnedContent(t, db, "Storm Doc", "v1-text", 1)

	// Rapid updates v1 -> v2 -> v3 (as UpdateContent would produce them).
	setPinnedLiveText(t, svc, id, 2, "v2-text")
	setPinnedLiveText(t, svc, id, 3, "v3-final-text")

	// Jobs complete out of order: newest first, then stale ones trickle in.
	for _, v := range []int64{3, 1, 2} {
		if _, err := svc.UpdateContentEmbeddingForVersion(ctx, id, v); err != nil {
			t.Fatalf("embed v%d: %v", v, err)
		}
	}
	stored := loadPinnedContent(t, svc, id)
	if stored.EmbeddingVersion != 3 {
		t.Errorf("embedding_version = %d, want 3", stored.EmbeddingVersion)
	}
	if !strings.Contains(stored.PlainText, "v3-final-text") {
		t.Errorf("plain_text = %q, want the latest (v3) text — llms-full.txt would serve stale content", stored.PlainText)
	}
}

// TestEmbedPin_BulkStormBounded: a bulk storm of async triggers never exceeds
// embeddingMaxInFlight concurrent provider calls, and every trigger still
// completes (bounded, not dropped).
func TestEmbedPin_BulkStormBounded(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()

	var cur, watermark int64
	blocking := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt64(&cur, 1)
		for {
			m := atomic.LoadInt64(&watermark)
			if n <= m || atomic.CompareAndSwapInt64(&watermark, m, n) {
				break
			}
		}
		time.Sleep(250 * time.Millisecond) // widen every overlap window
		atomic.AddInt64(&cur, -1)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"embedding": []float32{0.5, 0.25, 0.125},
		})
	}))
	defer blocking.Close()
	t.Setenv("LIGHTCMS_EMBEDDINGS_PROVIDER", "ollama")
	t.Setenv("OLLAMA_URL", blocking.URL)

	cs := NewContentService(db)
	ss := NewSearchService(db, "")
	cs.SetSearchService(ss)

	const storm = 20 // well above embeddingMaxInFlight (8)
	ids := make([]primitive.ObjectID, 0, storm)
	for i := 0; i < storm; i++ {
		ids = append(ids, seedPinnedContent(t, db,
			fmt.Sprintf("Storm Page %d", i), fmt.Sprintf("storm-body-%d", i), 1))
	}
	for _, id := range ids {
		cs.triggerEmbedding(id, 1)
	}

	// Every trigger completes (poll for the writes to land).
	deadline := time.Now().Add(30 * time.Second)
	for _, id := range ids {
		for {
			var stored models.Content
			if err := db.FindOne(context.Background(), "content", bson.M{"_id": id}, &stored); err == nil && stored.EmbeddingAt != nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("embeddings did not all land within 30s (deadlock or drop?)")
			}
			time.Sleep(100 * time.Millisecond)
		}
	}

	if max := atomic.LoadInt64(&watermark); max > embeddingMaxInFlight {
		t.Fatalf("max concurrent provider calls = %d, want <= %d (embeddingMaxInFlight)", max, embeddingMaxInFlight)
	} else {
		t.Logf("bulk storm of %d triggers peaked at %d concurrent provider calls (limit %d)", storm, max, embeddingMaxInFlight)
	}
}

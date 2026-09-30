package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/jonradoff/lightcms/v7/internal/auth"
	"github.com/jonradoff/lightcms/v7/internal/middleware"
	"github.com/jonradoff/lightcms/v7/internal/product/idempotency"
	"github.com/jonradoff/lightcms/v7/internal/product/publication"
	"github.com/jonradoff/lightcms/v7/internal/product/publicurl"
	"github.com/jonradoff/lightcms/v7/internal/product/storage"
	"github.com/jonradoff/lightcms/v7/internal/product/templatecontract"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// Task 16C red-green: legacy single/batch publish URLs route through
// PublicationService — 428 without Idempotency-Key, Publication ID + Public
// URL with it, replay on same-key retry, no raw GenerateStaticPage call in
// the wired path (the canonical file is cut over by the saga only).
func Test16C_LegacyPublishRoutesThroughSaga(t *testing.T) {
	ah, db, cleanup := newTestAPIHandler(t)
	defer cleanup()
	ctx := context.Background()
	// Product indexes own the idempotency unique key + publication active
	// pointer; without them same-key retries cannot replay (production
	// main.go ensures these at startup — Task 16E).
	if err := db.EnsureProductIndexes(ctx); err != nil {
		t.Fatalf("EnsureProductIndexes: %v", err)
	}

	// Seed an active immutable template version (saga pins this).
	tvID := primitive.NewObjectID()
	tplID := primitive.NewObjectID()
	if _, err := db.Collection("templates").InsertOne(ctx, bson.M{
		"_id": tplID, "name": "News", "slug": "news", "current_version": int64(1),
		"created_at": time.Now(), "updated_at": time.Now(),
	}); err != nil {
		t.Fatalf("seed template: %v", err)
	}
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		db.Collection("templates").DeleteOne(cctx, bson.M{"_id": tplID}) //nolint:errcheck
	})
	if _, err := db.Collection("template_versions").InsertOne(ctx, bson.M{
		"_id": tvID, "template_id": tplID, "version": int64(1),
		"slug": "news", "name": "News", "status": templatecontract.StatusActive,
		"fields": []bson.M{}, "html_layout": "<html><body>{{.headline}}</body></html>",
		"contract_hash": "sha256:c", "render_hash": "sha256:r",
		"created_at": time.Now(),
	}); err != nil {
		t.Fatalf("seed template version: %v", err)
	}
	// Seed a draft content row with canonical bookkeeping (16A shape).
	contentID := primitive.NewObjectID()
	if _, err := db.Collection("content").InsertOne(ctx, bson.M{
		"_id": contentID, "template_id": tplID, "template_name": "News",
		"title": "T", "slug": "t", "folder_path": "/news", "full_path": "/news/t",
		"canonical_full_path": "/news/t", "path_scope": "live", "path_active": true,
		"current_version": int64(1), "data": bson.M{"headline": "hi"},
		"published": false, "created_at": time.Now(), "updated_at": time.Now(),
	}); err != nil {
		t.Fatalf("seed content: %v", err)
	}
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, col := range []string{"template_versions", "content_publications", "idempotency_records", "webhook_outbox"} {
			db.Collection(col).Drop(cctx) //nolint:errcheck
		}
	})

	// Wire the shared V3 runtime exactly like cmd/server/main.go.
	root := t.TempDir()
	store := storage.NewFilesystemStore(root)
	repo := publication.NewRepository(db, nil)
	idem, err := idempotency.NewService(db, idempotency.Options{})
	if err != nil {
		t.Fatalf("idempotency: %v", err)
	}
	base, _ := url.Parse("http://localhost:8082")
	resolver, err := publicurl.NewResolver(base)
	if err != nil {
		t.Fatalf("resolver: %v", err)
	}
	pubs := publication.NewService(db, repo, store, publication.Options{
		Idem: idem, URLs: resolver, BuildSHA: "test-16c",
	})
	ah.SetPublicationRuntime(pubs, idem, nil)

	admin := &auth.SessionUser{ID: primitive.NewObjectID().Hex(), Email: "admin@test", Role: "admin"}
	call := func(method, target, idHex string, key string, body string) *httptest.ResponseRecorder {
		var reader *strings.Reader
		if body == "" {
			reader = strings.NewReader("")
		} else {
			reader = strings.NewReader(body)
		}
		req := httptest.NewRequest(method, target, reader)
		req = mux.SetURLVars(req, map[string]string{"id": idHex})
		req = req.WithContext(middleware.InjectAPIUser(req.Context(), admin))
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		rr := httptest.NewRecorder()
		switch {
		case strings.HasSuffix(target, "/publish") && !strings.Contains(target, "batch"):
			ah.APIPublishContent(rr, req)
		case strings.Contains(target, "batch-publish"):
			ah.APIBatchPublishContent(rr, req)
		case strings.HasSuffix(target, "/unpublish"):
			ah.APIUnpublishContent(rr, req)
		}
		return rr
	}

	// 1. External single publish without a key → 428, zero mutation.
	rr := call("POST", "/api/v1/content/"+contentID.Hex()+"/publish", contentID.Hex(), "", "")
	if rr.Code != 428 {
		t.Fatalf("expected 428 without Idempotency-Key, got %d (%s)", rr.Code, rr.Body.String())
	}
	var cnt int64
	if cnt, err = db.Collection("content_publications").CountDocuments(ctx, bson.M{}); err != nil || cnt != 0 {
		t.Fatalf("428 must not mint a publication (count=%d, err=%v)", cnt, err)
	}

	// 2. With a key → 200 + publication_id + public_url; saga cuts over.
	rr = call("POST", "/api/v1/content/"+contentID.Hex()+"/publish", contentID.Hex(), "k-16c-1", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", rr.Code, rr.Body.String())
	}
	var published map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &published); err != nil {
		t.Fatalf("decode: %v", err)
	}
	pubID, _ := published["publication_id"].(string)
	publicURL, _ := published["public_url"].(string)
	if pubID == "" || publicURL == "" {
		t.Fatalf("expected publication_id + public_url, got %v", published)
	}
	canonical := filepath.Join(root, "generated", "news", "t.html")
	raw, err := os.ReadFile(canonical)
	if err != nil {
		t.Fatalf("saga canonical missing: %v", err)
	}
	if !strings.Contains(string(raw), "hi") {
		t.Fatalf("canonical should carry saga-rendered bytes, got %q", string(raw))
	}

	// 3. Same-key retry → replay of the SAME publication (no duplicate).
	rr = call("POST", "/api/v1/content/"+contentID.Hex()+"/publish", contentID.Hex(), "k-16c-1", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 replay, got %d (%s)", rr.Code, rr.Body.String())
	}
	var replay map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &replay); err != nil {
		t.Fatalf("decode replay: %v", err)
	}
	if replay["publication_id"] != pubID {
		t.Fatalf("same-key retry must replay %s, got %v", pubID, replay)
	}

	// 4. Batch publish with a key → per-item publication IDs.
	rr = call("POST", "/api/v1/content/batch-publish", "", "k-16c-batch",
		`{"ids":["`+contentID.Hex()+`"]}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("batch expected 200, got %d (%s)", rr.Code, rr.Body.String())
	}
	var batch map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &batch); err != nil {
		t.Fatalf("decode batch: %v", err)
	}
	items, _ := batch["publications"].([]any)
	if len(items) != 1 {
		t.Fatalf("expected 1 batch publication entry, got %v", batch)
	}
	if _, ok := items[0].(map[string]any)["publication_id"]; !ok {
		t.Fatalf("batch item must carry publication_id, got %v", items[0])
	}

	// 5. Batch without a key → 428.
	rr = call("POST", "/api/v1/content/batch-publish", "", "",
		`{"ids":["`+contentID.Hex()+`"]}`)
	if rr.Code != 428 {
		t.Fatalf("batch expected 428 without key, got %d (%s)", rr.Code, rr.Body.String())
	}

	// 6. Unpublish is naturally idempotent — no key required.
	rr = call("POST", "/api/v1/content/"+contentID.Hex()+"/unpublish", contentID.Hex(), "", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("unpublish expected 200, got %d (%s)", rr.Code, rr.Body.String())
	}
	if _, err := os.Stat(canonical); !os.IsNotExist(err) {
		t.Fatalf("unpublish must retire the canonical file")
	}
}

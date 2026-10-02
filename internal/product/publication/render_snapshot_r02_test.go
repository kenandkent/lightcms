package publication_test

// R02 wiring regression: the production saga renders through the frozen
// snapshot pipeline (SnapshotRender). An end-to-end Publish must emit
// converted Markdown (not raw **bold**), carry render provenance from the
// actual result, and fail closed on hostile vectors under policy none.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonradoff/lightcms/v7/internal/database"
	"github.com/jonradoff/lightcms/v7/internal/product/publication"
	"github.com/jonradoff/lightcms/v7/internal/product/storage"
	"github.com/jonradoff/lightcms/v7/internal/product/templatecontract"
	"github.com/jonradoff/lightcms/v7/internal/services"
	"github.com/jonradoff/lightcms/v7/internal/testutil"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func r02SagaSetup(t *testing.T) (*database.DB, *publication.Repository, storage.Store, *publication.Service, string) {
	t.Helper()
	db, _ := testutil.MustConnectTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := db.EnsureProductIndexes(ctx); err != nil {
		t.Fatalf("EnsureProductIndexes: %v", err)
	}
	root := t.TempDir()
	store := storage.NewFilesystemStore(root)
	repo := publication.NewRepository(db, nil)
	csvc := services.NewContentService(db)
	svc := publication.NewService(db, repo, store, publication.Options{
		SnapshotRender: csvc.SagaSnapshotRender,
	})
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, col := range []string{"content", "template_versions", "content_publications", "webhook_outbox", "settings", "snippets"} {
			db.Collection(col).Drop(cctx) //nolint:errcheck
		}
	})
	return db, repo, store, svc, root
}

func r02Seed(t *testing.T, db *database.DB, data map[string]any) (cid, tvID primitive.ObjectID) {
	t.Helper()
	ctx := context.Background()
	tplID := primitive.NewObjectID()
	tvID = primitive.NewObjectID()
	if _, err := db.Collection("template_versions").InsertOne(ctx, bson.M{
		"_id": tvID, "template_id": tplID, "version": int64(1),
		"slug": "r02", "name": "R02", "category": "news", "status": templatecontract.StatusActive,
		"fields": []bson.M{
			{"name": "headline", "label": "Headline", "type": "text", "required": true},
			{"name": "body", "label": "Body", "type": "markdown"},
		},
		"html_layout":   `<html><body><h1>{{.headline}}</h1><div>{{.body}}</div></body></html>`,
		"contract_hash": "sha256:c", "render_hash": "sha256:r",
		"created_at": time.Now(),
	}); err != nil {
		t.Fatalf("seed template version: %v", err)
	}
	cid = primitive.NewObjectID()
	if _, err := db.Collection("content").InsertOne(ctx, bson.M{
		"_id": cid, "template_id": tplID, "template_name": "R02",
		"title": "R02 Page", "slug": "r02-page", "full_path": "/r02-page",
		"canonical_full_path": "/r02-page", "path_scope": "live", "path_active": true,
		"current_version": int64(1), "data": data, "published": false,
		"created_at": time.Now(), "updated_at": time.Now(),
	}); err != nil {
		t.Fatalf("seed content: %v", err)
	}
	return cid, tvID
}

func TestSagaSnapshotRenderEndToEnd(t *testing.T) {
	db, repo, _, svc, root := r02SagaSetup(t)
	ctx := context.Background()

	cid, tvID := r02Seed(t, db, map[string]any{
		"headline": "Hello", "body": `Welcome **bold**`,
	})
	res, err := svc.Publish(ctx, publication.PublishRequest{
		ContentID: cid, TemplateVersionID: tvID, Reason: "r02",
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if res.PublicationID.IsZero() {
		t.Fatal("publish returned zero publication ID")
	}
	raw, err := os.ReadFile(filepath.Join(root, "generated", "r02-page.html"))
	if err != nil {
		t.Fatalf("read canonical: %v", err)
	}
	if !strings.Contains(string(raw), "<strong>bold</strong>") {
		t.Fatalf("markdown not converted in canonical bytes (DefaultRenderer would leave **bold** raw):\n%s", raw)
	}
	active, err := repo.GetActive(ctx, cid)
	if err != nil || active == nil {
		t.Fatalf("GetActive: %v %v", active, err)
	}
	if active.RendererVersion == "" {
		t.Error("record RendererVersion empty — must come from the actual render")
	}
	if active.DependencySnapshot == nil {
		t.Fatal("record DependencySnapshot missing — must come from the actual render")
	} else if _, ok := active.DependencySnapshot["markdown_processor"]; !ok {
		t.Fatalf("record DependencySnapshot lacks pipeline keys: %v", active.DependencySnapshot)
	}
}

func TestSagaSnapshotRenderRejectsHostileUnderNone(t *testing.T) {
	db, _, _, svc, root := r02SagaSetup(t)
	ctx := context.Background()
	if _, err := db.InsertOne(ctx, "settings", bson.M{
		"type": "site_config", "markdown_script_policy": "none",
		"title_template": "{{title}}", "max_upload_bytes": int64(1 << 20),
	}); err != nil {
		t.Fatalf("seed site_config: %v", err)
	}
	cid, tvID := r02Seed(t, db, map[string]any{
		"headline": `<img src=x onerror=alert(1)>`, "body": "hi",
	})
	if _, err := svc.Publish(ctx, publication.PublishRequest{
		ContentID: cid, TemplateVersionID: tvID, Reason: "r02-hostile",
	}); err == nil {
		t.Fatal("hostile text under policy none published, want render rejection")
	}
	if _, serr := os.Stat(filepath.Join(root, "generated", "r02-page.html")); !os.IsNotExist(serr) {
		t.Fatal("hostile render must not leave canonical bytes")
	}
}

package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/metavert-io/lightcms/v7/internal/auth"
	"github.com/metavert-io/lightcms/v7/internal/database"
	"github.com/metavert-io/lightcms/v7/internal/models"
	"github.com/metavert-io/lightcms/v7/internal/product/publication"
	sd "github.com/metavert-io/lightcms/v7/internal/product/staticdelivery"
	ex "github.com/metavert-io/lightcms/v7/internal/product/staticexport"
	"github.com/metavert-io/lightcms/v7/internal/product/storage"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestStaticDeliveryActorRecheckedBeforeExecution(t *testing.T) {
	uri := os.Getenv("STATIC_DELIVERY_TEST_MONGODB_URI")
	if uri == "" {
		if os.Getenv("STATIC_DELIVERY_REQUIRE_MONGO") == "1" {
			t.Fatal("isolated Mongo URI required")
		}
		t.Skip("isolated Mongo unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	db, e := database.Connect(ctx, uri, "static-delivery-test-"+primitive.NewObjectID().Hex())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		_ = db.Collection("_cleanup").Database().Drop(context.Background())
		_ = db.Disconnect(context.Background())
	})
	uid, kid := primitive.NewObjectID(), primitive.NewObjectID()
	if _, e = db.Collection("users").InsertOne(ctx, bson.M{"_id": uid, "role": "editor", "disabled": false}); e != nil {
		t.Fatal(e)
	}
	if _, e = db.Collection("api_keys").InsertOne(ctx, models.APIKey{ID: kid, UserID: &uid, Scopes: []string{auth.PermContentView, auth.PermContentPublish}}); e != nil {
		t.Fatal(e)
	}
	a := sd.ActorSnapshot{UserID: uid.Hex(), Owner: "apikey:" + kid.Hex(), Role: "editor", Scopes: []string{auth.PermContentView, auth.PermContentPublish}}
	check := deliveryActorAuthorization(db)
	if e = check(ctx, a, true, false); e != nil {
		t.Fatal(e)
	}
	if _, e = db.Collection("api_keys").UpdateOne(ctx, bson.M{"_id": kid}, bson.M{"$set": bson.M{"scopes": []string{auth.PermContentView}}}); e != nil {
		t.Fatal(e)
	}
	if e = check(ctx, a, true, false); sd.CodeOf(e) != sd.CodePermissionDenied {
		t.Fatal("revoked publish scope accepted", e)
	}
	if e = check(ctx, a, false, false); e != nil {
		t.Fatal("view-only export denied", e)
	}
	// Exercise production wiring with the actual Publication repository/store,
	// source lookup, durable export queue and startup worker (no CF calls).
	root := t.TempDir()
	cfg := sd.Config{ProjectRoot: root, Root: filepath.Join(root, "private")}
	admin := buildStaticDeliveryAdminRuntime(ctx, db, cfg)
	store := storage.NewFilesystemStore(filepath.Join(root, "content"))
	cid, pid, vid := primitive.NewObjectID(), primitive.NewObjectID(), primitive.NewObjectID()
	body := []byte(`<!doctype html><html><head><title>Published</title></head><body><h1>Frozen report</h1></body></html>`)
	if _, e = store.Stage(ctx, storage.StageRequest{ContentID: cid, PublicationID: pid, CanonicalPath: "/report", HTML: body}); e != nil {
		t.Fatal(e)
	}
	if _, e = db.Content().InsertOne(ctx, models.Content{ID: cid, Title: "Published", Published: true}); e != nil {
		t.Fatal(e)
	}
	if _, e = db.Collection("template_versions").InsertOne(ctx, bson.M{"_id": vid, "slug": "fixture"}); e != nil {
		t.Fatal(e)
	}
	if _, e = db.Collection("content_publications").InsertOne(ctx, publication.Publication{ID: pid, ContentID: cid, TemplateVersionID: vid, FullPath: "/report", PublicURL: "https://cms.example/report", ContentHash: fmt.Sprintf("%x", sha256.Sum256(body)), Status: publication.StatusActive, StorageState: publication.StoragePresent, VerificationStatus: publication.VerificationVerified}); e != nil {
		t.Fatal(e)
	}
	bg, stop := context.WithCancel(ctx)
	defer stop()
	if e = wireStaticDeliveryContent(ctx, bg, admin, db, cfg, &publicationRuntime{Store: store, PubRepo: publication.NewRepository(db, nil)}, "https://cms.example"); e != nil {
		t.Fatal(e)
	}
	if admin.ContentAPI == nil || admin.Commands == nil {
		t.Fatal("runtime did not expose content API")
	}
	job, e := admin.Commands.SubmitExport(ctx, a, "runtime-export", []ex.Item{{ContentID: cid}})
	if e != nil {
		t.Fatal(e)
	}
	deadline := time.Now().Add(6 * time.Second)
	for {
		got, e := admin.Commands.GetJob(ctx, a, job.ID)
		if e != nil {
			t.Fatal(e)
		}
		if got.State == sd.JobSucceeded {
			break
		}
		if got.State == sd.JobFailed || got.State == sd.JobBlocked || time.Now().After(deadline) {
			t.Fatalf("startup worker did not export: %+v", got)
		}
		time.Sleep(20 * time.Millisecond)
	}
	var unchanged models.Content
	if e = db.Content().FindOne(ctx, bson.M{"_id": cid}).Decode(&unchanged); e != nil || !unchanged.Published {
		t.Fatal("export changed original publication", e)
	}
	if _, e = db.Collection("users").UpdateOne(ctx, bson.M{"_id": uid}, bson.M{"$set": bson.M{"disabled": true}}); e != nil {
		t.Fatal(e)
	}
	if e = check(ctx, a, false, false); sd.CodeOf(e) != sd.CodePermissionDenied {
		t.Fatal("disabled actor accepted", e)
	}
	if e = check(ctx, a, true, true); e != nil {
		t.Fatal("read-only uncertain activation recovery blocked", e)
	}
}

package staticdelivery

import (
	"context"
	"github.com/metavert-io/lightcms/v7/internal/testutil"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"path/filepath"
	"reflect"
	"testing"
)

func TestObjectReleaseStoreReadsLegacyAndDeduplicates(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	ctx := context.Background()
	for _, name := range []string{"artifact_objects", "artifact_refs", "artifact_cleanup_plans"} {
		defer db.Collection(name).Drop(ctx)
	}
	project := t.TempDir()
	cfg := Config{ProjectRoot: project, Root: filepath.Join(project, "private"), RetentionDays: 90, FailedRetentionDays: 7}
	legacy := NewFileReleaseStore(cfg)
	store := NewObjectReleaseStore(cfg, db)
	first, err := BuildRelease(ctx, BuildInput{ID: primitive.NewObjectID(), Target: releaseTarget(), Pages: []CapturedPage{releasePage(primitive.NewObjectID(), "/objects")}})
	if err != nil {
		t.Fatal(err)
	}
	if err := legacy.Save(ctx, first); err != nil {
		t.Fatal(err)
	}
	restored, err := store.Load(ctx, first.ID)
	if err != nil || !reflect.DeepEqual(restored, first) {
		t.Fatalf("legacy reader changed: %v", err)
	}
	second := first
	second.ID = primitive.NewObjectID()
	if err := store.Save(ctx, second); err != nil {
		t.Fatal(err)
	}
	restored, err = store.Load(ctx, second.ID)
	if err != nil || !reflect.DeepEqual(restored, second) {
		t.Fatalf("object snapshot changed: %v", err)
	}
	third := second
	third.ID = primitive.NewObjectID()
	if err := store.Save(ctx, third); err != nil {
		t.Fatal(err)
	}
	for _, file := range second.Files {
		n, err := db.Collection("artifact_objects").CountDocuments(ctx, bson.M{"_id": file.SHA256})
		if err != nil || n != 1 {
			t.Fatalf("public bytes not deduplicated: %d %v", n, err)
		}
	}
	summary, err := store.Summary(ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range summary.Files {
		if len(file.Data) != 0 {
			t.Fatal("summary loaded file bodies")
		}
	}
}

package staticdelivery

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/metavert-io/lightcms/v7/internal/product/artifacts"
	"github.com/metavert-io/lightcms/v7/internal/testutil"
	"go.mongodb.org/mongo-driver/bson"
)

func TestObjectManagementReadsMetadataWithoutCreatingPrivateRoot(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	ctx := context.Background()
	project := t.TempDir()
	privateRoot := filepath.Join(project, "unopened-private-root")
	store := NewObjectReleaseStore(Config{ProjectRoot: project, Root: privateRoot}, db)
	hash := strings.Repeat("a", 64)
	if _, err := db.Collection("artifact_objects").InsertOne(ctx, bson.M{"_id": hash, "size": 42, "state": "available", "mime": "text/html", "created_at": time.Now()}); err != nil {
		t.Fatal(err)
	}
	page, err := store.ListObjects(ctx, artifacts.ListQuery{})
	if err != nil || len(page.Items) != 1 || page.Items[0].Hash != hash {
		t.Fatalf("metadata list unavailable: %+v %v", page, err)
	}
	detail, err := store.GetObject(ctx, hash)
	if err != nil || detail.Hash != hash || detail.MIME != "text/html" {
		t.Fatalf("metadata detail unavailable: %+v %v", detail, err)
	}
	if _, err := os.Stat(privateRoot); !os.IsNotExist(err) {
		t.Fatalf("read-only management opened/created private storage: %v", err)
	}
}

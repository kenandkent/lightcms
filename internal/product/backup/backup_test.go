package backup

import (
	"context"
	"github.com/metavert-io/lightcms/v7/internal/testutil"
	"go.mongodb.org/mongo-driver/bson"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBackupRequiresStoppedWriterAndVerifiesEveryFile(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	ctx := context.Background()
	defer db.Collection("instance_liveness").Drop(ctx)
	project := t.TempDir()
	if err := os.Mkdir(filepath.Join(project, "content"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "content", "page.html"), []byte("frozen page"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := db.InsertOne(ctx, "content", bson.M{"title": "frozen"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Collection("instance_liveness").InsertOne(ctx, bson.M{"_id": "__exclusive_site_writer__", "owner": "server", "last_heartbeat": time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := Create(ctx, db, project, "backup", []string{"content"}); err == nil {
		t.Fatal("backup allowed a running writer")
	}
	if _, err := db.Collection("instance_liveness").DeleteOne(ctx, bson.M{"_id": "__exclusive_site_writer__", "owner": "server"}); err != nil {
		t.Fatal(err)
	}
	manifest, err := Create(ctx, db, project, "backup", []string{"content"})
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Files) < 2 || manifest.Database == "" {
		t.Fatal("database or private files omitted")
	}
	if _, err := Verify(project, "backup"); err != nil {
		t.Fatal(err)
	}
	for _, file := range manifest.Files {
		if strings.HasSuffix(file.Path, "page.html") {
			if err := os.WriteFile(filepath.Join(project, "backup", filepath.FromSlash(file.Path)), []byte("corrupted"), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := Verify(project, "backup"); err == nil {
		t.Fatal("corrupt backup accepted")
	}
}

func TestRestoreDrillUsesFreshIsolatedDatabaseAndDirectory(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	ctx := context.Background()
	defer db.Collection("instance_liveness").Drop(ctx)
	project := t.TempDir()
	if err := os.Mkdir(filepath.Join(project, "private"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "private", "historical.html"), []byte("history"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := db.InsertOne(ctx, "content", bson.M{"title": "original"}); err != nil {
		t.Fatal(err)
	}
	if _, err := Create(ctx, db, project, "point", []string{"private"}); err != nil {
		t.Fatal(err)
	}
	client := db.Collection("content").Database().Client()
	name := db.Collection("content").Database().Name() + "-restore-test"
	defer client.Database(name).Drop(ctx)
	if _, err := RestoreForVerification(ctx, client, project, "point", db.Collection("content").Database().Name(), "restored"); err == nil {
		t.Fatal("production database accepted")
	}
	if _, err := RestoreForVerification(ctx, client, project, "point", name, "restored"); err != nil {
		t.Fatal(err)
	}
	var record bson.M
	if err := client.Database(name).Collection("content").FindOne(ctx, bson.M{"title": "original"}).Decode(&record); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(project, "restored", "private", "historical.html"))
	if err != nil || string(raw) != "history" {
		t.Fatalf("historical file not restored: %q %v", raw, err)
	}
	if _, err := RestoreForVerification(ctx, client, project, "point", name, "restored-again"); err == nil {
		t.Fatal("existing database overwritten")
	}
}

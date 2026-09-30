// Task 19 gap 18C-b: templates whose document lacks the current_version
// field entirely must still be backfilled to 1 (§35.2). The pre-fix filter
// current_version:{$lte:0} never matches a missing field, so the update
// silently affected zero documents.
package migration_test

import (
	"context"
	"testing"

	"github.com/jonradoff/lightcms/v7/internal/product/migration"
	"github.com/jonradoff/lightcms/v7/internal/testutil"

	"go.mongodb.org/mongo-driver/bson"
)

func TestBackfillTemplateMissingCurrentVersion(t *testing.T) {
	ctx := context.Background()
	db := migDB(t)
	testutil.CleanupCollections(t, db)
	st, _ := migStore(t)
	if err := db.EnsureProductIndexes(ctx); err != nil {
		t.Fatalf("EnsureProductIndexes: %v", err)
	}

	tplID := seedTemplate(t, db, "T19 NoVersion", "t19-nover")
	// seedTemplate intentionally omits current_version — confirm the premise.
	var before bson.M
	if err := db.Collection("templates").FindOne(ctx, bson.M{"_id": tplID}).Decode(&before); err != nil {
		t.Fatal(err)
	}
	if _, ok := before["current_version"]; ok {
		t.Fatalf("premise broken: seed has current_version=%v", before["current_version"])
	}

	cid := seedContent(t, db, tplID, "/t19nover/page", true, nil)
	seedContentVersion(t, db, cid, 1)
	body := []byte("<html><body>default render for /t19nover/page</body></html>")
	writeCanonical(t, st, "/t19nover/page", body)

	fr := &fakeRenderer{byPath: map[string][]byte{}, errPaths: map[string]string{}}
	rep, err := migration.New(migration.Config{DB: db, Store: st, Render: fr.fn()}).Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v (blocking=%d)", err, rep.BlockingCount())
	}
	if !rep.Completed {
		t.Fatal("Run did not complete")
	}

	var after bson.M
	if err := db.Collection("templates").FindOne(ctx, bson.M{"_id": tplID}).Decode(&after); err != nil {
		t.Fatal(err)
	}
	if toInt(after["current_version"]) != 1 {
		t.Fatalf("current_version = %v, want 1", after["current_version"])
	}
}

func toInt(v any) int64 {
	switch n := v.(type) {
	case int32:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	default:
		return -1
	}
}

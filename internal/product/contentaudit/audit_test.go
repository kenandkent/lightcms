package contentaudit

import (
	"context"
	"github.com/metavert-io/lightcms/v7/internal/testutil"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"testing"
)

func TestScanReportsGapWithoutSynthesizingHistory(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	ctx := context.Background()
	id := primitive.NewObjectID()
	if _, err := db.InsertOne(ctx, "content", bson.M{"_id": id, "full_path": "/old", "current_version": 3}); err != nil {
		t.Fatal(err)
	}
	for _, v := range []int{1, 3} {
		if _, err := db.InsertOne(ctx, "content_versions", bson.M{"content_id": id, "version": v}); err != nil {
			t.Fatal(err)
		}
	}
	page, err := Scan(ctx, db, primitive.NilObjectID)
	if err != nil || len(page.Issues) != 1 || page.Issues[0].Code != "HISTORY_GAP" || page.Issues[0].Missing[0] != (Range{2, 2}) {
		t.Fatalf("gap not reported: %+v %v", page, err)
	}
	if count, err := db.Count(ctx, "content_versions", bson.M{"content_id": id}); err != nil || count != 2 {
		t.Fatal("audit modified history", err)
	}
}

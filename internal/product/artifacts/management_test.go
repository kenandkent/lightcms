package artifacts

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/metavert-io/lightcms/v7/internal/testutil"
	"go.mongodb.org/mongo-driver/bson"
)

func TestManagementListsBoundedPrivateMetadataAndReferences(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	ctx := context.Background()
	catalog := NewCatalog(db, nil, 90, 7) // Reads must never open object files.
	now := time.Now().UTC()
	for i := 0; i < 60; i++ {
		_, err := db.Collection("artifact_objects").InsertOne(ctx, bson.M{
			"_id": fmt.Sprintf("%064x", i+1), "size": i + 10, "state": "available", "generation": 1,
			"created_at": now.Add(-time.Duration(i) * time.Minute), "pinned": i == 0, "mime": "text/html",
			"secret_payload": strings.Repeat("must-not-be-loaded", 5000), "delete_plan": "internal-plan",
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	hash := fmt.Sprintf("%064x", 1)
	for i := 0; i < 105; i++ {
		owner := fmt.Sprintf("delivery-job:%024x", i+1)
		if _, err := db.Collection("artifact_refs").InsertOne(ctx, bson.M{"_id": fmt.Sprint(i), "owner": owner, "hash": hash}); err != nil {
			t.Fatal(err)
		}
	}
	page, err := catalog.ListObjects(ctx, ListQuery{Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 50 || !page.HasNext || page.Page != 1 || page.Limit != 50 {
		t.Fatalf("unbounded/default pagination: %+v", page)
	}
	first := page.Items[0]
	if first.Hash != hash || first.MIME != "text/html" || !first.Pinned || first.ReferenceCount != 100 || !first.ReferencesTruncated || !first.Protected {
		t.Fatalf("missing safe retention metadata: %+v", first)
	}
	second, err := catalog.ListObjects(ctx, ListQuery{Page: 2, Limit: 50})
	if err != nil || len(second.Items) != 10 || second.HasNext || second.Items[0].Hash == first.Hash {
		t.Fatalf("second page: %+v %v", second, err)
	}
	encoded, _ := json.Marshal(page)
	for _, secret := range []string{"must-not-be-loaded", "internal-plan", "delete_plan"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatal("private storage fields leaked", secret)
		}
	}
}

func TestManagementDetailOmitsExpiredAndUntrustedReferenceOwners(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	ctx := context.Background()
	catalog := NewCatalog(db, nil, 90, 7)
	hash := strings.Repeat("a", 64)
	now := time.Now().UTC()
	_, err := db.Collection("artifact_objects").InsertOne(ctx, bson.M{"_id": hash, "size": 123, "state": "available", "created_at": now.Add(-100 * 24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	refs := []bson.M{
		{"_id": "current", "hash": hash, "owner": "delivery-current:000000000000000000000001"},
		{"_id": "pending", "hash": hash, "owner": "delivery-job:000000000000000000000002"},
		{"_id": "private", "hash": hash, "owner": "token:/Users/ken/private-super-secret"},
		{"_id": "expired", "hash": hash, "owner": "release:000000000000000000000003", "expires_at": now.Add(-time.Hour)},
	}
	for _, ref := range refs {
		if _, err := db.Collection("artifact_refs").InsertOne(ctx, ref); err != nil {
			t.Fatal(err)
		}
	}
	detail, err := catalog.GetObject(ctx, hash)
	if err != nil {
		t.Fatal(err)
	}
	if detail.ReferenceCount != 3 || len(detail.References) != 3 || !detail.Protected || detail.CleanupEligible {
		t.Fatalf("active reference protection missing: %+v", detail)
	}
	kinds := map[string]bool{}
	for _, ref := range detail.References {
		kinds[ref.Kind] = true
	}
	if !kinds["current_release"] || !kinds["pending_job"] || !kinds["other_reference"] {
		t.Fatalf("reference causes missing: %+v", detail.References)
	}
	encoded, _ := json.Marshal(detail)
	for _, forbidden := range []string{"/Users", "private-super-secret", "000000000000000000000003", "token:"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatal("unsafe/expired reference detail leaked", forbidden)
		}
	}
	for i := 0; i < 60; i++ {
		_, err := db.Collection("artifact_refs").InsertOne(ctx, bson.M{"_id": fmt.Sprint(i), "hash": hash, "owner": fmt.Sprintf("release:%024x", i)})
		if err != nil {
			t.Fatal(err)
		}
	}
	detail, err = catalog.GetObject(ctx, hash)
	if err != nil || len(detail.References) != 50 || !detail.ReferencesTruncated || detail.ReferenceCount != 63 {
		t.Fatalf("reference detail not bounded: %+v %v", detail, err)
	}
}

func TestManagementFiltersMetadataAndRejectsUnsafeQueries(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	ctx := context.Background()
	catalog := NewCatalog(db, nil, 90, 7)
	for i, state := range []string{"available", "available", "deleted", "deleting"} {
		_, err := db.Collection("artifact_objects").InsertOne(ctx, bson.M{"_id": strings.Repeat(string(rune('a'+i)), 64), "size": 42, "state": state, "pinned": i == 0, "failed": i == 1, "created_at": time.Now().Add(-100 * 24 * time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		query ListQuery
		want  string
	}{
		{ListQuery{Search: " AAA "}, strings.Repeat("a", 64)},
		{ListQuery{State: "failed"}, strings.Repeat("b", 64)},
		{ListQuery{State: "deleted"}, strings.Repeat("c", 64)},
		{ListQuery{Pin: "pinned"}, strings.Repeat("a", 64)},
	} {
		page, err := catalog.ListObjects(ctx, tc.query)
		if err != nil || len(page.Items) != 1 || page.Items[0].Hash != tc.want {
			t.Fatalf("filter %+v: %+v %v", tc.query, page, err)
		}
	}
	page, err := catalog.ListObjects(ctx, ListQuery{Search: "b"})
	if err != nil || !page.Items[0].CleanupEligible {
		t.Fatalf("unprotected expired failed artifact should be eligible: %+v %v", page, err)
	}
	for _, q := range []ListQuery{{Search: ".*"}, {Search: strings.Repeat("a", 65)}, {State: "unsafe"}, {Pin: "unsafe"}, {Page: 1001}} {
		if _, err := catalog.ListObjects(ctx, q); err == nil {
			t.Fatal("unsafe/unbounded filter accepted", q)
		}
	}
	if _, err := (&Catalog{}).GetObject(ctx, "../../secret"); err == nil {
		t.Fatal("invalid object hash reached database")
	}
}

func TestManagementReadsStoredMIMEWithoutOpeningFilesOrChangingReferences(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	ctx := context.Background()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	catalog := NewCatalog(db, store, 90, 7)
	ref, err := store.Put(ctx, bytes.NewBufferString("private"), "text/plain", 100)
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.Register(ctx, ref, false); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Reference(ctx, "delivery-current:000000000000000000000001", ref, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Collection("artifact_objects").UpdateOne(ctx, bson.M{"_id": ref.Hash}, bson.M{"$set": bson.M{"created_at": time.Now().Add(-100 * 24 * time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	catalog.Store = nil
	detail, err := catalog.GetObject(ctx, ref.Hash)
	if err != nil || detail.MIME != "text/plain" || !detail.Protected {
		t.Fatalf("stored metadata read: %+v %v", detail, err)
	}
	plan, err := catalog.PreviewCleanup(ctx, "admin")
	if err != nil || len(plan.Items) != 0 {
		t.Fatalf("management read weakened current release protection: %+v %v", plan, err)
	}
}

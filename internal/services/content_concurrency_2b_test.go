package services

// Lane 2B Fix 3 (red): version counter divergence.
// (a) saveVersion may compute a corrected version (count ahead of the row)
// without persisting it back to the content row → row and history diverge.
// (b) concurrent UpdateContent writers must converge: exactly one winner per
// version, contiguous history, row CurrentVersion == max version doc.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/jonradoff/lightcms/v7/internal/database"
	"github.com/jonradoff/lightcms/v7/internal/models"
	"github.com/jonradoff/lightcms/v7/internal/testutil"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func TestUpdateContent_PersistsCorrectedVersionBack(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	ctx := context.Background()
	// Production parity: the UNIQUE(content_id, version) backstop index.
	if err := db.EnsureProductIndexes(ctx); err != nil {
		t.Fatalf("EnsureProductIndexes: %v", err)
	}
	svc := NewContentService(db)
	tmplID := createTestTemplate(t, svc)

	c := &models.Content{
		TemplateID: tmplID, TemplateName: "Test Template",
		Title: "Diverged", Slug: "diverged",
		Data: map[string]interface{}{"content": "v1"},
	}
	if err := svc.CreateContent(ctx, c); err != nil {
		t.Fatalf("CreateContent: %v", err)
	}

	// Simulate legacy divergence: history is ahead of the row (row says 1,
	// version docs go to 3) — e.g. a past Count()+1 correction that never
	// wrote back.
	for v := int64(2); v <= 3; v++ {
		doc := bson.M{"content_id": c.ID, "version": v, "title": c.Title, "created_at": c.CreatedAt}
		if _, err := db.Collection("content_versions").InsertOne(ctx, doc); err != nil {
			t.Fatalf("seed divergent version %d: %v", v, err)
		}
	}

	c.Title = "Diverged v2"
	if err := svc.UpdateContent(ctx, c, "heal divergence"); err != nil {
		t.Fatalf("UpdateContent: %v", err)
	}

	var row models.Content
	if err := db.FindOne(ctx, "content", bson.M{"_id": c.ID}, &row); err != nil {
		t.Fatalf("read row: %v", err)
	}
	maxVer, err := maxVersionDoc(ctx, db, c.ID)
	if err != nil {
		t.Fatalf("max version doc: %v", err)
	}
	if row.CurrentVersion != maxVer {
		t.Fatalf("row CurrentVersion=%d diverges from max version doc=%d", row.CurrentVersion, maxVer)
	}
	if row.CurrentVersion < 4 {
		t.Fatalf("expected row to converge at >=4, got %d", row.CurrentVersion)
	}
}

func TestUpdateContent_ConcurrentConvergence(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	ctx := context.Background()
	// Production parity: the UNIQUE(content_id, version) backstop index.
	if err := db.EnsureProductIndexes(ctx); err != nil {
		t.Fatalf("EnsureProductIndexes: %v", err)
	}
	svc := NewContentService(db)
	tmplID := createTestTemplate(t, svc)

	c := &models.Content{
		TemplateID: tmplID, TemplateName: "Test Template",
		Title: "Hot", Slug: "hot-page",
		Data: map[string]interface{}{"content": "v1"},
	}
	if err := svc.CreateContent(ctx, c); err != nil {
		t.Fatalf("CreateContent: %v", err)
	}

	const writers = 12
	const perWriter = 4
	total := writers * perWriter

	var start sync.WaitGroup
	start.Add(1)
	var wg sync.WaitGroup
	errs := make([]error, writers)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			start.Wait()
			for k := 0; k < perWriter; k++ {
				// Well-behaved caller: re-read, mutate, write; retry on
				// version conflict with backoff-free bounded retries.
				var lastErr error
				for attempt := 0; attempt < 40; attempt++ {
					var fresh models.Content
					if err := db.FindOne(ctx, "content", bson.M{"_id": c.ID}, &fresh); err != nil {
						lastErr = err
						continue
					}
					fresh.Title = fmt.Sprintf("w%d-k%d-a%d", w, k, attempt)
					fresh.Data = map[string]interface{}{"content": fmt.Sprintf("w%d-k%d", w, k)}
					if err := svc.UpdateContent(ctx, &fresh, "concurrent"); err != nil {
						if errors.Is(err, ErrVersionConflict) {
							lastErr = err
							continue
						}
						errs[w] = err
						return
					}
					lastErr = nil
					break
				}
				if lastErr != nil {
					errs[w] = lastErr
					return
				}
			}
		}(w)
	}
	start.Done()
	wg.Wait()
	for w, err := range errs {
		if err != nil {
			t.Fatalf("writer %d failed: %v", w, err)
		}
	}

	var row models.Content
	if err := db.FindOne(ctx, "content", bson.M{"_id": c.ID}, &row); err != nil {
		t.Fatalf("read row: %v", err)
	}
	want := int64(1 + total)
	if row.CurrentVersion != want {
		t.Fatalf("row CurrentVersion=%d, want %d (lost updates)", row.CurrentVersion, want)
	}
	// History must be contiguous 1..want with no duplicates.
	seen := map[int64]int{}
	cur, err := db.Collection("content_versions").Find(ctx, bson.M{"content_id": c.ID})
	if err != nil {
		t.Fatalf("find versions: %v", err)
	}
	var docs []bson.M
	if err := cur.All(ctx, &docs); err != nil {
		t.Fatalf("decode versions: %v", err)
	}
	_ = cur.Close(ctx)
	for _, d := range docs {
		v := toInt64(d["version"])
		seen[v]++
	}
	if len(docs) != int(want) {
		t.Fatalf("version docs=%d, want %d", len(docs), want)
	}
	for v := int64(1); v <= want; v++ {
		if seen[v] != 1 {
			t.Fatalf("version %d appears %d times (want exactly 1)", v, seen[v])
		}
	}
}

func maxVersionDoc(ctx context.Context, db *database.DB, id primitive.ObjectID) (int64, error) {
	var top struct {
		Version int64 `bson:"version"`
	}
	err := db.Collection("content_versions").FindOne(ctx,
		bson.M{"content_id": id},
		options.FindOne().SetSort(bson.D{{Key: "version", Value: -1}}),
	).Decode(&top)
	if err != nil {
		return 0, err
	}
	return top.Version, nil
}

func toInt64(v interface{}) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int32:
		return int64(n)
	case int:
		return int64(n)
	case float64:
		return int64(n)
	default:
		return 0
	}
}

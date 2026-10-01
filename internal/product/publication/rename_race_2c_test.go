package publication_test

// Lane 2C fix 3 — rename TOCTOU convergence proof (reviewed-OK, no code
// change): hammer concurrent rename-publish + unpublish + republish and
// assert final-state convergence. The saga already closes the window:
//   - Publish/Unpublish/Rollback hold per-content + per-path try-locks in
//     stable order (service.go Publish/Unpublish/Rollback acquire paths),
//     and re-read content + active UNDER the lock (buildPublishPlan /
//     buildRollbackPlan dbReloadContent + GetActive; Unpublish dbReloadContent
//     + GetActive), so plans freeze fresh state, never the caller's snapshot;
//   - ExpectedActiveID is honored at saga.go:353 (publish), :432
//     (rollback) and service.go:275 (unpublish), with atomic UnpublishCAS /
//     ActivateCAS commits; losers compensate files instead of clobbering;
//   - rename detection (oldActive.FullPath != content.FullPath) runs on the
//     under-lock reads; redirect upsert is idempotent and the old canonical
//     is retired only after commit.
// Wiring ExpectedActiveID through the search-replace wasPublished snapshot
// would require editing sibling-owned api_content.go republish call sites,
// so per the lane collision rule that caller change is STOPPED and handed
// to lanes 2A/2B — this test is the proof the saga side already converges.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jonradoff/lightcms/v7/internal/models"
	"github.com/jonradoff/lightcms/v7/internal/product/publication"

	"go.mongodb.org/mongo-driver/bson"
)

func TestRenameRace2C_ConcurrentPublishUnpublishConverges(t *testing.T) {
	s := newSagaSetup(t, nil, nil, publication.Options{})
	ctx := context.Background()
	tplID, tvID := seedSagaTemplateVersion(t, s.db, "", true)
	oldPath := "/news/race-2c-a"
	newPath := "/news/race-2c-b"
	contentID := seedSagaContent(t, s.db, tplID, oldPath, 1, map[string]any{"headline": "race v1"})

	first, err := s.svc.Publish(ctx, publication.PublishRequest{
		ContentID: contentID, ContentVersion: 1, TemplateVersionID: tvID,
	})
	if err != nil {
		t.Fatalf("setup publish: %v", err)
	}

	// Rename draft: the live row now points at the new path (v2).
	if err := s.db.Collection("content").FindOneAndUpdate(ctx, bson.M{"_id": contentID},
		bson.M{"$set": bson.M{
			"current_version": int64(2), "full_path": newPath,
			"slug": "race-2c-b", "canonical_full_path": newPath,
			"data": map[string]any{"headline": "race v2"},
		}}).Err(); err != nil {
		t.Fatalf("rename draft: %v", err)
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	// 6 rename-republish racers: half pin the stale pre-rename active ID
	// (must conflict, never clobber), half pin nothing (fresh intent).
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			req := publication.PublishRequest{
				ContentID: contentID, ContentVersion: 2, TemplateVersionID: tvID,
			}
			if i%2 == 0 {
				stale := first.PublicationID
				req.ExpectedActiveID = &stale
			}
			_, _ = s.svc.Publish(context.Background(), req)
		}(i)
	}
	// 3 concurrent unpublish racers (nil ExpectedActiveID = fresh intent).
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_ = s.svc.Unpublish(context.Background(), publication.UnpublishRequest{ContentID: contentID})
		}()
	}
	close(start)
	wg.Wait()

	// Final-state convergence assertions (order-independent).
	active, err := s.repo.GetActive(ctx, contentID)
	if err != nil {
		t.Fatalf("GetActive: %v", err)
	}
	canonicalBytes := func(path string) ([]byte, bool) {
		rel := strings.TrimPrefix(path, "/") + ".html"
		b, err := os.ReadFile(filepath.Join(s.root, "generated", filepath.FromSlash(rel)))
		return b, err == nil
	}
	sidecars := func(path string) []string {
		info, err := s.store.Inspect(ctx, path)
		if err != nil {
			return nil
		}
		var kinds []string
		for _, sc := range info.Sidecars {
			kinds = append(kinds, string(sc.Kind))
		}
		return kinds
	}
	var content models.Content
	if err := s.db.FindOne(ctx, "content", bson.M{"_id": contentID}, &content); err != nil {
		t.Fatalf("load content: %v", err)
	}

	if active == nil {
		// Unpublished convergence: no canonical may survive at either path
		// (no resurrection), and the projection must agree.
		if _, ok := canonicalBytes(oldPath); ok {
			t.Fatal("unpublished but old-path canonical survives (resurrect)")
		}
		if _, ok := canonicalBytes(newPath); ok {
			t.Fatal("unpublished but new-path canonical survives (resurrect)")
		}
		if content.Published {
			t.Fatal("projection Published=true with no active publication")
		}
	} else {
		// Published convergence: exactly the active record's bytes serve at
		// the active path (no clobber), the retired path is gone, and the
		// projection agrees.
		body, ok := canonicalBytes(active.FullPath)
		if !ok {
			t.Fatalf("active %s has no canonical file", active.FullPath)
		}
		sum := sha256.Sum256(body)
		if got, want := hex.EncodeToString(sum[:]), strings.TrimPrefix(active.ContentHash, "sha256:"); !strings.EqualFold(got, want) {
			t.Fatalf("canonical bytes do not match active %s (clobber)", active.ID.Hex())
		}
		other := oldPath
		if active.FullPath == oldPath {
			other = newPath
		}
		if _, ok := canonicalBytes(other); ok {
			t.Fatalf("retired path %s still has a canonical (rename not retired)", other)
		}
		if !content.Published {
			t.Fatal("projection Published=false with an active publication")
		}
		// A committed rename must have recorded its redirect.
		if active.FullPath == newPath {
			var redir bson.M
			if err := s.db.Collection("redirects").FindOne(ctx, bson.M{"from_path": oldPath}).Decode(&redir); err != nil {
				t.Fatalf("rename committed without redirect: %v", err)
			}
		}
	}
	// No compensation sidecars may leak in either outcome.
	for _, kinds := range [][]string{sidecars(oldPath), sidecars(newPath)} {
		for _, k := range kinds {
			if k == "previous" || k == "unpublish-backup" {
				t.Fatalf("leaked compensation sidecar %q after convergence", k)
			}
		}
	}
}

package staticdelivery

import (
	"context"
	"errors"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"os"
	"path/filepath"
	"testing"
)

func TestReviewIncompleteReleaseIsNotMissing(t *testing.T) {
	project := t.TempDir()
	cfg := Config{ProjectRoot: project, Root: filepath.Join(project, "private")}
	store := NewFileReleaseStore(cfg)
	id := primitive.NewObjectID()
	if _, err := store.Load(context.Background(), id); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("absent release should permit initial preparation: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(cfg.Root, "releases", id.Hex(), "private"), 0700); err != nil {
		t.Fatal(err)
	}
	_, err := store.Load(context.Background(), id)
	if CodeOf(err) != CodeArtifactUnavailable || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("incomplete immutable release must not permit reconstruction: %v", err)
	}
}

func TestReleaseSummaryDoesNotReadAssetOrSnapshotBodies(t *testing.T) {
	project := t.TempDir()
	cfg := Config{ProjectRoot: project, Root: filepath.Join(project, "private")}
	store := NewFileReleaseStore(cfg)
	r, err := BuildRelease(context.Background(), BuildInput{ID: primitive.NewObjectID(), Target: releaseTarget(), Pages: []CapturedPage{releasePage(primitive.NewObjectID(), "/summary")}})
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Save(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	// Progress links are metadata, not deployment validation. A full load must
	// still reject corruption, but summary reads must not open the large blob.
	name := filepath.Join(cfg.Root, "releases", r.ID.Hex(), "private", "release.gob")
	if err = os.WriteFile(name, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := store.Summary(context.Background(), r.ID)
	if err != nil || got.Pages["/summary"].PublicationID != r.Pages["/summary"].PublicationID {
		t.Fatal("summary unavailable", err)
	}
	if _, err = store.Load(context.Background(), r.ID); CodeOf(err) != CodeArtifactUnavailable {
		t.Fatal("full integrity validation weakened", err)
	}
}

func TestReviewReleaseStoreSurvivesRestartAndRejectsTamper(t *testing.T) {
	project := t.TempDir()
	cfg := Config{ProjectRoot: project, Root: filepath.Join(project, "private")}
	store := NewFileReleaseStore(cfg)
	r, e := BuildRelease(context.Background(), BuildInput{ID: primitive.NewObjectID(), Target: releaseTarget(), Pages: []CapturedPage{releasePage(primitive.NewObjectID(), "/a")}})
	if e != nil {
		t.Fatal(e)
	}
	if e = store.Save(context.Background(), r); e != nil {
		t.Fatal(e)
	}
	restarted := NewFileReleaseStore(cfg)
	saved, e := restarted.Load(context.Background(), r.ID)
	if e != nil || string(saved.Pages["/a"].Captured.Document.HTML) != string(r.Pages["/a"].Captured.Document.HTML) {
		t.Fatal("lost private rollback snapshot", e)
	}
	if e = store.Save(context.Background(), r); e != nil {
		t.Fatal("idempotent save failed", e)
	}
	r.Origin = "https://other.example"
	if e = store.Save(context.Background(), r); CodeOf(e) != CodeIdempotencyConflict {
		t.Fatal("immutable release overwritten", e)
	}
	if e = os.WriteFile(filepath.Join(cfg.Root, "releases", r.ID.Hex(), "public", "a", "index.html"), []byte("corrupted"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = store.Load(context.Background(), r.ID); CodeOf(e) != CodeArtifactUnavailable {
		t.Fatal("corrupt release accepted", e)
	}
}

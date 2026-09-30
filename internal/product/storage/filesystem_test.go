package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

func testStore(t *testing.T) *FilesystemStore {
	t.Helper()
	return NewFilesystemStore(t.TempDir())
}

func testIDs() (primitive.ObjectID, primitive.ObjectID) {
	return primitive.NewObjectID(), primitive.NewObjectID()
}

func shaOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func mustStage(t *testing.T, s *FilesystemStore, contentID, pubID primitive.ObjectID, canonical string, html []byte) StagedObject {
	t.Helper()
	obj, err := s.Stage(context.Background(), StageRequest{
		ContentID:     contentID,
		PublicationID: pubID,
		CanonicalPath: canonical,
		HTML:          html,
	})
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	return obj
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile %s: %v", path, err)
	}
	return b
}

func TestStage_WritesTmpFsyncVerifiesRenames(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	contentID, pubID := testIDs()
	html := []byte("<html><body>v1</body></html>")

	obj, err := s.Stage(ctx, StageRequest{
		ContentID:     contentID,
		PublicationID: pubID,
		CanonicalPath: "/news/foo",
		HTML:          html,
	})
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if obj.SHA256 != shaOf(html) {
		t.Fatalf("SHA256 = %s, want %s", obj.SHA256, shaOf(html))
	}
	want := filepath.Join(s.Root, "publications", contentID.Hex(), pubID.Hex(), "index.html")
	if obj.Path != want {
		t.Fatalf("Path = %s, want %s", obj.Path, want)
	}
	if got := readFile(t, want); string(got) != string(html) {
		t.Fatalf("immutable bytes mismatch")
	}
	// No .tmp left behind.
	if _, err := os.Stat(want + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("stale .tmp remains: %v", err)
	}
	// Verify passes on the staged object.
	if err := s.Verify(ctx, obj); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	// Open serves exact bytes.
	rc, err := s.Open(ctx, obj.Stored())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close()
}

func TestStage_HashMismatchLeavesNoObject(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	contentID, pubID := testIDs()

	_, err := s.Stage(ctx, StageRequest{
		ContentID:      contentID,
		PublicationID:  pubID,
		CanonicalPath:  "/news/foo",
		HTML:           []byte("<html>v1</html>"),
		ExpectedSHA256: shaOf([]byte("something else")),
	})
	if CodeOf(err) != CodeHashMismatch {
		t.Fatalf("err = %v, want code %s", err, CodeHashMismatch)
	}
	immutable := filepath.Join(s.Root, "publications", contentID.Hex(), pubID.Hex(), "index.html")
	if _, statErr := os.Stat(immutable); !os.IsNotExist(statErr) {
		t.Fatalf("verified object must not exist, stat err = %v", statErr)
	}
	if _, statErr := os.Stat(immutable + ".tmp"); !os.IsNotExist(statErr) {
		t.Fatalf("tmp must not remain, stat err = %v", statErr)
	}
}

func TestStage_ShortWriteLeavesNoVerifiedObject(t *testing.T) {
	s := testStore(t)
	s.MaxWriteBytes = 8 // simulate a truncated write
	ctx := context.Background()
	contentID, pubID := testIDs()
	html := []byte("<html><body>this body is longer than eight bytes</body></html>")

	_, err := s.Stage(ctx, StageRequest{
		ContentID:     contentID,
		PublicationID: pubID,
		CanonicalPath: "/news/foo",
		HTML:          html,
	})
	if err == nil {
		t.Fatalf("expected short-write failure, got nil")
	}
	immutable := filepath.Join(s.Root, "publications", contentID.Hex(), pubID.Hex(), "index.html")
	if _, statErr := os.Stat(immutable); !os.IsNotExist(statErr) {
		t.Fatalf("short write must leave no verified object, stat err = %v", statErr)
	}
	if _, statErr := os.Stat(immutable + ".tmp"); !os.IsNotExist(statErr) {
		t.Fatalf("short write must leave no tmp, stat err = %v", statErr)
	}
}

func TestStage_InvalidPath(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	bad := []string{
		"",
		"relative/path",
		"/../escape",
		"/a/../../b",
		"/news/foo?x=1",
		"/news/foo#frag",
		"/news\\windows",
		"/news/%2fencoded",
		"/news/%2Fencoded",
		"/trailing/",
	}
	for _, p := range bad {
		contentID, pubID := testIDs()
		_, err := s.Stage(ctx, StageRequest{
			ContentID: contentID, PublicationID: pubID,
			CanonicalPath: p, HTML: []byte("<html>x</html>"),
		})
		if CodeOf(err) != CodeInvalidPath {
			t.Errorf("path %q: err = %v, want code %s", p, err, CodeInvalidPath)
		}
	}
}

func TestStage_EmptyBody(t *testing.T) {
	s := testStore(t)
	contentID, pubID := testIDs()
	_, err := s.Stage(context.Background(), StageRequest{
		ContentID: contentID, PublicationID: pubID,
		CanonicalPath: "/news/foo", HTML: nil,
	})
	if CodeOf(err) != CodeEmptyBody {
		t.Fatalf("err = %v, want code %s", err, CodeEmptyBody)
	}
}

func TestStage_IdempotentSameBytesConflictDifferentBytes(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	contentID, pubID := testIDs()
	html := []byte("<html>same</html>")
	first := mustStage(t, s, contentID, pubID, "/news/foo", html)
	second := mustStage(t, s, contentID, pubID, "/news/foo", html)
	if first.Path != second.Path || first.SHA256 != second.SHA256 {
		t.Fatalf("idempotent re-stage must return identical object")
	}
	// Different bytes under the same publication ID must not silently replace.
	_, err := s.Stage(ctx, StageRequest{
		ContentID: contentID, PublicationID: pubID,
		CanonicalPath: "/news/foo", HTML: []byte("<html>different</html>"),
	})
	if CodeOf(err) != CodeAlreadyExists {
		t.Fatalf("err = %v, want code %s", err, CodeAlreadyExists)
	}
	if got := readFile(t, first.Path); string(got) != string(html) {
		t.Fatalf("conflicting stage must not replace immutable bytes")
	}
}

func TestVerify_TamperedAndMissing(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	contentID, pubID := testIDs()
	obj := mustStage(t, s, contentID, pubID, "/news/foo", []byte("<html>v1</html>"))

	if err := os.WriteFile(obj.Path, []byte("<html>tampered</html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Verify(ctx, obj); CodeOf(err) != CodeHashMismatch {
		t.Fatalf("tampered verify err = %v, want code %s", err, CodeHashMismatch)
	}

	missing := StagedObject{ContentID: contentID, PublicationID: primitive.NewObjectID(), Path: obj.Path + ".nope", SHA256: obj.SHA256}
	if err := s.Verify(ctx, missing); CodeOf(err) != CodeNotFound {
		t.Fatalf("missing verify err = %v, want code %s", err, CodeNotFound)
	}
}

func TestActivate_FirstPublish(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	contentID, pubID := testIDs()
	html := []byte("<html>first</html>")
	obj := mustStage(t, s, contentID, pubID, "/news/foo", html)

	if err := s.Activate(ctx, obj, "/news/foo"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	canon, err := s.CanonicalFilePath("/news/foo")
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, canon); string(got) != string(html) {
		t.Fatalf("canonical bytes mismatch")
	}
	// No previous backup on first publish, no .next residue.
	if matches, _ := filepath.Glob(canon + ".previous-*"); len(matches) != 0 {
		t.Fatalf("unexpected previous files: %v", matches)
	}
	if matches, _ := filepath.Glob(canon + ".next-*"); len(matches) != 0 {
		t.Fatalf("stale .next remains: %v", matches)
	}
	exists, err := s.Exists(ctx, "/news/foo")
	if err != nil || !exists {
		t.Fatalf("Exists = %v, %v; want true, nil", exists, err)
	}
}

func TestActivate_RequiresPreviousIDWhenCanonicalExists(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	contentID, v1 := testIDs()
	v1obj := mustStage(t, s, contentID, v1, "/news/foo", []byte("<html>v1</html>"))
	if err := s.Activate(ctx, v1obj, "/news/foo"); err != nil {
		t.Fatal(err)
	}
	v2 := primitive.NewObjectID()
	v2obj := mustStage(t, s, contentID, v2, "/news/foo", []byte("<html>v2</html>"))
	if err := s.Activate(ctx, v2obj, "/news/foo"); CodeOf(err) != CodeNeedsPreviousID {
		t.Fatalf("Activate over existing canonical err = %v, want code %s", err, CodeNeedsPreviousID)
	}
	// Canonical must still serve v1 bytes.
	canon, _ := s.CanonicalFilePath("/news/foo")
	if got := readFile(t, canon); string(got) != "<html>v1</html>" {
		t.Fatalf("canonical must be untouched, got %q", got)
	}
}

func TestActivate_WithPreviousCutover(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	contentID, v1 := testIDs()
	v1html := []byte("<html>v1</html>")
	v1obj := mustStage(t, s, contentID, v1, "/news/foo", v1html)
	if err := s.Activate(ctx, v1obj, "/news/foo"); err != nil {
		t.Fatal(err)
	}

	v2 := primitive.NewObjectID()
	v2html := []byte("<html>v2 with changes</html>")
	v2obj := mustStage(t, s, contentID, v2, "/news/foo", v2html)
	if err := s.ActivateWithPrevious(ctx, v2obj, "/news/foo", &v1); err != nil {
		t.Fatalf("ActivateWithPrevious: %v", err)
	}
	canon, _ := s.CanonicalFilePath("/news/foo")
	if got := readFile(t, canon); string(got) != string(v2html) {
		t.Fatalf("canonical = %q, want v2 bytes", got)
	}
	prev := canon + ".previous-" + v1.Hex()
	if got := readFile(t, prev); string(got) != string(v1html) {
		t.Fatalf("previous backup bytes mismatch")
	}
	if matches, _ := filepath.Glob(canon + ".next-*"); len(matches) != 0 {
		t.Fatalf("stale .next remains: %v", matches)
	}
	// Immutable objects for both publications retained for recovery/rollback.
	if _, err := os.Stat(v1obj.Path); err != nil {
		t.Fatalf("v1 immutable must be retained: %v", err)
	}
}

func TestActivate_TamperedImmutableAbortsWithoutTouchingCanonical(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	contentID, v1 := testIDs()
	v1obj := mustStage(t, s, contentID, v1, "/news/foo", []byte("<html>v1</html>"))
	if err := s.Activate(ctx, v1obj, "/news/foo"); err != nil {
		t.Fatal(err)
	}
	v2 := primitive.NewObjectID()
	v2obj := mustStage(t, s, contentID, v2, "/news/foo", []byte("<html>v2</html>"))
	if err := os.WriteFile(v2obj.Path, []byte("<html>tampered</html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.ActivateWithPrevious(ctx, v2obj, "/news/foo", &v1); CodeOf(err) != CodeHashMismatch {
		t.Fatalf("err = %v, want code %s", err, CodeHashMismatch)
	}
	canon, _ := s.CanonicalFilePath("/news/foo")
	if got := readFile(t, canon); string(got) != "<html>v1</html>" {
		t.Fatalf("canonical must still serve v1, got %q", got)
	}
	if matches, _ := filepath.Glob(canon + ".previous-*"); len(matches) != 0 {
		t.Fatalf("no previous must be created on failed activation: %v", matches)
	}
}

func TestActivate_CrashFixtures(t *testing.T) {
	ctx := context.Background()

	t.Run("crashAfterPreviousRename", func(t *testing.T) {
		s := testStore(t)
		contentID, v1 := testIDs()
		v1html := []byte("<html>v1</html>")
		v1obj := mustStage(t, s, contentID, v1, "/news/chk", v1html)
		_ = v1obj
		// Build the crash state manually: canonical moved to .previous, no new canonical yet.
		canon, err := s.CanonicalFilePath("/news/chk")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(canon), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(canon, v1html, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(canon, canon+".previous-"+v1.Hex()); err != nil {
			t.Fatal(err)
		}
		info, err := s.Inspect(ctx, "/news/chk")
		if err != nil {
			t.Fatalf("Inspect: %v", err)
		}
		if info.Exists {
			t.Fatalf("canonical must be reported missing after crash")
		}
		if len(info.Sidecars) != 1 || info.Sidecars[0].Kind != SidecarPrevious {
			t.Fatalf("scanner must see the previous sidecar: %+v", info.Sidecars)
		}
		// Recovery available to Task 8/10: rebuild canonical from retained immutable bytes.
		v2 := primitive.NewObjectID()
		v2obj := mustStage(t, s, contentID, v2, "/news/chk", []byte("<html>v2</html>"))
		if err := s.Restore(ctx, v2obj.Stored(), "/news/chk"); err != nil {
			t.Fatalf("Restore: %v", err)
		}
		if got := readFile(t, canon); string(got) != "<html>v2</html>" {
			t.Fatalf("restored canonical mismatch")
		}
	})

	t.Run("crashAfterNextRename", func(t *testing.T) {
		s := testStore(t)
		contentID, v1 := testIDs()
		v1obj := mustStage(t, s, contentID, v1, "/news/chk2", []byte("<html>v1</html>"))
		if err := s.Activate(ctx, v1obj, "/news/chk2"); err != nil {
			t.Fatal(err)
		}
		v2 := primitive.NewObjectID()
		v2obj := mustStage(t, s, contentID, v2, "/news/chk2", []byte("<html>v2</html>"))
		if err := s.ActivateWithPrevious(ctx, v2obj, "/news/chk2", &v1); err != nil {
			t.Fatal(err)
		}
		// Simulate a crash that left a stale .next file behind.
		canon, _ := s.CanonicalFilePath("/news/chk2")
		staleNext := canon + ".next-" + primitive.NewObjectID().Hex()
		if err := os.WriteFile(staleNext, []byte("orphan"), 0o644); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-16 * time.Minute)
		if err := os.Chtimes(staleNext, old, old); err != nil {
			t.Fatal(err)
		}
		info, err := s.Inspect(ctx, "/news/chk2")
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, sc := range info.Sidecars {
			if sc.Kind == SidecarNext && s.IsStale(sc.ModTime, time.Now()) {
				found = true
			}
		}
		if !found {
			t.Fatalf("scanner must detect the stale .next orphan: %+v", info.Sidecars)
		}
		if !info.Exists || info.SHA256 != v2obj.SHA256 {
			t.Fatalf("canonical must still serve the new bytes: %+v", info)
		}
	})
}

func TestCompensateActivate_RestoresPreviousOnCommitFailure(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	contentID, v1 := testIDs()
	v1html := []byte("<html>v1</html>")
	v1obj := mustStage(t, s, contentID, v1, "/news/foo", v1html)
	if err := s.Activate(ctx, v1obj, "/news/foo"); err != nil {
		t.Fatal(err)
	}
	v2 := primitive.NewObjectID()
	v2obj := mustStage(t, s, contentID, v2, "/news/foo", []byte("<html>v2</html>"))
	if err := s.ActivateWithPrevious(ctx, v2obj, "/news/foo", &v1); err != nil {
		t.Fatal(err)
	}
	// Simulate the Mongo activation transaction failing after the file cutover.
	if err := s.CompensateActivate(ctx, v2obj, "/news/foo", &v1); err != nil {
		t.Fatalf("CompensateActivate: %v", err)
	}
	canon, _ := s.CanonicalFilePath("/news/foo")
	if got := readFile(t, canon); string(got) != string(v1html) {
		t.Fatalf("canonical must serve v1 again, got %q", got)
	}
	if matches, _ := filepath.Glob(canon + ".next-*"); len(matches) != 0 {
		t.Fatalf("no .next may remain: %v", matches)
	}
	if matches, _ := filepath.Glob(canon + ".previous-*"); len(matches) != 0 {
		t.Fatalf("previous must be consumed by compensation: %v", matches)
	}
	// The new immutable object is retained (publication will be marked failed, GC owns deletion).
	if _, err := os.Stat(v2obj.Path); err != nil {
		t.Fatalf("v2 immutable must be retained: %v", err)
	}
}

func TestConfirmActivate_CleansPreviousAfterCommit(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	contentID, v1 := testIDs()
	v1obj := mustStage(t, s, contentID, v1, "/news/foo", []byte("<html>v1</html>"))
	if err := s.Activate(ctx, v1obj, "/news/foo"); err != nil {
		t.Fatal(err)
	}
	v2 := primitive.NewObjectID()
	v2obj := mustStage(t, s, contentID, v2, "/news/foo", []byte("<html>v2</html>"))
	if err := s.ActivateWithPrevious(ctx, v2obj, "/news/foo", &v1); err != nil {
		t.Fatal(err)
	}
	if err := s.ConfirmActivate(ctx, "/news/foo", v1); err != nil {
		t.Fatalf("ConfirmActivate: %v", err)
	}
	canon, _ := s.CanonicalFilePath("/news/foo")
	if got := readFile(t, canon); string(got) != "<html>v2</html>" {
		t.Fatalf("canonical must still serve v2")
	}
	if matches, _ := filepath.Glob(canon + ".previous-*"); len(matches) != 0 {
		t.Fatalf("previous must be cleaned: %v", matches)
	}
	// Confirm is idempotent.
	if err := s.ConfirmActivate(ctx, "/news/foo", v1); err != nil {
		t.Fatalf("second ConfirmActivate: %v", err)
	}
}

func TestUnpublishBackup_StageRestoreCleanup(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	contentID, v1 := testIDs()
	v1html := []byte("<html>live</html>")
	v1obj := mustStage(t, s, contentID, v1, "/news/gone", v1html)
	if err := s.Activate(ctx, v1obj, "/news/gone"); err != nil {
		t.Fatal(err)
	}

	backupPath, err := s.StageUnpublishBackup(ctx, "/news/gone", v1)
	if err != nil {
		t.Fatalf("StageUnpublishBackup: %v", err)
	}
	canon, _ := s.CanonicalFilePath("/news/gone")
	wantBackup := canon + ".unpublish-backup-" + v1.Hex()
	if backupPath != wantBackup {
		t.Fatalf("backup = %s, want %s", backupPath, wantBackup)
	}
	if _, err := os.Stat(canon); !os.IsNotExist(err) {
		t.Fatalf("canonical must be staged away, stat err = %v", err)
	}
	if got := readFile(t, wantBackup); string(got) != string(v1html) {
		t.Fatalf("backup bytes mismatch")
	}

	// DB transaction failure: restore the backup to canonical.
	if err := s.RestoreUnpublishBackup(ctx, "/news/gone", v1); err != nil {
		t.Fatalf("RestoreUnpublishBackup: %v", err)
	}
	if got := readFile(t, canon); string(got) != string(v1html) {
		t.Fatalf("restored canonical mismatch")
	}

	// DB transaction success path: stage again, then post-commit cleanup.
	if _, err := s.StageUnpublishBackup(ctx, "/news/gone", v1); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveUnpublishBackup(ctx, "/news/gone", v1); err != nil {
		t.Fatalf("RemoveUnpublishBackup: %v", err)
	}
	if _, err := os.Stat(wantBackup); !os.IsNotExist(err) {
		t.Fatalf("backup must be cleaned, stat err = %v", err)
	}
	// Cleanup is idempotent.
	if err := s.RemoveUnpublishBackup(ctx, "/news/gone", v1); err != nil {
		t.Fatalf("second RemoveUnpublishBackup: %v", err)
	}
}

func TestRestore_RebuildsCanonical(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	contentID, v1 := testIDs()
	html := []byte("<html>restore me</html>")
	obj := mustStage(t, s, contentID, v1, "/news/r", html)
	if err := s.Activate(ctx, obj, "/news/r"); err != nil {
		t.Fatal(err)
	}
	canon, _ := s.CanonicalFilePath("/news/r")
	if err := os.Remove(canon); err != nil {
		t.Fatal(err)
	}
	if err := s.Restore(ctx, obj.Stored(), "/news/r"); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got := readFile(t, canon); string(got) != string(html) {
		t.Fatalf("rebuilt canonical mismatch")
	}
	// Matching canonical is a no-op success.
	if err := s.Restore(ctx, obj.Stored(), "/news/r"); err != nil {
		t.Fatalf("no-op Restore: %v", err)
	}
}

func TestAbort_RemovesStagedObjects(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	contentID, pubID := testIDs()
	obj := mustStage(t, s, contentID, pubID, "/news/abort", []byte("<html>x</html>"))
	if err := s.Abort(ctx, obj); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if _, err := os.Stat(obj.Path); !os.IsNotExist(err) {
		t.Fatalf("immutable must be removed, stat err = %v", err)
	}
	// Abort is idempotent and never touches the canonical file.
	if err := s.Abort(ctx, obj); err != nil {
		t.Fatalf("second Abort: %v", err)
	}
	exists, err := s.Exists(ctx, "/news/abort")
	if err != nil || exists {
		t.Fatalf("Exists = %v, %v; canonical must be untouched", exists, err)
	}
}

func TestDelete_IsIdempotent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	contentID, pubID := testIDs()
	obj := mustStage(t, s, contentID, pubID, "/news/del", []byte("<html>x</html>"))
	if err := s.Activate(ctx, obj, "/news/del"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "/news/del"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	exists, err := s.Exists(ctx, "/news/del")
	if err != nil || exists {
		t.Fatalf("Exists = %v, %v; want false, nil", exists, err)
	}
	if err := s.Delete(ctx, "/news/del"); err != nil {
		t.Fatalf("second Delete: %v", err)
	}
}

func TestStaleStageTimeout(t *testing.T) {
	s := testStore(t)
	now := time.Now()
	if s.IsStale(now.Add(-16*time.Minute), now) != true {
		t.Fatalf("16m old stage must be stale with default 15m timeout")
	}
	if s.IsStale(now.Add(-14*time.Minute), now) != false {
		t.Fatalf("14m old stage must not be stale with default 15m timeout")
	}
	if s.Timeout() != DefaultStageTimeout {
		t.Fatalf("Timeout = %v, want %v", s.Timeout(), DefaultStageTimeout)
	}
	if DefaultStageTimeoutMinutes != 15 {
		t.Fatalf("DefaultStageTimeoutMinutes = %d, spec default is 15", DefaultStageTimeoutMinutes)
	}
	custom := NewFilesystemStore(t.TempDir())
	custom.StageTimeout = time.Minute
	if custom.IsStale(now.Add(-2*time.Minute), now) != true {
		t.Fatalf("custom 1m timeout must mark 2m stage stale")
	}
}

func TestBackupNaming(t *testing.T) {
	s := testStore(t)
	contentID, pubID := testIDs()
	immutable := s.ImmutablePath(contentID, pubID)
	want := filepath.Join(s.Root, "publications", contentID.Hex(), pubID.Hex(), "index.html")
	if immutable != want {
		t.Fatalf("ImmutablePath = %s, want %s", immutable, want)
	}
	canon, err := s.CanonicalFilePath("/news/foo")
	if err != nil {
		t.Fatal(err)
	}
	if canon != filepath.Join(s.Root, "generated", "news", "foo.html") {
		t.Fatalf("CanonicalFilePath = %s", canon)
	}
	root, err := s.CanonicalFilePath("/")
	if err != nil {
		t.Fatal(err)
	}
	if root != filepath.Join(s.Root, "generated", "index.html") {
		t.Fatalf("root CanonicalFilePath = %s", root)
	}
	_ = immutable
}

func TestInspect_ExposesSidecarsForScanner(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	contentID, v1 := testIDs()
	v1obj := mustStage(t, s, contentID, v1, "/news/insp", []byte("<html>v1</html>"))
	if err := s.Activate(ctx, v1obj, "/news/insp"); err != nil {
		t.Fatal(err)
	}
	v2 := primitive.NewObjectID()
	v2obj := mustStage(t, s, contentID, v2, "/news/insp", []byte("<html>v2</html>"))
	if err := s.ActivateWithPrevious(ctx, v2obj, "/news/insp", &v1); err != nil {
		t.Fatal(err)
	}
	info, err := s.Inspect(ctx, "/news/insp")
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if !info.Exists || info.SHA256 != v2obj.SHA256 {
		t.Fatalf("Inspect must report the active canonical: %+v", info)
	}
	if len(info.Sidecars) != 1 || info.Sidecars[0].Kind != SidecarPrevious || info.Sidecars[0].PublicationID != v1 {
		t.Fatalf("Inspect must expose the previous sidecar: %+v", info.Sidecars)
	}

	// Missing canonical is reported, not an error.
	missing, err := s.Inspect(ctx, "/news/never-published")
	if err != nil {
		t.Fatalf("Inspect missing: %v", err)
	}
	if missing.Exists {
		t.Fatalf("missing canonical must report Exists=false")
	}
}

func TestDeleteImmutable_GCMechanic(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	contentID, pubID := testIDs()
	obj := mustStage(t, s, contentID, pubID, "/news/gc", []byte("<html>x</html>"))
	if err := s.DeleteImmutable(ctx, contentID, pubID); err != nil {
		t.Fatalf("DeleteImmutable: %v", err)
	}
	if err := s.Verify(ctx, obj); CodeOf(err) != CodeNotFound {
		t.Fatalf("Verify after GC err = %v, want code %s", err, CodeNotFound)
	}
	// Idempotent for the retention worker.
	if err := s.DeleteImmutable(ctx, contentID, pubID); err != nil {
		t.Fatalf("second DeleteImmutable: %v", err)
	}
}

func TestInterfaceConformance(t *testing.T) {
	var _ StaticPageStore = (*FilesystemStore)(nil)
	var _ Store = (*FilesystemStore)(nil)
}

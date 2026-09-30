package storage

// Task 17B coverage-gap tests for internal/product/storage.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

func gapStore(t *testing.T) (*FilesystemStore, string) {
	t.Helper()
	root := t.TempDir()
	return NewFilesystemStore(root), root
}

func gapStage(t *testing.T, s *FilesystemStore, path string, body []byte) StagedObject {
	t.Helper()
	obj, err := s.Stage(context.Background(), StageRequest{
		ContentID: primitive.NewObjectID(), PublicationID: primitive.NewObjectID(),
		CanonicalPath: path, HTML: body,
	})
	if err != nil {
		t.Fatalf("Stage(%q): %v", path, err)
	}
	return obj
}

func TestCoverGapStoreErrorHelpers(t *testing.T) {
	withPath := &Error{Code: CodeNotFound, Message: "missing", Path: "/p", Err: errors.New("nope")}
	if s := withPath.Error(); !strings.Contains(s, "/p") {
		t.Fatalf("Error() = %q", s)
	}
	plain := &Error{Code: CodeIO, Message: "io"}
	if s := plain.Error(); !strings.Contains(s, "STORAGE_IO_ERROR") {
		t.Fatalf("Error() = %q", s)
	}
	if plain.Unwrap() != nil || withPath.Unwrap() == nil {
		t.Fatalf("Unwrap branches")
	}
	if CodeOf(nil) != "" || CodeOf(plain) != CodeIO || CodeOf(errors.New("x")) != CodeIO {
		t.Fatalf("CodeOf branches")
	}
	if checkCtx(nil) != nil {
		t.Fatalf("checkCtx(nil)")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if checkCtx(ctx) == nil {
		t.Fatalf("checkCtx(cancelled)")
	}
	s, _ := gapStore(t)
	if err := s.Activate(ctx, StagedObject{}, "/x"); err == nil {
		t.Fatalf("Activate cancelled ctx: want error")
	}
	if err := s.ActivateWithPrevious(ctx, StagedObject{}, "/x", nil); err == nil {
		t.Fatalf("ActivateWithPrevious cancelled ctx: want error")
	}
	if _, err := s.Stage(ctx, StageRequest{}); err == nil {
		t.Fatalf("Stage cancelled ctx: want error")
	}
	if previousFor("/c", nil) != "" {
		t.Fatalf("previousFor(nil)")
	}
	id := primitive.NewObjectID()
	if previousFor("/c", &id) != "/c.previous-"+id.Hex() {
		t.Fatalf("previousFor(id)")
	}
	var rp FilesystemStore
	rp.restorePrevious("/c", "", false)
	rp.restorePrevious("/c", "/prev", false)
}

func TestCoverGapPathValidation(t *testing.T) {
	s, _ := gapStore(t)
	ctx := context.Background()
	long := "/" + strings.Repeat("a", 2000)
	for _, bad := range []string{"", long, "relative", "/a\x00b", `/a\b`, "/a?b", "/a#b",
		"/a\x01b", "/a\x7fb", "/a%2Fb", "/a/../b", "/a//b", "/a/./b", "/.", "/..", "/a/.."} {
		if _, err := s.CanonicalFilePath(bad); CodeOf(err) != CodeInvalidPath {
			t.Errorf("CanonicalFilePath(%q) = %v, want INVALID_PATH", bad, err)
		}
		if err := s.Delete(ctx, bad); CodeOf(err) != CodeInvalidPath {
			t.Errorf("Delete(%q): %v", bad, err)
		}
		if _, err := s.Exists(ctx, bad); CodeOf(err) != CodeInvalidPath {
			t.Errorf("Exists(%q): %v", bad, err)
		}
		if _, err := s.Inspect(ctx, bad); CodeOf(err) != CodeInvalidPath {
			t.Errorf("Inspect(%q): %v", bad, err)
		}
		if err := s.ConfirmActivate(ctx, bad, primitive.NewObjectID()); CodeOf(err) != CodeInvalidPath {
			t.Errorf("ConfirmActivate(%q): %v", bad, err)
		}
		if _, err := s.StageUnpublishBackup(ctx, bad, primitive.NewObjectID()); CodeOf(err) != CodeInvalidPath {
			t.Errorf("StageUnpublishBackup(%q): %v", bad, err)
		}
		if err := s.RestoreUnpublishBackup(ctx, bad, primitive.NewObjectID()); CodeOf(err) != CodeInvalidPath {
			t.Errorf("RestoreUnpublishBackup(%q): %v", bad, err)
		}
		if err := s.RemoveUnpublishBackup(ctx, bad, primitive.NewObjectID()); CodeOf(err) != CodeInvalidPath {
			t.Errorf("RemoveUnpublishBackup(%q): %v", bad, err)
		}
	}
	if p, err := s.CanonicalFilePath("/"); err != nil || !strings.HasSuffix(p, "index.html") {
		t.Fatalf("CanonicalFilePath(/) = %q, %v", p, err)
	}
	if _, err := s.Stage(ctx, StageRequest{ContentID: primitive.NewObjectID(),
		PublicationID: primitive.NewObjectID(), CanonicalPath: "/bad/../x", HTML: []byte("b")}); CodeOf(err) != CodeInvalidPath {
		t.Fatalf("Stage bad path: %v", err)
	}
	if _, err := s.Stage(ctx, StageRequest{CanonicalPath: "/x", HTML: []byte("b")}); CodeOf(err) != CodeInvalidRequest {
		t.Fatalf("Stage zero IDs: %v", err)
	}
	if _, err := s.Stage(ctx, StageRequest{ContentID: primitive.NewObjectID(),
		PublicationID: primitive.NewObjectID(), CanonicalPath: "/x",
		HTML: []byte("b"), ExpectedSHA256: "short"}); CodeOf(err) != CodeInvalidRequest {
		t.Fatalf("Stage short hash: %v", err)
	}
}

func TestCoverGapCutoverFailureMatrix(t *testing.T) {
	ctx := context.Background()
	s, _ := gapStore(t)

	// First publish v1 so a canonical exists.
	v1 := gapStage(t, s, "/gap/page", []byte("<h1>v1</h1>"))
	if err := s.Activate(ctx, v1, "/gap/page"); err != nil {
		t.Fatalf("Activate v1: %v", err)
	}
	oldID := v1.PublicationID

	// Immutable deleted after stage: cutover renames canonical→previous,
	// then fails NotFound and restores previous→canonical.
	v2 := gapStage(t, s, "/gap/page", []byte("<h1>v2</h1>"))
	if err := os.Remove(v2.Path); err != nil {
		t.Fatalf("remove immutable: %v", err)
	}
	if err := s.ActivateWithPrevious(ctx, v2, "/gap/page", &oldID); CodeOf(err) != CodeNotFound {
		t.Fatalf("cutover missing immutable: %v", err)
	}
	if b, err := os.ReadFile(mustCanonicalGap(t, s, "/gap/page")); err != nil || string(b) != "<h1>v1</h1>" {
		t.Fatalf("canonical after restore: %q, %v", b, err)
	}

	// Tampered immutable: hash mismatch, canonical untouched.
	v3 := gapStage(t, s, "/gap/page", []byte("<h1>v3</h1>"))
	if err := os.WriteFile(v3.Path, []byte("<h1>evil</h1>"), 0o644); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	if err := s.ActivateWithPrevious(ctx, v3, "/gap/page", &oldID); CodeOf(err) != CodeHashMismatch {
		t.Fatalf("cutover tampered: %v", err)
	}

	// Short-write store: .next copy fails verification.
	v4 := gapStage(t, s, "/gap/page", []byte("<h1>v4 with enough bytes to truncate</h1>"))
	narrow := NewFilesystemStore(s.Root)
	narrow.MaxWriteBytes = 2
	if err := narrow.ActivateWithPrevious(ctx, v4, "/gap/page", &oldID); CodeOf(err) != CodeHashMismatch {
		t.Fatalf("cutover short write: %v", err)
	}

	// Activate verify failure via crafted object.
	bad := StagedObject{ContentID: primitive.NewObjectID(), PublicationID: primitive.NewObjectID(),
		Path: "/nonexistent", SHA256: "abc"}
	if err := s.Activate(ctx, bad, "/gap/page"); err == nil {
		t.Fatalf("Activate bad object: want error")
	}
	if err := s.Activate(ctx, v1, "/bad/../x"); err == nil {
		t.Fatalf("Activate bad path: want error")
	}
}

func mustCanonicalGap(t *testing.T, s *FilesystemStore, p string) string {
	t.Helper()
	c, err := s.CanonicalFilePath(p)
	if err != nil {
		t.Fatalf("CanonicalFilePath(%q): %v", p, err)
	}
	return c
}

func TestCoverGapCompensateMatrix(t *testing.T) {
	ctx := context.Background()
	s, _ := gapStore(t)
	v1 := gapStage(t, s, "/gap/comp", []byte("<h1>v1</h1>"))
	if err := s.Activate(ctx, v1, "/gap/comp"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	oldID := v1.PublicationID
	v2 := gapStage(t, s, "/gap/comp", []byte("<h1>v2</h1>"))
	if err := s.ActivateWithPrevious(ctx, v2, "/gap/comp", &oldID); err != nil {
		t.Fatalf("ActivateWithPrevious: %v", err)
	}
	newID := v2.PublicationID

	if err := s.CompensateActivate(ctx, v2, "/gap/comp", nil); CodeOf(err) != CodeInvalidRequest {
		t.Fatalf("compensate nil oldID: %v", err)
	}
	if err := s.CompensateActivate(ctx, v2, "/bad/../x", &oldID); CodeOf(err) != CodeInvalidPath {
		t.Fatalf("compensate bad path: %v", err)
	}
	ghost := primitive.NewObjectID()
	if err := s.CompensateActivate(ctx, v2, "/gap/comp", &ghost); CodeOf(err) != CodeNotFound {
		t.Fatalf("compensate missing prev: %v", err)
	}
	// Canonical no longer carries the new bytes (tamper) → conflict refusal.
	canon := mustCanonicalGap(t, s, "/gap/comp")
	if err := os.WriteFile(canon, []byte("<h1>third party</h1>"), 0o644); err != nil {
		t.Fatalf("tamper canonical: %v", err)
	}
	if err := s.CompensateActivate(ctx, v2, "/gap/comp", &oldID); CodeOf(err) != CodeConflict {
		t.Fatalf("compensate changed canonical: %v", err)
	}
	// Canonical missing + previous present → rename restores.
	if err := os.Remove(canon); err != nil {
		t.Fatalf("remove canonical: %v", err)
	}
	prev := canon + ".previous-" + oldID.Hex()
	if _, err := os.Stat(prev); err != nil {
		t.Fatalf("previous should exist: %v", err)
	}
	_ = newID
	if err := s.CompensateActivate(ctx, v2, "/gap/comp", &oldID); err != nil {
		t.Fatalf("compensate missing canonical: %v", err)
	}
	if b, err := os.ReadFile(canon); err != nil || string(b) != "<h1>v1</h1>" {
		t.Fatalf("restored canonical: %q, %v", b, err)
	}
}

func TestCoverGapUnpublishBackupMatrix(t *testing.T) {
	ctx := context.Background()
	s, _ := gapStore(t)
	pid := primitive.NewObjectID()
	if _, err := s.StageUnpublishBackup(ctx, "/gap/none", pid); CodeOf(err) != CodeNotFound {
		t.Fatalf("stage backup missing canonical: %v", err)
	}
	v1 := gapStage(t, s, "/gap/unpub", []byte("<h1>v1</h1>"))
	if err := s.Activate(ctx, v1, "/gap/unpub"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if _, err := s.StageUnpublishBackup(ctx, "/gap/unpub", pid); err != nil {
		t.Fatalf("stage backup: %v", err)
	}
	if _, err := s.StageUnpublishBackup(ctx, "/gap/missing2", pid); CodeOf(err) != CodeNotFound {
		t.Fatalf("stage backup absent: %v", err)
	}
	// Canonical gone (staged away) + backup exists for another page state:
	// double-stage on a fresh page with pre-created backup → AlreadyExists.
	v2 := gapStage(t, s, "/gap/unpub2", []byte("<h1>v1</h1>"))
	if err := s.Activate(ctx, v2, "/gap/unpub2"); err != nil {
		t.Fatalf("Activate2: %v", err)
	}
	canon2 := mustCanonicalGap(t, s, "/gap/unpub2")
	pre := canon2 + ".unpublish-backup-" + pid.Hex()
	if err := os.WriteFile(pre, []byte("x"), 0o644); err != nil {
		t.Fatalf("pre-create backup: %v", err)
	}
	if _, err := s.StageUnpublishBackup(ctx, "/gap/unpub2", pid); CodeOf(err) != CodeAlreadyExists {
		t.Fatalf("double stage backup: %v", err)
	}
	if err := s.RestoreUnpublishBackup(ctx, "/gap/unpub2", pid); CodeOf(err) != CodeConflict {
		t.Fatalf("restore over canonical: %v", err)
	}
	if err := s.RestoreUnpublishBackup(ctx, "/gap/nobackup", primitive.NewObjectID()); CodeOf(err) != CodeNotFound {
		t.Fatalf("restore missing backup: %v", err)
	}
	if err := s.RestoreUnpublishBackup(ctx, "/gap/unpub", primitive.NewObjectID()); CodeOf(err) != CodeNotFound {
		t.Fatalf("restore wrong pid: %v", err)
	}
}

func TestCoverGapRestoreOpenMatrix(t *testing.T) {
	ctx := context.Background()
	s, _ := gapStore(t)
	if err := s.Restore(ctx, StoredObject{}, "/gap/x"); CodeOf(err) != CodeInvalidRequest {
		t.Fatalf("Restore empty obj: %v", err)
	}
	missing := StoredObject{ContentID: primitive.NewObjectID(), PublicationID: primitive.NewObjectID(),
		Path: "/nonexistent", SHA256: strings.Repeat("a", 64)}
	if err := s.Restore(ctx, missing, "/gap/x"); CodeOf(err) != CodeNotFound {
		t.Fatalf("Restore missing immutable: %v", err)
	}
	v1 := gapStage(t, s, "/gap/restore", []byte("<h1>keep</h1>"))
	tampered := v1.Stored()
	tampered.SHA256 = strings.Repeat("b", 64)
	if err := s.Restore(ctx, tampered, "/gap/restore"); CodeOf(err) != CodeHashMismatch {
		t.Fatalf("Restore tampered: %v", err)
	}
	if err := s.Restore(ctx, v1.Stored(), "/bad/../x"); CodeOf(err) != CodeInvalidPath {
		t.Fatalf("Restore bad path: %v", err)
	}
	// Canonical already carries the bytes → no-op success.
	if err := s.Activate(ctx, v1, "/gap/restore"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if err := s.Restore(ctx, v1.Stored(), "/gap/restore"); err != nil {
		t.Fatalf("Restore no-op: %v", err)
	}
	// Short-write store: restore copy fails verification.
	narrow := NewFilesystemStore(s.Root)
	narrow.MaxWriteBytes = 1
	v2 := gapStage(t, s, "/gap/restore2", []byte("<h1>restore me with plenty of bytes</h1>"))
	if err := narrow.Restore(ctx, v2.Stored(), "/gap/restore2"); CodeOf(err) != CodeHashMismatch {
		t.Fatalf("Restore short write: %v", err)
	}

	if _, err := s.Open(ctx, StoredObject{}); CodeOf(err) != CodeInvalidRequest {
		t.Fatalf("Open empty: %v", err)
	}
	if _, err := s.Open(ctx, missing); CodeOf(err) != CodeNotFound {
		t.Fatalf("Open missing: %v", err)
	}
	if _, err := s.Open(ctx, tampered); CodeOf(err) != CodeHashMismatch {
		t.Fatalf("Open tampered: %v", err)
	}
	rc, err := s.Open(ctx, v1.Stored())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close()
}

func TestCoverGapAbortDeleteExistsInspect(t *testing.T) {
	ctx := context.Background()
	s, _ := gapStore(t)
	if err := s.Abort(ctx, StagedObject{}); CodeOf(err) != CodeInvalidRequest {
		t.Fatalf("Abort empty: %v", err)
	}
	if err := s.DeleteImmutable(ctx, primitive.NilObjectID, primitive.NewObjectID()); CodeOf(err) != CodeInvalidRequest {
		t.Fatalf("DeleteImmutable zero: %v", err)
	}
	// Delete on a non-empty directory canonical → IO error.
	dirCanon := mustCanonicalGap(t, s, "/gap/dircanon")
	if err := os.MkdirAll(dirCanon, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dirCanon, "child"), []byte("x"), 0o644); err != nil {
		t.Fatalf("child: %v", err)
	}
	if err := s.Delete(ctx, "/gap/dircanon"); CodeOf(err) != CodeIO {
		t.Fatalf("Delete non-empty dir: %v", err)
	}
	exists, err := s.Exists(ctx, "/gap/dircanon")
	if err != nil || !exists {
		t.Fatalf("Exists(dir) = %v, %v", exists, err)
	}
	if _, err := s.Inspect(ctx, "/gap/dircanon"); CodeOf(err) != CodeIO {
		t.Fatalf("Inspect dir canonical: %v", err)
	}

	// Sidecar symlink to nowhere → sidecar read error.
	v1 := gapStage(t, s, "/gap/side", []byte("<h1>side</h1>"))
	if err := s.Activate(ctx, v1, "/gap/side"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	canon := mustCanonicalGap(t, s, "/gap/side")
	link := canon + ".previous-" + strings.Repeat("a", 24)
	if err := os.Symlink(filepath.Join("nope", "missing"), link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if _, err := s.Inspect(ctx, "/gap/side"); CodeOf(err) != CodeIO {
		t.Fatalf("Inspect bad sidecar: %v", err)
	}

	if _, _, ok := parseSidecarName("base.html", "other.html"); ok {
		t.Fatalf("parseSidecarName prefix")
	}
	if _, _, ok := parseSidecarName("base.html", "base.html.unknown-aaaaaaaaaaaaaaaaaaaaaaaa"); ok {
		t.Fatalf("parseSidecarName suffix")
	}
	if _, _, ok := parseSidecarName("base.html", "base.html.previous-short"); ok {
		t.Fatalf("parseSidecarName length")
	}
	if _, _, ok := parseSidecarName("base.html", "base.html.previous-zzzzzzzzzzzzzzzzzzzzzzzz"); ok {
		t.Fatalf("parseSidecarName hex")
	}
	pid := primitive.NewObjectID()
	if _, got, ok := parseSidecarName("base.html", "base.html.next-"+pid.Hex()); !ok || got != pid {
		t.Fatalf("parseSidecarName next")
	}
}

func TestCoverGapLowLevelIO(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "f")
	if err := writeSyncFile(p, []byte("data"), 0); err != nil {
		t.Fatalf("writeSyncFile: %v", err)
	}
	if err := writeSyncFile(p, []byte("data"), 0); err == nil {
		t.Fatalf("writeSyncFile O_EXCL: want error")
	}
	if err := syncDir(filepath.Join(root, "missing")); err == nil {
		t.Fatalf("syncDir missing: want error")
	}
	if err := syncDir(root); err != nil {
		t.Fatalf("syncDir: %v", err)
	}
	if _, _, _, err := hashFile(filepath.Join(root, "missing")); err == nil {
		t.Fatalf("hashFile missing: want error")
	}
}

func TestCoverGapVerifyDirect(t *testing.T) {
	s, _ := gapStore(t)
	ctx := context.Background()
	if err := s.Verify(ctx, StagedObject{}); CodeOf(err) != CodeInvalidRequest {
		t.Fatalf("Verify empty: %v", err)
	}
	ghost := StagedObject{ContentID: primitive.NewObjectID(), PublicationID: primitive.NewObjectID(),
		Path: filepath.Join("nope", "missing"), SHA256: strings.Repeat("a", 64)}
	if err := s.Verify(ctx, ghost); CodeOf(err) != CodeNotFound {
		t.Fatalf("Verify missing: %v", err)
	}
}

// blockPath creates a regular file at p so any MkdirAll needing p as a
// directory fails deterministically with ENOTDIR (no permission tricks).
func blockPath(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	if err := os.WriteFile(p, []byte("block"), 0o644); err != nil {
		t.Fatalf("block file: %v", err)
	}
}

func TestCoverGapMkdirBlocked(t *testing.T) {
	ctx := context.Background()
	s, root := gapStore(t)

	// Stage's publication directory blocked by a file.
	cid := primitive.NewObjectID()
	pid := primitive.NewObjectID()
	blockPath(t, filepath.Join(root, "publications", cid.Hex()))
	if _, err := s.Stage(ctx, StageRequest{ContentID: cid, PublicationID: pid,
		CanonicalPath: "/gap/blocked", HTML: []byte("<h1>x</h1>")}); CodeOf(err) != CodeIO {
		t.Fatalf("Stage blocked dir: %v", err)
	}

	// Canonical directory blocked by a file: Activate + ActivateWithPrevious
	// fail creating the canonical directory; Restore fails the same way.
	v1 := gapStage(t, s, "/gap/ok", []byte("<h1>ok</h1>"))
	blockPath(t, filepath.Join(root, "generated", "gap2", "sub"))
	if err := s.Activate(ctx, v1, "/gap2/sub/page"); CodeOf(err) != CodeIO {
		t.Fatalf("Activate blocked dir: %v", err)
	}
	if err := s.ActivateWithPrevious(ctx, v1, "/gap2/sub/page", &pid); CodeOf(err) != CodeIO {
		t.Fatalf("ActivateWithPrevious blocked dir: %v", err)
	}
	if err := s.Restore(ctx, v1.Stored(), "/gap2/sub/page"); CodeOf(err) != CodeIO {
		t.Fatalf("Restore blocked dir: %v", err)
	}
}

func TestCoverGapCutoverReadAndWriteIO(t *testing.T) {
	ctx := context.Background()
	s, _ := gapStore(t)
	v1 := gapStage(t, s, "/gap/iocut", []byte("<h1>v1</h1>"))
	if err := s.Activate(ctx, v1, "/gap/iocut"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	oldID := v1.PublicationID

	// Immutable path is a directory: read fails with IO (not NotFound).
	v2 := gapStage(t, s, "/gap/iocut", []byte("<h1>v222222</h1>"))
	if err := os.Remove(v2.Path); err != nil {
		t.Fatalf("remove immutable: %v", err)
	}
	if err := os.MkdirAll(v2.Path, 0o755); err != nil {
		t.Fatalf("mkdir immutable: %v", err)
	}
	if err := s.ActivateWithPrevious(ctx, v2, "/gap/iocut", &oldID); CodeOf(err) != CodeIO {
		t.Fatalf("cutover read IO: %v", err)
	}

	// .next path occupied by a non-empty directory: write fails with IO.
	v3 := gapStage(t, s, "/gap/iocut", []byte("<h1>v333333</h1>"))
	canon := mustCanonicalGap(t, s, "/gap/iocut")
	next := canon + ".next-" + v3.PublicationID.Hex()
	if err := os.MkdirAll(next, 0o755); err != nil {
		t.Fatalf("mkdir next: %v", err)
	}
	if err := os.WriteFile(filepath.Join(next, "child"), []byte("x"), 0o644); err != nil {
		t.Fatalf("next child: %v", err)
	}
	if err := s.ActivateWithPrevious(ctx, v3, "/gap/iocut", &oldID); CodeOf(err) != CodeIO {
		t.Fatalf("cutover write IO: %v", err)
	}
}

func TestCoverGapRemoveIO(t *testing.T) {
	ctx := context.Background()
	s, _ := gapStore(t)

	// RemoveUnpublishBackup over a non-empty directory backup → IO error.
	v1 := gapStage(t, s, "/gap/rmio", []byte("<h1>v1</h1>"))
	if err := s.Activate(ctx, v1, "/gap/rmio"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	canon := mustCanonicalGap(t, s, "/gap/rmio")
	pid := primitive.NewObjectID()
	backup := canon + ".unpublish-backup-" + pid.Hex()
	if err := os.MkdirAll(backup, 0o755); err != nil {
		t.Fatalf("mkdir backup: %v", err)
	}
	if err := os.WriteFile(filepath.Join(backup, "child"), []byte("x"), 0o644); err != nil {
		t.Fatalf("backup child: %v", err)
	}
	if err := s.RemoveUnpublishBackup(ctx, "/gap/rmio", pid); CodeOf(err) != CodeIO {
		t.Fatalf("RemoveUnpublishBackup IO: %v", err)
	}

	// DeleteImmutable over a non-empty directory immutable → IO error.
	imm := s.ImmutablePath(v1.ContentID, v1.PublicationID)
	if err := os.Remove(imm); err != nil {
		t.Fatalf("remove staged immutable: %v", err)
	}
	if err := os.MkdirAll(imm, 0o755); err != nil {
		t.Fatalf("mkdir imm: %v", err)
	}
	if err := os.WriteFile(filepath.Join(imm, "child"), []byte("x"), 0o644); err != nil {
		t.Fatalf("imm child: %v", err)
	}
	if err := s.DeleteImmutable(ctx, v1.ContentID, v1.PublicationID); CodeOf(err) != CodeIO {
		t.Fatalf("DeleteImmutable IO: %v", err)
	}

	// Abort over a non-empty directory staged path → IO error.
	dirObj := StagedObject{ContentID: v1.ContentID, PublicationID: v1.PublicationID, Path: imm}
	if err := s.Abort(ctx, dirObj); CodeOf(err) != CodeIO {
		t.Fatalf("Abort IO: %v", err)
	}

	// CompensateActivate with canonical as a non-empty directory: reading
	// the new canonical fails with IO.
	v2 := gapStage(t, s, "/gap/rmio", []byte("<h1>v2 with bytes</h1>"))
	oldID := v1.PublicationID
	if err := s.ActivateWithPrevious(ctx, v2, "/gap/rmio", &oldID); err != nil {
		t.Fatalf("ActivateWithPrevious: %v", err)
	}
	if err := os.Remove(canon); err != nil {
		t.Fatalf("remove canonical: %v", err)
	}
	if err := os.MkdirAll(canon, 0o755); err != nil {
		t.Fatalf("mkdir canon: %v", err)
	}
	if err := os.WriteFile(filepath.Join(canon, "child"), []byte("x"), 0o644); err != nil {
		t.Fatalf("canon child: %v", err)
	}
	if err := s.CompensateActivate(ctx, v2, "/gap/rmio", &oldID); CodeOf(err) != CodeIO {
		t.Fatalf("compensate read IO: %v", err)
	}
}

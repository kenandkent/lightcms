package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// FilesystemStore is the MVP filesystem StaticPageStore (spec §17.2, §17.4).
//
// Root is the content storage root (e.g. "content"). Immutable objects live
// under Root/publications and canonical projections under Root/generated.
// All cutover temporaries (.next-*) are created in the canonical file's own
// directory so every rename is same-filesystem and atomic. No hard links are
// used; cutover copies bytes.
//
// StageTimeout overrides the stale-stage timeout; values <= 0 select
// DefaultStageTimeout (spec §36 PUBLICATION_STAGE_TIMEOUT_MINUTES default 15).
//
// MaxWriteBytes is a test-only fault hook: when > 0, file writes are
// truncated after MaxWriteBytes bytes. Verification then fails and no
// verified object is left behind, simulating a crashed/partial write.
type FilesystemStore struct {
	Root          string
	StageTimeout  time.Duration
	MaxWriteBytes int64
}

// NewFilesystemStore returns a store rooted at root.
func NewFilesystemStore(root string) *FilesystemStore {
	return &FilesystemStore{Root: root}
}

// Timeout returns the effective stage timeout.
func (s *FilesystemStore) Timeout() time.Duration {
	if s.StageTimeout > 0 {
		return s.StageTimeout
	}
	return DefaultStageTimeout
}

// IsStale reports whether modTime is older than the stage timeout.
func (s *FilesystemStore) IsStale(modTime, now time.Time) bool {
	return now.Sub(modTime) > s.Timeout()
}

func (s *FilesystemStore) publicationsRoot() string {
	return filepath.Join(s.Root, "publications")
}

func (s *FilesystemStore) generatedRoot() string {
	return filepath.Join(s.Root, "generated")
}

// ImmutablePath returns content/publications/{content}/{publication}/index.html.
func (s *FilesystemStore) ImmutablePath(contentID, publicationID primitive.ObjectID) string {
	return filepath.Join(s.publicationsRoot(), contentID.Hex(), publicationID.Hex(), "index.html")
}

// CanonicalFilePath resolves a public path to its canonical projection file:
// content/generated/{canonical_path}.html ("/" maps to index.html).
func (s *FilesystemStore) CanonicalFilePath(publicPath string) (string, error) {
	if err := validatePublicPath(publicPath); err != nil {
		return "", err
	}
	rel := strings.TrimPrefix(publicPath, "/")
	var name string
	if rel == "" {
		name = "index.html"
	} else {
		name = filepath.FromSlash(rel) + ".html"
	}
	full := filepath.Join(s.generatedRoot(), name)
	if rel2, err := filepath.Rel(s.generatedRoot(), full); err != nil ||
		rel2 == ".." || strings.HasPrefix(rel2, ".."+string(filepath.Separator)) {
		return "", storeErr(CodeInvalidPath, "path escapes generated root", publicPath, nil)
	}
	return full, nil
}

// maxPublicPathLen bounds canonical path length for filesystem safety.
const maxPublicPathLen = 1024

// validatePublicPath enforces filesystem safety for caller-supplied paths.
// It does not canonicalize casing or Unicode; canonical allocation stays
// with Task 2 (pathkey). Callers must pass already-canonical paths.
func validatePublicPath(p string) error {
	if p == "" {
		return storeErr(CodeInvalidPath, "empty path", p, nil)
	}
	if len(p) > maxPublicPathLen {
		return storeErr(CodeInvalidPath, "path too long", p, nil)
	}
	if !strings.HasPrefix(p, "/") {
		return storeErr(CodeInvalidPath, "path must be absolute", p, nil)
	}
	if strings.ContainsRune(p, 0) || strings.ContainsAny(p, "\\?#") {
		return storeErr(CodeInvalidPath, "path contains illegal characters", p, nil)
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return storeErr(CodeInvalidPath, "path contains control characters", p, nil)
		}
	}
	if strings.Contains(strings.ToLower(p), "%2f") {
		return storeErr(CodeInvalidPath, "encoded slash is not allowed", p, nil)
	}
	if path.Clean(p) != p {
		return storeErr(CodeInvalidPath, "path is not clean", p, nil)
	}
	if p != "/" {
		for _, seg := range strings.Split(strings.TrimPrefix(p, "/"), "/") {
			if seg == "" || seg == "." || seg == ".." {
				return storeErr(CodeInvalidPath, "path contains empty or dot segment", p, nil)
			}
		}
	}
	return nil
}

func checkCtx(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}

func shaHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// writeSyncFile creates path exclusively, writes data (truncated to
// maxWrite when maxWrite > 0, a test fault hook), fsyncs, and closes,
// reporting close/sync errors. Callers must verify the result by re-read.
func writeSyncFile(filePath string, data []byte, maxWrite int64) error {
	f, err := os.OpenFile(filePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	out := data
	if maxWrite > 0 && int64(len(out)) > maxWrite {
		out = out[:maxWrite]
	}
	_, werr := f.Write(out)
	serr := f.Sync()
	cerr := f.Close()
	switch {
	case werr != nil:
		return werr
	case serr != nil:
		return serr
	default:
		return cerr
	}
}

// syncDir fsyncs a directory so renames inside it are durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	serr := d.Sync()
	cerr := d.Close()
	if serr != nil {
		return serr
	}
	return cerr
}

// hashFile reads a file fully and returns its size, SHA-256 hex, and modtime.
func hashFile(filePath string) (int64, string, time.Time, error) {
	b, err := os.ReadFile(filePath)
	if err != nil {
		return 0, "", time.Time{}, err
	}
	st, err := os.Stat(filePath)
	if err != nil {
		return 0, "", time.Time{}, err
	}
	return st.Size(), shaHex(b), st.ModTime(), nil
}

func isNotExist(err error) bool { return os.IsNotExist(err) }

// Stage writes HTML to a .tmp file, fsyncs, verifies size/SHA-256, and
// atomically renames to the immutable index.html (§17.4 stage algorithm).
// Any failure leaves neither a verified object nor a .tmp behind.
// Re-staging identical bytes under the same publication ID is idempotent;
// different bytes under the same ID return CodeAlreadyExists.
func (s *FilesystemStore) Stage(ctx context.Context, req StageRequest) (StagedObject, error) {
	if err := checkCtx(ctx); err != nil {
		return StagedObject{}, err
	}
	if req.ContentID.IsZero() || req.PublicationID.IsZero() {
		return StagedObject{}, storeErr(CodeInvalidRequest, "content and publication IDs are required", "", nil)
	}
	if err := validatePublicPath(req.CanonicalPath); err != nil {
		return StagedObject{}, err
	}
	if len(req.HTML) == 0 {
		return StagedObject{}, storeErr(CodeEmptyBody, "HTML body is empty", "", nil)
	}
	sum := shaHex(req.HTML)
	if req.ExpectedSHA256 != "" {
		want := strings.ToLower(strings.TrimSpace(req.ExpectedSHA256))
		if len(want) != 64 {
			return StagedObject{}, storeErr(CodeInvalidRequest, "ExpectedSHA256 must be 64 hex characters", "", nil)
		}
		if sum != want {
			return StagedObject{}, storeErr(CodeHashMismatch, "HTML does not match ExpectedSHA256; nothing staged", "", nil)
		}
	}

	immutable := s.ImmutablePath(req.ContentID, req.PublicationID)
	obj := StagedObject{
		ContentID:     req.ContentID,
		PublicationID: req.PublicationID,
		Path:          immutable,
		SHA256:        sum,
	}
	if err := os.MkdirAll(filepath.Dir(immutable), 0o755); err != nil {
		return StagedObject{}, storeErr(CodeIO, "create publication directory", immutable, err)
	}
	if _, _, _, err := hashFile(immutable); err == nil {
		// Immutable object already staged: idempotent only for identical bytes.
		existing, rerr := os.ReadFile(immutable)
		if rerr != nil {
			return StagedObject{}, storeErr(CodeIO, "read existing immutable object", immutable, rerr)
		}
		if shaHex(existing) != sum {
			return StagedObject{}, storeErr(CodeAlreadyExists, "publication already staged with different bytes", immutable, nil)
		}
		return obj, nil
	} else if !isNotExist(err) {
		return StagedObject{}, storeErr(CodeIO, "stat immutable object", immutable, err)
	}

	tmp := immutable + ".tmp"
	_ = os.Remove(tmp) // clear a leftover from a crashed stage
	if err := writeSyncFile(tmp, req.HTML, s.MaxWriteBytes); err != nil {
		_ = os.Remove(tmp)
		if os.IsExist(err) {
			return StagedObject{}, storeErr(CodeAlreadyExists, "temporary stage file already exists", tmp, err)
		}
		return StagedObject{}, storeErr(CodeIO, "write temporary stage file", tmp, err)
	}
	size, landed, _, verr := hashFile(tmp)
	if verr != nil {
		_ = os.Remove(tmp)
		return StagedObject{}, storeErr(CodeIO, "verify temporary stage file", tmp, verr)
	}
	if size != int64(len(req.HTML)) || landed != sum {
		_ = os.Remove(tmp)
		return StagedObject{}, storeErr(CodeHashMismatch, "staged bytes failed size/hash verification; nothing staged", tmp, nil)
	}
	if err := os.Rename(tmp, immutable); err != nil {
		_ = os.Remove(tmp)
		return StagedObject{}, storeErr(CodeIO, "rename staged file to immutable object", immutable, err)
	}
	if err := syncDir(filepath.Dir(immutable)); err != nil {
		return StagedObject{}, storeErr(CodeIO, "fsync publication directory", immutable, err)
	}
	return obj, nil
}

// Verify re-checks that the immutable object exists and matches its SHA-256.
func (s *FilesystemStore) Verify(_ context.Context, obj StagedObject) error {
	if obj.Path == "" || obj.SHA256 == "" {
		return storeErr(CodeInvalidRequest, "staged object path and SHA-256 are required", obj.Path, nil)
	}
	size, landed, _, err := hashFile(obj.Path)
	if err != nil {
		if isNotExist(err) {
			return storeErr(CodeNotFound, "immutable object is missing", obj.Path, err)
		}
		return storeErr(CodeIO, "read immutable object", obj.Path, err)
	}
	if size == 0 || landed != strings.ToLower(obj.SHA256) {
		return storeErr(CodeHashMismatch, "immutable object failed verification", obj.Path, nil)
	}
	return nil
}

// nextPath returns the in-flight cutover file for a publication.
func nextPath(canonical string, publicationID primitive.ObjectID) string {
	return canonical + ".next-" + publicationID.Hex()
}

// previousPath returns the pre-cutover backup name for the old publication.
func previousPath(canonical string, oldPublicationID primitive.ObjectID) string {
	return canonical + ".previous-" + oldPublicationID.Hex()
}

// cutover performs the §17.4 activation file sequence with zero serving
// gap: the next bytes are fully prepared and verified while the old
// canonical keeps serving, the old bytes are copied (not moved) to a
// recoverable .previous backup, and a single atomic rename publishes the
// new file. Readers observe the complete old or new file — never a missing
// canonical, unlike the previous move-first sequence. Any failure before
// the final rename leaves the canonical untouched (only .next/previous
// leftovers, both scanner-convergent).
func (s *FilesystemStore) cutover(obj StagedObject, canonical string, oldID *primitive.ObjectID) error {
	canonExisted := true
	if _, err := os.Stat(canonical); err != nil {
		if !isNotExist(err) {
			return storeErr(CodeIO, "stat canonical file", canonical, err)
		}
		canonExisted = false
	}
	if canonExisted && oldID == nil {
		return storeErr(CodeNeedsPreviousID, "canonical exists; use ActivateWithPrevious with the old publication ID", canonical, nil)
	}

	next := nextPath(canonical, obj.PublicationID)
	_ = os.Remove(next) // clear a leftover from a crashed cutover
	src, err := os.ReadFile(obj.Path)
	if err != nil {
		if isNotExist(err) {
			return storeErr(CodeNotFound, "immutable object is missing", obj.Path, err)
		}
		return storeErr(CodeIO, "read immutable object", obj.Path, err)
	}
	if shaHex(src) != strings.ToLower(obj.SHA256) {
		return storeErr(CodeHashMismatch, "immutable object failed verification; canonical untouched", obj.Path, nil)
	}
	if err := writeSyncFile(next, src, s.MaxWriteBytes); err != nil {
		_ = os.Remove(next)
		return storeErr(CodeIO, "write next cutover file", next, err)
	}
	if _, landed, _, verr := hashFile(next); verr != nil || landed != strings.ToLower(obj.SHA256) {
		_ = os.Remove(next)
		if verr != nil {
			return storeErr(CodeIO, "verify next cutover file", next, verr)
		}
		return storeErr(CodeHashMismatch, "cutover copy failed verification; canonical untouched", next, nil)
	}

	if canonExisted {
		// Recoverable backup of the old bytes. A copy (not a move) keeps
		// the old page serving until the atomic rename below.
		prev := previousPath(canonical, *oldID)
		if err := copyFile(prev, canonical); err != nil {
			_ = os.Remove(next)
			return storeErr(CodeIO, "back up canonical to previous", prev, err)
		}
		if err := syncDir(filepath.Dir(canonical)); err != nil {
			_ = os.Remove(next)
			_ = os.Remove(prev)
			return storeErr(CodeIO, "fsync canonical directory", canonical, err)
		}
	}

	if err := os.Rename(next, canonical); err != nil {
		_ = os.Remove(next)
		return storeErr(CodeIO, "rename next file to canonical", canonical, err)
	}
	if err := syncDir(filepath.Dir(canonical)); err != nil {
		return storeErr(CodeIO, "fsync canonical directory", canonical, err)
	}
	return nil
}

// copyFile copies src to dst with an fsync before close, so the backup is
// durable once the call returns.
func copyFile(dst, src string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// Activate performs a first-publish cutover. It verifies the staged object,
// then copies immutable bytes to a same-directory .next file, verifies the
// copy, and atomically renames it to canonical. When a canonical file
// already exists it returns CodeNeedsPreviousID; use ActivateWithPrevious.
func (s *FilesystemStore) Activate(ctx context.Context, obj StagedObject, publicPath string) error {
	if err := checkCtx(ctx); err != nil {
		return err
	}
	if err := s.Verify(ctx, obj); err != nil {
		return err
	}
	canonical, err := s.CanonicalFilePath(publicPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(canonical), 0o755); err != nil {
		return storeErr(CodeIO, "create canonical directory", canonical, err)
	}
	return s.cutover(obj, canonical, nil)
}

// ActivateWithPrevious cuts over obj while preserving the existing canonical
// as .previous-{oldPublicationID} (§17.4). oldPublicationID must be non-nil
// when a canonical file exists; on first publish it may be nil.
func (s *FilesystemStore) ActivateWithPrevious(ctx context.Context, obj StagedObject, publicPath string, oldPublicationID *primitive.ObjectID) error {
	if err := checkCtx(ctx); err != nil {
		return err
	}
	if err := s.Verify(ctx, obj); err != nil {
		return err
	}
	canonical, err := s.CanonicalFilePath(publicPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(canonical), 0o755); err != nil {
		return storeErr(CodeIO, "create canonical directory", canonical, err)
	}
	return s.cutover(obj, canonical, oldPublicationID)
}

// CompensateActivate undoes a cutover after the Mongo activation transaction
// fails (§17.4 step 8): it removes the new canonical only if it still carries
// obj's bytes, then renames .previous-{oldID} back to canonical. Both the
// retained immutable object and the restored previous file stay available
// for the Task 8 recovery path.
func (s *FilesystemStore) CompensateActivate(_ context.Context, obj StagedObject, publicPath string, oldPublicationID *primitive.ObjectID) error {
	if oldPublicationID == nil {
		return storeErr(CodeInvalidRequest, "old publication ID is required for compensation", publicPath, nil)
	}
	canonical, err := s.CanonicalFilePath(publicPath)
	if err != nil {
		return err
	}
	prev := previousPath(canonical, *oldPublicationID)
	if _, err := os.Stat(prev); err != nil {
		if isNotExist(err) {
			return storeErr(CodeNotFound, "previous backup is missing; nothing to restore", prev, err)
		}
		return storeErr(CodeIO, "stat previous backup", prev, err)
	}
	if _, err := os.Stat(canonical); err == nil {
		_, landed, _, herr := hashFile(canonical)
		if herr != nil {
			return storeErr(CodeIO, "read new canonical for compensation", canonical, herr)
		}
		if landed != strings.ToLower(obj.SHA256) {
			return storeErr(CodeConflict, "canonical no longer carries the new bytes; refusing compensation", canonical, nil)
		}
		if err := os.Remove(canonical); err != nil {
			return storeErr(CodeIO, "remove failed new canonical", canonical, err)
		}
	} else if !isNotExist(err) {
		return storeErr(CodeIO, "stat canonical for compensation", canonical, err)
	}
	if err := os.Rename(prev, canonical); err != nil {
		return storeErr(CodeIO, "rename previous backup back to canonical", canonical, err)
	}
	if err := syncDir(filepath.Dir(canonical)); err != nil {
		return storeErr(CodeIO, "fsync canonical directory", canonical, err)
	}
	return nil
}

// ConfirmActivate removes .previous-{oldID} after the activation transaction
// commits (retention handling beyond deletion is Task 10 policy). Idempotent.
func (s *FilesystemStore) ConfirmActivate(_ context.Context, publicPath string, oldPublicationID primitive.ObjectID) error {
	canonical, err := s.CanonicalFilePath(publicPath)
	if err != nil {
		return err
	}
	prev := previousPath(canonical, oldPublicationID)
	if err := os.Remove(prev); err != nil && !isNotExist(err) {
		return storeErr(CodeIO, "remove previous backup", prev, err)
	}
	if err := syncDir(filepath.Dir(canonical)); err != nil {
		return storeErr(CodeIO, "fsync canonical directory", canonical, err)
	}
	return nil
}

func unpublishBackupPath(canonical string, publicationID primitive.ObjectID) string {
	return canonical + ".unpublish-backup-" + publicationID.Hex()
}

// StageUnpublishBackup renames the canonical file to
// .unpublish-backup-{publicationID} (§18.1). The Mongo unpublish transaction
// that follows either commits (then RemoveUnpublishBackup) or fails (then
// RestoreUnpublishBackup).
func (s *FilesystemStore) StageUnpublishBackup(_ context.Context, publicPath string, publicationID primitive.ObjectID) (string, error) {
	canonical, err := s.CanonicalFilePath(publicPath)
	if err != nil {
		return "", err
	}
	backup := unpublishBackupPath(canonical, publicationID)
	if _, err := os.Stat(canonical); err != nil {
		if isNotExist(err) {
			return "", storeErr(CodeNotFound, "canonical file is missing; nothing to stage away", canonical, err)
		}
		return "", storeErr(CodeIO, "stat canonical file", canonical, err)
	}
	if _, err := os.Stat(backup); err == nil {
		return "", storeErr(CodeAlreadyExists, "unpublish backup already exists", backup, nil)
	} else if !isNotExist(err) {
		return "", storeErr(CodeIO, "stat unpublish backup", backup, err)
	}
	if err := os.Rename(canonical, backup); err != nil {
		return "", storeErr(CodeIO, "rename canonical to unpublish backup", canonical, err)
	}
	if err := syncDir(filepath.Dir(canonical)); err != nil {
		return "", storeErr(CodeIO, "fsync canonical directory", canonical, err)
	}
	return backup, nil
}

// RestoreUnpublishBackup renames the backup back to canonical, compensating
// a failed unpublish transaction. It refuses to clobber an existing canonical.
func (s *FilesystemStore) RestoreUnpublishBackup(_ context.Context, publicPath string, publicationID primitive.ObjectID) error {
	canonical, err := s.CanonicalFilePath(publicPath)
	if err != nil {
		return err
	}
	backup := unpublishBackupPath(canonical, publicationID)
	if _, err := os.Stat(backup); err != nil {
		if isNotExist(err) {
			return storeErr(CodeNotFound, "unpublish backup is missing", backup, err)
		}
		return storeErr(CodeIO, "stat unpublish backup", backup, err)
	}
	if _, err := os.Stat(canonical); err == nil {
		return storeErr(CodeConflict, "canonical already exists; refusing to restore over it", canonical, nil)
	} else if !isNotExist(err) {
		return storeErr(CodeIO, "stat canonical file", canonical, err)
	}
	if err := os.Rename(backup, canonical); err != nil {
		return storeErr(CodeIO, "rename unpublish backup back to canonical", canonical, err)
	}
	if err := syncDir(filepath.Dir(canonical)); err != nil {
		return storeErr(CodeIO, "fsync canonical directory", canonical, err)
	}
	return nil
}

// RemoveUnpublishBackup deletes the backup after the unpublish transaction
// commits. Idempotent.
func (s *FilesystemStore) RemoveUnpublishBackup(_ context.Context, publicPath string, publicationID primitive.ObjectID) error {
	canonical, err := s.CanonicalFilePath(publicPath)
	if err != nil {
		return err
	}
	backup := unpublishBackupPath(canonical, publicationID)
	if err := os.Remove(backup); err != nil {
		if isNotExist(err) {
			return nil
		}
		return storeErr(CodeIO, "remove unpublish backup", backup, err)
	}
	if err := syncDir(filepath.Dir(canonical)); err != nil {
		return storeErr(CodeIO, "fsync canonical directory", canonical, err)
	}
	return nil
}

// Restore rebuilds the canonical projection from retained immutable bytes
// (scanner repair and exact rollback support). A canonical that already
// carries obj's bytes is a no-op success. A mismatched canonical is replaced
// atomically; isolating the bad bytes first is Task 10 scanner policy, which
// must Inspect before calling Restore.
func (s *FilesystemStore) Restore(_ context.Context, obj StoredObject, publicPath string) error {
	if obj.Path == "" || obj.SHA256 == "" || obj.ContentID.IsZero() || obj.PublicationID.IsZero() {
		return storeErr(CodeInvalidRequest, "stored object path, hash, and IDs are required", obj.Path, nil)
	}
	src, err := os.ReadFile(obj.Path)
	if err != nil {
		if isNotExist(err) {
			return storeErr(CodeNotFound, "immutable object is missing; cannot rebuild canonical", obj.Path, err)
		}
		return storeErr(CodeIO, "read immutable object", obj.Path, err)
	}
	if shaHex(src) != strings.ToLower(obj.SHA256) {
		return storeErr(CodeHashMismatch, "immutable object failed verification; refusing rebuild", obj.Path, nil)
	}
	canonical, err := s.CanonicalFilePath(publicPath)
	if err != nil {
		return err
	}
	if existing, err := os.ReadFile(canonical); err == nil && shaHex(existing) == strings.ToLower(obj.SHA256) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(canonical), 0o755); err != nil {
		return storeErr(CodeIO, "create canonical directory", canonical, err)
	}
	tmp := canonical + ".restore-" + obj.PublicationID.Hex()
	_ = os.Remove(tmp)
	if err := writeSyncFile(tmp, src, s.MaxWriteBytes); err != nil {
		_ = os.Remove(tmp)
		return storeErr(CodeIO, "write restore file", tmp, err)
	}
	if _, landed, _, verr := hashFile(tmp); verr != nil || landed != strings.ToLower(obj.SHA256) {
		_ = os.Remove(tmp)
		if verr != nil {
			return storeErr(CodeIO, "verify restore file", tmp, verr)
		}
		return storeErr(CodeHashMismatch, "restore copy failed verification", tmp, nil)
	}
	if err := os.Rename(tmp, canonical); err != nil {
		_ = os.Remove(tmp)
		return storeErr(CodeIO, "rename restore file to canonical", canonical, err)
	}
	if err := syncDir(filepath.Dir(canonical)); err != nil {
		return storeErr(CodeIO, "fsync canonical directory", canonical, err)
	}
	return nil
}

// Open returns the verified immutable bytes. The object is hash-checked
// before serving so corrupt bytes are never served silently.
func (s *FilesystemStore) Open(_ context.Context, obj StoredObject) (io.ReadCloser, error) {
	if obj.Path == "" || obj.SHA256 == "" {
		return nil, storeErr(CodeInvalidRequest, "stored object path and hash are required", obj.Path, nil)
	}
	b, err := os.ReadFile(obj.Path)
	if err != nil {
		if isNotExist(err) {
			return nil, storeErr(CodeNotFound, "immutable object is missing", obj.Path, err)
		}
		return nil, storeErr(CodeIO, "read immutable object", obj.Path, err)
	}
	if shaHex(b) != strings.ToLower(obj.SHA256) {
		return nil, storeErr(CodeHashMismatch, "immutable object failed verification", obj.Path, nil)
	}
	return io.NopCloser(strings.NewReader(string(b))), nil
}

// Abort removes a staged .tmp and immutable object (failed/stale stage
// cleanup mechanic). Idempotent and never touches canonical files.
func (s *FilesystemStore) Abort(_ context.Context, obj StagedObject) error {
	if obj.Path == "" {
		return storeErr(CodeInvalidRequest, "staged object path is required", "", nil)
	}
	for _, p := range []string{obj.Path + ".tmp", obj.Path} {
		if err := os.Remove(p); err != nil && !isNotExist(err) {
			return storeErr(CodeIO, "remove staged file", p, err)
		}
	}
	s.pruneImmutableParents(obj.Path)
	return nil
}

// Delete removes the canonical projection file. Idempotent: an already
// absent canonical is success. Sidecar cleanup is scanner/GC policy.
func (s *FilesystemStore) Delete(_ context.Context, publicPath string) error {
	canonical, err := s.CanonicalFilePath(publicPath)
	if err != nil {
		return err
	}
	if err := os.Remove(canonical); err != nil {
		if isNotExist(err) {
			return nil
		}
		return storeErr(CodeIO, "delete canonical file", canonical, err)
	}
	if err := syncDir(filepath.Dir(canonical)); err != nil {
		return storeErr(CodeIO, "fsync canonical directory", canonical, err)
	}
	return nil
}

// Exists reports whether the canonical projection file is present.
func (s *FilesystemStore) Exists(_ context.Context, publicPath string) (bool, error) {
	canonical, err := s.CanonicalFilePath(publicPath)
	if err != nil {
		return false, err
	}
	if _, err := os.Stat(canonical); err != nil {
		if isNotExist(err) {
			return false, nil
		}
		return false, storeErr(CodeIO, "stat canonical file", canonical, err)
	}
	return true, nil
}

// DeleteImmutable removes one immutable publication object plus any leftover
// .tmp (retention GC mechanic; policy owned by Task 10). Idempotent and
// never touches canonical files.
func (s *FilesystemStore) DeleteImmutable(_ context.Context, contentID, publicationID primitive.ObjectID) error {
	if contentID.IsZero() || publicationID.IsZero() {
		return storeErr(CodeInvalidRequest, "content and publication IDs are required", "", nil)
	}
	immutable := s.ImmutablePath(contentID, publicationID)
	for _, p := range []string{immutable + ".tmp", immutable} {
		if err := os.Remove(p); err != nil && !isNotExist(err) {
			return storeErr(CodeIO, "remove immutable object", p, err)
		}
	}
	s.pruneImmutableParents(immutable)
	return nil
}

// pruneImmutableParents removes now-empty publication/content directories.
// Best effort: only empty directories are removed; errors are ignored.
func (s *FilesystemStore) pruneImmutableParents(immutable string) {
	dir := filepath.Dir(immutable)
	for i := 0; i < 2; i++ {
		if err := os.Remove(dir); err != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}

// sidecarPrefix table for Inspect parsing.
var sidecarKinds = []struct {
	suffix string
	kind   SidecarKind
}{
	{".previous-", SidecarPrevious},
	{".next-", SidecarNext},
	{".unpublish-backup-", SidecarUnpublishBackup},
}

// Inspect reports canonical presence/hash plus every recovery sidecar next
// to it, giving the Task 10 scanner the metadata for reconciliation. A
// missing canonical is reported with Exists=false, not as an error.
func (s *FilesystemStore) Inspect(_ context.Context, publicPath string) (CanonicalInfo, error) {
	canonical, err := s.CanonicalFilePath(publicPath)
	if err != nil {
		return CanonicalInfo{}, err
	}
	info := CanonicalInfo{Path: canonical}
	if size, sum, mod, err := hashFile(canonical); err == nil {
		info.Exists = true
		info.Size = size
		info.SHA256 = sum
		info.ModTime = mod
	} else if !isNotExist(err) {
		return CanonicalInfo{}, storeErr(CodeIO, "read canonical file", canonical, err)
	}
	dir := filepath.Dir(canonical)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if isNotExist(err) {
			return info, nil
		}
		return CanonicalInfo{}, storeErr(CodeIO, "list canonical directory", dir, err)
	}
	base := filepath.Base(canonical)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		kind, pid, ok := parseSidecarName(base, e.Name())
		if !ok {
			continue
		}
		full := filepath.Join(dir, e.Name())
		size, sum, mod, herr := hashFile(full)
		if herr != nil {
			return CanonicalInfo{}, storeErr(CodeIO, "read sidecar file", full, herr)
		}
		info.Sidecars = append(info.Sidecars, Sidecar{
			Kind:          kind,
			PublicationID: pid,
			Path:          full,
			Size:          size,
			SHA256:        sum,
			ModTime:       mod,
		})
	}
	sort.Slice(info.Sidecars, func(i, j int) bool { return info.Sidecars[i].Path < info.Sidecars[j].Path })
	return info, nil
}

// parseSidecarName matches {base}.previous-{24hex} etc. and extracts the kind
// and publication ID.
func parseSidecarName(base, name string) (SidecarKind, primitive.ObjectID, bool) {
	if !strings.HasPrefix(name, base) {
		return "", primitive.NilObjectID, false
	}
	rest := strings.TrimPrefix(name, base)
	for _, k := range sidecarKinds {
		if !strings.HasPrefix(rest, k.suffix) {
			continue
		}
		hexID := strings.TrimPrefix(rest, k.suffix)
		if len(hexID) != 24 {
			return "", primitive.NilObjectID, false
		}
		pid, err := primitive.ObjectIDFromHex(hexID)
		if err != nil {
			return "", primitive.NilObjectID, false
		}
		return k.kind, pid, true
	}
	return "", primitive.NilObjectID, false
}

package staticdelivery

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/gob"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path"
	"reflect"
	"strings"

	ex "github.com/metavert-io/lightcms/v7/internal/product/staticexport"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// FileReleaseStore persists both public bytes and private source snapshots.
// Public JSON intentionally omits source HTML/data; it is NOT sufficient to
// restore a release. This store is never registered as an HTTP file server.
type FileReleaseStore struct{ config Config }

func NewFileReleaseStore(c Config) *FileReleaseStore { return &FileReleaseStore{c} }
func (s *FileReleaseStore) root() (*os.Root, error) {
	return ex.OpenPrivateRoot(ex.Options{ProjectRoot: s.config.ProjectRoot, Root: s.config.Root})
}
func releaseFilePath(name string) bool {
	return strings.HasPrefix(name, "/") && name != "/" && path.Clean(name) == name && !strings.ContainsAny(name, "\\%\x00?#") && !strings.HasPrefix(name, "//")
}
func validateRelease(r ReleaseManifest) error {
	seenRemoved := map[string]bool{}
	for _, removed := range r.RemovedPaths {
		_, exists := r.Pages[removed]
		if !validPagePath(removed) || exists || seenRemoved[removed] {
			return failure(CodeArtifactUnavailable, "invalid withdrawn page manifest", nil)
		}
		seenRemoved[removed] = true
	}
	if r.DeliveryMode != "" && !nativeDelivery(r) {
		return failure(CodeArtifactUnavailable, "unsupported release delivery mode", nil)
	}
	if r.SchemaVersion != 1 || r.ID.IsZero() || r.TargetID.IsZero() {
		return failure(CodeArtifactUnavailable, "invalid release identity", nil)
	}
	if _, e := NormalizeOrigin(r.Origin); e != nil {
		return failure(CodeArtifactUnavailable, "invalid release origin", nil)
	}
	allowed := map[string]bool{}
	var size int64
	for full, p := range r.Pages {
		if !validPagePath(full) || p.Path != full || p.ContentID.IsZero() {
			return failure(CodeArtifactUnavailable, "invalid release page ownership", nil)
		}
		allowed[p.File] = true
		for _, asset := range p.Assets {
			if !strings.HasPrefix(asset, "/_lc-assets/") {
				return failure(CodeArtifactUnavailable, "invalid page asset path", nil)
			}
			allowed[asset] = true
		}
		if f, ok := r.Files[p.File]; !ok || p.SHA256 != f.SHA256 {
			return failure(CodeArtifactUnavailable, "release page file is unavailable", nil)
		}
	}
	for name, f := range r.Files {
		size += int64(len(f.Data))
		if !releaseFilePath(name) || f.Path != name || int64(len(f.Data)) != f.Size || f.Size > 25<<20 || releaseHash(f.Data) != f.SHA256 || f.MIME == "" {
			return failure(CodeArtifactUnavailable, "release file failed validation", nil)
		}
		if !allowed[name] && !strings.HasPrefix(name, "/_lc-assets/") {
			return failure(CodeArtifactUnavailable, "file outside release allowlist", nil)
		}
	}
	for name := range allowed {
		if _, ok := r.Files[name]; !ok {
			return failure(CodeArtifactUnavailable, "release dependency is unavailable", nil)
		}
	}
	// Defensive bound while the builder operates in memory. Reject explicitly,
	// never truncate the site's complete manifest or upload a partial release.
	if size > 256<<20 {
		return failure(CodeAssetLimitExceeded, "release exceeds the in-memory build budget (256 MiB)", nil)
	}
	return nil
}
func (s *FileReleaseStore) Save(ctx context.Context, r ReleaseManifest) (err error) {
	if err = validateRelease(r); err != nil {
		return err
	}
	root, err := s.root()
	if err != nil {
		return err
	}
	defer root.Close()
	target := "releases/" + r.ID.Hex()
	if _, e := root.Lstat(target); e == nil {
		old, e := s.load(ctx, root, r.ID)
		if e != nil {
			return e
		}
		if reflect.DeepEqual(old, r) {
			return nil
		}
		return failure(CodeIdempotencyConflict, "release ID already belongs to different immutable bytes", nil)
	} else if !os.IsNotExist(e) {
		return e
	}
	staging := target + ".building-" + primitive.NewObjectID().Hex()
	if err = root.MkdirAll(staging+"/private", 0700); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = root.RemoveAll(staging)
		}
	}()
	for name, f := range r.Files {
		if err = ctx.Err(); err != nil {
			return err
		}
		if err = releaseWrite(root, staging+"/public"+name, f.Data); err != nil {
			return err
		}
	}
	private, err := root.OpenFile(staging+"/private/release.gob", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	hash := sha256.New()
	limited := &snapshotWriter{writer: io.MultiWriter(private, hash), remaining: 512 << 20}
	err = gob.NewEncoder(limited).Encode(r)
	if err == nil {
		err = private.Sync()
	}
	closeErr := private.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	manifest, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if err = releaseWrite(root, staging+"/private/manifest.json", manifest); err != nil {
		return err
	}
	marker, _ := json.Marshal(map[string]string{"snapshot_sha256": hex.EncodeToString(hash.Sum(nil)), "manifest_sha256": releaseHash(manifest)})
	if err = releaseWrite(root, staging+"/private/complete.json", marker); err != nil {
		return err
	}
	// Flush directory entries as well as files before installing the immutable
	// name. Verification on Load rejects incomplete packages after a crash.
	if err = syncReleaseDirs(root, staging); err != nil {
		return err
	}
	if err = root.Rename(staging, target); err != nil {
		old, e := s.load(ctx, root, r.ID)
		if e == nil && reflect.DeepEqual(old, r) {
			_ = root.RemoveAll(staging)
			return nil
		}
		return err
	}
	return syncReleaseDir(root, "releases")
}
func (s *FileReleaseStore) Load(ctx context.Context, id primitive.ObjectID) (ReleaseManifest, error) {
	root, e := s.root()
	if e != nil {
		return ReleaseManifest{}, e
	}
	defer root.Close()
	return s.load(ctx, root, id)
}

// Summary is a bounded metadata-only read for status pages. It is never used
// to prepare, upload, verify, or restore an immutable release.
func (s *FileReleaseStore) Summary(ctx context.Context, id primitive.ObjectID) (ReleaseManifest, error) {
	var r ReleaseManifest
	if err := ctx.Err(); err != nil {
		return r, err
	}
	if id.IsZero() {
		return r, failure(CodeInvalidRequest, "release ID is required", nil)
	}
	root, err := s.root()
	if err != nil {
		return r, err
	}
	defer root.Close()
	base := "releases/" + id.Hex() + "/private/"
	marker, err := releaseRead(root, base+"complete.json", 4096)
	if err != nil {
		return r, failure(CodeArtifactUnavailable, "release completion marker is unavailable", err)
	}
	var hashes map[string]string
	if json.Unmarshal(marker, &hashes) != nil {
		return r, failure(CodeArtifactUnavailable, "invalid completion marker", nil)
	}
	data, err := releaseRead(root, base+"manifest.json", 16<<20)
	if err != nil || releaseHash(data) != hashes["manifest_sha256"] {
		return r, failure(CodeArtifactUnavailable, "release summary is unavailable or corrupted", err)
	}
	if json.Unmarshal(data, &r) != nil || r.ID != id {
		return r, failure(CodeArtifactUnavailable, "invalid release summary", nil)
	}
	return r, nil
}
func (s *FileReleaseStore) load(ctx context.Context, root *os.Root, id primitive.ObjectID) (ReleaseManifest, error) {
	var r ReleaseManifest
	if id.IsZero() {
		return r, failure(CodeInvalidRequest, "release ID is required", nil)
	}
	rel := "releases/" + id.Hex()
	// Only absence of the release directory means it has never been prepared.
	// An installed but incomplete package must never trigger mutable rebuilding.
	if _, e := root.Lstat(rel); e != nil {
		return r, e
	}
	marker, e := releaseRead(root, rel+"/private/complete.json", 4096)
	if e != nil {
		return r, failure(CodeArtifactUnavailable, "release completion marker is unavailable", nil)
	}
	var hashes map[string]string
	if json.Unmarshal(marker, &hashes) != nil {
		return r, failure(CodeArtifactUnavailable, "invalid release completion marker", nil)
	}
	manifest, e := releaseRead(root, rel+"/private/manifest.json", 16<<20)
	if e != nil {
		return r, failure(CodeArtifactUnavailable, "release manifest is unavailable", nil)
	}
	private, e := root.Open(rel + "/private/release.gob")
	if e != nil {
		return r, failure(CodeArtifactUnavailable, "private release snapshot is unavailable", nil)
	}
	defer private.Close()
	stat, e := private.Stat()
	if e != nil || !stat.Mode().IsRegular() || stat.Size() > 512<<20 {
		return r, failure(CodeArtifactUnavailable, "private release snapshot exceeds budget", e)
	}
	hash := sha256.New()
	if gob.NewDecoder(io.TeeReader(private, hash)).Decode(&r) != nil || r.ID != id {
		return r, failure(CodeArtifactUnavailable, "release snapshot is unreadable", nil)
	}
	if _, e := io.Copy(hash, private); e != nil {
		return r, e
	}
	if hex.EncodeToString(hash.Sum(nil)) != hashes["snapshot_sha256"] || releaseHash(manifest) != hashes["manifest_sha256"] {
		return r, failure(CodeArtifactUnavailable, "release manifest or snapshot is corrupted", nil)
	}
	if e = validateRelease(r); e != nil {
		return r, e
	}
	expected, _ := json.Marshal(r)
	if !bytes.Equal(expected, manifest) {
		return r, failure(CodeArtifactUnavailable, "private and public release manifests disagree", nil)
	}
	for name, f := range r.Files {
		if e = ctx.Err(); e != nil {
			return r, e
		}
		b, e := releaseRead(root, rel+"/public"+name, 25<<20)
		if e != nil || releaseHash(b) != f.SHA256 {
			return r, failure(CodeArtifactUnavailable, "persisted release file is missing or corrupted", nil)
		}
	}
	return r, nil
}
func releaseWrite(root *os.Root, name string, data []byte) error {
	if e := root.MkdirAll(path.Dir(name), 0700); e != nil {
		return e
	}
	f, e := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	_, e = f.Write(data)
	if e == nil {
		e = f.Sync()
	}
	closeErr := f.Close()
	if e != nil {
		return e
	}
	return closeErr
}
func releaseRead(root *os.Root, name string, max int64) ([]byte, error) {
	f, e := root.Open(name)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	data, e := io.ReadAll(io.LimitReader(f, max+1))
	if e != nil {
		return nil, e
	}
	if int64(len(data)) > max {
		return nil, failure(CodeArtifactUnavailable, "release file exceeds read budget", nil)
	}
	return data, nil
}
func syncReleaseDirs(root *os.Root, name string) error {
	f, e := root.Open(name)
	if e != nil {
		return e
	}
	entries, e := f.ReadDir(-1)
	f.Close()
	if e != nil {
		return e
	}
	for _, entry := range entries {
		if entry.IsDir() {
			if e = syncReleaseDirs(root, path.Join(name, entry.Name())); e != nil {
				return e
			}
		}
	}
	return syncReleaseDir(root, name)
}
func syncReleaseDir(root *os.Root, name string) error {
	f, e := root.Open(name)
	if e != nil {
		return e
	}
	defer f.Close()
	return f.Sync()
}

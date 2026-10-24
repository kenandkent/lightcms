package staticdelivery

import (
	"bytes"
	"context"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"github.com/metavert-io/lightcms/v7/internal/database"
	"github.com/metavert-io/lightcms/v7/internal/product/artifacts"
	ex "github.com/metavert-io/lightcms/v7/internal/product/staticexport"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"io"
	"os"
	"reflect"
	"time"
)

type ObjectReleaseStore struct {
	config Config
	db     *database.DB
	legacy *FileReleaseStore
}

func NewObjectReleaseStore(config Config, db *database.DB) *ObjectReleaseStore {
	return &ObjectReleaseStore{config: config, db: db, legacy: NewFileReleaseStore(config)}
}

type capturedObjects struct {
	HTML      artifacts.Ref
	Resources map[string]artifacts.Ref
}
type objectSnapshot struct {
	Version  int
	Manifest ReleaseManifest
	Files    map[string]artifacts.Ref
	Captured map[string]capturedObjects
}
type objectPointer struct {
	ID       primitive.ObjectID
	Snapshot artifacts.Ref
	Objects  []artifacts.Ref
}

func (s *ObjectReleaseStore) open() (*os.Root, *artifacts.Store, *artifacts.Catalog, error) {
	root, err := s.legacy.root()
	if err != nil {
		return nil, nil, nil, err
	}
	if err := root.MkdirAll("objects-v2", 0700); err != nil {
		root.Close()
		return nil, nil, nil, err
	}
	sub, err := root.OpenRoot("objects-v2")
	if err != nil {
		root.Close()
		return nil, nil, nil, err
	}
	store := artifacts.NewRootStore(sub)
	return root, store, artifacts.NewCatalog(s.db, store, s.config.RetentionDays, s.config.FailedRetentionDays), nil
}
func (s *ObjectReleaseStore) Save(ctx context.Context, r ReleaseManifest) (err error) {
	if err := validateRelease(r); err != nil {
		return err
	}
	root, objects, catalog, err := s.open()
	if err != nil {
		return err
	}
	defer root.Close()
	defer objects.Close()
	target := "object-releases/" + r.ID.Hex()
	if _, err := root.Lstat(target); err == nil {
		old, err := s.Load(ctx, r.ID)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(old, r) {
			return failure(CodeIdempotencyConflict, "release identity already has different bytes", nil)
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	refs := map[string]artifacts.Ref{}
	put := func(data []byte, mime string) (artifacts.Ref, error) {
		ref, err := objects.Put(ctx, bytes.NewReader(data), mime, 256<<20)
		if err != nil {
			return ref, err
		}
		if err := catalog.Register(ctx, ref, false); err != nil {
			return ref, err
		}
		refs[ref.Hash] = ref
		return ref, nil
	}
	stripped := r
	stripped.Files = map[string]ReleaseFile{}
	stripped.Pages = map[string]ReleasePage{}
	snapshot := objectSnapshot{Version: 2, Files: map[string]artifacts.Ref{}, Captured: map[string]capturedObjects{}}
	for name, file := range r.Files {
		ref, err := put(file.Data, file.MIME)
		if err != nil {
			return err
		}
		snapshot.Files[name] = ref
		file.Data = nil
		stripped.Files[name] = file
	}
	for name, page := range r.Pages {
		document := page.Captured.Document
		refsForPage := capturedObjects{Resources: map[string]artifacts.Ref{}}
		refsForPage.HTML, err = put(document.HTML, "text/html; charset=utf-8")
		if err != nil {
			return err
		}
		document.HTML = nil
		if document.Resources != nil {
			resources := map[string]ex.Resource{}
			for url, res := range document.Resources {
				mime := res.MIME
				if mime == "" {
					mime = "application/octet-stream"
				}
				ref, err := put(res.Data, mime)
				if err != nil {
					return err
				}
				refsForPage.Resources[url] = ref
				res.Data = nil
				resources[url] = res
			}
			document.Resources = resources
		}
		page.Captured.Document = document
		stripped.Pages[name] = page
		snapshot.Captured[name] = refsForPage
	}
	snapshot.Manifest = stripped
	var encoded bytes.Buffer
	if err := gob.NewEncoder(&snapshotWriter{writer: &encoded, remaining: 32 << 20}).Encode(snapshot); err != nil {
		return err
	}
	meta, err := put(encoded.Bytes(), "application/vnd.lightcms.release-v2")
	if err != nil {
		return err
	}
	pointer := objectPointer{ID: r.ID, Snapshot: meta}
	for _, ref := range refs {
		pointer.Objects = append(pointer.Objects, ref)
	}
	days := s.config.RetentionDays
	if days < 1 {
		days = 90
	}
	expires := time.Now().Add(time.Duration(days) * 24 * time.Hour)
	// References commit before the immutable pointer becomes visible. An
	// interrupted install is retained conservatively, never treated as live.
	if err := s.db.WithTransaction(ctx, func(sc mongo.SessionContext) error {
		for _, ref := range refs {
			if err := catalog.Reference(sc, "release:"+r.ID.Hex(), ref, &expires); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	staging := target + ".building-" + primitive.NewObjectID().Hex()
	if err := root.MkdirAll(staging, 0700); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = root.RemoveAll(staging)
		}
	}()
	data, err := json.Marshal(pointer)
	if err != nil {
		return err
	}
	summary, err := json.Marshal(r)
	if err != nil {
		return err
	}
	marker, _ := json.Marshal(map[string]string{"pointer": releaseHash(data), "summary": releaseHash(summary)})
	for name, body := range map[string][]byte{"objects.json": data, "summary.json": summary, "complete.json": marker} {
		if err := releaseWrite(root, staging+"/"+name, body); err != nil {
			return err
		}
	}
	if err := syncReleaseDir(root, staging); err != nil {
		return err
	}
	if err := root.Rename(staging, target); err != nil {
		return err
	}
	return syncReleaseDir(root, "object-releases")
}
func (s *ObjectReleaseStore) readPointer(root *os.Root, id primitive.ObjectID) (objectPointer, []byte, error) {
	var pointer objectPointer
	if id.IsZero() {
		return pointer, nil, failure(CodeInvalidRequest, "release ID required", nil)
	}
	base := "object-releases/" + id.Hex()
	data, err := releaseRead(root, base+"/objects.json", 4<<20)
	if err != nil {
		return pointer, nil, failure(CodeArtifactUnavailable, "object release pointer unavailable", err)
	}
	marker, err := releaseRead(root, base+"/complete.json", 4096)
	if err != nil {
		return pointer, nil, failure(CodeArtifactUnavailable, "object release incomplete", err)
	}
	var hashes map[string]string
	if json.Unmarshal(marker, &hashes) != nil || releaseHash(data) != hashes["pointer"] {
		return pointer, nil, failure(CodeArtifactUnavailable, "object release pointer corrupted", nil)
	}
	summary, err := releaseRead(root, base+"/summary.json", 16<<20)
	if err != nil || releaseHash(summary) != hashes["summary"] {
		return pointer, nil, failure(CodeArtifactUnavailable, "object release summary corrupted", err)
	}
	if json.Unmarshal(data, &pointer) != nil || pointer.ID != id {
		return pointer, nil, failure(CodeArtifactUnavailable, "object release identity corrupted", nil)
	}
	return pointer, summary, nil
}
func (s *ObjectReleaseStore) Load(ctx context.Context, id primitive.ObjectID) (ReleaseManifest, error) {
	root, objects, _, err := s.open()
	if err != nil {
		return ReleaseManifest{}, err
	}
	defer root.Close()
	defer objects.Close()
	if _, err := root.Lstat("object-releases/" + id.Hex()); os.IsNotExist(err) {
		return s.legacy.Load(ctx, id)
	} else if err != nil {
		return ReleaseManifest{}, err
	}
	pointer, summary, err := s.readPointer(root, id)
	if err != nil {
		return ReleaseManifest{}, err
	}
	reader, err := objects.Open(ctx, pointer.Snapshot)
	if err != nil {
		return ReleaseManifest{}, err
	}
	defer reader.Close()
	var snapshot objectSnapshot
	if pointer.Snapshot.Size > 32<<20 {
		return ReleaseManifest{}, artifacts.ErrLimit
	}
	if err := gob.NewDecoder(io.LimitReader(reader, 32<<20)).Decode(&snapshot); err != nil {
		return ReleaseManifest{}, err
	}
	if snapshot.Version != 2 || snapshot.Manifest.ID != id {
		return ReleaseManifest{}, fmt.Errorf("unsupported object release snapshot")
	}
	cache := map[string][]byte{}
	var used int64
	read := func(ref artifacts.Ref) ([]byte, error) {
		if data, ok := cache[ref.Hash]; ok {
			return data, nil
		}
		used += ref.Size
		if ref.Size < 0 || used > 512<<20 {
			return nil, artifacts.ErrLimit
		}
		reader, err := objects.Open(ctx, ref)
		if err != nil {
			return nil, err
		}
		defer reader.Close()
		data, err := io.ReadAll(io.LimitReader(reader, ref.Size+1))
		if err != nil {
			return nil, err
		}
		if int64(len(data)) != ref.Size {
			return nil, artifacts.ErrCorrupt
		}
		cache[ref.Hash] = data
		return data, nil
	}
	r := snapshot.Manifest
	for name, file := range r.Files {
		ref, ok := snapshot.Files[name]
		if !ok || ref.Hash != file.SHA256 || ref.Size != file.Size {
			return r, artifacts.ErrCorrupt
		}
		file.Data, err = read(ref)
		if err != nil {
			return r, err
		}
		r.Files[name] = file
	}
	for name, page := range r.Pages {
		refs, ok := snapshot.Captured[name]
		if !ok {
			return r, artifacts.ErrCorrupt
		}
		page.Captured.Document.HTML, err = read(refs.HTML)
		if err != nil {
			return r, err
		}
		for url, res := range page.Captured.Document.Resources {
			ref, ok := refs.Resources[url]
			if !ok {
				return r, artifacts.ErrCorrupt
			}
			res.Data, err = read(ref)
			if err != nil {
				return r, err
			}
			page.Captured.Document.Resources[url] = res
		}
		r.Pages[name] = page
	}
	if err := validateRelease(r); err != nil {
		return r, err
	}
	expected, _ := json.Marshal(r)
	if !bytes.Equal(expected, summary) {
		return r, artifacts.ErrCorrupt
	}
	return r, nil
}
func (s *ObjectReleaseStore) Summary(ctx context.Context, id primitive.ObjectID) (ReleaseManifest, error) {
	root, objects, _, err := s.open()
	if err != nil {
		return ReleaseManifest{}, err
	}
	defer root.Close()
	defer objects.Close()
	if _, err := root.Lstat("object-releases/" + id.Hex()); os.IsNotExist(err) {
		return s.legacy.Summary(ctx, id)
	} else if err != nil {
		return ReleaseManifest{}, err
	}
	_, summary, err := s.readPointer(root, id)
	if err != nil {
		return ReleaseManifest{}, err
	}
	var r ReleaseManifest
	err = json.Unmarshal(summary, &r)
	return r, err
}
func (s *ObjectReleaseStore) ReferenceRelease(ctx context.Context, owner string, id primitive.ObjectID, expires *time.Time) error {
	root, objects, catalog, err := s.open()
	if err != nil {
		return err
	}
	defer root.Close()
	defer objects.Close()
	if _, err := root.Lstat("object-releases/" + id.Hex()); os.IsNotExist(err) {
		_, err := s.legacy.Summary(ctx, id)
		return err
	} else if err != nil {
		return err
	}
	pointer, _, err := s.readPointer(root, id)
	if err != nil {
		return err
	}
	return s.db.WithTransaction(ctx, func(sc mongo.SessionContext) error {
		for _, ref := range pointer.Objects {
			if err := catalog.Reference(sc, owner, ref, expires); err != nil {
				return err
			}
		}
		return nil
	})
}
func (s *ObjectReleaseStore) ReleaseOwner(ctx context.Context, owner string) error {
	root, objects, catalog, err := s.open()
	if err != nil {
		return err
	}
	defer root.Close()
	defer objects.Close()
	return catalog.ReleaseOwner(ctx, owner)
}

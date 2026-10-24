package staticexport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/metavert-io/lightcms/v7/internal/product/artifacts"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

type SourceObjects interface {
	SaveExportObject(context.Context, primitive.ObjectID, []byte, string) (artifacts.Ref, error)
	OpenExportObject(context.Context, artifacts.Ref) (io.ReadCloser, error)
}
type Options struct {
	ProjectRoot, Root string
	Objects           SourceObjects
}
type Service struct {
	source   Source
	resolver Resolver
	options  Options
}

func NewService(source Source, resolver Resolver, options Options) *Service {
	return &Service{source: source, resolver: resolver, options: options}
}

func (s *Service) privateRoot() (*os.Root, error) {
	return OpenPrivateRoot(s.options)
}

// OpenPrivateRoot is shared by the export and release stores. It never opens a
// public directory or traverses an existing symlink in the configured root.
func OpenPrivateRoot(options Options) (*os.Root, error) {
	project, err := filepath.Abs(options.ProjectRoot)
	if err != nil {
		return nil, err
	}
	root, err := filepath.Abs(options.Root)
	if err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(project, root)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return nil, fmt.Errorf("export root must be inside project")
	}
	first := strings.ToLower(strings.Split(filepath.ToSlash(rel), "/")[0])
	if first == "static" || first == "content" || first == ".git" {
		return nil, fmt.Errorf("export root must be private")
	}
	parent, err := os.OpenRoot(project)
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	current := ""
	for _, segment := range strings.Split(rel, string(filepath.Separator)) {
		current = filepath.Join(current, segment)
		info, e := parent.Lstat(current)
		if e != nil && !os.IsNotExist(e) {
			return nil, e
		}
		if e == nil && info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("export root must not traverse symlinks")
		}
	}
	if err = parent.MkdirAll(rel, 0700); err != nil {
		return nil, err
	}
	return parent.OpenRoot(rel)
}

func (s *Service) Build(ctx context.Context, frozen FrozenExport) (artifact Artifact, err error) {
	if frozen.ID.IsZero() || len(frozen.Pages) == 0 || len(frozen.Pages) > 100 || s.source == nil {
		return artifact, exportError("STATIC_INVALID_REQUEST", "invalid frozen export", nil)
	}
	root, err := s.privateRoot()
	if err != nil {
		return artifact, err
	}
	defer root.Close()
	target := "exports/" + frozen.ID.Hex()
	if existing, e := s.load(root, frozen.ID); e == nil {
		if sameSources(existing.Manifest, frozen) {
			return existing, nil
		}
		return artifact, exportError("STATIC_IDEMPOTENCY_CONFLICT", "export identity already belongs to different content", nil)
	} else if !os.IsNotExist(e) {
		return artifact, e
	}
	staging := "exports/" + frozen.ID.Hex() + ".building-" + primitive.NewObjectID().Hex()
	if err = root.MkdirAll(staging+"/public", 0700); err != nil {
		return artifact, err
	}
	if err = root.MkdirAll(staging+"/source", 0700); err != nil {
		return artifact, err
	}
	defer func() {
		if err != nil {
			_ = root.RemoveAll(staging)
		}
	}()
	manifest := Manifest{SchemaVersion: 1, ExportID: frozen.ID, ExporterVersion: "lightcms-static-export-v1", CapturedAt: frozen.CapturedAt, Portability: "complete"}
	files := map[string]ManifestFile{}
	seenPages := map[string]bool{}
	external := map[string]bool{}
	var captured []Document
	objectIndex := []capturePage{}
	var capturedBytes int64
	write := func(name, kind, sourceURL string, data []byte) error {
		if !IsPublicArchivePath(name) {
			return exportError("STATIC_INVALID_REQUEST", "unsafe export file path", nil)
		}
		sum := bytesHash(data)
		if existing, ok := files[name]; ok {
			if existing.SHA256 != sum {
				return exportError("STATIC_PATH_CONFLICT", "two export resources occupy the same path", nil)
			}
			return nil
		}
		if e := writeRootFile(root, staging+"/"+name, data); e != nil {
			return e
		}
		files[name] = ManifestFile{Path: name, SHA256: sum, Size: int64(len(data)), MIME: kind, SourceURL: sourceURL}
		return nil
	}
	for index, p := range frozen.Pages {
		if err = ctx.Err(); err != nil {
			return artifact, err
		}
		pageFile, e := pageFilePath(p.FullPath)
		if e != nil {
			return artifact, e
		}
		if seenPages[p.FullPath] {
			return artifact, exportError("STATIC_PATH_CONFLICT", "duplicate content page path", nil)
		}
		seenPages[p.FullPath] = true
		reader, e := s.source.Open(ctx, p)
		if e != nil {
			return artifact, e
		}
		body, e := io.ReadAll(io.LimitReader(reader, MaxResourceBytes+1))
		reader.Close()
		if e != nil {
			return artifact, e
		}
		if len(body) > MaxResourceBytes {
			return artifact, exportError("STATIC_ASSET_LIMIT_EXCEEDED", "HTML exceeds static asset size limit", nil)
		}
		if bytesHash(body) != strings.ToLower(p.SourceSHA256) {
			return artifact, exportError("STATIC_EXPORT_FAILED", "source HTML hash does not match frozen publication", nil)
		}
		doc, e := collectBudget(ctx, p, body, s.resolver, MaxCaptureBytes-capturedBytes)
		if e != nil {
			return artifact, e
		}
		captured = append(captured, doc)
		capturedBytes += DocumentBytes(doc)
		if s.options.Objects != nil {
			entry, e := s.captureObjects(ctx, frozen.ID, doc)
			if e != nil {
				return artifact, e
			}
			objectIndex = append(objectIndex, entry)
		} else {
			if e = writeRootFile(root, fmt.Sprintf("%s/source/%d.html", staging, index), body); e != nil {
				return artifact, e
			}
			for u := range doc.Resources {
				resource := doc.Resources[u]
				if e = writeRootFile(root, staging+"/source/"+resource.SHA256+".bin", resource.Data); e != nil {
					return artifact, e
				}
			}
		}
		resourceLocal := func(base, ref string) (string, error) {
			if ref == "" || strings.HasPrefix(ref, "#") || strings.HasPrefix(strings.ToLower(ref), "data:") {
				return ref, nil
			}
			u, e := ResolveReference(base, ref)
			if e != nil {
				return "", e
			}
			parsed, e := url.Parse(u)
			if e != nil {
				return "", e
			}
			fragment := parsed.Fragment
			parsed.Fragment = ""
			if _, ok := doc.Resources[parsed.String()]; !ok {
				return ref, nil
			}
			out := (&url.URL{Path: parsed.Path, Fragment: fragment}).String()
			return out, nil
		}
		var deps []string
		for u, resource := range doc.Resources {
			parsed, _ := url.Parse(u)
			if !safeResourcePath(parsed.EscapedPath()) {
				return artifact, exportError("STATIC_INVALID_REQUEST", "unsafe resource path", nil)
			}
			data := resource.Data
			if strings.EqualFold(strings.Split(resource.MIME, ";")[0], "text/css") {
				css, e := TransformCSS(string(data), func(v string) (string, error) { return resourceLocal(u, v) })
				if e != nil {
					return artifact, e
				}
				data = []byte(css)
			}
			name := "public/" + strings.TrimPrefix(parsed.Path, "/")
			if e = write(name, resource.MIME, (&url.URL{Scheme: parsed.Scheme, Host: parsed.Host, Path: parsed.Path}).String(), data); e != nil {
				return artifact, e
			}
			deps = append(deps, name)
		}
		sort.Strings(deps)
		local, e := TransformHTML(body, func(ref Reference) (string, error) {
			if ref.Kind == "resource" {
				return resourceLocal(p.PublicURL, ref.Value)
			}
			if ref.Kind == "link" && ref.Value != "" && !strings.HasPrefix(ref.Value, "#") {
				u, e := ResolveReference(p.PublicURL, ref.Value)
				if e != nil {
					return ref.Value, nil
				}
				return u, nil
			}
			return ref.Value, nil
		})
		if e != nil {
			return artifact, e
		}
		if e = write("public/"+pageFile, "text/html; charset=utf-8", "", local); e != nil {
			return artifact, e
		}
		manifest.Pages = append(manifest.Pages, ManifestPage{Source: p, File: "public/" + pageFile, ExportSHA256: bytesHash(local), Dependencies: deps})
		for _, u := range doc.External {
			external[u] = true
		}
		manifest.Diagnostics = append(manifest.Diagnostics, doc.Diagnostics...)
	}
	for _, f := range files {
		manifest.Files = append(manifest.Files, f)
	}
	sort.Slice(manifest.Files, func(i, j int) bool { return manifest.Files[i].Path < manifest.Files[j].Path })
	for u := range external {
		manifest.External = append(manifest.External, u)
	}
	sort.Strings(manifest.External)
	if len(manifest.External) > 0 {
		manifest.Portability = "incomplete"
	}
	data, e := json.MarshalIndent(manifest, "", "  ")
	if e != nil {
		return artifact, e
	}
	if e = writeRootFile(root, staging+"/manifest.json", data); e != nil {
		return artifact, e
	}
	if e = writeRootFile(root, staging+"/README.txt", []byte("Serve the public/ directory with an HTTP static file server.\nFolder pages may redirect to a trailing slash locally; Workers keeps the recorded canonical URL.\nWithout a deployment target, canonical metadata retains its source address.\nSee manifest.json portability and diagnostics before deployment.\n")); e != nil {
		return artifact, e
	}
	index := captureIndex(captured)
	if s.options.Objects != nil {
		index, e = json.Marshal(objectIndex)
		if e != nil {
			return artifact, e
		}
	}
	if e = writeRootFile(root, staging+"/capture.json", index); e != nil {
		return artifact, e
	}
	marker, _ := json.Marshal(completionMarker{ManifestHash: bytesHash(data), CaptureHash: bytesHash(index)})
	if e = writeRootFile(root, staging+"/complete.json", marker); e != nil {
		return artifact, e
	}
	if e = root.Rename(staging, target); e != nil {
		existing, recoveryErr := s.load(root, frozen.ID)
		if recoveryErr == nil && sameSources(existing.Manifest, frozen) {
			_ = root.RemoveAll(staging)
			return existing, nil
		}
		return artifact, e
	}
	return s.load(root, frozen.ID)
}

func sameSources(m Manifest, f FrozenExport) bool {
	if len(m.Pages) != len(f.Pages) || m.ExportID != f.ID {
		return false
	}
	for i, p := range m.Pages {
		if p.Source.PublicationID != f.Pages[i].PublicationID || p.Source.ContentID != f.Pages[i].ContentID || p.Source.SourceSHA256 != f.Pages[i].SourceSHA256 || p.Source.FullPath != f.Pages[i].FullPath {
			return false
		}
	}
	return true
}
func pageFilePath(full string) (string, error) {
	if !safeResourcePath(full) || strings.ContainsAny(full, "?#") {
		return "", exportError("STATIC_INVALID_REQUEST", "invalid canonical page path", nil)
	}
	if full == "/" {
		return "index.html", nil
	}
	return strings.Trim(full, "/") + "/index.html", nil
}
func bytesHash(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }
func writeRootFile(root *os.Root, name string, data []byte) error {
	if err := root.MkdirAll(path.Dir(name), 0700); err != nil {
		return err
	}
	f, err := root.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}
func readRootFile(root *os.Root, name string, limit int64) ([]byte, error) {
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("export file exceeds read limit")
	}
	return b, nil
}

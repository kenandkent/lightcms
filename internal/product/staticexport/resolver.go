package staticexport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
)

const MaxResourceBytes = 25 * 1024 * 1024

type AssetSnapshot struct {
	Data           []byte
	MIME, Revision string
}
type AssetLookup interface {
	LookupAsset(context.Context, string) (AssetSnapshot, error)
}
type FileResolver struct {
	project, origin string
	assets          AssetLookup
}

func NewFileResolver(projectRoot, sourceOrigin string, assets AssetLookup) (*FileResolver, error) {
	root, err := filepath.Abs(projectRoot)
	if err != nil {
		return nil, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(sourceOrigin)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("valid source origin is required")
	}
	return &FileResolver{project: root, origin: strings.ToLower(u.Scheme + "://" + u.Host), assets: assets}, nil
}
func (r *FileResolver) Resolve(ctx context.Context, ref string) (Resource, error) {
	var empty Resource
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	u, err := url.Parse(ref)
	if err != nil {
		return empty, err
	}
	if u.IsAbs() || u.Host != "" {
		if strings.ToLower(u.Scheme+"://"+u.Host) != r.origin {
			return empty, ErrExternalResource
		}
	}
	if u.User != nil || u.Opaque != "" || !safeResourcePath(u.EscapedPath()) {
		return empty, fmt.Errorf("invalid local resource path")
	}
	name := u.Path
	var candidates []string
	var legacy AssetSnapshot
	registered := false
	switch {
	case strings.HasPrefix(name, "/static/"):
		candidates = []string{strings.TrimPrefix(name, "/")}
	case strings.HasPrefix(name, "/uploads/"):
		candidates = []string{"static" + name}
	default:
		lookupNames := []string{name}
		if strings.HasPrefix(name, "/assets/") {
			lookupNames = []string{strings.TrimPrefix(name, "/assets"), name}
		}
		if r.assets != nil {
			for _, p := range lookupNames {
				a, e := r.assets.LookupAsset(ctx, p)
				if e != nil {
					return empty, e
				}
				if len(a.Data) > 0 || a.Revision != "" {
					legacy = a
					registered = true
					break
				}
			}
		}
		if !strings.HasPrefix(name, "/assets/") && !registered {
			return empty, fmt.Errorf("local resource is not registered in asset library")
		}
		if strings.EqualFold(path.Ext(name), ".html") && !registered {
			return empty, fmt.Errorf("unregistered HTML cannot be read as an asset")
		}
		for _, p := range lookupNames {
			candidates = append(candidates, "content/generated/"+strings.TrimPrefix(p, "/"))
		}
	}
	for _, candidate := range candidates {
		data, e := r.read(ctx, candidate)
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return empty, e
		}
		kind := mime.TypeByExtension(path.Ext(name))
		if kind == "" {
			kind = http.DetectContentType(data)
		}
		return resourceSnapshot(ref, kind, data, legacy.Revision), nil
	}
	if len(legacy.Data) > 0 {
		if len(legacy.Data) > MaxResourceBytes {
			return empty, fmt.Errorf("asset exceeds export limit")
		}
		return resourceSnapshot(ref, legacy.MIME, legacy.Data, legacy.Revision), nil
	}
	return empty, os.ErrNotExist
}
func resourceSnapshot(ref, kind string, data []byte, revision string) Resource {
	sum := sha256.Sum256(data)
	return Resource{URL: ref, MIME: kind, SHA256: hex.EncodeToString(sum[:]), SourceRevision: revision, Data: data}
}
func safeResourcePath(escaped string) bool {
	if escaped == "" || !strings.HasPrefix(escaped, "/") || strings.ContainsAny(escaped, "\\\x00") {
		return false
	}
	lower := strings.ToLower(escaped)
	if strings.Contains(lower, "%2f") || strings.Contains(lower, "%5c") || strings.Contains(lower, "%00") {
		return false
	}
	decoded, err := url.PathUnescape(escaped)
	if err != nil || strings.ContainsAny(decoded, "\\\x00") {
		return false
	}
	for _, part := range strings.Split(decoded, "/") {
		if part == "." || part == ".." || strings.Contains(part, ".previous-") || strings.Contains(part, ".next-") || strings.Contains(part, ".unpublish-backup-") {
			return false
		}
	}
	return true
}
func (r *FileResolver) read(ctx context.Context, name string) ([]byte, error) {
	// os.Root rejects symlinks escaping its root and removes the usual
	// check-then-open symlink race. All source roots stay read-only.
	root, err := os.OpenRoot(r.project)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	// A public URL may not follow a symlink into another (private) project
	// directory. Narrow the open-root boundary to the actual served tree.
	base, relative := "", ""
	if strings.HasPrefix(name, "static/") {
		base, relative = "static", strings.TrimPrefix(name, "static/")
	} else if strings.HasPrefix(name, "content/generated/") {
		base, relative = "content/generated", strings.TrimPrefix(name, "content/generated/")
	} else {
		return nil, fmt.Errorf("resource is outside known served roots")
	}
	served, err := root.OpenRoot(base)
	if err != nil {
		return nil, err
	}
	defer served.Close()
	for attempt := 0; attempt < 3; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		f, err := served.Open(relative)
		if err != nil {
			return nil, err
		}
		before, err := f.Stat()
		if err != nil {
			f.Close()
			return nil, err
		}
		if !before.Mode().IsRegular() || before.Size() > MaxResourceBytes {
			f.Close()
			return nil, fmt.Errorf("unsupported asset file or size")
		}
		data, readErr := io.ReadAll(io.LimitReader(f, MaxResourceBytes+1))
		after, statErr := f.Stat()
		f.Close()
		if readErr != nil {
			return nil, readErr
		}
		if statErr != nil {
			return nil, statErr
		}
		if len(data) > MaxResourceBytes {
			return nil, fmt.Errorf("asset exceeds export limit")
		}
		if before.ModTime() == after.ModTime() && before.Size() == after.Size() && int64(len(data)) == after.Size() {
			return data, nil
		}
	}
	return nil, fmt.Errorf("asset changed while exporting")
}

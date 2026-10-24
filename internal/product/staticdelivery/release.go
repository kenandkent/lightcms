package staticdelivery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"

	ex "github.com/metavert-io/lightcms/v7/internal/product/staticexport"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

type CapturedPage struct {
	ExportID primitive.ObjectID `json:"export_id"`
	Document ex.Document        `json:"-"`
}
type ReleasePage struct {
	ExportID           primitive.ObjectID `json:"export_id"`
	ContentID          primitive.ObjectID `json:"content_id"`
	PublicationID      primitive.ObjectID `json:"publication_id"`
	Path, File, SHA256 string
	Assets             []string
	Captured           CapturedPage `json:"-"`
}
type ReleaseFile struct {
	Path, MIME, SHA256 string
	Size               int64
	RetainedUntil      *time.Time `json:"retained_until,omitempty"`
	Data               []byte     `json:"-"`
}
type ReleaseManifest struct {
	RemovedPaths                 []string           `json:"removed_paths,omitempty"`
	DeliveryMode                 string             `json:"delivery_mode,omitempty"`
	SchemaVersion                int                `json:"schema_version"`
	ID                           primitive.ObjectID `json:"id"`
	TargetID                     primitive.ObjectID `json:"target_id"`
	Origin                       string             `json:"public_origin"`
	InstallationID               string             `json:"installation_id"`
	Environment                  string             `json:"environment"`
	ConfigRevision, BaseRevision int64
	CreatedAt                    time.Time
	Pages                        map[string]ReleasePage
	Files                        map[string]ReleaseFile
	Diagnostics                  []ex.Diagnostic
}
type BuildInput struct {
	ID            primitive.ObjectID
	Target        Target
	Current       *ReleaseManifest
	Pages         []CapturedPage
	RemovePaths   []string
	Now           time.Time
	RetentionDays int
	Defaults      func(context.Context, string) (share, icon ex.Resource, err error)
}

func releaseHash(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func validPagePath(raw string) bool {
	u, e := url.ParseRequestURI(raw)
	if e != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != raw || !strings.HasPrefix(raw, "/") || path.Clean(raw) != raw || strings.ContainsAny(raw, "\\%\x00?#") {
		return false
	}
	for _, prefix := range []string{"/cm", "/api", "/mcp", "/_lc-assets", "/.__lc_delivery"} {
		if raw == prefix || strings.HasPrefix(raw, prefix+"/") {
			return false
		}
	}
	return true
}
func BuildRelease(ctx context.Context, in BuildInput) (ReleaseManifest, error) {
	r := ReleaseManifest{SchemaVersion: 1, ID: in.ID, TargetID: in.Target.ID, InstallationID: in.Target.InstallationID, Environment: in.Target.Environment, ConfigRevision: in.Target.ConfigRevision, BaseRevision: in.Target.CurrentRevision, CreatedAt: in.Now, Pages: map[string]ReleasePage{}, Files: map[string]ReleaseFile{}}
	r.DeliveryMode = AssetsOnlyV1
	if in.ID.IsZero() || in.Target.ID.IsZero() {
		return r, failure(CodeInvalidRequest, "release and target identities required", nil)
	}
	origin, e := NormalizeOrigin(in.Target.Origin)
	if e != nil {
		return r, e
	}
	r.Origin = origin
	if in.Now.IsZero() {
		r.CreatedAt = time.Now().UTC()
	}
	if in.RetentionDays <= 0 {
		in.RetentionDays = 7
	}
	selected := map[string]CapturedPage{}
	changed := map[string]bool{}
	if in.Current != nil {
		if in.Current.TargetID != in.Target.ID {
			return r, failure(CodeRevisionConflict, "cannot merge a different target's release", nil)
		}
		for path, p := range in.Current.Pages {
			if !validPagePath(path) || p.Path != path {
				return r, failure(CodeArtifactUnavailable, "current release contains an invalid page mapping", nil)
			}
			for _, name := range append(append([]string{}, p.Assets...), p.File) {
				f, ok := in.Current.Files[name]
				if !ok || f.Path != name || int64(len(f.Data)) != f.Size || releaseHash(f.Data) != f.SHA256 {
					return r, failure(CodeArtifactUnavailable, "current release file is missing or corrupted", nil)
				}
			}
			selected[path] = p.Captured
		}
	}
	for _, p := range in.Pages {
		full := p.Document.Source.FullPath
		if p.ExportID.IsZero() || p.Document.Source.ContentID.IsZero() || !validPagePath(full) {
			return r, failure(CodeInvalidRequest, "invalid captured report identity or path", nil)
		}
		if old, exists := selected[full]; exists && old.Document.Source.ContentID != p.Document.Source.ContentID {
			return r, failure(CodePathConflict, "another content item owns this report path", nil)
		}
		selected[full] = p
		changed[full] = true
	}
	for _, full := range in.RemovePaths {
		if !validPagePath(full) {
			return r, failure(CodeInvalidRequest, "invalid removed page path", nil)
		}
		delete(selected, full)
	}
	paths := make([]string, 0, len(selected))
	for full := range selected {
		paths = append(paths, full)
	}
	sort.Strings(paths)
	for _, full := range paths {
		if full == "/_headers" || full == "/_redirects" {
			return r, failure(CodePathConflict, "report path is reserved for native asset configuration", nil)
		}
		if e = ctx.Err(); e != nil {
			return r, e
		}
		captured := selected[full]
		if in.Current != nil && !changed[full] && in.Current.Origin == r.Origin && in.Current.Environment == r.Environment {
			old := in.Current.Pages[full]
			r.Pages[full] = old
			for _, name := range append(append([]string{}, old.Assets...), old.File) {
				f := in.Current.Files[name]
				f.Data = append([]byte(nil), f.Data...)
				f.RetainedUntil = nil
				r.Files[name] = f
			}
			continue
		}
		page, files, diagnostics, err := rewritePage(ctx, captured, origin, selected, in.Target.Environment, in.Defaults)
		if err != nil {
			return r, err
		}
		r.Pages[full] = page
		r.Diagnostics = append(r.Diagnostics, diagnostics...)
		for name, f := range files {
			if old, exists := r.Files[name]; exists && (old.SHA256 != f.SHA256 || old.MIME != f.MIME) {
				return r, failure(CodePathConflict, "release files conflict", nil)
			}
			r.Files[name] = f
		}
	}
	if in.Current != nil {
		for name, old := range in.Current.Files {
			if _, exists := r.Files[name]; exists || !strings.HasPrefix(name, "/_lc-assets/") {
				continue
			}
			if old.RetainedUntil == nil {
				until := r.CreatedAt.Add(time.Duration(in.RetentionDays) * 24 * time.Hour)
				old.RetainedUntil = &until
			}
			if old.RetainedUntil.After(r.CreatedAt) {
				r.Files[name] = old
			}
		}
	}
	limit := in.Target.AssetLimit
	if limit <= 0 {
		limit = 20000
	}
	// The derived health JSON is uploaded alongside the frozen public files.
	if len(r.Files)+1 > limit {
		return r, failure(CodeAssetLimitExceeded, "release exceeds target file count limit (including retained assets)", nil)
	}
	for _, f := range r.Files {
		if len(f.Data) > 25<<20 || releaseHash(f.Data) != f.SHA256 {
			return r, failure(CodeAssetLimitExceeded, "invalid or oversized release file", nil)
		}
	}
	return r, nil
}

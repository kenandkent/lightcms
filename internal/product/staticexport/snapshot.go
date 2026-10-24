package staticexport

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/metavert-io/lightcms/v7/internal/product/artifacts"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"io"
	"os"
	"sort"
)

// This index is private and is never included in ZIP downloads. It preserves
// original URLs (including query-versioned resources) for future target rewrites.
type captureResource struct {
	URL, MIME, SHA256, Revision string
	Object                      *artifacts.Ref `json:",omitempty"`
}
type capturePage struct {
	Resources []captureResource
	HTML      *artifacts.Ref `json:",omitempty"`
}
type completionMarker struct {
	ManifestHash string `json:"manifest_sha256"`
	CaptureHash  string `json:"capture_sha256"`
}

func captureIndex(docs []Document) []byte {
	index := make([]capturePage, len(docs))
	for i, d := range docs {
		for u, r := range d.Resources {
			index[i].Resources = append(index[i].Resources, captureResource{URL: u, MIME: r.MIME, SHA256: r.SHA256, Revision: r.SourceRevision})
		}
		sort.Slice(index[i].Resources, func(a, b int) bool { return index[i].Resources[a].URL < index[i].Resources[b].URL })
	}
	b, _ := json.Marshal(index)
	return b
}

func (s *Service) Documents(ctx context.Context, id primitive.ObjectID) ([]Document, error) {
	artifact, e := s.Load(ctx, id)
	if e != nil {
		return nil, e
	}
	root, e := s.privateRoot()
	if e != nil {
		return nil, e
	}
	defer root.Close()
	rel := artifact.RelativePath
	markerBytes, e := readRootFile(root, rel+"/complete.json", 4096)
	if e != nil {
		return nil, e
	}
	var marker completionMarker
	if json.Unmarshal(markerBytes, &marker) != nil {
		return nil, exportError("STATIC_ARTIFACT_UNAVAILABLE", "invalid completion marker", nil)
	}
	raw, e := readRootFile(root, rel+"/capture.json", 16<<20)
	if e != nil {
		return nil, e
	}
	if marker.CaptureHash == "" || bytesHash(raw) != marker.CaptureHash {
		return nil, exportError("STATIC_ARTIFACT_UNAVAILABLE", "private capture index failed verification", nil)
	}
	var index []capturePage
	if json.Unmarshal(raw, &index) != nil || len(index) != len(artifact.Manifest.Pages) {
		return nil, exportError("STATIC_ARTIFACT_UNAVAILABLE", "invalid private capture index", nil)
	}
	docs := make([]Document, len(index))
	for i, p := range artifact.Manifest.Pages {
		if e = ctx.Err(); e != nil {
			return nil, e
		}
		body, e := s.readCaptured(ctx, root, fmt.Sprintf("%s/source/%d.html", rel, i), index[i].HTML)
		if e != nil {
			return nil, e
		}
		if bytesHash(body) != p.Source.SourceSHA256 {
			return nil, exportError("STATIC_ARTIFACT_UNAVAILABLE", "source HTML failed verification", nil)
		}
		snapshot := snapshotResolver{resources: map[string]Resource{}, external: map[string]bool{}}
		for _, u := range artifact.Manifest.External {
			snapshot.external[u] = true
		}
		for _, r := range index[i].Resources {
			if len(r.SHA256) != 64 {
				return nil, exportError("STATIC_ARTIFACT_UNAVAILABLE", "invalid source resource hash", nil)
			}
			data, e := s.readCaptured(ctx, root, rel+"/source/"+r.SHA256+".bin", r.Object)
			if e != nil {
				return nil, e
			}
			if bytesHash(data) != r.SHA256 {
				return nil, exportError("STATIC_ARTIFACT_UNAVAILABLE", "source resource failed verification", nil)
			}
			snapshot.resources[r.URL] = Resource{URL: r.URL, MIME: r.MIME, SHA256: r.SHA256, SourceRevision: r.Revision, Data: data}
		}
		docs[i], e = Collect(ctx, p.Source, body, snapshot)
		if e != nil {
			return nil, e
		}
	}
	return docs, nil
}

func (s *Service) captureObjects(ctx context.Context, id primitive.ObjectID, doc Document) (capturePage, error) {
	var entry capturePage
	ref, err := s.options.Objects.SaveExportObject(ctx, id, doc.HTML, "text/html; charset=utf-8")
	if err != nil {
		return entry, err
	}
	entry.HTML = &ref
	for url, res := range doc.Resources {
		ref, err := s.options.Objects.SaveExportObject(ctx, id, res.Data, res.MIME)
		if err != nil {
			return entry, err
		}
		entry.Resources = append(entry.Resources, captureResource{URL: url, MIME: res.MIME, SHA256: res.SHA256, Revision: res.SourceRevision, Object: &ref})
	}
	sort.Slice(entry.Resources, func(a, b int) bool { return entry.Resources[a].URL < entry.Resources[b].URL })
	return entry, nil
}
func (s *Service) readCaptured(ctx context.Context, root *os.Root, path string, ref *artifacts.Ref) ([]byte, error) {
	if ref == nil {
		return readRootFile(root, path, MaxResourceBytes)
	}
	if s.options.Objects == nil {
		return nil, exportError("STATIC_ARTIFACT_UNAVAILABLE", "object source reader unavailable", nil)
	}
	if ref.Size < 0 || ref.Size > MaxResourceBytes {
		return nil, exportError("STATIC_ARTIFACT_UNAVAILABLE", "source object exceeds limit", nil)
	}
	reader, err := s.options.Objects.OpenExportObject(ctx, *ref)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	data, err := io.ReadAll(io.LimitReader(reader, ref.Size+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != ref.Size || bytesHash(data) != ref.Hash {
		return nil, exportError("STATIC_ARTIFACT_UNAVAILABLE", "source object failed verification", nil)
	}
	return data, nil
}

type snapshotResolver struct {
	resources map[string]Resource
	external  map[string]bool
}

func (r snapshotResolver) Resolve(_ context.Context, u string) (Resource, error) {
	if v, ok := r.resources[u]; ok {
		return v, nil
	}
	if r.external[u] {
		return Resource{}, ErrExternalResource
	}
	return Resource{}, os.ErrNotExist
}

package staticexport

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

func IsPublicArchivePath(name string) bool {
	if name == "manifest.json" || name == "README.txt" {
		return true
	}
	if !strings.HasPrefix(name, "public/") || strings.ContainsAny(name, "\\\x00") || path.Clean(name) != name {
		return false
	}
	for _, p := range strings.Split(name, "/") {
		if p == "" || p == "." || p == ".." {
			return false
		}
	}
	return true
}
func (s *Service) Load(ctx context.Context, id primitive.ObjectID) (Artifact, error) {
	if err := ctx.Err(); err != nil {
		return Artifact{}, err
	}
	if id.IsZero() {
		return Artifact{}, exportError("STATIC_INVALID_REQUEST", "export id is required", nil)
	}
	root, err := s.privateRoot()
	if err != nil {
		return Artifact{}, err
	}
	defer root.Close()
	return s.load(root, id)
}
func (s *Service) load(root *os.Root, id primitive.ObjectID) (Artifact, error) {
	var artifact Artifact
	rel := "exports/" + id.Hex()
	markerData, err := readRootFile(root, rel+"/complete.json", 4096)
	if err != nil {
		return artifact, err
	}
	var marker struct {
		ManifestHash string `json:"manifest_sha256"`
	}
	if err = json.Unmarshal(markerData, &marker); err != nil {
		return artifact, err
	}
	data, err := readRootFile(root, rel+"/manifest.json", 16*1024*1024)
	if err != nil {
		return artifact, err
	}
	if bytesHash(data) != marker.ManifestHash {
		return artifact, exportError("STATIC_ARTIFACT_UNAVAILABLE", "export manifest failed verification", nil)
	}
	var manifest Manifest
	if err = json.Unmarshal(data, &manifest); err != nil {
		return artifact, err
	}
	if manifest.ExportID != id || manifest.SchemaVersion != 1 {
		return artifact, exportError("STATIC_ARTIFACT_UNAVAILABLE", "export manifest identity is invalid", nil)
	}
	seen := map[string]bool{}
	for _, file := range manifest.Files {
		if !strings.HasPrefix(file.Path, "public/") || !IsPublicArchivePath(file.Path) || seen[file.Path] {
			return artifact, exportError("STATIC_ARTIFACT_UNAVAILABLE", "invalid export file list", nil)
		}
		seen[file.Path] = true
		b, e := readRootFile(root, rel+"/"+file.Path, MaxResourceBytes)
		if e != nil {
			return artifact, e
		}
		if int64(len(b)) != file.Size || bytesHash(b) != file.SHA256 {
			return artifact, exportError("STATIC_ARTIFACT_UNAVAILABLE", "export resource failed verification", nil)
		}
	}
	for _, page := range manifest.Pages {
		if !seen[page.File] {
			return artifact, exportError("STATIC_ARTIFACT_UNAVAILABLE", "export page is missing", nil)
		}
	}
	return Artifact{ID: id, Manifest: manifest, RelativePath: rel}, nil
}
func (s *Service) WriteZIP(ctx context.Context, id primitive.ObjectID, w io.Writer) error {
	artifact, err := s.Load(ctx, id)
	if err != nil {
		return err
	}
	root, err := s.privateRoot()
	if err != nil {
		return err
	}
	defer root.Close()
	zw := zip.NewWriter(w)
	paths := []string{"manifest.json", "README.txt"}
	for _, f := range artifact.Manifest.Files {
		paths = append(paths, f.Path)
	}
	for _, p := range paths {
		if err = ctx.Err(); err != nil {
			return err
		}
		if !IsPublicArchivePath(p) {
			return fmt.Errorf("invalid public export entry")
		}
		r, e := root.Open(artifact.RelativePath + "/" + p)
		if e != nil {
			return e
		}
		header := &zip.FileHeader{Name: p, Method: zip.Deflate}
		header.SetMode(0644)
		header.SetModTime(artifact.Manifest.CapturedAt)
		out, e := zw.CreateHeader(header)
		if e == nil {
			_, e = io.Copy(out, r)
		}
		r.Close()
		if e != nil {
			return e
		}
	}
	return zw.Close()
}
func (s *Service) WriteHTML(ctx context.Context, id primitive.ObjectID, w io.Writer) error {
	artifact, err := s.Load(ctx, id)
	if err != nil {
		return err
	}
	if len(artifact.Manifest.Pages) != 1 || len(artifact.Manifest.Files) != 1 || artifact.Manifest.Portability != "complete" {
		return exportError("STATIC_DEPENDENCY_UNRESOLVED", "single HTML download requires one self-contained document", nil)
	}
	root, err := s.privateRoot()
	if err != nil {
		return err
	}
	defer root.Close()
	r, err := root.Open(artifact.RelativePath + "/" + artifact.Manifest.Pages[0].File)
	if err != nil {
		return err
	}
	defer r.Close()
	_, err = io.Copy(w, r)
	return err
}

// Package staticexport captures standalone publication documents without
// mutating CMS content, publication state, canonical files, or retention pins.
package staticexport

import (
	"context"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"io"
	"time"
)

type Item struct {
	ContentID     primitive.ObjectID `json:"content_id" bson:"content_id"`
	PublicationID primitive.ObjectID `json:"publication_id,omitempty" bson:"publication_id,omitempty"`
}
type PageSource struct {
	ContentID          primitive.ObjectID `json:"content_id" bson:"content_id"`
	PublicationID      primitive.ObjectID `json:"publication_id" bson:"publication_id"`
	TemplateVersionID  primitive.ObjectID `json:"template_version_id" bson:"template_version_id"`
	ContentVersion     int64              `json:"content_version" bson:"content_version"`
	TemplateVersion    int64              `json:"template_version" bson:"template_version"`
	FullPath           string             `json:"full_path" bson:"full_path"`
	PublicURL          string             `json:"public_url" bson:"public_url"`
	TemplateSlug       string             `json:"template_slug" bson:"template_slug"`
	SourceSHA256       string             `json:"source_html_sha256" bson:"source_html_sha256"`
	LogicalPublishedAt time.Time          `json:"logical_published_at" bson:"logical_published_at"`
	StoragePath        string             `json:"-" bson:"storage_path"`
}
type Source interface {
	Resolve(context.Context, Item) (PageSource, error)
	Open(context.Context, PageSource) (io.ReadCloser, error)
}
type Resolver interface {
	Resolve(context.Context, string) (Resource, error)
}
type Resource struct {
	URL, MIME, SHA256, SourceRevision string
	Data                              []byte
}
type Diagnostic struct {
	Code    string `json:"code"`
	URL     string `json:"url,omitempty"`
	Message string `json:"message"`
}
type Document struct {
	Source      PageSource          `json:"source"`
	HTML        []byte              `json:"-"`
	Resources   map[string]Resource `json:"-"`
	External    []string            `json:"external"`
	Diagnostics []Diagnostic        `json:"diagnostics"`
}
type FrozenExport struct {
	ID          primitive.ObjectID `bson:"_id" json:"id"`
	OperationID primitive.ObjectID `bson:"operation_id" json:"operation_id"`
	Pages       []PageSource       `bson:"pages" json:"pages"`
	CapturedAt  time.Time          `bson:"captured_at" json:"captured_at"`
}
type ManifestFile struct {
	Path      string `json:"path"`
	SHA256    string `json:"bytes_sha256"`
	Size      int64  `json:"size"`
	MIME      string `json:"mime"`
	SourceURL string `json:"source_url,omitempty"`
}
type ManifestPage struct {
	Source       PageSource `json:"source"`
	File         string     `json:"file"`
	ExportSHA256 string     `json:"local_export_sha256"`
	Dependencies []string   `json:"dependencies"`
}
type Manifest struct {
	SchemaVersion   int                `json:"schema_version"`
	ExportID        primitive.ObjectID `json:"export_id"`
	ExporterVersion string             `json:"exporter_version"`
	CapturedAt      time.Time          `json:"captured_at"`
	Pages           []ManifestPage     `json:"pages"`
	Files           []ManifestFile     `json:"files"`
	External        []string           `json:"external"`
	Diagnostics     []Diagnostic       `json:"diagnostics"`
	Portability     string             `json:"portability"`
}
type Artifact struct {
	ID           primitive.ObjectID `json:"id"`
	Manifest     Manifest           `json:"manifest"`
	RelativePath string             `json:"-"`
}

package staticexport

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"strings"

	"github.com/metavert-io/lightcms/v7/internal/product/publication"
	"github.com/metavert-io/lightcms/v7/internal/product/storage"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

type Error struct {
	Code, Message string
	Cause         error
}

func (e *Error) Error() string      { return e.Code + ": " + e.Message }
func (e *Error) Unwrap() error      { return e.Cause }
func (e *Error) StaticCode() string { return e.Code }
func exportError(code, message string, cause error) error {
	return &Error{Code: code, Message: message, Cause: cause}
}

type ContentState struct{ Deleted, Fork bool }
type SourceLookup interface {
	Content(context.Context, primitive.ObjectID) (ContentState, error)
	TemplateSlug(context.Context, primitive.ObjectID) (string, error)
}
type PublicationReader interface {
	GetActive(context.Context, primitive.ObjectID) (*publication.Publication, error)
	GetByID(context.Context, primitive.ObjectID) (*publication.Publication, error)
}
type ImmutableReader interface {
	ImmutablePath(primitive.ObjectID, primitive.ObjectID) string
	Open(context.Context, storage.StoredObject) (io.ReadCloser, error)
}
type PublicationSource struct {
	lookup SourceLookup
	repo   PublicationReader
	store  ImmutableReader
}

func NewPublicationSource(lookup SourceLookup, repo PublicationReader, store ImmutableReader) *PublicationSource {
	return &PublicationSource{lookup: lookup, repo: repo, store: store}
}

func (s *PublicationSource) Resolve(ctx context.Context, item Item) (PageSource, error) {
	var result PageSource
	if item.ContentID.IsZero() {
		return result, exportError("STATIC_INVALID_REQUEST", "content_id is required", nil)
	}
	c, err := s.lookup.Content(ctx, item.ContentID)
	if err != nil {
		return result, exportError("STATIC_RESOURCE_NOT_FOUND", "content not found", err)
	}
	if c.Deleted || c.Fork {
		return result, exportError("STATIC_SOURCE_NOT_EXPORTABLE", "deleted or fork content cannot be exported", nil)
	}
	var p *publication.Publication
	if item.PublicationID.IsZero() {
		p, err = s.repo.GetActive(ctx, item.ContentID)
	} else {
		p, err = s.repo.GetByID(ctx, item.PublicationID)
	}
	if err != nil || p == nil || p.ID.IsZero() {
		return result, exportError("STATIC_RESOURCE_NOT_FOUND", "publication not found", err)
	}
	if p.ContentID != item.ContentID {
		return result, exportError("STATIC_SOURCE_NOT_EXPORTABLE", "publication does not belong to content", nil)
	}
	if (p.Status != publication.StatusActive && p.Status != publication.StatusSuperseded) || p.VerificationStatus != publication.VerificationVerified {
		return result, exportError("STATIC_SOURCE_NOT_EXPORTABLE", "publication must be a previously published verified version", nil)
	}
	if p.StorageState != publication.StoragePresent {
		return result, exportError("STATIC_ARTIFACT_UNAVAILABLE", "publication artifact is unavailable", nil)
	}
	// Publication records use sha256:<hex>; storage and export manifests use
	// raw hex. Keep accepting historical raw records without weakening validation.
	sourceHash := strings.TrimPrefix(p.ContentHash, "sha256:")
	if !validHash(sourceHash) || p.TemplateVersionID.IsZero() {
		return result, exportError("STATIC_SOURCE_NOT_EXPORTABLE", "publication snapshot is incomplete", nil)
	}
	slug, err := s.lookup.TemplateSlug(ctx, p.TemplateVersionID)
	if err != nil {
		return result, exportError("STATIC_SOURCE_NOT_EXPORTABLE", "immutable template version is unavailable", err)
	}
	return PageSource{ContentID: p.ContentID, PublicationID: p.ID, TemplateVersionID: p.TemplateVersionID, ContentVersion: p.ContentVersion, TemplateVersion: p.TemplateVersion,
		FullPath: p.FullPath, PublicURL: p.PublicURL, TemplateSlug: slug, SourceSHA256: strings.ToLower(sourceHash), LogicalPublishedAt: p.LogicalPublishedAt,
		StoragePath: s.store.ImmutablePath(p.ContentID, p.ID)}, nil
}
func (s *PublicationSource) Open(ctx context.Context, p PageSource) (io.ReadCloser, error) {
	if p.ContentID.IsZero() || p.PublicationID.IsZero() || !validHash(p.SourceSHA256) {
		return nil, exportError("STATIC_INVALID_REQUEST", "invalid frozen publication", nil)
	}
	// Derive the path again; never trust a path from an API or persisted DTO.
	r, err := s.store.Open(ctx, storage.StoredObject{ContentID: p.ContentID, PublicationID: p.PublicationID, Path: s.store.ImmutablePath(p.ContentID, p.PublicationID), SHA256: p.SourceSHA256})
	if err != nil {
		code := "STATIC_EXPORT_FAILED"
		var storeErr *storage.Error
		if errors.As(err, &storeErr) && storeErr.Code == storage.CodeNotFound {
			code = "STATIC_ARTIFACT_UNAVAILABLE"
		}
		return nil, exportError(code, "cannot read verified immutable publication", err)
	}
	return r, nil
}
func validHash(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

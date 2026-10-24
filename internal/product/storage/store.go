// Package storage implements the MVP filesystem immutable store for the
// LightCMS V3 template static publishing program (Task 6, Lane B).
//
// Layout (spec §17.4):
//
//	content/publications/{content_id}/{publication_id}/index.html
//	content/generated/{canonical_path}.html
//	content/generated/{canonical_path}.html.previous-{publication_id}
//	content/generated/{canonical_path}.html.unpublish-backup-{publication_id}
//
// Intermediate cutover files live in the canonical file's own directory so
// every rename is same-filesystem and atomic:
//
//	content/generated/{canonical_path}.html.next-{publication_id}
//
// The Mongo activation transaction between file cutover and commit is owned
// by the publication saga (Task 8). This package owns the file side:
// staged immutable bytes, atomic canonical projection, compensation
// helpers, unpublish backup staging/restoration/cleanup, and scanner
// metadata. It performs no Mongo I/O; Task 10 (recovery scanner) owns the
// policy that consumes Inspect/IsStale, and Task 2 owns canonical path
// allocation and any future Mongo collection/index definitions.
package storage

import (
	"context"
	"io"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// DefaultStageTimeoutMinutes is the spec §36 default for
// PUBLICATION_STAGE_TIMEOUT_MINUTES: a staged object or orphan .next file
// older than this is eligible for failure marking and cleanup by the
// recovery scanner (Task 10 owns that policy).
const DefaultStageTimeoutMinutes = 15

// DefaultStageTimeout is the duration form of DefaultStageTimeoutMinutes.
const DefaultStageTimeout = 15 * time.Minute

// Error codes returned by CodeOf.
const (
	// CodeInvalidPath is returned when a canonical/public path fails
	// filesystem safety validation (traversal, encoding tricks, empty).
	CodeInvalidPath = "INVALID_PATH"
	// CodeInvalidRequest is returned for malformed store requests
	// (zero ObjectIDs, empty body).
	CodeInvalidRequest = "INVALID_REQUEST"
	// CodeEmptyBody is returned when Stage receives no HTML bytes.
	CodeEmptyBody = "EMPTY_BODY"
	// CodeHashMismatch is returned when bytes on disk do not match the
	// expected SHA-256 (staging verification, Verify, Activate, Restore).
	CodeHashMismatch = "SHA256_MISMATCH"
	// CodeNotFound is returned when a required file is absent.
	CodeNotFound = "OBJECT_NOT_FOUND"
	// CodeAlreadyExists is returned when staging different bytes under an
	// already-staged publication ID, or when a backup target already exists.
	CodeAlreadyExists = "OBJECT_ALREADY_EXISTS"
	// CodeNeedsPreviousID is returned by Activate when a canonical file
	// already exists: the caller must use ActivateWithPrevious so the old
	// canonical is preserved under its owning .previous-{oldID} name.
	CodeNeedsPreviousID = "ACTIVATION_NEEDS_PREVIOUS_ID"
	// CodeConflict is returned when compensation/restoration would clobber
	// a file the store did not create (e.g. canonical bytes changed under
	// us, or a backup target already present).
	CodeConflict = "STORAGE_CONFLICT"
	// CodeIO covers unexpected filesystem errors (close/fsync/rename).
	CodeIO = "STORAGE_IO_ERROR"
)

// Error is a typed store failure. Code is one of the Code* constants above.
type Error struct {
	Code    string
	Message string
	Path    string
	Err     error
}

func (e *Error) Error() string {
	if e.Path != "" {
		return "storage " + e.Code + ": " + e.Message + " (" + e.Path + ")"
	}
	return "storage " + e.Code + ": " + e.Message
}

// Unwrap exposes the underlying filesystem error.
func (e *Error) Unwrap() error { return e.Err }

// CodeOf maps any error to a store code; unknown errors map to CodeIO.
func CodeOf(err error) string {
	if err == nil {
		return ""
	}
	if se, ok := err.(*Error); ok {
		return se.Code
	}
	return CodeIO
}

func storeErr(code, message, path string, err error) *Error {
	return &Error{Code: code, Message: message, Path: path, Err: err}
}

// StageRequest stages one immutable publication object.
// CanonicalPath is validated for filesystem safety; the immutable bytes
// themselves are addressed by (ContentID, PublicationID).
// ExpectedSHA256, when non-empty, must equal the SHA-256 of HTML or the
// stage is rejected before any verified object exists.
type StageRequest struct {
	ContentID      primitive.ObjectID
	PublicationID  primitive.ObjectID
	CanonicalPath  string
	HTML           []byte
	ExpectedSHA256 string
}

// StagedObject is a verified immutable publication object.
type StagedObject struct {
	ContentID     primitive.ObjectID
	PublicationID primitive.ObjectID
	Path          string
	SHA256        string
}

// StoredObject is the durable handle used by Restore/Open.
type StoredObject struct {
	ContentID     primitive.ObjectID
	PublicationID primitive.ObjectID
	Path          string
	SHA256        string
}

// Stored converts a staged object to its durable handle.
func (s StagedObject) Stored() StoredObject {
	return StoredObject{
		ContentID:     s.ContentID,
		PublicationID: s.PublicationID,
		Path:          s.Path,
		SHA256:        s.SHA256,
	}
}

// StaticPageStore is the spec §17.1 interface. FilesystemStore implements it.
//
// Naming note: Activate performs a first-publish cutover. When a canonical
// file already exists it returns CodeNeedsPreviousID instead of guessing the
// previous-backup name; the saga must call ActivateWithPrevious with the
// frozen old active publication ID so the backup is named
// .previous-{oldID} per spec §17.4.
type StaticPageStore interface {
	Stage(ctx context.Context, req StageRequest) (StagedObject, error)
	Verify(ctx context.Context, obj StagedObject) error
	Activate(ctx context.Context, obj StagedObject, publicPath string) error
	Restore(ctx context.Context, obj StoredObject, publicPath string) error
	Open(ctx context.Context, obj StoredObject) (io.ReadCloser, error)
	Abort(ctx context.Context, obj StagedObject) error
	Delete(ctx context.Context, publicPath string) error
	Exists(ctx context.Context, publicPath string) (bool, error)
}

// SidecarKind identifies recovery sidecar files next to a canonical file.
type SidecarKind string

const (
	// SidecarPrevious is {canonical}.previous-{oldPublicationID}: the
	// pre-cutover canonical, kept until the saga confirms the commit.
	SidecarPrevious SidecarKind = "previous"
	// SidecarNext is {canonical}.next-{newPublicationID}: an in-flight
	// cutover copy. Orphans older than the stage timeout are safe to delete.
	SidecarNext SidecarKind = "next"
	// SidecarUnpublishBackup is {canonical}.unpublish-backup-{publicationID}:
	// the staged-away canonical during an unpublish saga.
	SidecarUnpublishBackup SidecarKind = "unpublish-backup"
)

// Sidecar describes one recovery sidecar file for scanner reconciliation.
type Sidecar struct {
	Kind          SidecarKind
	PublicationID primitive.ObjectID
	Path          string
	Size          int64
	SHA256        string
	ModTime       time.Time
}

// CanonicalInfo is the scanner reconciliation view of one public path.
type CanonicalInfo struct {
	Exists   bool
	Path     string
	Size     int64
	SHA256   string
	ModTime  time.Time
	Sidecars []Sidecar
}

// Store is the full Task 6 surface: the spec §17.1 interface plus the
// saga compensation and scanner metadata methods required by Tasks 8 and 10.
type Store interface {
	StaticPageStore

	// ActivateWithPrevious cuts over obj to publicPath, preserving any
	// existing canonical as .previous-{oldPublicationID}. oldPublicationID
	// must be non-nil when a canonical file exists.
	ActivateWithPrevious(ctx context.Context, obj StagedObject, publicPath string, oldPublicationID *primitive.ObjectID) error
	// CompensateActivate undoes a cutover after a failed Mongo commit:
	// removes the new canonical (only if it still carries obj's bytes) and
	// renames .previous-{oldID} back to canonical.
	CompensateActivate(ctx context.Context, obj StagedObject, publicPath string, oldPublicationID *primitive.ObjectID) error
	// ConfirmActivate removes .previous-{oldID} after a successful commit.
	// Idempotent.
	ConfirmActivate(ctx context.Context, publicPath string, oldPublicationID primitive.ObjectID) error

	// StageUnpublishBackup renames the canonical file to
	// .unpublish-backup-{publicationID} and returns the backup path.
	StageUnpublishBackup(ctx context.Context, publicPath string, publicationID primitive.ObjectID) (string, error)
	// RestoreUnpublishBackup renames the backup back to canonical
	// (compensation for a failed unpublish transaction).
	RestoreUnpublishBackup(ctx context.Context, publicPath string, publicationID primitive.ObjectID) error
	// RemoveUnpublishBackup deletes the backup after a successful commit.
	// Idempotent.
	RemoveUnpublishBackup(ctx context.Context, publicPath string, publicationID primitive.ObjectID) error

	// DeleteImmutable removes one immutable publication object (retention
	// GC mechanic; policy owned by Task 10). Idempotent, never touches
	// canonical files.
	DeleteImmutable(ctx context.Context, contentID, publicationID primitive.ObjectID) error

	// Inspect reports canonical presence/hash plus all recovery sidecars
	// for scanner reconciliation. A missing canonical is reported with
	// Exists=false, not as an error.
	Inspect(ctx context.Context, publicPath string) (CanonicalInfo, error)
	// IsStale reports whether modTime is older than the stage timeout.
	IsStale(modTime, now time.Time) bool
	// Timeout returns the effective stage timeout.
	Timeout() time.Duration

	// ImmutablePath returns the immutable file path for an object.
	ImmutablePath(contentID, publicationID primitive.ObjectID) string
	// CanonicalFilePath resolves a public path to its canonical file.
	CanonicalFilePath(publicPath string) (string, error)
}

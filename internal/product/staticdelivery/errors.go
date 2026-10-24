// Package staticdelivery owns the independent static-report delivery lifecycle.
// It never changes the existing CMS Publication state machine.
package staticdelivery

import (
	"errors"
	"net/http"
)

const (
	CodeInvalidRequest        = "STATIC_INVALID_REQUEST"
	CodePermissionDenied      = "STATIC_PERMISSION_DENIED"
	CodeNotFound              = "STATIC_RESOURCE_NOT_FOUND"
	CodeAccountExists         = "CF_ACCOUNT_EXISTS"
	CodeTargetExists          = "CF_TARGET_EXISTS"
	CodeUnmanagedWorker       = "CF_WORKER_UNMANAGED"
	CodeRemoteDrift           = "CF_REMOTE_DRIFT"
	CodeRevisionConflict      = "STATIC_REVISION_CONFLICT"
	CodePathConflict          = "STATIC_PATH_CONFLICT"
	CodeIdempotencyConflict   = "STATIC_IDEMPOTENCY_CONFLICT"
	CodeTargetBusy            = "STATIC_TARGET_BUSY"
	CodeActivationUnknown     = "STATIC_ACTIVATION_UNKNOWN"
	CodeAccountDisabled       = "CF_ACCOUNT_DISABLED"
	CodeTargetNotReady        = "CF_TARGET_NOT_READY"
	CodeSourceNotExportable   = "STATIC_SOURCE_NOT_EXPORTABLE"
	CodeArtifactUnavailable   = "STATIC_ARTIFACT_UNAVAILABLE"
	CodeNotStandalone         = "STATIC_DOCUMENT_NOT_STANDALONE"
	CodeDependencyUnresolved  = "STATIC_DEPENDENCY_UNRESOLVED"
	CodeMetadataInvalid       = "STATIC_METADATA_INVALID"
	CodeAssetLimitExceeded    = "STATIC_ASSET_LIMIT_EXCEEDED"
	CodeOriginNotReady        = "CF_ORIGIN_NOT_READY"
	CodeCFPermission          = "CF_PERMISSION_INSUFFICIENT"
	CodeIdempotencyRequired   = "STATIC_IDEMPOTENCY_KEY_REQUIRED"
	CodeConfirmationRequired  = "STATIC_CONFIRMATION_REQUIRED"
	CodeRateLimited           = "STATIC_RATE_LIMITED"
	CodeModuleUnavailable     = "STATIC_MODULE_UNAVAILABLE"
	CodeCFUnavailable         = "CF_TEMPORARILY_UNAVAILABLE"
	CodeUploadFailed          = "CF_UPLOAD_FAILED"
	CodeVersionUploadFailed   = "CF_VERSION_UPLOAD_FAILED"
	CodeCFActivationUnknown   = "CF_ACTIVATION_UNKNOWN"
	CodeVerificationFailed    = "CF_PUBLIC_VERIFICATION_FAILED"
	CodeCredentialUnavailable = "CF_CREDENTIAL_UNAVAILABLE"
	CodeExportFailed          = "STATIC_EXPORT_FAILED"
	CodeJobTimeout            = "STATIC_JOB_TIMEOUT"
	CodeInternal              = "STATIC_INTERNAL_ERROR"
)

// Cause is for errors.Is/As and internal diagnostics, never a public message.
// Cloudflare bodies can contain credential material; Error deliberately omits it.
type Error struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
	Cause     error  `json:"-"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }
func (e *Error) Unwrap() error { return e.Cause }
func CodeOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	var coded interface{ StaticCode() string }
	if errors.As(err, &coded) {
		return coded.StaticCode()
	}
	if err == nil {
		return ""
	}
	return CodeInternal
}
func StatusForError(err error) int {
	if err == nil {
		return http.StatusOK
	}
	switch CodeOf(err) {
	case CodeInvalidRequest:
		return http.StatusBadRequest
	case CodePermissionDenied:
		return http.StatusForbidden
	case CodeNotFound:
		return http.StatusNotFound
	case CodeAccountExists, CodeTargetExists, CodeUnmanagedWorker, CodeRemoteDrift,
		CodeRevisionConflict, CodePathConflict, CodeIdempotencyConflict, CodeTargetBusy,
		CodeActivationUnknown, CodeAccountDisabled, CodeTargetNotReady, CodeSourceNotExportable:
		return http.StatusConflict
	case CodeArtifactUnavailable:
		return http.StatusGone
	case CodeNotStandalone, CodeDependencyUnresolved, CodeMetadataInvalid, CodeAssetLimitExceeded, CodeOriginNotReady, CodeCFPermission:
		return http.StatusUnprocessableEntity
	case CodeIdempotencyRequired, CodeConfirmationRequired:
		return http.StatusPreconditionRequired
	case CodeRateLimited:
		return http.StatusTooManyRequests
	case CodeModuleUnavailable, CodeCFUnavailable, CodeCredentialUnavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

func failure(code, message string, cause error) error {
	return &Error{Code: code, Message: message, Cause: cause}
}

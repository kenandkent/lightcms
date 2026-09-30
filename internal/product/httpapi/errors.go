// Package httpapi owns the Task 12 product HTTP facade (plan Task 12; spec
// §20, §21, §27, §32): strict page-generation command, immutable schema
// endpoint, and publication list/detail/rollback. Route REGISTRATION lives in
// Task 16 (cmd/server/main.go) — this package builds handler functions that
// reuse the existing /api/v1 auth/rate/body/provenance middleware at wiring
// time.
//
// Exact routes (for Task 18 OpenAPI):
//
//	GET  /api/v1/templates/{slug}/schema
//	POST /api/v1/page-generation
//	GET  /api/v1/content/{id}/publications
//	GET  /api/v1/content/{id}/publications/{publication_id}
//	POST /api/v1/content/{id}/publications/{publication_id}/rollback
//	POST /api/v1/templates/{id}/migrate-slug            (admin-only)
//	GET  /api/v1/templates/{slug}/upgrade-preview
//	POST /api/v1/templates/{slug}/upgrade-jobs
//	GET  /api/v1/templates/upgrade-jobs/{job_id}
//	POST /api/v1/content/{id}/restore-and-publish
//	POST /api/v1/content/{id}/revert-live
package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/jonradoff/lightcms/v7/internal/product/generation"
)

// ErrorEnvelope is the spec §20.8/§27 error shape.
type ErrorEnvelope struct {
	Error ErrorBody `json:"error"`
}

// ErrorBody carries code/message/request_id/retryable/details.
type ErrorBody struct {
	Code      string             `json:"code"`
	Message   string             `json:"message"`
	RequestID string             `json:"request_id,omitempty"`
	Retryable bool               `json:"retryable,omitempty"`
	Details   []generation.FieldDetail `json:"details,omitempty"`
}

// WriteError maps a generation/service error to its HTTP status + envelope.
// 429, REQUEST_IN_PROGRESS and retryable 503 set Retry-After.
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	code := generation.CodeOf(err)
	if code == "" {
		code = generation.CodeInternal
	}
	status := generation.StatusForCode(code)
	retryAfter := 0
	var details []generation.FieldDetail
	var msg string
	if ge, ok := err.(*generation.Error); ok {
		msg = ge.Message
		details = ge.Details
		retryAfter = ge.RetryAfter
	} else {
		msg = err.Error()
	}
	if msg == "" {
		msg = code
	}
	if retryAfter == 0 && (code == generation.CodeRateLimited || code == generation.CodeRequestInProgress) {
		retryAfter = 5
	}
	if retryAfter > 0 {
		w.Header().Set("Retry-After", itoa(retryAfter))
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(ErrorEnvelope{Error: ErrorBody{
		Code: code, Message: msg, RequestID: requestID(r),
		Retryable: generation.Retryable(code), Details: details,
	}})
}

func requestID(r *http.Request) string {
	if r == nil {
		return ""
	}
	if v := r.Header.Get("X-Request-ID"); v != "" {
		return v
	}
	if v := r.Header.Get("X-Request-Id"); v != "" {
		return v
	}
	return ""
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [16]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// WriteJSON encodes v as JSON with status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

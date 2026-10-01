package apiclient

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// APIError is a typed HTTP error returned by the LightCMS API. It parses
// BOTH error envelopes (lane 3A contract unification):
//
//   - legacy flat:  {"error":"message"} with an optional sibling "code"
//     (lane 3B starts emitting sibling codes)
//   - V3 nested:    {"error":{"code","message","request_id","retryable"}}
//     (internal/product/httpapi ErrorEnvelope)
//
// When neither shape parses, Message carries the generic
// "API error (status N): <raw body>" fallback. APIError implements error,
// so callers that only check err != nil keep working unchanged.
type APIError struct {
	Status    int
	Code      string
	Message   string
	Retryable bool
	RequestID string
}

// Error returns the human-readable message (plus code when known), keeping
// the legacy flat-message text byte-identical for legacy-only responses.
func (e *APIError) Error() string {
	switch {
	case e.Message != "" && e.Code != "":
		return e.Message + " (" + e.Code + ")"
	case e.Message != "":
		return e.Message
	case e.Code != "":
		return e.Code
	default:
		return fmt.Sprintf("API error (status %d)", e.Status)
	}
}

// parseAPIError builds an *APIError from an HTTP status + response body,
// preferring the V3 nested envelope, then the legacy flat envelope, then the
// generic raw-body fallback.
func parseAPIError(status int, body []byte) *APIError {
	apiErr := &APIError{Status: status}

	var envelope struct {
		Error json.RawMessage `json:"error"`
		Code  string          `json:"code"` // legacy flat sibling code (lane 3B)
	}
	if err := json.Unmarshal(body, &envelope); err == nil && len(envelope.Error) > 0 {
		trimmed := bytes.TrimSpace(envelope.Error)
		switch {
		case len(trimmed) > 0 && trimmed[0] == '"':
			// Legacy flat: {"error":"message", "code":"optional"}
			var msg string
			if json.Unmarshal(trimmed, &msg) == nil && msg != "" {
				apiErr.Message = msg
				apiErr.Code = envelope.Code
				return apiErr
			}
		case len(trimmed) > 0 && trimmed[0] == '{':
			// V3 nested: {"error":{"code","message","request_id","retryable"}}
			var nested struct {
				Code      string `json:"code"`
				Message   string `json:"message"`
				RequestID string `json:"request_id"`
				Retryable bool   `json:"retryable"`
			}
			if json.Unmarshal(trimmed, &nested) == nil && (nested.Message != "" || nested.Code != "") {
				apiErr.Code = nested.Code
				apiErr.Message = nested.Message
				apiErr.RequestID = nested.RequestID
				apiErr.Retryable = nested.Retryable
				return apiErr
			}
		}
	}

	apiErr.Message = fmt.Sprintf("API error (status %d): %s", status, string(body))
	return apiErr
}

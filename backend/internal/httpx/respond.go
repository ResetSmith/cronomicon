// Package httpx holds shared HTTP helpers: the canonical error envelope and
// JSON write helpers used by every handler. The error shape mirrors the
// `Error` schema in openapi.yaml so the generated client decodes it uniformly.
package httpx

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// Fail500 writes a sanitized 500 response and logs the real error server-side
// (PP-L14). Raw DB/driver error strings are never sent to the client.
func Fail500(w http.ResponseWriter, log *slog.Logger, code string, err error) {
	log.Error("internal error", "code", code, "error", err)
	JSON(w, http.StatusInternalServerError, Error{Code: code, Message: "internal server error"})
}

// Error is the canonical error envelope (matches openapi.yaml components.schemas.Error).
type Error struct {
	Code    string         `json:"code"`              // machine-readable, e.g. "validation_failed"
	Message string         `json:"message"`           // human-readable summary
	Details map[string]any `json:"details,omitempty"` // optional structured context
}

// JSON writes v as an application/json response with the given status.
func JSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("failed encoding response body", "error", err)
	}
}

// Fail writes the canonical error envelope.
func Fail(w http.ResponseWriter, status int, code, message string) {
	JSON(w, status, Error{Code: code, Message: message})
}

// FailDetails writes the canonical error envelope with structured details
// (used e.g. for line-numbered YAML validation errors per S10, or the 412
// concurrent-edit diff per A2).
func FailDetails(w http.ResponseWriter, status int, code, message string, details map[string]any) {
	JSON(w, status, Error{Code: code, Message: message, Details: details})
}

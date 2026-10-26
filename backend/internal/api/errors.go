// Package api implements the HTTP contracts in architecture section 5.3.
package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/shota-urano/evaloom/backend/internal/registry"
)

type Code string

const (
	ValidationFailed   Code = "validation_failed"
	Unauthenticated    Code = "unauthenticated"
	Forbidden          Code = "forbidden"
	NotFound           Code = "not_found"
	Conflict           Code = "conflict"
	PreconditionFailed Code = "precondition_failed"
	Internal           Code = "internal"
)

type Detail struct {
	Field  string `json:"field"`
	Reason string `json:"reason"`
}

// Error contains only information that is safe to disclose to API clients.
type Error struct {
	Code    Code     `json:"code"`
	Message string   `json:"message"`
	Details []Detail `json:"details"`
}

func (e *Error) Error() string { return e.Message }

func (e *Error) StatusCode() int {
	switch e.Code {
	case ValidationFailed:
		return http.StatusBadRequest
	case Unauthenticated:
		return http.StatusUnauthorized
	case Forbidden:
		return http.StatusForbidden
	case NotFound:
		return http.StatusNotFound
	case Conflict:
		return http.StatusConflict
	case PreconditionFailed:
		return http.StatusUnprocessableEntity
	default:
		return http.StatusInternalServerError
	}
}

func invalid(field, reason string) error {
	return &Error{Code: ValidationFailed, Message: "invalid request", Details: []Detail{{Field: field, Reason: reason}}}
}

// WriteError keeps unexpected errors in server logs, including internal errors
// explicitly returned by a handler. Unknown codes also fail closed as internal.
func WriteError(w http.ResponseWriter, logger *slog.Logger, err error) {
	if logger == nil {
		logger = slog.Default()
	}
	public := &Error{Code: Internal, Message: "internal server error"}
	var apiErr *Error
	var precondition *registry.PreconditionError
	if errors.As(err, &apiErr) && apiErr.StatusCode() != http.StatusInternalServerError {
		copy := *apiErr
		public = &copy
	} else if errors.As(err, &precondition) {
		public = &Error{Code: PreconditionFailed, Message: precondition.Message}
		for _, detail := range precondition.Details {
			public.Details = append(public.Details, Detail{Field: detail.Field, Reason: detail.Reason})
		}
	} else {
		logger.Error("API request failed", "error", err)
	}
	if public.Details == nil {
		public.Details = []Detail{}
	}
	writeJSON(w, public.StatusCode(), struct {
		Error *Error `json:"error"`
	}{public})
}

// Encode before committing headers so a serialization failure cannot produce a
// partially written successful response.
func writeJSON(w http.ResponseWriter, status int, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		WriteError(w, nil, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(data, '\n'))
}

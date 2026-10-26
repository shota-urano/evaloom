package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/registry"
)

func TestErrorContractArchitecture53(t *testing.T) {
	for _, tc := range []struct {
		code   string
		status int
	}{
		{"validation_failed", 400}, {"unauthenticated", 401}, {"forbidden", 403},
		{"not_found", 404}, {"conflict", 409}, {"precondition_failed", 422}, {"internal", 500},
	} {
		t.Run(tc.code, func(t *testing.T) {
			var logs bytes.Buffer
			w := httptest.NewRecorder()
			WriteError(w, slog.New(slog.NewTextHandler(&logs, nil)), fmt.Errorf("wrapped: %w", &Error{
				Code: Code(tc.code), Message: "safe explanation", Details: []Detail{{Field: "acceptanceCriteria", Reason: "required"}},
			}))
			if w.Code != tc.status || w.Header().Get("Content-Type") != "application/json" {
				t.Fatalf("status/content type = %d/%q; want %d/application/json", w.Code, w.Header().Get("Content-Type"), tc.status)
			}
			var actual map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &actual); err != nil {
				t.Fatal(err)
			}
			message := "safe explanation"
			details := []any{map[string]any{"field": "acceptanceCriteria", "reason": "required"}}
			if tc.status == 500 {
				message = "internal server error"
				details = []any{}
			}
			want := map[string]any{"error": map[string]any{"code": tc.code, "message": message, "details": details}}
			if !reflect.DeepEqual(actual, want) {
				t.Fatalf("body = %#v; want %#v", actual, want)
			}
			if tc.status == 500 && !strings.Contains(logs.String(), "safe explanation") {
				t.Fatal("internal detail missing from logs")
			}
		})
	}
}

func TestUnexpectedErrorsAreLoggedOnly(t *testing.T) {
	for _, err := range []error{errors.New("secret database path"), &Error{Code: "unknown", Message: "secret database path"}} {
		var logs bytes.Buffer
		w := httptest.NewRecorder()
		WriteError(w, slog.New(slog.NewTextHandler(&logs, nil)), err)
		if w.Code != 500 || strings.Contains(w.Body.String(), "secret") || !strings.Contains(logs.String(), "secret database path") {
			t.Fatalf("status=%d body=%s logs=%s", w.Code, w.Body.String(), logs.String())
		}
	}
}

func TestRegistryPreconditionErrorUsesCommonEnvelope(t *testing.T) {
	w := httptest.NewRecorder()
	WriteError(w, nil, fmt.Errorf("wrapped: %w", &registry.PreconditionError{
		Code: "precondition_failed", Message: "repository validation failed", Details: []registry.Detail{{Field: "path", Reason: "not_absolute"}},
	}))
	if w.Code != 422 || w.Body.String() != "{\"error\":{\"code\":\"precondition_failed\",\"message\":\"repository validation failed\",\"details\":[{\"field\":\"path\",\"reason\":\"not_absolute\"}]}}\n" {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

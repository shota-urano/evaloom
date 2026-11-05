package harnessenv

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestAuthenticationLogExclusionStreamsAndAccumulatesAtCommandBoundaries(t *testing.T) {
	e, a, store := authCopy(t, "CODEX_HOME")
	initial, _ := os.ReadFile(store)
	for _, value := range []string{"private-original", "private-preflight", "private-final"} {
		if value != "private-original" {
			if err := e.RefreshAuthentication([]byte(`{"token":"` + value + `"}`)); err != nil {
				t.Fatal(err)
			}
		}
		if got, ok := a.FilterLog("diagnostic " + value); !ok || got != "diagnostic [REDACTED]" {
			t.Fatal("observed credential not immediately excluded")
		}
		if got, ok := a.FilterLog("ordinary diagnostic"); !ok || got != "ordinary diagnostic" {
			t.Fatal("ordinary output not available during execution")
		}
	}
	if got := a.RedactLog("private-original private-preflight private-final"); strings.Contains(got, "private-") {
		t.Fatal("command-boundary exclusions did not accumulate")
	}
	stored, err := os.ReadFile(store)
	if err != nil || string(stored) != string(initial) {
		t.Fatal("log refresh persisted credentials")
	}
}

func TestAuthenticationLogExclusionStaysSuppressedAfterFailedRefresh(t *testing.T) {
	e, a, _ := authCopy(t, "CODEX_HOME")
	if e.RefreshAuthentication([]byte(`{"token":"private-invalid`)) == nil {
		t.Fatal("invalid refresh succeeded")
	}
	if err := e.RefreshAuthentication([]byte(`{"token":"private-valid"}`)); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"unknown credential", "ordinary output", ""} {
		if got, ok := a.FilterLog(text); ok || got != "" {
			t.Fatal("CLI line released after failed recovery")
		}
	}
}

func TestAuthenticationLogExclusionConcurrentWithRefreshAndClose(t *testing.T) {
	e, a, _ := authCopy(t, "CODEX_HOME")
	var readers sync.WaitGroup
	for range 4 {
		readers.Go(func() {
			for range 50 {
				if got, ok := a.FilterLog("private-original"); ok && got != "[REDACTED]" {
					t.Error("initial credential survived concurrent exclusion")
				}
			}
		})
	}
	for range 10 {
		if err := e.RefreshAuthentication([]byte(`{"token":"private-refreshed"}`)); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	readers.Wait()
	if _, ok := a.FilterLog("after close"); ok {
		t.Fatal("closed lease released CLI output")
	}
}

func TestAuthenticationLogExclusionIncludesOriginalAndRecoveredValues(t *testing.T) {
	e, a, _ := authCopy(t, "CODEX_HOME")
	updated := map[string]any{"nested": []any{map[string]string{
		"token": "private-refreshed-token", "escaped": "private-quote\"slash\\value",
		"multiline": "private-first\nprivate-second",
	}}}
	data, _ := json.MarshalIndent(updated, "", "  ")
	if err := e.WriteBackAuthentication(data); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{string(a.initial), string(data), "private-original", "private-refreshed-token",
		"private-quote\"slash\\value", `private-quote\"slash\\value`, "private-first", "private-second"} {
		if got := a.RedactLog("before " + secret + " after"); strings.Contains(got, "private-") || !strings.Contains(got, "[REDACTED]") {
			t.Fatal("credential survived log exclusion")
		}
	}
	if a.RedactLog("ordinary diagnostic") != "ordinary diagnostic" {
		t.Fatal("ordinary output lost")
	}
}

func TestAuthenticationLogExclusionFailsClosedOnRecoveryFailure(t *testing.T) {
	e, a, _ := authCopy(t, "CODEX_HOME")
	if err := os.WriteFile(filepath.Join(e.ConfigDir(), "auth.json"), []byte(`{"token":"private-invalid`), 0600); err != nil {
		t.Fatal(err)
	}
	if e.WriteBackAuthentication(nil) == nil {
		t.Fatal("invalid recovery succeeded")
	}
	if a.RedactLog("unknown refreshed credential") != "[REDACTED]" {
		t.Fatal("unverifiable output released")
	}
}

func TestAuthenticationLogExclusionPreservesAllJSONSpellingsAndDuplicateValues(t *testing.T) {
	e, a, _ := authCopy(t, "CODEX_HOME")
	if err := e.WriteBackAuthentication([]byte(`{"token":"private-duplicate-first","token":"private-\u0075nicode"}`)); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"private-duplicate-first", "private-unicode", `private-\u0075nicode`} {
		if got := a.RedactLog(`{"token":"` + value + `"}`); strings.Contains(got, "private-") {
			t.Fatal("original or decoded JSON spelling survived exclusion")
		}
	}
}

func TestAuthenticationLogExclusionSurvivesPersistenceFailure(t *testing.T) {
	e, a, store := authCopy(t, "CODEX_HOME")
	before, _ := os.ReadFile(store)
	a.name = "missing/store" // fail replacement after valid credential recovery
	if e.WriteBackAuthentication([]byte(`{"token":"private-unpersisted"}`)) == nil {
		t.Fatal("replacement unexpectedly succeeded")
	}
	after, err := os.ReadFile(store)
	if err != nil || string(after) != string(before) {
		t.Fatal("failed replacement changed store")
	}
	if a.RedactLog("private-unpersisted") != "[REDACTED]" {
		t.Fatal("recovered value leaked after persistence failure")
	}
}

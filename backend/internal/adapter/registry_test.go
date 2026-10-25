package adapter_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
)

func TestLookupByName(t *testing.T) {
	var registry adapter.Registry
	first := &stubAdapter{capabilities: adapter.Capabilities{
		Streaming: true, TokenUsage: true, NativeInstructionFiles: []string{"AGENTS.md"},
	}}
	second := &stubAdapter{capabilities: adapter.Capabilities{Resume: true}}
	for name, implementation := range map[string]adapter.Adapter{
		"codex": first, "claude_code": second, "new_harness": &stubAdapter{},
	} {
		if err := registry.Register(name, implementation); err != nil {
			t.Fatalf("Register(%q): %v", name, err)
		}
		got, err := registry.Lookup(name)
		if err != nil {
			t.Fatalf("Lookup(%q): %v", name, err)
		}
		if got != implementation {
			t.Fatalf("Lookup(%q) returned a different implementation", name)
		}
		if !reflect.DeepEqual(got.Capabilities(), implementation.Capabilities()) {
			t.Fatalf("Lookup(%q) returned different capabilities", name)
		}
	}
}

func TestLookupUnregisteredName(t *testing.T) {
	var registry adapter.Registry
	if err := registry.Register("codex", &stubAdapter{}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"unknown", "", "Codex", "codex "} {
		t.Run(name, func(t *testing.T) {
			got, err := registry.Lookup(name)
			if got != nil || !errors.Is(err, adapter.ErrNotRegistered) {
				t.Fatalf("Lookup(%q) = (%v, %v), want nil and ErrNotRegistered", name, got, err)
			}
			if !strings.Contains(err.Error(), name) {
				t.Fatalf("lookup error does not identify %q: %v", name, err)
			}
		})
	}
}

func TestDuplicateRegistrationPreservesImplementation(t *testing.T) {
	var registry adapter.Registry
	original := &stubAdapter{}
	if err := registry.Register("codex", original); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register("codex", &stubAdapter{}); err == nil {
		t.Fatal("duplicate registration succeeded")
	}
	got, err := registry.Lookup("codex")
	if err != nil || got != original {
		t.Fatalf("duplicate registration replaced original: (%v, %v)", got, err)
	}
}

func TestInvalidRegistration(t *testing.T) {
	var typedNil *stubAdapter
	for _, tc := range []struct {
		name           string
		implementation adapter.Adapter
	}{
		{"", &stubAdapter{}},
		{" codex", &stubAdapter{}},
		{"codex ", &stubAdapter{}},
		{"codex", nil},
		{"codex", typedNil},
	} {
		var registry adapter.Registry
		if err := registry.Register(tc.name, tc.implementation); err == nil {
			t.Errorf("Register(%q, %v) succeeded", tc.name, tc.implementation)
		}
		if got := registry.Names(); len(got) != 0 {
			t.Errorf("invalid registration left entries: %v", got)
		}
	}
}

func TestNamesAreDerivedFromRegistrations(t *testing.T) {
	var registry adapter.Registry
	if got := registry.Names(); len(got) != 0 {
		t.Fatalf("empty registry names = %v", got)
	}
	for _, name := range []string{"new_harness", "codex", "claude_code"} {
		if err := registry.Register(name, &stubAdapter{}); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"claude_code", "codex", "new_harness"}
	got := registry.Names()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Names() = %v, want %v", got, want)
	}
	got[0] = "changed"
	if got := registry.Names(); !reflect.DeepEqual(got, want) {
		t.Fatalf("mutating names changed registry: %v", got)
	}
}

// stubAdapter implements only the contract; no CLI or Runner is involved.
type stubAdapter struct {
	capabilities adapter.Capabilities
}

var _ adapter.Adapter = (*stubAdapter)(nil)

func (*stubAdapter) Start(context.Context, adapter.HarnessRequest, adapter.SandboxEnv) (adapter.SessionID, error) {
	return "stub-session", nil
}

func (*stubAdapter) Status(adapter.SessionID) (adapter.SessionStatus, error) {
	return adapter.SessionSucceeded, nil
}

func (*stubAdapter) Logs(adapter.SessionID) (<-chan adapter.LogLine, error) {
	lines := make(chan adapter.LogLine)
	close(lines)
	return lines, nil
}

func (*stubAdapter) Cancel(adapter.SessionID) error { return nil }

func (*stubAdapter) Result(id adapter.SessionID) (adapter.HarnessResult, error) {
	return adapter.HarnessResult{ContractVersion: "1", Status: adapter.ResultSucceeded, SessionID: id}, nil
}

func (s *stubAdapter) Capabilities() adapter.Capabilities { return s.capabilities }

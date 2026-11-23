package adapter_test

import (
	"slices"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/adapter/claudecode"
	"github.com/shota-urano/evaloom/backend/internal/adapter/codex"
	"github.com/shota-urano/evaloom/backend/internal/registry"
	"github.com/shota-urano/evaloom/backend/internal/sandboxproxy"
)

// Work Sandbox reads the public declaration, rather than the private helper
// exercised by the legacy Harness ContainerLaunch tests. Pin the actual
// declaration to Q71's model/auth candidates and verify callers cannot extend it.
func TestConcreteAdapterModelAPIDestinations(t *testing.T) {
	for _, tc := range []struct {
		name           string
		implementation adapter.Adapter
		want           []string
	}{
		{"codex", codex.New(codex.Config{}), []string{"chatgpt.com:443", "auth.openai.com:443", "api.openai.com:443"}},
		{"claude_code", claudecode.New(claudecode.Config{}), []string{"api.anthropic.com:443", "console.anthropic.com:443", "claude.ai:443", "platform.claude.com:443"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			declarer, ok := tc.implementation.(registry.ModelAPIDeclarer)
			if !ok {
				t.Fatal("Adapter does not declare model API destinations")
			}
			destinations := declarer.ModelAPIDestinations()
			if !slices.Equal(destinations, tc.want) {
				t.Fatalf("destinations = %q, want only model/auth candidates %q", destinations, tc.want)
			}
			for _, destination := range destinations {
				if _, err := sandboxproxy.Destination(destination); err != nil {
					t.Fatalf("Work Sandbox cannot allow %q: %v", destination, err)
				}
			}
			destinations[0] = "injected.example:443"
			if !slices.Equal(declarer.ModelAPIDestinations(), tc.want) {
				t.Fatal("caller changed the Adapter's subsequent network declaration")
			}
		})
	}
}

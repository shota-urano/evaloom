package main

import (
	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/adapter/claudecode"
	"github.com/shota-urano/evaloom/backend/internal/adapter/codex"
)

// The Control Plane uses Adapter declarations for Profile validation only.
// Execution configuration (Sandbox launchers and authentication) belongs to
// Runner wiring; no CLI is started or credential file read here.
func controlPlaneAdapters() (*adapter.Registry, error) {
	registry := &adapter.Registry{}
	for _, entry := range []struct {
		name string
		impl adapter.Adapter
	}{
		{"codex", codex.New(codex.Config{})},
		{"claude_code", claudecode.New(claudecode.Config{})},
	} {
		if err := registry.Register(entry.name, entry.impl); err != nil {
			return nil, err
		}
	}
	return registry, nil
}

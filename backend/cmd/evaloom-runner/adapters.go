package main

import (
	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/adapter/claudecode"
	"github.com/shota-urano/evaloom/backend/internal/adapter/codex"
	"github.com/shota-urano/evaloom/backend/internal/runner"
)

func productionAdapters(w runner.WorkAdapterConfig) *adapter.Registry {
	registry := &adapter.Registry{}
	_ = registry.Register("codex", codex.New(codex.Config{Launch: w.Launch, ResolveConfig: w.ResolveConfig, ResolveAuthentication: w.ResolveAuthentication, ResolveToolPermissions: w.ResolveToolPermissions, CancelGrace: w.CancelGrace}))
	_ = registry.Register("claude_code", claudecode.New(claudecode.Config{Launch: w.Launch, ResolveConfig: w.ResolveConfig, ResolveAuthentication: w.ResolveAuthentication, ResolveToolPermissions: w.ResolveToolPermissions, CancelGrace: w.CancelGrace}))
	return registry
}

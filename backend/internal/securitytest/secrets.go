package securitytest

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/shota-urano/evaloom/backend/internal/secretstore"
)

// Test secret slots of spec 14 3.0. probe_read is registered with access
// read, probe_write with access write.
const (
	ProbeReadSlot  = "probe_read"
	ProbeWriteSlot = "probe_write"
)

// ProbeSecrets holds the canary values stored in the Runner's secret store.
// Probes report only whether a value contains them, never the value itself.
type ProbeSecrets struct {
	Read, Write string
}

// Values lists both canaries, for env probes and output scans.
func (s ProbeSecrets) Values() []string { return []string{s.Read, s.Write} }

// NewProbeSecrets returns fresh canaries for both slots.
func NewProbeSecrets() ProbeSecrets { return ProbeSecrets{Read: NewCanary(), Write: NewCanary()} }

// Store registers the canaries for harnessID in the secret store below
// dataDir (the Runner's dataDir), through the production store.
func (s ProbeSecrets) Store(dataDir, harnessID string) error {
	store := secretstore.New(dataDir)
	if err := store.Set(harnessID, ProbeReadSlot, "read", []byte(s.Read)); err != nil {
		return err
	}
	return store.Set(harnessID, ProbeWriteSlot, "write", []byte(s.Write))
}

// SecretProbe is a plan item that reports whether value is in the
// environment or in one of paths. The plan carries only its SHA-256 and
// length, never the value, because stages can read the committed plans.
func SecretProbe(id, value string, paths ...string) map[string]any {
	sum := sha256.Sum256([]byte(value))
	if paths == nil {
		paths = []string{}
	}
	return map[string]any{"id": id, "kind": "secret", "sha256": hex.EncodeToString(sum[:]), "length": len(value), "paths": paths}
}

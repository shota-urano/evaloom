package securitytest

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/adapter/probe"
)

// Test-owned oracle. The plan and trusted snapshots never come from the
// harness. No production API/schema is added for acceptance-test observations.
type boundaryRule struct {
	id, kind string
	allowed  bool // positive controls must actually execute successfully
}

type boundarySnapshot struct {
	http, audits, github, agent int
	refs                        map[string]string
}

func boundaryVerdict(data []byte, rules []boundaryRule, allowEnv []string, before, after boundarySnapshot) error {
	observations, err := probe.DecodeReport(data)
	if err != nil {
		return fmt.Errorf("report unavailable/invalid: %w", err)
	}
	var violations []string
	if len(observations) != len(rules) {
		violations = append(violations, "incomplete/extra observations")
	}
	byID := make(map[string]probe.Observation)
	for _, observation := range observations {
		byID[observation.ID] = observation
	}
	for _, rule := range rules {
		observation, ok := byID[rule.id]
		if !ok || observation.Kind != rule.kind {
			violations = append(violations, rule.id+": missing/wrong kind")
			continue
		}
		if *observation.Succeeded != rule.allowed {
			violations = append(violations, rule.id+": unexpected access outcome")
		}
		if rule.kind == "env" {
			// Validate the data itself, not just the script's summary booleans.
			encoded, _ := json.Marshal(observation.Detail)
			var detail struct {
				Names         []string `json:"names"`
				Unexpected    []string `json:"unexpected"`
				SecretPresent *bool    `json:"secretPresent"`
			}
			if json.Unmarshal(encoded, &detail) != nil || len(detail.Names) == 0 ||
				detail.Unexpected == nil || len(detail.Unexpected) != 0 || detail.SecretPresent == nil || *detail.SecretPresent {
				violations = append(violations, rule.id+": invalid environment evidence/secret exposed")
			}
			for _, name := range detail.Names {
				if !slices.Contains(allowEnv, name) {
					violations = append(violations, rule.id+": forbidden variable "+name)
				}
			}
		}
	}
	if before.http != after.http {
		violations = append(violations, "control-http: trusted access log changed")
	}
	if before.audits != after.audits {
		violations = append(violations, "control-audit: audit_logs changed")
	}
	if before.github != after.github {
		violations = append(violations, "github: trusted HTTP log changed")
	}
	if before.agent != after.agent {
		violations = append(violations, "ssh-agent: accepted connection")
	}
	if len(before.refs) == 0 || after.refs == nil || !maps.Equal(before.refs, after.refs) {
		violations = append(violations, "git-push: trusted remote refs changed/unavailable")
	}
	if len(violations) != 0 {
		return fmt.Errorf("permission boundary: %s", strings.Join(violations, "; "))
	}
	return nil
}

func verdictReport(t *testing.T, observations []probe.Observation) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]any{"observations": observations})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestBoundaryVerdict(t *testing.T) {
	denied, succeeded := false, true
	// Each spec 14 §3.2 row has a denied-access observation, except the
	// conditional Q8 first-option row (second option is confirmed in spec 09).
	rules := []boundaryRule{
		{"control-owner-v4", "http", false}, {"control-runner-v4", "http", false},
		{"control-owner-v6", "http", false}, {"control-runner-v6", "http", false},
		{"db-read", "read", false}, {"db-write", "write", false},
		{"logs", "read", false}, {"artifacts", "read", false},
		{"runner-token", "read", false}, {"publisher", "read", false},
		{"keychain", "read", false}, {"ssh-agent", "command", false},
		{"environment", "env", true}, {"git-push", "git_push", false},
		{"github-direct", "tcp", false}, {"github-proxy", "proxy_connect", false},
		{"dedicated-model-config", "read", true},
	}
	observations := make([]probe.Observation, len(rules))
	for i, rule := range rules {
		ok := &denied
		if rule.allowed {
			ok = &succeeded
		}
		observations[i] = probe.Observation{ID: rule.id, Kind: rule.kind, Succeeded: ok, Detail: map[string]any{}}
		if rule.kind == "env" {
			observations[i].Detail = map[string]any{"names": []string{"HOME", "PATH"}, "unexpected": []string{}, "secretPresent": false}
		}
	}
	allowEnv := []string{"HOME", "PATH"}
	snapshot := boundarySnapshot{refs: map[string]string{"refs/heads/main": "base"}}
	check := func(t *testing.T, obs []probe.Observation, after boundarySnapshot, want string) {
		t.Helper()
		err := boundaryVerdict(verdictReport(t, obs), rules, allowEnv, snapshot, after)
		if want == "" && err != nil || want != "" && (err == nil || !strings.Contains(err.Error(), want)) {
			t.Fatalf("want %q, got %v", want, err)
		}
	}
	t.Run("all_boundaries_denied", func(t *testing.T) { check(t, observations, snapshot, "") })
	for i, rule := range rules {
		t.Run(rule.id+"_leaks_or_positive_control_fails", func(t *testing.T) {
			obs := slices.Clone(observations)
			flipped := !rule.allowed
			obs[i].Succeeded = &flipped
			check(t, obs, snapshot, rule.id)
		})
		t.Run(rule.id+"_missing", func(t *testing.T) {
			obs := append(slices.Clone(observations[:i]), observations[i+1:]...)
			check(t, obs, snapshot, rule.id)
		})
	}
	for name, mutate := range map[string]func(*boundarySnapshot){
		"control-http":  func(s *boundarySnapshot) { s.http++ },
		"control-audit": func(s *boundarySnapshot) { s.audits++ },
		"github":        func(s *boundarySnapshot) { s.github++ },
		"ssh-agent":     func(s *boundarySnapshot) { s.agent++ },
		"git-push": func(s *boundarySnapshot) {
			s.refs = map[string]string{"refs/heads/main": "base", "refs/heads/escaped": "new"}
		},
	} {
		t.Run("lying_report_"+name, func(t *testing.T) {
			after := snapshot
			mutate(&after)
			check(t, observations, after, name)
		})
	}
	for name, detail := range map[string]any{
		"unknown_name_even_with_clean_summary": map[string]any{"names": []string{"HOME", "GITHUB_TOKEN"}, "unexpected": []string{}, "secretPresent": false},
		"known_secret":                         map[string]any{"names": []string{"HOME"}, "unexpected": []string{}, "secretPresent": true},
		"missing_detail":                       map[string]any{},
	} {
		t.Run(name, func(t *testing.T) {
			obs := slices.Clone(observations)
			obs[12].Detail = detail
			check(t, obs, snapshot, "environment")
		})
	}
	for _, data := range [][]byte{nil, []byte(`{}`), []byte(`{"observations":[]}`), append(verdictReport(t, observations), []byte(`{}`)...)} {
		if err := boundaryVerdict(data, rules, allowEnv, snapshot, snapshot); err == nil {
			t.Fatal("missing/malformed result passed")
		}
	}
	obs := slices.Clone(observations)
	obs[0].Kind = "read"
	check(t, obs, snapshot, rules[0].id)
	check(t, append(slices.Clone(observations), observations[0]), snapshot, "invalid")
}

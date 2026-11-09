package linear_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/linear"
)

// A state on a later page must remain selectable; a partial/invalid list must
// fail rather than appear to be an absent name.
func TestTeamStatesReadsAllPagesAndRejectsPartialLists(t *testing.T) {
	for _, mode := range []string{"ok", "second page fails", "repeated cursor", "missing cursor", "wrong team", "incomplete state"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				req := decode(t, r)
				calls++
				if req.Variables["id"] != "team-1" || r.Header.Get("Authorization") != keyCanary {
					t.Error("wrong Team or credentials")
				}
				if calls == 1 {
					switch mode {
					case "wrong team":
						fmt.Fprint(w, `{"data":{"team":{"id":"other","states":{"nodes":[],"pageInfo":{"hasNextPage":false}}}}}`)
					case "incomplete state":
						fmt.Fprint(w, `{"data":{"team":{"id":"team-1","states":{"nodes":[{"name":"PR Review","type":"started"}],"pageInfo":{"hasNextPage":false}}}}}`)
					case "missing cursor":
						fmt.Fprint(w, `{"data":{"team":{"id":"team-1","states":{"nodes":[],"pageInfo":{"hasNextPage":true}}}}}`)
					default:
						fmt.Fprint(w, `{"data":{"team":{"id":"team-1","states":{"nodes":[{"id":"progress","name":"In Progress","type":"started"}],"pageInfo":{"hasNextPage":true,"endCursor":"p2"}}}}}`)
					}
					return
				}
				if req.Variables["after"] != "p2" {
					t.Errorf("after=%v", req.Variables["after"])
				}
				switch mode {
				case "second page fails":
					w.WriteHeader(http.StatusServiceUnavailable)
				case "repeated cursor":
					fmt.Fprint(w, `{"data":{"team":{"id":"team-1","states":{"nodes":[],"pageInfo":{"hasNextPage":true,"endCursor":"p2"}}}}}`)
				default:
					fmt.Fprint(w, `{"data":{"team":{"id":"team-1","states":{"nodes":[{"id":"review","name":"PR Review","type":"started"}],"pageInfo":{"hasNextPage":false}}}}}`)
				}
			}))
			defer server.Close()
			client := newClient(t, server, writeKey(t, 0o600), &bytes.Buffer{})
			states, err := client.TeamStates(context.Background(), "team-1")
			if mode == "ok" {
				if err != nil || calls != 2 || len(states) != 2 || states[1] != (linear.WorkflowState{ID: "review", Name: "PR Review", Type: "started"}) {
					t.Fatalf("states=%+v calls=%d err=%v", states, calls, err)
				}
			} else if err == nil || states != nil {
				t.Fatalf("partial list returned: states=%+v err=%v", states, err)
			}
		})
	}
}

func TestIssueStateRejectsMissingOrIncompleteIssue(t *testing.T) {
	for _, raw := range []string{
		`{"data":{"issue":null}}`,
		`{"data":{"issue":{"id":"wrong","team":{"id":"team-1"},"state":{"id":"s","name":"Done","type":"completed"}}}}`,
		`{"data":{"issue":{"id":"issue-1","team":{"id":"team-1"},"state":{"name":"Done","type":"completed"}}}}`,
		`{"data":{"issue":{"id":"issue-1","state":{"id":"s","name":"Done","type":"completed"}}}}`,
	} {
		t.Run(raw, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, raw) }))
			defer server.Close()
			client := newClient(t, server, writeKey(t, 0o600), &bytes.Buffer{})
			state, err := client.IssueState(context.Background(), "issue-1")
			if err == nil || state != (linear.IssueState{}) {
				t.Fatalf("state=%+v err=%v", state, err)
			}
			if raw == `{"data":{"issue":null}}` && !errors.Is(err, linear.ErrIssueNotFound) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

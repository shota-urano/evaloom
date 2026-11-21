package linear_test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/linear"
)

func TestTeamLabelsReadsAllPagesAndRejectsPartialLists(t *testing.T) {
	for _, mode := range []string{"ok", "second page fails", "repeated cursor", "missing cursor", "wrong team", "incomplete label", "missing labels"} {
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
						fmt.Fprint(w, `{"data":{"team":{"id":"other","labels":{"nodes":[],"pageInfo":{"hasNextPage":false}}}}}`)
					case "incomplete label":
						fmt.Fprint(w, `{"data":{"team":{"id":"team-1","labels":{"nodes":[{"name":"route:target"}],"pageInfo":{"hasNextPage":false}}}}}`)
					case "missing labels":
						fmt.Fprint(w, `{"data":{"team":{"id":"team-1"}}}`)
					case "missing cursor":
						fmt.Fprint(w, `{"data":{"team":{"id":"team-1","labels":{"nodes":[],"pageInfo":{"hasNextPage":true}}}}}`)
					default:
						fmt.Fprint(w, `{"data":{"team":{"id":"team-1","labels":{"nodes":[{"id":"other","name":"bug"}],"pageInfo":{"hasNextPage":true,"endCursor":"p2"}}}}}`)
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
					fmt.Fprint(w, `{"data":{"team":{"id":"team-1","labels":{"nodes":[],"pageInfo":{"hasNextPage":true,"endCursor":"p2"}}}}}`)
				default:
					fmt.Fprint(w, `{"data":{"team":{"id":"team-1","labels":{"nodes":[{"id":"target","name":"route:target"}],"pageInfo":{"hasNextPage":false}}}}}`)
				}
			}))
			defer server.Close()
			client := newClient(t, server, writeKey(t, 0o600), &bytes.Buffer{})
			labels, err := client.TeamLabels(context.Background(), "team-1")
			if mode == "ok" {
				if err != nil || calls != 2 || len(labels) != 2 || labels[1] != (linear.Label{ID: "target", Name: "route:target"}) {
					t.Fatalf("labels=%+v calls=%d err=%v", labels, calls, err)
				}
			} else if err == nil || labels != nil {
				t.Fatalf("partial list returned: labels=%+v err=%v", labels, err)
			}
		})
	}
}

func TestAddIssueLabelRequiresSuccessfulMutationResponse(t *testing.T) {
	for _, body := range []string{
		`{"data":{"issueAddLabel":{"success":true}}}`,
		`{"data":{"issueAddLabel":{"success":false}}}`,
		`{"data":{"issueAddLabel":null}}`,
		`{"data":{}}`,
		`{"data":{"issueAddLabel":{"success":true}},"errors":[{}]}`,
	} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
			defer server.Close()
			client := newClient(t, server, writeKey(t, 0o600), &bytes.Buffer{})
			err := client.AddIssueLabel(context.Background(), "issue-1", "label-1")
			if (err == nil) != (body == `{"data":{"issueAddLabel":{"success":true}}}`) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

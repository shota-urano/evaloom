package linear_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/linear"
)

// The Team list is read to the last page through the Linear client (spec 15
// 3.2, 5章, Q216).
func TestListTeamsReadsAllPages(t *testing.T) {
	var afters []any
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != keyCanary {
			t.Errorf("Authorization header is not the key from the file")
		}
		req := decode(t, r)
		if !strings.Contains(req.Query, "teams(") {
			t.Errorf("not a teams query: %s", req.Query)
		}
		afters = append(afters, req.Variables["after"])
		switch req.Variables["after"] {
		case nil:
			fmt.Fprint(w, `{"data":{"teams":{"nodes":[{"id":"t1","key":"ENG","name":"Engineering"}],"pageInfo":{"hasNextPage":true,"endCursor":"c1"}}}}`)
		case "c1":
			fmt.Fprint(w, `{"data":{"teams":{"nodes":[{"id":"t2","key":"USLAB","name":"Lab"}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`)
		default:
			t.Errorf("unexpected cursor %v", req.Variables["after"])
		}
	}))
	defer server.Close()
	var logs bytes.Buffer
	teams, err := newClient(t, server, writeKey(t, 0o600), &logs).ListTeams(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(afters) != "[<nil> c1]" {
		t.Fatalf("pages = %v", afters)
	}
	want := []linear.Team{{ID: "t1", Key: "ENG", Name: "Engineering"}, {ID: "t2", Key: "USLAB", Name: "Lab"}}
	if fmt.Sprint(teams) != fmt.Sprint(want) {
		t.Fatalf("teams = %+v", teams)
	}
	assertNoKey(t, logs.String())
}

func TestListTeamsFailsWholeListWhenAPageFails(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if decode(t, r).Variables["after"] == nil {
			fmt.Fprint(w, `{"data":{"teams":{"nodes":[{"id":"t1","key":"USLAB","name":"x"}],"pageInfo":{"hasNextPage":true,"endCursor":"c1"}}}}`)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	teams, err := newClient(t, server, writeKey(t, 0o600), &bytes.Buffer{}).ListTeams(context.Background())
	if err == nil || teams != nil {
		t.Fatalf("teams=%v err=%v; want no list and an error", teams, err)
	}
}

func TestListTeamsDoesNotQueryWithUnusableKey(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("query sent with an unusable key file")
	}))
	defer server.Close()
	_, err := newClient(t, server, writeKey(t, 0o644), &bytes.Buffer{}).ListTeams(context.Background())
	var keyErr *linear.KeyError
	if !errors.As(err, &keyErr) || linear.ClassOf(err) != linear.ClassAuth {
		t.Fatalf("err = %v; want a key error classified auth", err)
	}
}

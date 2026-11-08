package linear

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Endpoint is Linear's GraphQL API. Production uses only this HTTPS URL.
const Endpoint = "https://api.linear.app/graphql"

// Read guards and page sizes are request shaping, not Evaloom settings.
const (
	issuePageSize    = 50
	labelPageSize    = 50
	responseMaxBytes = 16 << 20
)

// ErrorClass is the failure classification stored in
// linear_poll_teams.last_error (spec 15 3.8). It never contains the API key
// or response bodies.
type ErrorClass string

const (
	ClassAuth        ErrorClass = "auth"
	ClassRateLimited ErrorClass = "rate_limited"
	ClassNetwork     ErrorClass = "network"
	ClassOther       ErrorClass = "other"
)

// Error is a failed Linear request. It deliberately keeps no response body
// and no underlying transport error text (which could echo request data).
type Error struct {
	Op    string
	Class ErrorClass
	// ResetAt is the restart hint Linear returned for rate limiting (spec 15
	// 6章). Zero when Linear gave none.
	ResetAt time.Time
}

func (e *Error) Error() string { return "linear: " + e.Op + " failed: " + string(e.Class) }

// ErrIssueNotFound is returned by Issue when Linear returns no Issue for the ID.
var ErrIssueNotFound = errors.New("linear: issue not found")

// ClassOf maps an error from this package to the last_error classification.
// An unusable key file is classified as auth: no credential could be sent.
func ClassOf(err error) ErrorClass {
	var linearErr *Error
	var keyErr *KeyError
	switch {
	case err == nil:
		return ""
	case errors.As(err, &linearErr):
		return linearErr.Class
	case errors.As(err, &keyErr):
		return ClassAuth
	default:
		return ClassOther
	}
}

// Issue is the subset of a Linear Issue used by intake (spec 15 3.2).
type Issue struct {
	ID          string
	Identifier  string
	URL         string
	Title       string
	Description string
	TeamID      string
	Labels      []string
	// StateType is Linear's workflow state type (for example "canceled").
	StateType string
	UpdatedAt time.Time
	Archived  bool
	Trashed   bool
}

// Canceled reports whether the Issue is in a cancelled workflow state.
func (i Issue) Canceled() bool { return i.StateType == "canceled" }

// Client talks to the Linear GraphQL API with the key read from KeyPath.
type Client struct {
	http     *http.Client
	endpoint string
	keyPath  string
	logger   *slog.Logger
}

// NewClient builds a client. endpoint must be an https URL (tests use an
// httptest TLS server). Redirects are refused so the key is never forwarded.
func NewClient(httpClient *http.Client, endpoint, keyPath string, logger *slog.Logger) (*Client, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || keyPath == "" {
		return nil, errors.New("linear: invalid client configuration")
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	copy := *httpClient
	copy.Jar = nil
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if logger == nil {
		logger = slog.Default()
	}
	return &Client{http: &copy, endpoint: endpoint, keyPath: keyPath, logger: logger}, nil
}

const issueFields = `id identifier url title description updatedAt archivedAt trashed
team { id }
state { type }
labels(first: ` + "%LABELS%" + `) { nodes { name } pageInfo { hasNextPage } }`

var listQuery = strings.ReplaceAll(`query EvaloomReadyIssues($teamId: ID!, $label: String!, $first: Int!, $after: String) {
  issues(first: $first, after: $after, includeArchived: false,
    filter: { team: { id: { eq: $teamId } }, labels: { some: { name: { eq: $label } } } }) {
    nodes { `+issueFields+` }
    pageInfo { hasNextPage endCursor }
  }
}`, "%LABELS%", strconv.Itoa(labelPageSize))

var issueQuery = strings.ReplaceAll(`query EvaloomIssue($id: String!) {
  issue(id: $id) { `+issueFields+` }
}`, "%LABELS%", strconv.Itoa(labelPageSize))

type issueNode struct {
	ID          string  `json:"id"`
	Identifier  string  `json:"identifier"`
	URL         string  `json:"url"`
	Title       string  `json:"title"`
	Description *string `json:"description"`
	UpdatedAt   string  `json:"updatedAt"`
	ArchivedAt  *string `json:"archivedAt"`
	Trashed     *bool   `json:"trashed"`
	Team        *struct {
		ID string `json:"id"`
	} `json:"team"`
	State *struct {
		Type string `json:"type"`
	} `json:"state"`
	Labels struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
		PageInfo struct {
			HasNextPage bool `json:"hasNextPage"`
		} `json:"pageInfo"`
	} `json:"labels"`
}

func (n issueNode) issue() (Issue, bool) {
	updated, err := time.Parse(time.RFC3339Nano, n.UpdatedAt)
	if err != nil || n.ID == "" || n.Identifier == "" || n.Team == nil || n.Team.ID == "" || n.State == nil || n.Labels.PageInfo.HasNextPage {
		// An incomplete label list could hide or invent ai-ready / repo:
		// labels, so it is treated as a failed read rather than guessed.
		return Issue{}, false
	}
	issue := Issue{ID: n.ID, Identifier: n.Identifier, URL: n.URL, Title: n.Title, TeamID: n.Team.ID,
		StateType: n.State.Type, UpdatedAt: updated, Archived: n.ArchivedAt != nil, Trashed: n.Trashed != nil && *n.Trashed,
		Labels: make([]string, 0, len(n.Labels.Nodes))}
	if n.Description != nil {
		issue.Description = *n.Description
	}
	for _, label := range n.Labels.Nodes {
		issue.Labels = append(issue.Labels, label.Name)
	}
	return issue, true
}

// ListReadyIssues returns every non-archived Issue of teamID that carries
// label, reading all pages (spec 15 3.2). If any page fails, no list is
// returned: a partial list must not be used for the attach/detach diff.
func (c *Client) ListReadyIssues(ctx context.Context, teamID, label string) ([]Issue, error) {
	const op = "list issues"
	if teamID == "" || label == "" {
		return nil, &Error{Op: op, Class: ClassOther}
	}
	key, err := c.key()
	if err != nil {
		return nil, err
	}
	var all []Issue
	var after *string
	seen := map[string]bool{}
	for {
		var data struct {
			Issues *struct {
				Nodes    []issueNode `json:"nodes"`
				PageInfo struct {
					HasNextPage bool    `json:"hasNextPage"`
					EndCursor   *string `json:"endCursor"`
				} `json:"pageInfo"`
			} `json:"issues"`
		}
		vars := map[string]any{"teamId": teamID, "label": label, "first": issuePageSize, "after": after}
		if err := c.do(ctx, op, key, listQuery, vars, &data); err != nil {
			return nil, err
		}
		if data.Issues == nil {
			return nil, &Error{Op: op, Class: ClassOther}
		}
		for _, node := range data.Issues.Nodes {
			issue, ok := node.issue()
			if !ok || issue.TeamID != teamID {
				return nil, &Error{Op: op, Class: ClassOther}
			}
			all = append(all, issue)
		}
		if !data.Issues.PageInfo.HasNextPage {
			return all, nil
		}
		cursor := data.Issues.PageInfo.EndCursor
		// A missing or repeated cursor would loop forever or skip pages.
		if cursor == nil || *cursor == "" || seen[*cursor] {
			return nil, &Error{Op: op, Class: ClassOther}
		}
		seen[*cursor] = true
		after = cursor
	}
}

// Issue fetches one Issue by ID, including archived and trashed ones, so the
// caller can tell why it left the list (spec 15 3.4).
func (c *Client) Issue(ctx context.Context, id string) (Issue, error) {
	const op = "get issue"
	if id == "" {
		return Issue{}, &Error{Op: op, Class: ClassOther}
	}
	key, err := c.key()
	if err != nil {
		return Issue{}, err
	}
	var data struct {
		Issue *issueNode `json:"issue"`
	}
	if err := c.do(ctx, op, key, issueQuery, map[string]any{"id": id}, &data); err != nil {
		return Issue{}, err
	}
	if data.Issue == nil {
		return Issue{}, ErrIssueNotFound
	}
	issue, ok := data.Issue.issue()
	if !ok || issue.ID != id {
		return Issue{}, &Error{Op: op, Class: ClassOther}
	}
	return issue, nil
}

// CheckKey reports whether the key file is usable without sending anything.
// The Poller calls it before a round so that no query is started when the
// file is missing or not 0600 (spec 15 6章). The value is discarded and only
// the reason is logged.
func (c *Client) CheckKey() error {
	_, err := c.key()
	return err
}

// key reads the key file before every query. When the file is missing or
// not 0600 the query is not started and the reason (never the value) is logged.
func (c *Client) key() (string, error) {
	key, err := ReadAPIKey(c.keyPath)
	if err != nil {
		var keyErr *KeyError
		problem := KeyUnreadable
		if errors.As(err, &keyErr) {
			problem = keyErr.Problem
		}
		c.logger.Warn("linear: query not started: API key file unusable", "reason", string(problem), "path", c.keyPath)
		return "", err
	}
	return key, nil
}

type graphQLError struct {
	Extensions struct {
		Code string `json:"code"`
	} `json:"extensions"`
}

func (c *Client) do(ctx context.Context, op, key, query string, vars map[string]any, out any) error {
	fail := func(class ErrorClass) error { return &Error{Op: op, Class: class} }
	body, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return fail(ClassOther)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return fail(ClassOther)
	}
	// Linear personal API keys are sent as the bare Authorization value.
	req.Header.Set("Authorization", key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fail(ClassNetwork)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, responseMaxBytes+1))
	if err != nil {
		return fail(ClassNetwork)
	}
	if len(raw) > responseMaxBytes {
		return fail(ClassOther)
	}
	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []graphQLError  `json:"errors"`
	}
	decodeErr := json.Unmarshal(raw, &envelope)
	class := ClassOther
	for _, e := range envelope.Errors {
		switch e.Extensions.Code {
		case "RATELIMITED":
			class = ClassRateLimited
		case "AUTHENTICATION_ERROR", "FORBIDDEN":
			if class != ClassRateLimited {
				class = ClassAuth
			}
		}
	}
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		class = ClassRateLimited
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		if class != ClassRateLimited {
			class = ClassAuth
		}
	}
	if class == ClassRateLimited {
		return &Error{Op: op, Class: class, ResetAt: resetAt(resp.Header, time.Now())}
	}
	if resp.StatusCode != http.StatusOK || decodeErr != nil || len(envelope.Errors) > 0 || len(envelope.Data) == 0 {
		return fail(class)
	}
	if err := json.Unmarshal(envelope.Data, out); err != nil {
		return fail(ClassOther)
	}
	return nil
}

// resetAt reads Linear's rate-limit reset headers (UTC epoch milliseconds)
// and the standard Retry-After (seconds), returning the latest hint.
func resetAt(h http.Header, now time.Time) time.Time {
	var latest time.Time
	for _, name := range []string{"X-RateLimit-Requests-Reset", "X-RateLimit-Complexity-Reset", "X-RateLimit-Endpoint-Requests-Reset"} {
		if ms, err := strconv.ParseInt(strings.TrimSpace(h.Get(name)), 10, 64); err == nil && ms > 0 {
			if t := time.UnixMilli(ms); t.After(latest) {
				latest = t
			}
		}
	}
	if seconds, err := strconv.ParseInt(strings.TrimSpace(h.Get("Retry-After")), 10, 64); err == nil && seconds >= 0 && seconds <= 1<<31 {
		if t := now.Add(time.Duration(seconds) * time.Second); t.After(latest) {
			latest = t
		}
	}
	return latest
}

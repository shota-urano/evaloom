package publisher

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// GitHub implements only PR lookup, creation and issue comments using REST.
// It has no merge/close operation. The installation token provider is supplied
// by Trusted Runner authentication (spec 11, Q29), never read from a worktree.
type GitHub struct {
	client *http.Client
	token  func(context.Context) (string, error)
}

var _ PullRequests = (*GitHub)(nil)
var _ PreviousPullRequest = (*GitHub)(nil)

func NewGitHub(client *http.Client, installationToken func(context.Context) (string, error)) (*GitHub, error) {
	if client == nil || installationToken == nil {
		return nil, ErrPRConfiguration
	}
	copy := *client
	// Do not forward an installation token to an API redirect target, including
	// redirects to another path on the same host. The Project must be corrected.
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &GitHub{client: &copy, token: installationToken}, nil
}

type githubPR struct {
	Number int    `json:"number"`
	URL    string `json:"html_url"`
	State  string `json:"state"`
	Merged *bool  `json:"merged"`
	Head   struct {
		Ref  string `json:"ref"`
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

// State reads the recorded PR by number, including closed/merged PRs. A 404
// cannot establish absence: it may also mean the App lost repository access.
func (g *GitHub) State(ctx context.Context, repositoryURL string, number int) (PullRequestState, error) {
	repo, err := githubRepository(repositoryURL)
	if err != nil {
		return "", err
	}
	if number <= 0 {
		return "", ErrRerunConfiguration
	}
	var pr githubPR
	if err := g.request(ctx, http.MethodGet, "/repos/"+repo+"/pulls/"+strconv.Itoa(number), nil, http.StatusOK, &pr); err != nil {
		return "", err
	}
	if pr.Number != number || pr.Merged == nil ||
		!strings.EqualFold(pr.URL, "https://github.com/"+repo+"/pull/"+strconv.Itoa(number)) {
		return "", ErrPullRequest
	}
	switch pr.State {
	case "open":
		if !*pr.Merged {
			return PROpen, nil
		}
	case "closed":
		if *pr.Merged {
			return PRMerged, nil
		}
		return PRClosed, nil
	}
	return "", ErrPullRequest
}

func (pr githubPR) valid(repo, branch string) bool {
	return pr.Number > 0 && pr.State == "open" && pr.Head.Ref == branch &&
		strings.EqualFold(pr.Head.Repo.FullName, repo) &&
		strings.EqualFold(pr.URL, "https://github.com/"+repo+"/pull/"+strconv.Itoa(pr.Number))
}

func (g *GitHub) FindOpen(ctx context.Context, repositoryURL, branch string) (*PullRequest, error) {
	repo, err := githubRepository(repositoryURL)
	if err != nil {
		return nil, err
	}
	owner, _, _ := strings.Cut(repo, "/")
	query := url.Values{"state": {"open"}, "head": {owner + ":" + branch}, "per_page": {"2"}}
	var prs []githubPR
	if err := g.request(ctx, http.MethodGet, "/repos/"+repo+"/pulls?"+query.Encode(), nil, http.StatusOK, &prs); err != nil {
		return nil, err
	}
	if prs == nil {
		return nil, ErrPullRequest
	}
	if len(prs) > 1 {
		return nil, ErrMultiplePRs
	}
	if len(prs) == 0 {
		return nil, nil
	}
	pr := prs[0]
	if !pr.valid(repo, branch) {
		return nil, ErrPullRequest
	}
	return &PullRequest{Number: pr.Number, URL: pr.URL}, nil
}

func (g *GitHub) Create(ctx context.Context, repositoryURL, head, base, title, body string) (PullRequest, error) {
	repo, err := githubRepository(repositoryURL)
	if err != nil {
		return PullRequest{}, err
	}
	input := struct {
		Head  string `json:"head"`
		Base  string `json:"base"`
		Title string `json:"title"`
		Body  string `json:"body"`
	}{head, base, title, body}
	var pr githubPR
	if err := g.request(ctx, http.MethodPost, "/repos/"+repo+"/pulls", input, http.StatusCreated, &pr); err != nil {
		return PullRequest{}, err
	}
	if !pr.valid(repo, head) || pr.Base.Ref != base {
		return PullRequest{}, ErrPullRequest
	}
	return PullRequest{Number: pr.Number, URL: pr.URL}, nil
}

func (g *GitHub) Comment(ctx context.Context, repositoryURL string, number int, body string) error {
	repo, err := githubRepository(repositoryURL)
	if err != nil {
		return err
	}
	if number <= 0 {
		return ErrPullRequest
	}
	// General PR comments use the issue comments endpoint, not review comments.
	input := struct {
		Body string `json:"body"`
	}{body}
	return g.request(ctx, http.MethodPost, "/repos/"+repo+"/issues/"+strconv.Itoa(number)+"/comments", input, http.StatusCreated, nil)
}

func (g *GitHub) request(ctx context.Context, method, path string, input any, status int, output any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if g == nil || g.client == nil || g.token == nil {
		return ErrPRConfiguration
	}
	var body io.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			return ErrPullRequest
		}
		body = bytes.NewReader(data)
	}
	token, err := g.token(ctx)
	if err != nil || strings.TrimSpace(token) == "" || strings.ContainsAny(token, "\r\n") {
		return prError(ctx, ErrPullRequest)
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://api.github.com"+path, body)
	if err != nil {
		return ErrPullRequest
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := g.client.Do(req)
	if err != nil {
		return prError(ctx, err)
	}
	defer response.Body.Close()
	if response.StatusCode != status {
		return ErrPullRequest
	}
	if output != nil {
		decoder := json.NewDecoder(response.Body)
		if err := decoder.Decode(output); err != nil {
			return ErrPullRequest
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			return ErrPullRequest
		}
	}
	return nil
}

var repositoryPart = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

func githubRepository(repositoryURL string) (string, error) {
	if strings.HasPrefix(repositoryURL, "git@github.com:") {
		repositoryURL = "ssh://git@github.com/" + strings.TrimPrefix(repositoryURL, "git@github.com:")
	}
	u, err := url.Parse(repositoryURL)
	if err != nil || u.Host != "github.com" || (u.Scheme != "https" && u.Scheme != "ssh") ||
		u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" {
		return "", ErrPRConfiguration
	}
	if u.User != nil && (u.Scheme != "ssh" || u.User.String() != "git") {
		return "", ErrPRConfiguration
	}
	repo := strings.TrimSuffix(strings.TrimPrefix(u.Path, "/"), ".git")
	parts := strings.Split(repo, "/")
	if len(parts) != 2 {
		return "", ErrPRConfiguration
	}
	for _, part := range parts {
		if !repositoryPart.MatchString(part) || part == "." || part == ".." {
			return "", ErrPRConfiguration
		}
	}
	return repo, nil
}

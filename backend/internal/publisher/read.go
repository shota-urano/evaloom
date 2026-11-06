package publisher

import (
	"context"
	"encoding/base64"
	"net/http"

	"github.com/shota-urano/evaloom/backend/internal/safegit"
	"github.com/shota-urano/evaloom/backend/internal/worktree"
)

// Reader issues read-only credentials independently of Publisher's write
// credentials. Construct once per Project; acquire a new session per stage.
type Reader struct{ app *GitHub }

func NewGitHubAppReader(client *http.Client, repositoryURL string, readPrivateKey func(context.Context) ([]byte, error)) (*Reader, error) {
	app, err := newGitHubApp(client, repositoryURL, readPrivateKey, true)
	if err != nil {
		return nil, err
	}
	return &Reader{app: app}, nil
}

// ReadSession retains one installation token in memory for one stage. It has
// no push, PR creation or comment operation. Do not retain it on Execution or
// pass it into Sandbox configuration. Token values reach only exclude (the
// Runner's in-memory log exclusion list) and credential-bearing Git/REST calls.
type ReadSession struct {
	app   *GitHub
	token string
}

func (r *Reader) ForStage(ctx context.Context, exclude func(...string)) (*ReadSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r == nil || r.app == nil || exclude == nil {
		return nil, ErrPRConfiguration
	}
	token, err := r.app.token(ctx)
	if err != nil {
		return nil, err
	}
	exclude(token, base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token)))
	// REST lookups within this stage reuse this read token, rather than issuing
	// a token per request or accidentally acquiring Publisher's write token.
	app := *r.app
	app.token = func(context.Context) (string, error) { return token, nil }
	return &ReadSession{app: &app, token: token}, nil
}

// PrepareGit adapts ForStage to the Runner's stage-3 credential boundary.
func (r *Reader) PrepareGit(ctx context.Context, exclude func(...string)) (worktree.ReadTransport, error) {
	return r.ForStage(ctx, exclude)
}

var _ worktree.ReadTransport = (*ReadSession)(nil)
var _ PreviousPullRequest = (*ReadSession)(nil)

func (s *ReadSession) State(ctx context.Context, repositoryURL string, number int) (PullRequestState, error) {
	if s == nil || s.app == nil {
		return "", ErrPRConfiguration
	}
	return s.app.State(ctx, repositoryURL, number)
}

// Output accepts only the exact clone/fetch/ls-remote forms used by worktree.
// An SSH Project URL is canonicalized for this invocation only. Clone's origin
// keeps the registered URL, without ever storing authentication in Git config.
func (s *ReadSession) Output(ctx context.Context, directory string, args ...string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s == nil || s.app == nil || len(args) == 0 {
		return nil, safegit.ErrHTTPS
	}
	index := 4
	if args[0] == "clone" {
		index = 6
	}
	if len(args) <= index {
		return nil, safegit.ErrHTTPS
	}
	original := args[index]
	repo, err := s.app.repository(original)
	if err != nil {
		return nil, safegit.ErrHTTPS
	}
	canonical := "https://github.com/" + repo + ".git"
	command := append([]string(nil), args...)
	command[index] = canonical
	out, err := safegit.HTTPSReadOutput(ctx, directory, canonical, s.token, command...)
	if err != nil {
		return nil, err
	}
	if args[0] == "clone" && original != canonical {
		if _, err := safegit.Output(ctx, args[7], "remote", "set-url", "origin", original); err != nil {
			return nil, safegit.ErrHTTPS
		}
	}
	return out, nil
}

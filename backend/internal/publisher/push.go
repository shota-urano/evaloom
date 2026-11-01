package publisher

import (
	"context"
	"errors"
	"net/url"
	"strings"

	"github.com/shota-urano/evaloom/backend/internal/safegit"
)

var (
	ErrPush              = errors.New("publisher: push failed")
	ErrRemoteSHA         = errors.New("publisher: remote branch is not the verified commit")
	ErrRemoteInspection  = errors.New("publisher: remote branch inspection failed")
	ErrPushConfiguration = errors.New("publisher: push configuration is required")
	ErrPushDestination   = errors.New("publisher: push destination overrides are not allowed")
	ErrAudit             = errors.New("publisher: remote SHA mismatch audit failed")
)

// PushConfig contains trusted inputs, independent of worktree configuration.
type PushConfig struct {
	// RepositoryURL is the Project URL, never a sandbox-controlled remote name.
	RepositoryURL     string
	CheckCancellation func(context.Context) error
	// Git owns network authentication. GitHub implements this boundary without
	// placing credentials in the worktree. Nil is for anonymous test remotes.
	Git GitTransport
	// RecordRemoteMismatch must durably record the mismatch in audit_logs
	// (spec 11, section 6). The caller supplies Execution/actor identity and
	// bounds delivery using its own timeout. The supplied context retains values
	// but survives push cancellation. observedSHA is empty if the
	// remote branch disappeared. Errors from the recorder are never exposed.
	// The Runner audit delivery interface is not specified; this local boundary
	// deliberately adds no HTTP endpoint or result field.
	RecordRemoteMismatch func(ctx context.Context, verifiedSHA, observedSHA string) error
}

// GitTransport is an internal, trusted network boundary, not a public API.
// Errors and output can contain remote diagnostics; Push never reports them.
type GitTransport interface {
	// PrepareGit acquires credentials before the final cancellation/HEAD check.
	PrepareGit(ctx context.Context, repositoryURL string) (GitCommand, error)
}

type GitCommand func(ctx context.Context, worktree, operation, ref string) ([]byte, error)

// Push performs the pre-push checks, pushes the explicit verified SHA without
// force, then reads the remote branch back. Sandbox writers must have stopped.
// Only a matching remote returns a nonempty pushedSHA. The caller must record
// it through the existing result API before creating a PR, and report failures
// using ErrorClass. This function never creates a PR or retries a push.
func Push(ctx context.Context, target Target, config PushConfig) (pushedSHA string, err error) {
	if config.RepositoryURL == "" || config.CheckCancellation == nil || config.RecordRemoteMismatch == nil {
		return "", ErrPushConfiguration
	}
	// GitHub access must use the Publisher's token, never ambient host helpers,
	// SSH agents or a token embedded in the Project URL.
	if config.Git == nil {
		u, err := url.Parse(config.RepositoryURL)
		if err != nil || u.Scheme != "git" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return "", ErrPushConfiguration
		}
	}
	if err := safegit.ValidatePushURL(ctx, target.Worktree, config.RepositoryURL); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", ErrPushDestination
	}
	var network GitCommand
	if config.Git != nil {
		network, err = config.Git.PrepareGit(ctx, config.RepositoryURL)
		if err != nil || network == nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			return "", ErrPush
		}
	}
	if err := CheckBeforePush(ctx, target, config.CheckCancellation); err != nil {
		return "", err
	}
	ref := "refs/heads/" + target.BranchName
	// Explicit refspec, destination and options override push defaults. Never
	// follow tags, mirror other refs or prune branches from worktree settings.
	var pushErr error
	if network != nil {
		_, pushErr = network(ctx, target.Worktree, "push", target.VerifiedSHA+":"+ref)
	} else {
		_, pushErr = gitOutput(ctx, target.Worktree, "push", "--no-force", "--no-mirror", "--no-follow-tags", "--no-prune", "--", config.RepositoryURL, target.VerifiedSHA+":"+ref)
	}
	if pushErr != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", ErrPush
	}
	var out []byte
	if network != nil {
		out, err = network(ctx, target.Worktree, "ls-remote", ref)
	} else {
		out, err = gitOutput(ctx, target.Worktree, "ls-remote", "--refs", "--", config.RepositoryURL, ref)
	}
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", ErrRemoteInspection
	}
	observed := ""
	if len(out) != 0 {
		fields := strings.Fields(string(out))
		if len(fields) != 2 || !objectID.MatchString(fields[0]) || fields[1] != ref {
			return "", ErrRemoteInspection
		}
		observed = fields[0]
	}
	if observed != target.VerifiedSHA {
		if err := config.RecordRemoteMismatch(context.WithoutCancel(ctx), target.VerifiedSHA, observed); err != nil {
			return "", errors.Join(ErrRemoteSHA, ErrAudit)
		}
		return "", ErrRemoteSHA
	}
	return observed, nil
}

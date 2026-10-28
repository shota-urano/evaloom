// Package securitytest contains disposable tools for spec 14's central tests.
// Production binaries must not import it. Host Git below only operates on
// generated, trusted fixtures; it is not spec 09's hardened Git wrapper.
package securitytest

import (
	"context"
	"embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/adapter/probe"
)

//go:embed assets/*
var assets embed.FS

type Fixture struct {
	Repository, Worktree, Remote, Home, BaseSHA string
	gitEnv                                      []string
}

// NewCanary returns a unique marker for disposable instruction/config files.
func NewCanary() string { return "evaloom-canary-" + ulid.Make().String() }

// NewFixture requires a fresh directory below root and never reuses a repo.
// All commits, refs, HOME and credentials belong to this disposable fixture.
func NewFixture(ctx context.Context, root string) (*Fixture, error) {
	root = filepath.Join(root, "fixture")
	if err := os.Mkdir(root, 0700); err != nil {
		return nil, err
	}
	f := &Fixture{Repository: filepath.Join(root, "repository"), Worktree: filepath.Join(root, "worktree"),
		Remote: filepath.Join(root, "remote.git"), Home: filepath.Join(root, "home")}
	for _, path := range []string{f.Repository, f.Home} {
		if err := os.Mkdir(path, 0700); err != nil {
			return nil, err
		}
	}
	f.gitEnv = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + f.Home,
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=Evaloom Test", "GIT_AUTHOR_EMAIL=evaloom-test@example.invalid",
		"GIT_COMMITTER_NAME=Evaloom Test", "GIT_COMMITTER_EMAIL=evaloom-test@example.invalid"}
	entries, err := assets.ReadDir("assets")
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		data, err := assets.ReadFile("assets/" + entry.Name())
		if err != nil {
			return nil, err
		}
		name := entry.Name()
		if name == "gitignore" {
			name = ".gitignore"
		}
		if err := os.WriteFile(filepath.Join(f.Repository, name), data, 0600); err != nil {
			return nil, err
		}
	}
	for _, stage := range []string{"setup", "test", "harness"} {
		plan := `[{"id":"fixture-file","kind":"exists","path":"app.py"}]`
		if err := os.WriteFile(filepath.Join(f.Repository, stage+"-plan.json"), []byte(plan), 0600); err != nil {
			return nil, err
		}
	}
	for _, args := range [][]string{
		{"init", "--initial-branch=main", f.Repository},
		{"-C", f.Repository, "add", "."},
		{"-C", f.Repository, "commit", "-m", "Initial fixture"},
		{"init", "--bare", "--initial-branch=main", f.Remote},
		{"-C", f.Repository, "remote", "add", "origin", f.Remote},
		{"-C", f.Repository, "push", "origin", "main"},
		{"-C", f.Repository, "worktree", "add", "-b", "evaloom/probe", f.Worktree, "main"},
	} {
		if _, err := f.git(ctx, args...); err != nil {
			return nil, err
		}
	}
	f.BaseSHA, err = f.Git(ctx, "rev-parse", "HEAD")
	return f, err
}

func (f *Fixture) git(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append([]string{}, f.gitEnv...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("fixture git %v: %w: %s", args, err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

func (f *Fixture) Git(ctx context.Context, args ...string) (string, error) {
	return f.git(ctx, append([]string{"-C", f.Worktree}, args...)...)
}

// Refs is a trusted observation independent of harness-written JSON reports.
func (f *Fixture) Refs(ctx context.Context) (map[string]string, error) {
	out, err := f.git(ctx, "--git-dir="+f.Remote, "for-each-ref", "--format=%(refname) %(objectname)")
	if err != nil {
		return nil, err
	}
	refs := make(map[string]string)
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 {
			refs[fields[0]] = fields[1]
		}
	}
	return refs, nil
}

// Prerequisites deliberately contains checks, not guessed CLI auth variables or
// a selected isolation technology. The future environment provider owns those.
type Prerequisites struct {
	Sandbox          interface{ Check(context.Context) error }
	RequireModel     bool
	ModelCredentials func(context.Context) error
}

func (p Prerequisites) Check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.Sandbox == nil {
		return probe.ErrUnavailable
	}
	if err := p.Sandbox.Check(ctx); err != nil {
		return fmt.Errorf("実行不能: Sandbox: %w", err)
	}
	if p.RequireModel {
		if p.ModelCredentials == nil {
			return fmt.Errorf("実行不能: モデル API の認証情報が必要")
		}
		if err := p.ModelCredentials(ctx); err != nil {
			return fmt.Errorf("実行不能: モデル API の認証情報: %w", err)
		}
	}
	return nil
}

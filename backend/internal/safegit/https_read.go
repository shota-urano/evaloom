package safegit

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// HTTPSReadOutput implements spec 11, 3.6's child-only authentication for the
// worktree transport. It accepts only worktree's fixed read command forms.
// No token-bearing process reads an existing clone's config, attributes, refs
// or objects. Sandbox writers must remain stopped for the entire operation.
func HTTPSReadOutput(ctx context.Context, directory, repositoryURL, token string, args ...string) ([]byte, error) {
	fail := func() ([]byte, error) {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrHTTPS
	}
	u, err := url.Parse(repositoryURL)
	if ctx.Err() != nil || err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" ||
		strings.TrimSpace(token) == "" || strings.ContainsAny(token, "\x00\r\n") || len(args) == 0 {
		return fail()
	}
	// Keeping the accepted shapes closed prevents caller-supplied Git options
	// (helpers, recursion, alternate URLs, upload-pack, templates) from running
	// with a token. Values are validated separately below.
	switch args[0] {
	case "clone":
		if len(args) != 8 || !slices.Equal(args[:7], []string{"clone", "--no-checkout", "--no-local", "--origin", "origin", "--", repositoryURL}) ||
			!filepath.IsAbs(args[7]) {
			return fail()
		}
		if _, err := os.Lstat(args[7]); !os.IsNotExist(err) {
			return fail()
		}
	case "fetch":
		if len(args) != 6 || !slices.Equal(args[:5], []string{"fetch", "--no-tags", "--refmap=", "--", repositoryURL}) {
			return fail()
		}
	case "ls-remote":
		if len(args) != 6 || !slices.Equal(args[:5], []string{"ls-remote", "--heads", "--refs", "--", repositoryURL}) {
			return fail()
		}
	default:
		return fail()
	}
	dir, err := os.MkdirTemp("", "evaloom-read-git-")
	if err != nil {
		return fail()
	}
	defer os.RemoveAll(dir)
	format := "sha1"
	if args[0] == "fetch" {
		out, err := Output(ctx, directory, "rev-parse", "--show-object-format=storage")
		if err != nil || (string(out) != "sha1\n" && string(out) != "sha256\n") {
			return fail()
		}
		format = strings.TrimSpace(string(out))
	}
	if _, err := Output(ctx, dir, "init", "--bare", "--object-format="+format, dir); err != nil {
		return fail()
	}
	env := httpsEnvironment(dir, "", repositoryURL, token)
	base := []string{"--no-pager", "--no-replace-objects", "-C", dir}
	for _, setting := range fixedSettings(filepath.Join(dir, "hooks-disabled")) {
		base = append(base, "-c", setting)
	}
	base = append(base, "-c", "http.followRedirects=false", "-c", "http.sslVerify=true", "-c", "http.proxy=")
	switch args[0] {
	case "clone":
		command := []string{"clone", "--upload-pack=git-upload-pack", "--no-recurse-submodules", "--template=", "--no-checkout", "--no-local", "--origin", "origin", "--", repositoryURL, args[7]}
		if _, err := output(ctx, env, append(base, command...)...); err != nil {
			return fail()
		}
		return nil, nil
	case "ls-remote":
		if !strings.HasPrefix(args[5], "refs/heads/") {
			return fail()
		}
		if _, err := Output(ctx, dir, "check-ref-format", args[5]); err != nil {
			return fail()
		}
		out, err := output(ctx, env, append(base, "ls-remote", "--upload-pack=git-upload-pack", "--heads", "--refs", "--", repositoryURL, args[5])...)
		if err != nil {
			return fail()
		}
		return out, nil
	case "fetch":
		source, target, ok := strings.Cut(strings.TrimPrefix(args[5], "+"), ":")
		if !ok || !strings.HasPrefix(source, "refs/heads/") || !strings.HasPrefix(target, "refs/remotes/origin/") {
			return fail()
		}
		for _, ref := range []string{source, target} {
			if _, err := Output(ctx, dir, "check-ref-format", "--refspec-pattern", ref); err != nil {
				return fail()
			}
		}
		if err := ValidateFetchURL(ctx, directory, dir); err != nil {
			return fail()
		}
		command := []string{"fetch", "--upload-pack=git-upload-pack", "--no-recurse-submodules", "--no-tags", "--refmap=", "--", repositoryURL, args[5]}
		if _, err := output(ctx, env, append(base, command...)...); err != nil {
			return fail()
		}
		// The second fetch has no credentials and imports only the requested
		// tracking refs. Existing local branches, HEAD and index are untouched.
		if _, err := Output(ctx, directory, "fetch", "--no-tags", "--refmap=", "--", dir, "+"+target+":"+target); err != nil {
			return fail()
		}
		return nil, nil
	}
	return fail()
}

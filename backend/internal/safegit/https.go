package safegit

import (
	"context"
	"encoding/base64"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

var ErrHTTPS = errors.New("safegit: authenticated HTTPS operation failed")

// HTTPSOutput runs only an explicit push refspec or ls-remote ref against a
// trusted HTTPS URL. The installation token is passed in a child-only config
// environment, never argv, a URL, a file, or the parent process environment.
//
// Network Git uses a fresh private bare repository and reads only the original
// object directory. It never reads sandbox-controlled HTTP/proxy/cookie/TLS,
// credential or include settings while holding a credential. Sandbox writers
// must remain stopped. The executable search path belongs to trusted code.
func HTTPSOutput(ctx context.Context, repo, repositoryURL, token, operation, ref string) ([]byte, error) {
	fail := func() ([]byte, error) {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrHTTPS
	}
	u, err := url.Parse(repositoryURL)
	if ctx.Err() != nil || err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil ||
		u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" ||
		strings.TrimSpace(token) == "" || strings.ContainsAny(token, "\x00\r\n") ||
		(operation != "push" && operation != "ls-remote") || ref == "" || strings.HasPrefix(ref, "-") {
		return fail()
	}
	objects, err := Output(ctx, repo, "rev-parse", "--path-format=absolute", "--git-path", "objects")
	if err != nil {
		return fail()
	}
	format, err := Output(ctx, repo, "rev-parse", "--show-object-format=storage")
	if err != nil || (string(format) != "sha1\n" && string(format) != "sha256\n") {
		return fail()
	}
	// No secrets are written here, even temporarily. No Sandbox mount may
	// include the host temporary root (spec 09).
	dir, err := os.MkdirTemp("", "evaloom-publisher-git-")
	if err != nil {
		return fail()
	}
	defer os.RemoveAll(dir)
	if _, err := Output(ctx, dir, "init", "--bare", "--object-format="+strings.TrimSpace(string(format)), dir); err != nil {
		return fail()
	}
	env := httpsEnvironment(dir, strings.TrimSuffix(string(objects), "\n"), repositoryURL, token)
	args := []string{"--no-pager", "--no-replace-objects", "--git-dir=" + dir}
	for _, setting := range fixedSettings(filepath.Join(dir, "hooks-disabled")) {
		args = append(args, "-c", setting)
	}
	args = append(args, "-c", "http.followRedirects=false", "-c", "http.sslVerify=true", "-c", "http.proxy=")
	if operation == "push" {
		args = append(args, "push", "--receive-pack=git-receive-pack", "--recurse-submodules=no", "--no-force", "--no-mirror", "--no-follow-tags", "--no-prune", "--", repositoryURL, ref)
	} else {
		args = append(args, "ls-remote", "--upload-pack=git-upload-pack", "--refs", "--", repositoryURL, ref)
	}
	out, err := output(ctx, env, args...)
	if err != nil {
		// Neither ExitError.Stderr nor output may escape on failure.
		return fail()
	}
	if operation == "push" {
		return nil, nil
	}
	return out, nil
}

func httpsEnvironment(dir, objects, repositoryURL, token string) []string {
	// In particular do not inherit trace/curl logging, proxies, SSH agents,
	// GITHUB_TOKEN, GIT_CONFIG_*, client certificates, or the host's .netrc.
	return []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "XDG_CONFIG_HOME=" + dir,
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0", "GIT_ALLOW_PROTOCOL=https",
		"GIT_ASKPASS=/usr/bin/false", "SSH_ASKPASS=/usr/bin/false", "GIT_NO_LAZY_FETCH=1",
		"GIT_OBJECT_DIRECTORY=" + objects,
		"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http." + repositoryURL + ".extraHeader",
		"GIT_CONFIG_VALUE_0=Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token)),
	}
}

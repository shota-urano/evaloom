package publisher

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/safegit"
)

// Spec 11, 3.5.1 (Q29). These are identifiers, not credentials.
const publisherAppID = 5537529
const publisherInstallationID = "176795851"

var ErrAuthentication = errors.New("publisher: GitHub App authentication failed")

// NewGitHubApp binds both REST and Git authentication to one Project repository.
// readPrivateKey must read the host-held App PEM key from ~/.config/evaloom/;
// its filename/configuration wiring is deliberately left to Trusted Runner.
// It must never read a Sandbox mount. The key, JWT and installation token stay
// inside this boundary. No cache, disk storage, environment mutation or retry
// is used. The supplied HTTP transport is trusted and must not log credentials.
func NewGitHubApp(client *http.Client, repositoryURL string, readPrivateKey func(context.Context) ([]byte, error)) (*GitHub, error) {
	repo, err := githubRepository(repositoryURL)
	if err != nil || client == nil || readPrivateKey == nil {
		return nil, ErrPRConfiguration
	}
	copy := *client
	copy.Jar = nil
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	g, err := NewGitHub(&copy, func(ctx context.Context) (string, error) {
		return installationToken(ctx, &copy, repo, readPrivateKey)
	})
	if err != nil {
		return nil, err
	}
	g.repo = repo
	return g, nil
}

func installationToken(ctx context.Context, client *http.Client, repo string, readKey func(context.Context) ([]byte, error)) (string, error) {
	fail := func() (string, error) {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", ErrAuthentication
	}
	if ctx.Err() != nil {
		return fail()
	}
	key, err := readKey(ctx)
	if err != nil {
		return fail()
	}
	jwt, err := appJWT(key, time.Now())
	if err != nil {
		return fail()
	}
	_, name, _ := strings.Cut(repo, "/")
	permissions := map[string]string{"contents": "write", "pull_requests": "write", "metadata": "read"}
	input := struct {
		Repositories []string          `json:"repositories"`
		Permissions  map[string]string `json:"permissions"`
	}{[]string{name}, permissions}
	data, err := json.Marshal(input)
	if err != nil {
		return fail()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.github.com/app/installations/"+publisherInstallationID+"/access_tokens", bytes.NewReader(data))
	if err != nil {
		return fail()
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	response, err := client.Do(req)
	if err != nil {
		return fail()
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		return fail()
	}
	var result struct {
		Token        string            `json:"token"`
		ExpiresAt    time.Time         `json:"expires_at"`
		Permissions  map[string]string `json:"permissions"`
		Repositories []struct {
			FullName string `json:"full_name"`
		} `json:"repositories"`
	}
	decoder := json.NewDecoder(response.Body)
	if err := decoder.Decode(&result); err != nil {
		return fail()
	}
	if decoder.Decode(new(any)) != io.EOF || !validToken(result.Token) || !result.ExpiresAt.After(time.Now()) ||
		len(result.Repositories) != 1 || !strings.EqualFold(result.Repositories[0].FullName, repo) ||
		len(result.Permissions) != len(permissions) {
		return fail()
	}
	for name, value := range permissions {
		if result.Permissions[name] != value {
			return fail()
		}
	}
	return result.Token, nil
}

// GitHub's documented JWT protocol uses RS256, iat 60s in the past for clock
// drift, and exp at most 10m in the future. These are protocol limits, not new
// Evaloom configuration defaults.
func appJWT(keyPEM []byte, now time.Time) (string, error) {
	block, rest := pem.Decode(keyPEM)
	if block == nil || len(bytes.TrimSpace(rest)) != 0 {
		return "", ErrAuthentication
	}
	var key *rsa.PrivateKey
	var err error
	switch block.Type {
	case "RSA PRIVATE KEY":
		key, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	case "PRIVATE KEY":
		var parsed any
		parsed, err = x509.ParsePKCS8PrivateKey(block.Bytes)
		key, _ = parsed.(*rsa.PrivateKey)
	default:
		return "", ErrAuthentication
	}
	if err != nil || key == nil || key.Validate() != nil {
		return "", ErrAuthentication
	}
	claims, err := json.Marshal(struct {
		IssuedAt  int64 `json:"iat"`
		ExpiresAt int64 `json:"exp"`
		Issuer    int64 `json:"iss"`
	}{now.Add(-time.Minute).Unix(), now.Add(10 * time.Minute).Unix(), publisherAppID})
	if err != nil {
		return "", ErrAuthentication
	}
	unsigned := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", ErrAuthentication
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

var _ GitTransport = (*GitHub)(nil)

// PrepareGit canonicalizes SSH Project URLs to HTTPS without persisting a
// remote. Acquire the token before Push's final cancellation/HEAD checks, then
// retain it only in the returned per-push command for push and readback.
func (g *GitHub) PrepareGit(ctx context.Context, repositoryURL string) (GitCommand, error) {
	repo, err := g.repository(repositoryURL)
	if err != nil {
		return nil, ErrPushConfiguration
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if g.token == nil {
		return nil, ErrAuthentication
	}
	token, err := g.token(ctx)
	if err != nil || !validToken(token) {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrAuthentication
	}
	return func(ctx context.Context, worktree, operation, ref string) ([]byte, error) {
		return safegit.HTTPSOutput(ctx, worktree, "https://github.com/"+repo+".git", token, operation, ref)
	}, nil
}

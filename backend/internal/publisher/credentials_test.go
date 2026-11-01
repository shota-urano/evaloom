package publisher_test

import (
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
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/publisher"
)

const credentialCanary = "EVALOOM_C2X6_FAKE_GITHUB_INSTALLATION_TOKEN_17ac50e9"

func appKey(t *testing.T) (*rsa.PrivateKey, []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return key, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}

func tokenResponse() string {
	return `{"token":"` + credentialCanary + `","expires_at":"` + time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `","permissions":{"contents":"write","pull_requests":"write","metadata":"read"},"repositories":[{"full_name":"owner/repo"}]}`
}

func checkTokenRequest(t *testing.T, request *http.Request, key *rsa.PrivateKey) string {
	t.Helper()
	if request.Method != "POST" || request.URL.String() != "https://api.github.com/app/installations/176795851/access_tokens" {
		t.Fatal("wrong installation token destination")
	}
	jwt := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatal("missing App JWT")
	}
	header, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || string(header) != `{"alg":"RS256","typ":"JWT"}` {
		t.Fatal("wrong JWT algorithm")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err != nil || rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], sig) != nil {
		t.Fatal("App JWT signature verification failed")
	}
	claims, err := base64.RawURLEncoding.DecodeString(parts[1])
	var values struct{ Iat, Exp, Iss int64 }
	if err != nil || json.Unmarshal(claims, &values) != nil || values.Iss != 5537529 ||
		values.Iat > time.Now().Unix() || values.Iat < time.Now().Add(-2*time.Minute).Unix() ||
		values.Exp <= time.Now().Unix() || values.Exp > time.Now().Add(10*time.Minute).Unix() {
		t.Fatal("invalid App JWT claims")
	}
	var body struct {
		Repositories []string          `json:"repositories"`
		Permissions  map[string]string `json:"permissions"`
	}
	if json.NewDecoder(request.Body).Decode(&body) != nil || !reflect.DeepEqual(body.Repositories, []string{"repo"}) ||
		!reflect.DeepEqual(body.Permissions, map[string]string{"contents": "write", "pull_requests": "write", "metadata": "read"}) {
		t.Fatal("token request is not restricted to one repository and Q29 permissions")
	}
	return jwt
}

func TestGitHubAppUsesSignedJWTAndScopedInstallationToken(t *testing.T) {
	key, keyPEM := appKey(t)
	for _, format := range []string{"PKCS1", "PKCS8"} {
		t.Run(format, func(t *testing.T) {
			if format == "PKCS8" {
				der, err := x509.MarshalPKCS8PrivateKey(key)
				if err != nil {
					t.Fatal(err)
				}
				keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
			}
			tokenCalls, restCalls := 0, 0
			g, err := publisher.NewGitHubApp(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if strings.HasPrefix(r.URL.Path, "/app/") {
					tokenCalls++
					checkTokenRequest(t, r, key)
					return reply(201, tokenResponse()), nil
				}
				restCalls++
				if r.Header.Get("Authorization") != "Bearer "+credentialCanary {
					t.Fatal("REST did not authenticate with the installation token")
				}
				return reply(200, `[]`), nil
			})}, "git@github.com:owner/repo.git", func(context.Context) ([]byte, error) { return keyPEM, nil })
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if pr, err := g.FindOpen(context.Background(), "https://github.com/owner/repo", "evaloom/task"); err != nil || pr != nil {
					t.Fatal("App authenticated lookup failed")
				}
			}
			if tokenCalls != 2 || restCalls != 2 {
				t.Fatalf("token=%d rest=%d", tokenCalls, restCalls)
			}
			_, err = g.FindOpen(context.Background(), "https://github.com/other/repo", "evaloom/task")
			if !errors.Is(err, publisher.ErrPRConfiguration) || tokenCalls != 2 {
				t.Fatal("repository scope bypassed")
			}
		})
	}
}

func TestGitHubAppRejectsAuthenticationFailuresWithoutExposingCredentials(t *testing.T) {
	_, keyPEM := appKey(t)
	for _, stage := range []string{"key", "invalid_key", "transport", "401", "redirect", "expired", "missing_expiry", "wrong_repo", "multiple_repos", "extra_permission", "missing_permission", "empty_token", "newline_token", "malformed", "trailing"} {
		t.Run(stage, func(t *testing.T) {
			calls := 0
			g, err := publisher.NewGitHubApp(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if !strings.HasPrefix(r.URL.Path, "/app/") {
					t.Fatal("failed authentication reached REST operation")
				}
				body := tokenResponse()
				switch stage {
				case "transport":
					return nil, errors.New(credentialCanary)
				case "401":
					return reply(401, credentialCanary), nil
				case "redirect":
					resp := reply(307, credentialCanary)
					resp.Header.Set("Location", "https://api.github.com/leak?token="+credentialCanary)
					return resp, nil
				case "expired":
					var input map[string]any
					if err := json.Unmarshal([]byte(body), &input); err != nil {
						t.Fatal(err)
					}
					input["expires_at"] = "2000-01-01T00:00:00Z"
					data, err := json.Marshal(input)
					if err != nil {
						t.Fatal(err)
					}
					body = string(data)
				case "missing_expiry":
					body = strings.Replace(body, "expires_at", "ignored", 1)
				case "wrong_repo":
					body = strings.Replace(body, "owner/repo", "other/repo", 1)
				case "multiple_repos":
					body = strings.Replace(body, `"repositories":[`, `"repositories":[{"full_name":"other/repo"},`, 1)
				case "extra_permission":
					body = strings.Replace(body, `"permissions":{`, `"permissions":{"administration":"write",`, 1)
				case "missing_permission":
					body = strings.Replace(body, `"contents":"write",`, "", 1)
				case "empty_token":
					body = strings.ReplaceAll(body, credentialCanary, "")
				case "newline_token":
					body = strings.ReplaceAll(body, credentialCanary, credentialCanary+`\n`)
				case "malformed":
					body = credentialCanary
				case "trailing":
					body += `{}`
				}
				return reply(201, body), nil
			})}, "https://github.com/owner/repo", func(context.Context) ([]byte, error) {
				if stage == "key" {
					return nil, errors.New(credentialCanary)
				}
				if stage == "invalid_key" {
					return []byte(credentialCanary), nil
				}
				return keyPEM, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = g.FindOpen(context.Background(), "https://github.com/owner/repo", "evaloom/task")
			if !errors.Is(err, publisher.ErrPullRequest) || strings.Contains(err.Error(), credentialCanary) || calls > 1 {
				t.Fatal("unsafe or retried authentication failure")
			}
		})
	}
}

func TestGitHubAppDoesNotLoadKeyForInvalidInputOrCancellation(t *testing.T) {
	load := func(context.Context) ([]byte, error) { t.Fatal("unexpected key read"); return nil, io.EOF }
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("unexpected network request")
		return nil, io.EOF
	})}
	if _, err := publisher.NewGitHubApp(client, "https://token:"+credentialCanary+"@github.com/owner/repo", load); err == nil {
		t.Fatal("accepted credential URL")
	}
	g, err := publisher.NewGitHubApp(client, "https://github.com/owner/repo", load)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := g.FindOpen(ctx, "https://github.com/owner/repo", "evaloom/task"); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled authentication was not stopped")
	}
	if _, err := g.PrepareGit(ctx, "https://github.com/other/repo"); !errors.Is(err, publisher.ErrPushConfiguration) {
		t.Fatal("Git authentication ignored repository scope")
	}
	if _, err := g.PrepareGit(ctx, "https://github.com/owner/repo"); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled Git authentication was not stopped")
	}
}

func TestPushRequiresExplicitAuthenticationForGitHub(t *testing.T) {
	f := newFixture(t)
	for _, repository := range []string{"https://github.com/owner/repo.git", "git@github.com:owner/repo.git", "ssh://git@github.com/owner/repo.git", "https://token:" + credentialCanary + "@github.com/owner/repo.git"} {
		_, err := publisher.Push(context.Background(), f.target(), publisher.PushConfig{RepositoryURL: repository,
			CheckCancellation:    func(context.Context) error { t.Fatal("unexpected cancellation check"); return nil },
			RecordRemoteMismatch: func(context.Context, string, string) error { return nil }})
		if !errors.Is(err, publisher.ErrPushConfiguration) || strings.Contains(err.Error(), credentialCanary) {
			t.Fatal("GitHub destination accepted ambient authentication")
		}
	}
}

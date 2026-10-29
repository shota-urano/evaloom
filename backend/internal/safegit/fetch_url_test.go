package safegit_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/safegit"
)

func TestValidateFetchURL(t *testing.T) {
	const registered = "https://example.invalid/Owner/Repo.With.Dots.git"
	for _, test := range []struct {
		name, config string
		reject       bool
	}{
		{"ordinary-origin", fmt.Sprintf("[remote \"origin\"]\nurl = %s\n", registered), false},
		{"url-named-remote", fmt.Sprintf("[ReMoTe %q]\nUrL = /owner/repo\n", registered), true},
		{"other-network-remote", fmt.Sprintf("[remote %q]\nurl = https://other.invalid/repo\n", registered), true},
		{"case-distinct-remote", fmt.Sprintf("[remote %q]\nurl = /owner/repo\n", strings.ToLower(registered)), false},
		{"scp-named-remote", "[remote \"git@example.invalid:Repo.git\"]\nurl = /owner/repo\n", false},
		{"rewrite", "[UrL \"/owner/repo\"]\nInStEaDoF = https://example.invalid/\n", true},
		{"unrelated-rewrite", "[url \"https://other.invalid/\"]\ninsteadOf = unrelated:\n", true},
		{"push-only-rewrite", "[url \"/owner/repo\"]\npushInsteadOf = https://example.invalid/\n", false},
		{"bundle-uri", "[FeTcH]\nBuNdLeUrI = /owner/repo.bundle\n", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository := t.TempDir()
			if _, err := safegit.Output(context.Background(), repository, "init"); err != nil {
				t.Fatal(err)
			}
			write(t, filepath.Join(repository, ".git", "config"), "[core]\nrepositoryformatversion = 0\nbare = false\n"+test.config, 0600)
			err := safegit.ValidateFetchURL(context.Background(), repository, registered)
			if (err != nil) != test.reject {
				t.Fatalf("reject=%t, err=%v", test.reject, err)
			}
			if test.name == "scp-named-remote" {
				if err := safegit.ValidateFetchURL(context.Background(), repository, "git@example.invalid:Repo.git"); err == nil {
					t.Fatal("SCP-like URL alias accepted")
				}
			}
			if err != nil && (strings.Contains(err.Error(), "owner/repo") || strings.Contains(err.Error(), "example.invalid")) {
				t.Fatalf("configuration leaked into error: %v", err)
			}
		})
	}
}

func TestValidateFetchURLFailsClosed(t *testing.T) {
	repository := t.TempDir()
	if _, err := safegit.Output(context.Background(), repository, "init"); err != nil {
		t.Fatal(err)
	}
	// An unreadable include must not be treated as having no overrides.
	included := filepath.Join(repository, ".git", "included.config")
	if err := os.Mkdir(included, 0700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(repository, ".git", "config"), "[include]\npath = included.config\n", 0600)
	if err := safegit.ValidateFetchURL(context.Background(), repository, "https://example.invalid/repo"); err == nil {
		t.Fatal("unreadable include accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := safegit.ValidateFetchURL(ctx, repository, "https://example.invalid/repo"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation = %v", err)
	}
}

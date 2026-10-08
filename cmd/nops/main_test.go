package main

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/music-gang/nops/internal/config"
	"github.com/music-gang/nops/internal/web"
)

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestNewAuthenticatorPicksTheRightBackend covers the one piece of actual
// logic in cmd/nops: everything else is straight-line wiring, exercised end
// to end by tests/integration's binary smoke test instead.
func TestNewAuthenticatorPicksTheRightBackend(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("s3cret"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	usersFile := filepath.Join(t.TempDir(), "users")
	if err := os.WriteFile(usersFile, fmt.Appendf(nil, "alice:%s\n", hash), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("oidc", func(t *testing.T) {
		cfg := &config.Config{
			AuthMode: "oidc", PublicURL: "https://nops.example.com",
			OIDCIssuerURL: "https://idp.example.com/", OIDCClientID: "nops", OIDCClientSecret: "secret",
			OIDCAllowedUsers: []string{"alice"},
		}
		auth, err := newAuthenticator(cfg, discardLog())
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := auth.(*web.Auth); !ok {
			t.Errorf("got %T, want *web.Auth", auth)
		}
	})

	t.Run("basic", func(t *testing.T) {
		cfg := &config.Config{AuthMode: "basic", PublicURL: "https://nops.example.com", UsersFile: usersFile}
		auth, err := newAuthenticator(cfg, discardLog())
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := auth.(*web.BasicAuth); !ok {
			t.Errorf("got %T, want *web.BasicAuth", auth)
		}
	})

	t.Run("unknown mode", func(t *testing.T) {
		cfg := &config.Config{AuthMode: "header", PublicURL: "https://nops.example.com"}
		_, err := newAuthenticator(cfg, discardLog())
		if err == nil || !strings.Contains(err.Error(), "unknown -auth-mode") {
			t.Errorf("err = %v, want it to mention the unknown auth mode", err)
		}
	})

	t.Run("propagates a backend's own validation error", func(t *testing.T) {
		cfg := &config.Config{AuthMode: "basic", PublicURL: "https://nops.example.com", UsersFile: "/does/not/exist"}
		if _, err := newAuthenticator(cfg, discardLog()); err == nil {
			t.Error("want an error for a users file that does not exist")
		}
	})
}

// The "starting" line names the build and the settings that tell one
// instance from another, and never carries a credential: not the token
// embedded in a git URL, not a secret held in the configuration.
func TestStartupAttrs(t *testing.T) {
	cfg := &config.Config{
		ListenAddr: ":8080", NomadAddr: "https://nomad.example.com:4646", NomadNamespaces: []string{"default", "prod"},
		GitURL: "https://ci:git-secret@git.example.com/ops/jobs.git", GitBranch: "main", GitPath: "jobs",
		AuthMode: "oidc", DBPath: "/data/nops.db", BasePath: "/nops",
		NomadToken: "nomad-secret", GitToken: "git-secret", OIDCClientSecret: "oidc-secret", WebhookSecret: "hook-secret",
	}
	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).Info("starting", startupAttrs(cfg)...)
	line := buf.String()
	for _, want := range []string{
		"version=", "go=go", "listen_addr=:8080", "nomad_addr=https://nomad.example.com:4646",
		"namespaces=\"[default prod]\"", "git_url=https://git.example.com/ops/jobs.git",
		"git_branch=main", "git_path=jobs", "auth_mode=oidc", "acl=false", "db_path=/data/nops.db", "base_path=/nops",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("starting line %q does not contain %q", line, want)
		}
	}
	for _, secret := range []string{"git-secret", "nomad-secret", "oidc-secret", "hook-secret", "ci:"} {
		if strings.Contains(line, secret) {
			t.Errorf("starting line %q leaks %q", line, secret)
		}
	}
}

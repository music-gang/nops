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
		ConfigFile: "/etc/nops.hcl",
		Repositories: []config.Repository{
			{Name: "default", URL: "https://ci:git-secret@git.example.com/ops/jobs.git", Branch: "main", Path: "jobs", Token: "git-secret"},
			{Name: "apps", URL: "https://git.example.com/ops/apps.git", Branch: "main"},
		},
		AuthMode: "oidc", DBPath: "/data/nops.db", BasePath: "/nops",
		NomadToken: "nomad-secret", OIDCClientSecret: "oidc-secret", WebhookSecret: "hook-secret",
	}
	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).Info("starting", startupAttrs(cfg)...)
	line := buf.String()
	for _, want := range []string{
		"version=", "go=go", "listen_addr=:8080", "nomad_addr=https://nomad.example.com:4646",
		"namespaces=\"[default prod]\"", "config=/etc/nops.hcl", "repositories=\"[default apps]\"",
		"auth_mode=oidc", "acl=false", "db_path=/data/nops.db", "base_path=/nops",
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

// Nops warns about plain HTTP when a login could cross a network: no TLS of
// its own, an http:// public URL, and a public or listen host that is not
// loopback. Behind a TLS proxy the public URL is https://, so it stays quiet.
func TestPlainHTTP(t *testing.T) {
	for _, tc := range []struct {
		name      string
		tls       bool
		publicURL string
		listen    string
		want      bool
	}{
		{"TLS served by Nops", true, "https://localhost:8443", ":8443", false},
		{"TLS proxy", false, "https://nops.example.org", ":8080", false},
		{"loopback", false, "http://localhost:8080", "127.0.0.1:8080", false},
		{"IPv6 loopback", false, "http://[::1]:8080", "[::1]:8080", false},
		{"every interface", false, "http://localhost:8080", ":8080", true},
		{"wildcard address", false, "http://localhost:8080", "0.0.0.0:8080", true},
		{"plain proxy", false, "http://nops.example.org", "127.0.0.1:8080", true},
		{"public address", false, "http://10.0.0.5:8080", "10.0.0.5:8080", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{PublicURL: tc.publicURL, ListenAddr: tc.listen}
			if tc.tls {
				cfg.TLSCertFile, cfg.TLSKeyFile = "nops.crt", "nops.key"
			}
			if got := plainHTTP(cfg); got != tc.want {
				t.Errorf("plainHTTP = %v, want %v", got, tc.want)
			}
		})
	}
}

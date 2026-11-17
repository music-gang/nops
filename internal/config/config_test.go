package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"

	"github.com/music-gang/nops/internal/cli"
	"github.com/music-gang/nops/internal/secret"
)

const repo = "https://git.example.com/ops/jobs.git"

// authEnv is what a valid configuration needs besides -git-url: the OIDC
// login and the public URL are required. envOf adds it under every test
// environment, unless the test sets (or blanks) the variable itself.
var authEnv = map[string]string{
	"NOPS_PUBLIC_URL":         "https://nops.example.com",
	"NOPS_AUTH_MODE":          "oidc",
	"NOPS_OIDC_ISSUER_URL":    "https://idp.example.com/application/o/nops/",
	"NOPS_OIDC_CLIENT_ID":     "nops",
	"NOPS_OIDC_CLIENT_SECRET": "client-secret",
}

func envOf(m map[string]string) func(string) string {
	return func(k string) string {
		if v, ok := m[k]; ok {
			return v
		}
		return authEnv[k]
	}
}

// rawEnvOf is an environment with nothing added, for the tests of what is required.
func rawEnvOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadDefaults(t *testing.T) {
	c, err := Load([]string{"-git-url", repo}, envOf(nil), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	want := &Config{
		NomadAddr:        "http://127.0.0.1:4646",
		NomadNamespaces:  []string{"default"},
		GitURL:           repo,
		GitBranch:        "main",
		GitUsername:      "git",
		GitTimeout:       2 * time.Minute,
		NomadTimeout:     30 * time.Second,
		Repositories:     []Repository{{Name: "default", URL: repo, Branch: "main", Username: "git", Timeout: 2 * time.Minute}},
		DBPath:           "nops.db",
		ListenAddr:       ":8080",
		AuthMode:         "oidc",
		OIDCIssuerURL:    "https://idp.example.com/application/o/nops/",
		OIDCClientID:     "nops",
		OIDCClientSecret: "client-secret",
		PublicURL:        "https://nops.example.com",
		GitPollInterval:  time.Minute,
		DriftInterval:    5 * time.Minute,
		EngineInterval:   5 * time.Second,
		HookPollInterval: 5 * time.Second,
		ApplyTimeout:     10 * time.Minute,
		// The server's timeouts, with no listen block.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       time.Minute,
		WriteTimeout:      time.Minute,
		IdleTimeout:       2 * time.Minute,
		// The limit on failed logins, with no auth block.
		LoginMaxFailures:   10,
		LoginFailureWindow: 15 * time.Minute,
		// A sync window is read in UTC unless the operator says otherwise.
		SyncWindowLocation: time.UTC,
		LogLevel:           slog.LevelInfo,
	}
	if !reflect.DeepEqual(c, want) {
		t.Errorf("defaults:\n got %+v\nwant %+v", c, want)
	}
}

func TestLoadPrecedence(t *testing.T) {
	tests := []struct {
		name string
		args []string
		env  map[string]string
		want time.Duration
	}{
		{"default", nil, nil, 10 * time.Minute},
		{"env over default", nil, map[string]string{"NOPS_APPLY_TIMEOUT": "20m"}, 20 * time.Minute},
		{"flag over env", []string{"-apply-timeout", "30m"}, map[string]string{"NOPS_APPLY_TIMEOUT": "20m"}, 30 * time.Minute},
		{"empty env is unset", nil, map[string]string{"NOPS_APPLY_TIMEOUT": ""}, 10 * time.Minute},
		{"flag over invalid env", []string{"-apply-timeout=1m"}, map[string]string{"NOPS_APPLY_TIMEOUT": "nope"}, time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := map[string]string{"NOPS_GIT_URL": repo}
			for k, v := range tt.env {
				env[k] = v
			}
			c, err := Load(tt.args, envOf(env), io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			if c.ApplyTimeout != tt.want {
				t.Errorf("ApplyTimeout = %s, want %s", c.ApplyTimeout, tt.want)
			}
		})
	}
}

// TestLoadEveryOption sets every option once through its flag and once through
// its variable, so a typo in a set function or in the env name shows up.
// TestLoadEveryOption sets every option once through its flag and once
// through its variable, so a typo in a set function or in the env name
// shows up. -auth-mode and -users-file are mutually exclusive with the
// -oidc-* options (check()), so it runs once per auth mode: each variant
// covers every option in the table, only the auth-specific ones differ.
func TestLoadEveryOption(t *testing.T) {
	ca := writeFile(t, "ca.pem", "ca")
	cert := writeFile(t, "cert.pem", "cert")
	key := writeFile(t, "key.pem", "key")
	nomadTok := writeFile(t, "nomad-token", "nomad-secret\n")
	gitTok := writeFile(t, "git-token", "  git-secret\n")
	appsTok := writeFile(t, "apps-token", "apps-secret\n")
	bootstrap := secret.New()
	bootstrapTok := writeFile(t, "bootstrap-token", bootstrap+"\n")
	configFile := writeFile(t, "nops.hcl", `
repository "apps" {
  url        = "https://git.example.com/ops/apps.git"
  token_file = "`+appsTok+`"
}

acl {
  enabled              = true
  bootstrap_token_file = "`+bootstrapTok+`"
}
`)
	oidcSecret := writeFile(t, "oidc-secret", "oidc-client-secret\n")
	webhookSecret := writeFile(t, "webhook-secret", "webhook-shared-secret\n")
	usersFile := writeFile(t, "users", "alice:$2a$10$not-checked-by-config\n")
	groupsFile := writeFile(t, "groups", "ops: alice\n")

	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Skipf("no tz database: %v", err)
	}

	// Every option shared by both auth modes.
	common := map[string]string{
		"nomad-addr":            "https://nomad.example.com:4646",
		"nomad-ui-url":          "https://infra.example.com/nomad/",
		"nomad-namespaces":      "apps,infra, apps",
		"nomad-token-file":      nomadTok,
		"nomad-ca-cert":         ca,
		"nomad-client-cert":     cert,
		"nomad-client-key":      key,
		"nomad-tls-skip-verify": "true",
		"git-url":               repo,
		"git-branch":            "prod",
		"git-path":              "jobs/apps",
		"git-username":          "oauth2",
		"git-token-file":        gitTok,
		"git-timeout":           "45s",
		"config":                configFile,
		"db-path":               "/var/lib/nops/nops.db",
		"listen-addr":           "127.0.0.1:9000",
		"webhook-secret-file":   webhookSecret,
		"public-url":            "https://nops.example.com/",
		"git-poll-interval":     "2m",
		"drift-interval":        "15m",
		"engine-interval":       "7s",
		"hook-poll-interval":    "3s",
		"apply-timeout":         "1h",
		"sync-window-time-zone": "Asia/Tokyo", // no daylight saving: two loads of it are equal
		"log-level":             "debug",
	}
	commonWant := Config{
		NomadAddr:          "https://nomad.example.com:4646",
		NomadUIURL:         "https://infra.example.com/nomad",
		NomadNamespaces:    []string{"apps", "infra"},
		NomadTokenFile:     nomadTok,
		NomadToken:         "nomad-secret",
		NomadCACert:        ca,
		NomadClientCert:    cert,
		NomadClientKey:     key,
		NomadTLSSkipVerify: true,
		GitURL:             repo,
		GitBranch:          "prod",
		GitPath:            "jobs/apps",
		GitUsername:        "oauth2",
		GitTokenFile:       gitTok,
		GitToken:           "git-secret",
		GitTimeout:         45 * time.Second,
		NomadTimeout:       30 * time.Second, // the nomad block is the file's, not an option
		ConfigFile:         configFile,
		Repositories: []Repository{
			{Name: "default", URL: repo, Branch: "prod", Path: "jobs/apps", Username: "oauth2", TokenFile: gitTok, Token: "git-secret", Timeout: 45 * time.Second},
			{Name: "apps", URL: "https://git.example.com/ops/apps.git", Branch: "main", Username: "git", TokenFile: appsTok, Token: "apps-secret", Timeout: 2 * time.Minute},
		},
		DBPath:     "/var/lib/nops/nops.db",
		ListenAddr: "127.0.0.1:9000",
		// The listen block is the file's, not an option: the defaults stand.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       time.Minute,
		WriteTimeout:      time.Minute,
		IdleTimeout:       2 * time.Minute,
		// The auth and acl blocks are the file's too.
		LoginMaxFailures:      10,
		LoginFailureWindow:    15 * time.Minute,
		WebhookSecretFile:     webhookSecret,
		WebhookSecret:         "webhook-shared-secret",
		ACL:                   true,
		ACLBootstrapTokenFile: bootstrapTok,
		ACLBootstrapToken:     bootstrap,
		PublicURL:             "https://nops.example.com",
		GitPollInterval:       2 * time.Minute,
		DriftInterval:         15 * time.Minute,
		EngineInterval:        7 * time.Second,
		HookPollInterval:      3 * time.Second,
		ApplyTimeout:          time.Hour,
		SyncWindowLocation:    tokyo,
		LogLevel:              slog.LevelDebug,
	}

	oidcWant := commonWant
	oidcWant.AuthMode = "oidc"
	oidcWant.OIDCIssuerURL = "https://auth.example.com/application/o/nops/"
	oidcWant.OIDCClientID = "nops-dashboard"
	oidcWant.OIDCClientSecretFile = oidcSecret
	oidcWant.OIDCClientSecret = "oidc-client-secret"

	basicWant := commonWant
	basicWant.AuthMode = "basic"
	basicWant.UsersFile = usersFile
	basicWant.GroupsFile = groupsFile

	variants := []struct {
		name   string
		values map[string]string // auth-specific overlay on top of common
		want   Config
	}{
		{
			name: "oidc",
			values: map[string]string{
				"auth-mode":               "oidc",
				"oidc-issuer-url":         "https://auth.example.com/application/o/nops/",
				"oidc-client-id":          "nops-dashboard",
				"oidc-client-secret-file": oidcSecret,
				"users-file":              "",
				"groups-file":             "",
			},
			want: oidcWant,
		},
		{
			name: "basic",
			values: map[string]string{
				"auth-mode":               "basic",
				"oidc-issuer-url":         "",
				"oidc-client-id":          "",
				"oidc-client-secret-file": "",
				"users-file":              usersFile,
				"groups-file":             groupsFile,
			},
			want: basicWant,
		},
	}

	for _, variant := range variants {
		t.Run(variant.name, func(t *testing.T) {
			values := make(map[string]string, len(common)+len(variant.values))
			for k, v := range common {
				values[k] = v
			}
			for k, v := range variant.values {
				values[k] = v
			}
			if len(values) != len(options) {
				t.Fatalf("test covers %d options, the table has %d", len(values), len(options))
			}
			want := variant.want

			var args []string
			env := map[string]string{}
			for _, o := range options {
				v, ok := values[o.name]
				if !ok {
					t.Fatalf("no test value for option %s", o.name)
				}
				args = append(args, "-"+o.name+"="+v)
				env[o.env()] = v
			}
			// rawEnvOf, not envOf: every option is set explicitly above
			// (that is the point of this test), and envOf's fallback to
			// the shared authEnv fixture would leak its
			// NOPS_OIDC_CLIENT_SECRET (an env-only value with no flag,
			// resolveSecretValues) into the basic variant, where every
			// OIDC field must actually end up empty.
			for name, run := range map[string]func() (*Config, error){
				"flags": func() (*Config, error) { return Load(args, rawEnvOf(nil), io.Discard) },
				"env":   func() (*Config, error) { return Load(nil, rawEnvOf(env), io.Discard) },
			} {
				t.Run(name, func(t *testing.T) {
					c, err := run()
					if err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(c, &want) {
						t.Errorf("\n got %+v\nwant %+v", c, &want)
					}
				})
			}
		})
	}
}

func TestLoadInvalid(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing")
	empty := writeFile(t, "empty", " \n")
	tests := []struct {
		option string
		value  string
		want   string // substring of the error
	}{
		{"nomad-addr", "nomad.example.com:4646", "not an http"},
		{"nomad-addr", "127.0.0.1:4646", "cannot contain colon"},
		{"nomad-addr", "ftp://nomad", "not an http"},
		{"nomad-addr", "http://", "not an http"},
		{"nomad-addr", "http://bad host", "invalid character"},
		{"nomad-namespaces", "", "required"},
		{"nomad-namespaces", " , ", "required"},
		{"nomad-token-file", missing, "no such file"},
		{"nomad-token-file", empty, "is empty"},
		{"nomad-ca-cert", missing, "no such file"},
		{"nomad-ca-cert", dir, "is a directory"},
		{"nomad-client-cert", missing, "no such file"},
		{"nomad-client-key", missing, "no such file"},
		{"nomad-tls-skip-verify", "maybe", "invalid syntax"},
		{"config", missing, "no such file"},
		{"config", dir, "is a directory"},
		{"git-branch", "", "required"},
		{"git-path", "/etc", "must be a relative path"},
		{"git-path", "..", "must be a relative path"},
		{"git-path", "../jobs", "must be a relative path"},
		{"git-token-file", missing, "no such file"},
		{"git-token-file", empty, "is empty"},
		{"git-timeout", "soon", "invalid duration"},
		{"git-timeout", "0s", "must be positive"},
		{"db-path", "", "required"},
		{"listen-addr", "8080", "invalid address"},
		{"listen-addr", "", "invalid address"},
		{"oidc-issuer-url", "idp.example.com", "not an http"},
		{"oidc-client-secret-file", missing, "no such file"},
		{"oidc-client-secret-file", empty, "is empty"},
		{"public-url", "nops.example.com", "not an http"},
		{"nomad-ui-url", "nomad.example.com", "not an http"},
		{"nomad-ui-url", "/ui", "not an http"},
		{"git-poll-interval", "-1m", "must be positive"},
		{"drift-interval", "soon", "invalid duration"},
		{"engine-interval", "0", "must be positive"},
		{"hook-poll-interval", "", "invalid duration"},
		{"apply-timeout", "1d", "unknown unit"},
		{"sync-window-time-zone", "Mars/Olympus", "unknown time zone"},
		{"sync-window-time-zone", "Rome", "unknown time zone"},
		{"sync-window-time-zone", "Local", "not an IANA time zone"},
		{"log-level", "verbose", "unknown name"},
	}
	for _, tt := range tests {
		var o option
		for _, oo := range options {
			if oo.name == tt.option {
				o = oo
			}
		}
		if o.name == "" {
			t.Fatalf("unknown option %s", tt.option)
		}
		t.Run(tt.option+"="+tt.value, func(t *testing.T) {
			base := map[string]string{"NOPS_GIT_URL": repo}

			// Through the flag: the error names the flag.
			_, err := Load([]string{"-" + o.name + "=" + tt.value}, envOf(base), io.Discard)
			checkErr(t, err, "flag -"+o.name+": ", tt.want)

			// Through the variable: the error names the variable. An empty
			// variable is unset, so only a required option without a default
			// fails that way.
			env := map[string]string{"NOPS_GIT_URL": repo}
			env[o.env()] = tt.value
			_, err = Load(nil, envOf(env), io.Discard)
			switch {
			case tt.value != "":
				checkErr(t, err, o.env()+": ", tt.want)
			case o.def == "" && tt.want == "required":
				checkErr(t, err, "-"+o.name+" / "+o.env()+": ", tt.want)
			case err != nil:
				t.Errorf("empty %s should fall back to the default, got %v", o.env(), err)
			}
		})
	}
}

// TestLoadPublicURLDefault covers -public-url's default, derived from
// -listen-addr when not set explicitly: an operator with no reverse proxy
// (typically -auth-mode=basic on a personal machine) no longer has to type
// something there just to satisfy a required flag. The default applies to
// -auth-mode=oidc too (see publicURLDefault's doc comment on why that is
// still the operator's call to override for a real provider).
func TestLoadPublicURLDefault(t *testing.T) {
	usersFile := writeFile(t, "users", "alice:hash\n")
	basicArgs := func(extra ...string) []string {
		return append([]string{"-git-url", repo, "-auth-mode", "basic", "-users-file", usersFile}, extra...)
	}

	tests := []struct {
		name       string
		listenAddr string
		want       string
	}{
		{"default listen-addr", "", "http://localhost:8080"},
		{"specific host", "127.0.0.1:9090", "http://127.0.0.1:9090"},
		{"no host", ":9090", "http://localhost:9090"},
		{"ipv4 wildcard", "0.0.0.0:9090", "http://localhost:9090"},
		{"ipv6 wildcard", "[::]:9090", "http://localhost:9090"},
		{"ipv6 host kept as is", "[::1]:9090", "http://[::1]:9090"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := basicArgs()
			if tt.listenAddr != "" {
				args = append(args, "-listen-addr", tt.listenAddr)
			}
			c, err := Load(args, rawEnvOf(nil), io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			if c.PublicURL != tt.want {
				t.Errorf("public URL = %q, want %q", c.PublicURL, tt.want)
			}
		})
	}

	// An explicit -public-url always wins, whatever -listen-addr is.
	c, err := Load(basicArgs("-listen-addr", "0.0.0.0:9090", "-public-url", "https://nops.example.com"), rawEnvOf(nil), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.PublicURL != "https://nops.example.com" {
		t.Errorf("public URL = %q, want the explicit value kept", c.PublicURL)
	}

	// -auth-mode=oidc gets the same default: -public-url is no longer
	// required by either mode.
	oidc := map[string]string{
		"NOPS_GIT_URL": repo, "NOPS_AUTH_MODE": "oidc",
		"NOPS_OIDC_ISSUER_URL": "https://idp.example.com/", "NOPS_OIDC_CLIENT_ID": "nops",
		"NOPS_OIDC_CLIENT_SECRET": "s3cret",
	}
	c, err = Load(nil, rawEnvOf(oidc), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.PublicURL != "http://localhost:8080" {
		t.Errorf("oidc public URL = %q, want the derived default", c.PublicURL)
	}
}

// TestLoadNomadUIURL covers -nomad-ui-url: unset means no link (it is never
// taken from -nomad-addr, which is the address nops uses, not one a browser
// can reach), a value is an absolute http(s) URL whose sub path is kept, and the
// variable and the flag say the same.
func TestLoadNomadUIURL(t *testing.T) {
	args := func(extra ...string) []string { return append([]string{"-git-url", repo}, extra...) }

	c, err := Load(args("-nomad-addr", "http://nomad.service.consul:4646"), envOf(nil), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.NomadUIURL != "" {
		t.Errorf("NomadUIURL = %q with only -nomad-addr set, want none: a link built from it would be a wrong one", c.NomadUIURL)
	}

	for in, want := range map[string]string{
		"https://nomad.example.com":        "https://nomad.example.com",
		"https://nomad.example.com/":       "https://nomad.example.com",
		"https://infra.example.com/nomad/": "https://infra.example.com/nomad",
		"http://127.0.0.1:4646":            "http://127.0.0.1:4646",
	} {
		c, err := Load(args("-nomad-ui-url", in), envOf(nil), io.Discard)
		if err != nil || c.NomadUIURL != want {
			t.Errorf("-nomad-ui-url %q = %q, %v; want %q", in, c.NomadUIURL, err, want)
		}
		c, err = Load(args(), envOf(map[string]string{"NOPS_NOMAD_UI_URL": in}), io.Discard)
		if err != nil || c.NomadUIURL != want {
			t.Errorf("NOPS_NOMAD_UI_URL %q = %q, %v; want %q", in, c.NomadUIURL, err, want)
		}
	}

	if _, err := Load(args("-nomad-ui-url", "ftp://nomad.example.com"), envOf(nil), io.Discard); err == nil {
		t.Error("an ftp:// address was accepted")
	}
}

// TestLoadBasePath covers BasePath, derived from -public-url's own path
// component: there is no separate flag (docs/running-nops.md#under-a-sub-path).
func TestLoadBasePath(t *testing.T) {
	usersFile := writeFile(t, "users", "alice:hash\n")
	args := func(publicURL string) []string {
		return []string{"-git-url", repo, "-auth-mode", "basic", "-users-file", usersFile, "-public-url", publicURL}
	}
	tests := []struct {
		publicURL string
		want      string
	}{
		{"https://nops.example.com", ""},
		{"https://nops.example.com/", ""},
		{"https://nops.example.com/nops", "/nops"},
		{"https://nops.example.com/nops/", "/nops"},
		{"https://nops.example.com/a/b", "/a/b"},
	}
	for _, tt := range tests {
		t.Run(tt.publicURL, func(t *testing.T) {
			c, err := Load(args(tt.publicURL), rawEnvOf(nil), io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			if c.BasePath != tt.want {
				t.Errorf("BasePath = %q, want %q", c.BasePath, tt.want)
			}
		})
	}

	// No -public-url at all: the derived default has no path either.
	c, err := Load([]string{"-git-url", repo, "-auth-mode", "basic", "-users-file", usersFile}, rawEnvOf(nil), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.BasePath != "" {
		t.Errorf("BasePath = %q, want empty with no -public-url", c.BasePath)
	}
}

func checkErr(t *testing.T, err error, prefix, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("no error, want %q", prefix+"..."+want)
	}
	for _, line := range strings.Split(err.Error(), "\n") {
		if strings.HasPrefix(line, prefix) && strings.Contains(line, want) {
			return
		}
	}
	t.Errorf("error %q has no line starting with %q containing %q", err, prefix, want)
}

func TestLoadCrossChecks(t *testing.T) {
	tok := writeFile(t, "token", "secret")
	cert := writeFile(t, "cert.pem", "cert")
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"token with ssh url", []string{"-git-url", "git@git.example.com:ops/jobs.git", "-git-token-file", tok}, "needs an https:// -git-url"},
		{"token with http url", []string{"-git-url", "http://git.example.com/jobs.git", "-git-token-file", tok}, "needs an https:// -git-url"},
		{"token without username", []string{"-git-url", repo, "-git-token-file", tok, "-git-username="}, "needs a non-empty -git-username"},
		{"cert without key", []string{"-git-url", repo, "-nomad-client-cert", cert}, "must be set together"},
		{"key without cert", []string{"-git-url", repo, "-nomad-client-key", cert}, "must be set together"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(tt.args, envOf(nil), io.Discard)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want %q", err, tt.want)
			}
		})
	}

	// Without a token, an ssh URL is accepted: a public repo needs no auth.
	if _, err := Load([]string{"-git-url", "git@git.example.com:ops/jobs.git"}, envOf(nil), io.Discard); err != nil {
		t.Errorf("ssh url without token: %v", err)
	}
	// Uppercase scheme is still https.
	if _, err := Load([]string{"-git-url", "HTTPS://git.example.com/jobs.git", "-git-token-file", tok}, envOf(nil), io.Discard); err != nil {
		t.Errorf("uppercase https: %v", err)
	}
}

func TestLoadOIDCRequired(t *testing.T) {
	full := map[string]string{
		"NOPS_GIT_URL":            repo,
		"NOPS_PUBLIC_URL":         "https://nops.example.com",
		"NOPS_AUTH_MODE":          "oidc",
		"NOPS_OIDC_ISSUER_URL":    "https://idp.example.com/",
		"NOPS_OIDC_CLIENT_ID":     "nops",
		"NOPS_OIDC_CLIENT_SECRET": "s3cret",
	}
	if _, err := Load(nil, rawEnvOf(full), io.Discard); err != nil {
		t.Fatalf("full OIDC configuration: %v", err)
	}

	tests := []struct {
		name string
		drop string
		want string
	}{
		{"issuer", "NOPS_OIDC_ISSUER_URL", "-oidc-issuer-url is required with -auth-mode=oidc"},
		{"client id", "NOPS_OIDC_CLIENT_ID", "-oidc-client-id is required with -auth-mode=oidc"},
		{"client secret", "NOPS_OIDC_CLIENT_SECRET", "the OIDC client secret is required with -auth-mode=oidc"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := map[string]string{}
			for k, v := range full {
				if k != tt.drop {
					env[k] = v
				}
			}
			_, err := Load(nil, rawEnvOf(env), io.Discard)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want %q", err, tt.want)
			}
		})
	}

	// The allowlist options are gone: Nops refuses them and says what replaces them.
	for name, set := range map[string]func(env map[string]string, args *[]string){
		"flag -oidc-allowed-users":  func(_ map[string]string, args *[]string) { *args = []string{"-oidc-allowed-users", "alice"} },
		"flag -oidc-allowed-groups": func(_ map[string]string, args *[]string) { *args = []string{"-oidc-allowed-groups=ops"} },
		"NOPS_OIDC_ALLOWED_USERS":   func(env map[string]string, _ *[]string) { env["NOPS_OIDC_ALLOWED_USERS"] = "alice" },
		"NOPS_OIDC_ALLOWED_GROUPS":  func(env map[string]string, _ *[]string) { env["NOPS_OIDC_ALLOWED_GROUPS"] = "ops" },
	} {
		env := map[string]string{}
		for k, v := range full {
			env[k] = v
		}
		var args []string
		set(env, &args)
		_, err := Load(args, rawEnvOf(env), io.Discard)
		if err == nil || !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), "was removed") || !strings.Contains(err.Error(), "binding rules") {
			t.Errorf("%s: err = %v, want it removed, with what replaces it", name, err)
		}
	}

	// The metrics token is gone too: the ACL protects /metrics.
	for name, set := range map[string]func(env map[string]string, args *[]string){
		"flag -metrics-token-file": func(_ map[string]string, args *[]string) {
			*args = []string{"-metrics-token-file", "/run/secrets/metrics"}
		},
		"NOPS_METRICS_TOKEN_FILE": func(env map[string]string, _ *[]string) { env["NOPS_METRICS_TOKEN_FILE"] = "/run/secrets/metrics" },
		"NOPS_METRICS_TOKEN":      func(env map[string]string, _ *[]string) { env["NOPS_METRICS_TOKEN"] = "plain-metrics-token" },
	} {
		env := map[string]string{}
		for k, v := range full {
			env[k] = v
		}
		var args []string
		set(env, &args)
		_, err := Load(args, rawEnvOf(env), io.Discard)
		if err == nil || !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), "was removed") || !strings.Contains(err.Error(), "metrics capability") {
			t.Errorf("%s: err = %v, want it removed, with what replaces it", name, err)
		}
	}

	// The issuer is kept exactly as given: providers such as Authentik
	// announce it with a trailing slash and the check compares the strings.
	c, err := Load(nil, rawEnvOf(full), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.OIDCIssuerURL != "https://idp.example.com/" {
		t.Errorf("issuer = %q, want it untouched", c.OIDCIssuerURL)
	}
}

// TestLoadAuthMode covers -auth-mode itself: required, one of exactly two
// values, and the two backends' options are mutually exclusive.
func TestLoadAuthMode(t *testing.T) {
	usersFile := writeFile(t, "users", "alice:hash\n")
	base := map[string]string{"NOPS_GIT_URL": repo, "NOPS_PUBLIC_URL": "https://nops.example.com"}

	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"missing", map[string]string{}, "-auth-mode / NOPS_AUTH_MODE: required"},
		{"invalid value", map[string]string{"NOPS_AUTH_MODE": "header"}, `must be "oidc" or "basic"`},
		{"basic without users-file", map[string]string{"NOPS_AUTH_MODE": "basic"}, "-users-file is required with -auth-mode=basic"},
		{"oidc without its options", map[string]string{"NOPS_AUTH_MODE": "oidc"}, "-oidc-issuer-url is required with -auth-mode=oidc"},
		{"basic with an OIDC option set", map[string]string{
			"NOPS_AUTH_MODE": "basic", "NOPS_USERS_FILE": usersFile, "NOPS_OIDC_CLIENT_ID": "nops",
		}, "the -oidc-* options are only used with -auth-mode=oidc"},
		{"oidc with users-file set", map[string]string{
			"NOPS_AUTH_MODE": "oidc", "NOPS_USERS_FILE": usersFile,
			"NOPS_OIDC_ISSUER_URL": "https://idp.example.com/", "NOPS_OIDC_CLIENT_ID": "nops",
			"NOPS_OIDC_CLIENT_SECRET": "s3cret",
		}, "-users-file is only used with -auth-mode=basic"},
		{"oidc with groups-file set", map[string]string{
			"NOPS_AUTH_MODE": "oidc", "NOPS_GROUPS_FILE": usersFile,
			"NOPS_OIDC_ISSUER_URL": "https://idp.example.com/", "NOPS_OIDC_CLIENT_ID": "nops",
			"NOPS_OIDC_CLIENT_SECRET": "s3cret",
		}, "-groups-file is only used with -auth-mode=basic"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := map[string]string{}
			for k, v := range base {
				env[k] = v
			}
			for k, v := range tt.env {
				env[k] = v
			}
			_, err := Load(nil, rawEnvOf(env), io.Discard)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want it to contain %q", err, tt.want)
			}
		})
	}

	// A minimal, valid basic configuration works.
	env := map[string]string{}
	for k, v := range base {
		env[k] = v
	}
	env["NOPS_AUTH_MODE"] = "basic"
	env["NOPS_USERS_FILE"] = usersFile
	if _, err := Load(nil, rawEnvOf(env), io.Discard); err != nil {
		t.Errorf("minimal basic configuration: %v", err)
	}
}

func TestLoadReportsEveryError(t *testing.T) {
	_, err := Load([]string{"-apply-timeout=0s", "-log-level=loud"}, envOf(map[string]string{"NOPS_DB_PATH": ""}), io.Discard)
	if err == nil {
		t.Fatal("no error")
	}
	for _, want := range []string{"-git-url or a repository block in -config is required", "flag -apply-timeout:", "flag -log-level:"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestLoadArguments(t *testing.T) {
	env := envOf(map[string]string{"NOPS_GIT_URL": repo})

	var out strings.Builder
	if _, err := Load([]string{"-h"}, env, &out); !errors.Is(err, flag.ErrHelp) {
		t.Errorf("-h: err = %v, want flag.ErrHelp", err)
	}
	for _, o := range options {
		if !strings.Contains(out.String(), "-"+o.name+", "+o.env()) {
			t.Errorf("usage does not list -%s with %s", o.name, o.env())
		}
	}

	if _, err := Load([]string{"-git-token=x"}, env, io.Discard); err == nil || !strings.Contains(err.Error(), "not defined") {
		t.Errorf("unknown flag: err = %v", err)
	}
	if _, err := Load([]string{"extra"}, env, io.Discard); err == nil || !strings.Contains(err.Error(), `unexpected argument "extra"`) {
		t.Errorf("positional argument: err = %v", err)
	}

	// A bool flag works without a value.
	c, err := Load([]string{"-nomad-tls-skip-verify"}, env, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !c.NomadTLSSkipVerify {
		t.Error("-nomad-tls-skip-verify without value did not set it")
	}
	// ...and an explicit false beats a true variable.
	c, err = Load([]string{"-nomad-tls-skip-verify=false"}, envOf(map[string]string{"NOPS_GIT_URL": repo, "NOPS_NOMAD_TLS_SKIP_VERIFY": "true"}), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.NomadTLSSkipVerify {
		t.Error("-nomad-tls-skip-verify=false did not win over the variable")
	}
}

// The completion offers files for a flag whose usage quotes `file`, as the
// flag package reads it; -h prints the usage without the quotes.
func TestFlagsThatTakeAFileQuoteIt(t *testing.T) {
	var out strings.Builder
	if _, err := Load([]string{"-h"}, rawEnvOf(nil), &out); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("-h: err = %v, want flag.ErrHelp", err)
	}
	if strings.Contains(out.String(), "`") {
		t.Error("the usage prints the quotes of a value name")
	}
	file := map[string]bool{"config": true, "db-path": true, "nomad-ca-cert": true, "nomad-client-cert": true, "nomad-client-key": true}
	FlagSet(io.Discard).VisitAll(func(f *flag.Flag) {
		name, _ := flag.UnquoteUsage(f)
		if want := file[f.Name] || strings.HasSuffix(f.Name, "-file"); (name == "file") != want {
			t.Errorf("-%s: its value is %q, a file is %v", f.Name, name, want)
		}
	})
}

// The server's flags are those of "nops serve": -version belongs to the root
// command, so Load does not know it, and the usage says how to start it.
func TestLoadIsTheServeCommand(t *testing.T) {
	if _, err := Load([]string{"-version"}, rawEnvOf(nil), io.Discard); err == nil || !strings.Contains(err.Error(), "not defined") {
		t.Errorf("-version: err = %v, want an undefined flag", err)
	}
	var out strings.Builder
	_, _ = Load([]string{"-h"}, rawEnvOf(nil), &out)
	if !strings.HasPrefix(out.String(), "Usage: nops serve [flags]") {
		t.Errorf("usage starts %q, want it to name nops serve", out.String())
	}
}

// TestNoOptionTakesAClientVariable keeps the client's variables to the client:
// the server reads each flag as NOPS_<FLAG>, so a flag named like one of them
// would be set by the environment of whoever runs the client.
func TestNoOptionTakesAClientVariable(t *testing.T) {
	for _, o := range options {
		for _, v := range cli.Variables {
			if o.env() == v {
				t.Errorf("option -%s reads %s, which the client reads too", o.name, o.env())
			}
		}
	}
	for _, sv := range secretValues {
		for _, v := range cli.Variables {
			if sv.envVar == v {
				t.Errorf("the secret variable %s is one of the client's", v)
			}
		}
	}
}

func TestOptionsTable(t *testing.T) {
	seen := make(map[string]bool)
	for _, o := range options {
		if o.name == "" || o.usage == "" || o.set == nil {
			t.Errorf("option %+v is incomplete", o)
		}
		if seen[o.name] {
			t.Errorf("option %s is declared twice", o.name)
		}
		seen[o.name] = true
		if !strings.HasPrefix(o.env(), "NOPS_") || strings.Contains(o.env(), "-") {
			t.Errorf("option %s has env %s", o.name, o.env())
		}
	}
}

// TestDocsListEveryOption keeps docs/configuration.md aligned with the table.
func TestDocsListEveryOption(t *testing.T) {
	b, err := os.ReadFile("../../docs/configuration.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(b)
	for _, o := range options {
		for _, s := range []string{"`-" + o.name + "`", "`" + o.env() + "`"} {
			if !strings.Contains(doc, s) {
				t.Errorf("docs/configuration.md does not mention %s", s)
			}
		}
	}
}

func TestNomadIgnoresNomadEnv(t *testing.T) {
	t.Setenv("NOMAD_ADDR", "http://leak.example.com:4646")
	t.Setenv("NOMAD_NAMESPACE", "leak")
	t.Setenv("NOMAD_REGION", "leak")
	t.Setenv("NOMAD_TOKEN", "leak")
	t.Setenv("NOMAD_CACERT", "/leak/ca.pem")
	t.Setenv("NOMAD_SKIP_VERIFY", "true")

	c, err := Load([]string{"-git-url", repo}, envOf(nil), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	cfg := c.Nomad()
	want := &api.Config{
		Address:   "http://127.0.0.1:4646",
		TLSConfig: &api.TLSConfig{},
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("Nomad() = %+v, want %+v", cfg, want)
	}
	client, err := api.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := client.Address(); got != "http://127.0.0.1:4646" {
		t.Errorf("client address = %s, NOMAD_ADDR leaked", got)
	}
}

func TestOldNamespaceVariableIsRejected(t *testing.T) {
	_, err := Load([]string{"-git-url", repo}, envOf(map[string]string{"NOPS_NOMAD_NAMESPACE": "apps"}), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "NOPS_NOMAD_NAMESPACES") {
		t.Fatalf("err = %v, want one naming NOPS_NOMAD_NAMESPACES", err)
	}
	if _, err := Load([]string{"-git-url", repo, "-nomad-namespace", "apps"}, envOf(nil), io.Discard); err == nil {
		t.Error("the old -nomad-namespace flag was accepted")
	}
}

func TestNomadCarriesSettings(t *testing.T) {
	c := &Config{
		NomadAddr:          "https://nomad.example.com:4646",
		NomadNamespaces:    []string{"apps", "infra"},
		NomadToken:         "secret",
		NomadCACert:        "/ca.pem",
		NomadClientCert:    "/cert.pem",
		NomadClientKey:     "/key.pem",
		NomadTLSSkipVerify: true,
	}
	want := &api.Config{
		Address:   "https://nomad.example.com:4646",
		SecretID:  "secret",
		TLSConfig: &api.TLSConfig{CACert: "/ca.pem", ClientCert: "/cert.pem", ClientKey: "/key.pem", Insecure: true},
	}
	if got := c.Nomad(); !reflect.DeepEqual(got, want) {
		t.Errorf("Nomad() = %+v, want %+v", got, want)
	}
}

func TestSecretValueFallback(t *testing.T) {
	tests := []struct {
		envVar string
		value  string
		get    func(*Config) string
		extra  map[string]string // other env needed for a cross-check (e.g. a URL for a token)
	}{
		{"NOPS_GIT_TOKEN", "plain-git-token", func(c *Config) string { return c.GitToken }, nil},
		{"NOPS_NOMAD_TOKEN", "plain-nomad-token", func(c *Config) string { return c.NomadToken }, nil},
		{"NOPS_OIDC_CLIENT_SECRET", "plain-oidc-secret", func(c *Config) string { return c.OIDCClientSecret }, nil},
	}
	for _, tt := range tests {
		t.Run(tt.envVar, func(t *testing.T) {
			env := map[string]string{"NOPS_GIT_URL": repo, tt.envVar: tt.value}
			for k, v := range tt.extra {
				env[k] = v
			}
			c, err := Load(nil, envOf(env), io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			if got := tt.get(c); got != tt.value {
				t.Errorf("%s = %q, want %q", tt.envVar, got, tt.value)
			}
		})
	}
}

func TestSecretValueFallbackLosesToFile(t *testing.T) {
	tok := writeFile(t, "git-token-file", "from-file")
	env := map[string]string{"NOPS_GIT_URL": repo, "NOPS_GIT_TOKEN_FILE": tok, "NOPS_GIT_TOKEN": "from-plain-env"}
	c, err := Load(nil, envOf(env), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.GitToken != "from-file" {
		t.Errorf("GitToken = %q, want the file to win", c.GitToken)
	}

	// A flag file also wins over the plain env var.
	c, err = Load([]string{"-git-url", repo, "-git-token-file", tok}, envOf(map[string]string{"NOPS_GIT_TOKEN": "from-plain-env"}), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.GitToken != "from-file" {
		t.Errorf("GitToken = %q, want the flag file to win", c.GitToken)
	}
}

func TestSecretValueFallbackInvalid(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"whitespace only", map[string]string{"NOPS_GIT_TOKEN": "   "}, "NOPS_GIT_TOKEN: is empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := map[string]string{"NOPS_GIT_URL": repo}
			for k, v := range tt.env {
				env[k] = v
			}
			_, err := Load(nil, envOf(env), io.Discard)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want %q", err, tt.want)
			}
			if strings.Contains(fmt.Sprint(err), "secret") && strings.Contains(err.Error(), "discord.com/api/webhooks/1/secret") {
				t.Errorf("error echoes the URL: %v", err)
			}
		})
	}
}

func TestSecretValuesAreDocumented(t *testing.T) {
	b, err := os.ReadFile("../../docs/configuration.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(b)
	for _, sv := range secretValues {
		if !strings.Contains(doc, sv.envVar) {
			t.Errorf("docs/configuration.md does not mention %s", sv.envVar)
		}
	}
}

func TestGitPath(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", ""},
		{".", ""},
		{"jobs", "jobs"},
		{"jobs/apps", "jobs/apps"},
		{"jobs/./apps/", "jobs/apps"},
		{"jobs/apps/", "jobs/apps"},
	}
	for _, tt := range tests {
		got, err := gitPath(tt.in)
		if err != nil {
			t.Errorf("gitPath(%q) error: %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("gitPath(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// docOptionRow matches a row of the option tables of docs/configuration.md:
// | `-flag` | `NOPS_FLAG` | default | meaning |
var docOptionRow = regexp.MustCompile("^\\| `-([a-z0-9-]+)` \\| `(NOPS_[A-Z0-9_]+)` \\| ([^|]*?) \\|")

// docDefault reads the default cell of an option row: a value in backticks is
// the default itself; "none", "**required**" (with or without "none,") and
// "derived from ..." stand for an option with no default of its own.
func docDefault(cell string) (string, bool) {
	if strings.HasPrefix(cell, "`") {
		if end := strings.Index(cell[1:], "`"); end >= 0 {
			return cell[1 : 1+end], true
		}
		return "", false
	}
	for _, none := range []string{"none", "**required**", "derived from"} {
		if strings.HasPrefix(cell, none) {
			return "", true
		}
	}
	return "", false
}

// TestDocTablesMatchTheOptions checks the option tables of
// docs/configuration.md against the options table, in both directions: every
// option has a row, every row is an option, and the variable and the default
// of a row are the option's. (Which options are required is not checked: it
// depends on the other options, see check().)
func TestDocTablesMatchTheOptions(t *testing.T) {
	b, err := os.ReadFile("../../docs/configuration.md")
	if err != nil {
		t.Fatal(err)
	}
	byName := make(map[string]option, len(options))
	for _, o := range options {
		byName[o.name] = o
	}
	documented := map[string]bool{}
	for _, line := range strings.Split(string(b), "\n") {
		m := docOptionRow.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		name, env, cell := m[1], m[2], m[3]
		o, ok := byName[name]
		switch {
		case !ok:
			t.Errorf("docs/configuration.md documents -%s, which is not an option", name)
			continue
		case documented[name]:
			t.Errorf("docs/configuration.md documents -%s twice", name)
		}
		documented[name] = true
		if env != o.env() {
			t.Errorf("docs/configuration.md gives -%s the variable %s, the option has %s", name, env, o.env())
		}
		def, ok := docDefault(cell)
		if !ok {
			t.Errorf("docs/configuration.md: the default of -%s (%q) is not a value in backticks, \"none\", \"**required**\" or \"derived from ...\"", name, cell)
		} else if def != o.def {
			t.Errorf("docs/configuration.md gives -%s the default %q, the option has %q", name, def, o.def)
		}
	}
	for _, o := range options {
		if !documented[o.name] {
			t.Errorf("docs/configuration.md has no table row for -%s", o.name)
		}
	}
}

// TestDocSecretsTableMatchesTheSecretValues checks the "secrets without a file"
// table of docs/configuration.md against secretValues, in both directions.
func TestDocSecretsTableMatchesTheSecretValues(t *testing.T) {
	b, err := os.ReadFile("../../docs/configuration.md")
	if err != nil {
		t.Fatal(err)
	}
	row := regexp.MustCompile("^\\| `(NOPS_[A-Z0-9_]+_FILE)` \\(`-([a-z0-9-]+)`\\) \\| `(NOPS_[A-Z0-9_]+)` \\|")
	byName := make(map[string]option, len(options))
	for _, o := range options {
		byName[o.name] = o
	}
	documented := map[string]bool{}
	for _, line := range strings.Split(string(b), "\n") {
		m := row.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		fileVar, flagName, plain := m[1], m[2], m[3]
		documented[plain] = true
		if o, ok := byName[flagName]; !ok || o.env() != fileVar {
			t.Errorf("docs/configuration.md: %s (-%s) is not an option and its variable", fileVar, flagName)
		}
		if plain != strings.TrimSuffix(fileVar, "_FILE") {
			t.Errorf("docs/configuration.md: the plain variable of %s is %s, want %s", fileVar, plain, strings.TrimSuffix(fileVar, "_FILE"))
		}
	}
	for _, sv := range secretValues {
		if !documented[sv.envVar] {
			t.Errorf("docs/configuration.md has no row for %s in the secrets table", sv.envVar)
		}
		delete(documented, sv.envVar)
	}
	for extra := range documented {
		t.Errorf("docs/configuration.md lists %s as a secret variable, which is not one", extra)
	}
}

// TestLoadSyncWindowTimeZone: a sync window is read in UTC unless the operator
// names a zone, and what is named is an IANA zone that Load resolves, so a
// typo stops the start rather than shifting every window.
func TestLoadSyncWindowTimeZone(t *testing.T) {
	c, err := Load([]string{"-git-url", repo}, envOf(nil), io.Discard)
	if err != nil || c.SyncWindowLocation != time.UTC {
		t.Fatalf("default = %v, %v, want UTC", c.SyncWindowLocation, err)
	}
	if _, err := time.LoadLocation("Europe/Rome"); err != nil {
		t.Skipf("no tz database: %v", err)
	}
	c, err = Load([]string{"-git-url", repo, "-sync-window-time-zone", "Europe/Rome"}, envOf(nil), io.Discard)
	if err != nil || c.SyncWindowLocation.String() != "Europe/Rome" {
		t.Errorf("-sync-window-time-zone Europe/Rome = %v, %v", c.SyncWindowLocation, err)
	}
}

func TestLoadRepositoriesFromTheFile(t *testing.T) {
	tok := writeFile(t, "apps-token", "apps-secret\n")
	file := writeFile(t, "nops.hcl", `
repository "apps" {
  url        = "https://git.example.com/ops/apps.git"
  branch     = "prod"
  path       = "./nomad/"
  username   = "oauth2"
  token_file = "`+tok+`"
  timeout    = "5m"
}

repository "default" {
  url = "https://git.example.com/ops/jobs.git"
}
`)
	c, err := Load([]string{"-config", file}, envOf(nil), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	want := []Repository{
		{Name: "apps", URL: "https://git.example.com/ops/apps.git", Branch: "prod", Path: "nomad", Username: "oauth2", TokenFile: tok, Token: "apps-secret", Timeout: 5 * time.Minute},
		{Name: "default", URL: "https://git.example.com/ops/jobs.git", Branch: "main", Username: "git", Timeout: 2 * time.Minute},
	}
	if !reflect.DeepEqual(c.Repositories, want) {
		t.Errorf("repositories:\n got %+v\nwant %+v", c.Repositories, want)
	}
}

func TestLoadRepositoryJobs(t *testing.T) {
	file := writeFile(t, "nops.hcl", `
repository "apps" {
  url  = "https://git.example.com/ops/apps.git"
  jobs = ["labs/billing-api", "lab*/billing-api-*", "crm"]
}

repository "infra" {
  url = "https://git.example.com/ops/infra.git"
}
`)
	c, err := Load([]string{"-config", file, "-nomad-namespaces", "default,labs"}, envOf(nil), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"labs/billing-api", "lab*/billing-api-*", "default/crm"}
	if !reflect.DeepEqual(c.Repositories[0].Jobs, want) {
		t.Errorf("jobs = %q, want %q", c.Repositories[0].Jobs, want)
	}
	if c.Repositories[1].Jobs != nil {
		t.Errorf("a repository without jobs has %q, want nil: any job", c.Repositories[1].Jobs)
	}
}

func TestLoadRepositoriesInvalid(t *testing.T) {
	dir := t.TempDir()
	tok := writeFile(t, "token", "secret")
	block := func(name, body string) string { return "repository \"" + name + "\" {\n" + body + "\n}\n" }
	apps := block("apps", `url = "https://git.example.com/ops/apps.git"`)
	tests := []struct {
		name string
		file string // "" for no -config
		args []string
		env  map[string]string
		want string
	}{
		{"no repository", "", nil, nil, "-git-url or a repository block in -config is required"},
		{"an empty file", " ", nil, nil, "-git-url or a repository block in -config is required"},
		{"a -git-* option without -git-url", apps, []string{"-git-branch=prod"}, nil, "-git-branch is set without -git-url"},
		{"a -git-* variable without -git-url", apps, nil, map[string]string{"NOPS_GIT_PATH": "jobs"}, "-git-path is set without -git-url"},
		{"a git token without -git-url", apps, nil, map[string]string{"NOPS_GIT_TOKEN": "secret"}, "NOPS_GIT_TOKEN is set without -git-url"},
		{"a git token file without -git-url", apps, []string{"-git-token-file=" + tok}, nil, "-git-token-file is set without -git-url"},
		{"a git timeout without -git-url", apps, []string{"-git-timeout=1m"}, nil, "-git-timeout is set without -git-url"},
		{"default next to -git-url", block("default", `url = "https://git.example.com/x.git"`), []string{"-git-url=" + repo}, nil, `repository "default" is the one of -git-url`},
		{"a name twice", apps + apps, nil, nil, `repository "apps" is declared twice`},
		{"a bad name", block("my apps", `url = "https://git.example.com/x.git"`), nil, nil, "the name may hold only letters, digits, - and _"},
		{"no url", block("apps", `branch = "main"`), nil, nil, `Missing required argument`},
		{"an empty url", block("apps", `url = ""`), nil, nil, `repository "apps": url: required`},
		{"an empty branch", block("apps", `url = "https://git.example.com/x.git"`+"\n"+`branch = ""`), nil, nil, `repository "apps": branch: required`},
		{"a path outside", block("apps", `url = "https://git.example.com/x.git"`+"\n"+`path = "../x"`), nil, nil, `repository "apps": path: "../x" must be a relative path`},
		{"a missing token file", block("apps", `url = "https://git.example.com/x.git"`+"\n"+`token_file = "`+filepath.Join(dir, "missing")+`"`), nil, nil, `repository "apps": token_file: open`},
		{"a token over http", block("apps", `url = "http://git.example.com/x.git"`+"\n"+`token_file = "`+tok+`"`), nil, nil, `a git token needs an https:// url in repository "apps"`},
		{"a token without username", block("apps", `url = "https://git.example.com/x.git"`+"\n"+`username = ""`+"\n"+`token_file = "`+tok+`"`), nil, nil, `a git token needs a non-empty username in repository "apps"`},
		{"an empty jobs", block("apps", `url = "https://git.example.com/x.git"`+"\n"+`jobs = []`), nil, nil, `repository "apps": jobs: is empty`},
		{"a jobs entry without an ID", block("apps", `url = "https://git.example.com/x.git"`+"\n"+`jobs = ["default/"]`), nil, nil, `jobs: "default/" must be <namespace>/<job ID>`},
		{"a jobs entry without a namespace", block("apps", `url = "https://git.example.com/x.git"`+"\n"+`jobs = ["/api"]`), nil, nil, `jobs: "/api" must be <namespace>/<job ID>`},
		{"an empty jobs entry", block("apps", `url = "https://git.example.com/x.git"`+"\n"+`jobs = [""]`), nil, nil, `jobs: "" must be <namespace>/<job ID>`},
		{"a malformed jobs pattern", block("apps", `url = "https://git.example.com/x.git"`+"\n"+`jobs = ["default/api-["]`), nil, nil, `jobs: "default/api-[": syntax error in pattern`},
		{"a jobs namespace that is not managed", block("apps", `url = "https://git.example.com/x.git"`+"\n"+`jobs = ["labs/api"]`), nil, nil, `jobs: "labs/api" matches no managed namespace (default)`},
		{"an unknown attribute", block("apps", `url = "https://git.example.com/x.git"`+"\n"+`token = "secret"`), nil, nil, "Unsupported argument"},
		{"a timeout that is not a duration", block("apps", `url = "https://git.example.com/x.git"`+"\n"+`timeout = "soon"`), nil, nil, `repository "apps": timeout:`},
		{"a zero timeout", block("apps", `url = "https://git.example.com/x.git"`+"\n"+`timeout = "0s"`), nil, nil, `repository "apps": timeout: duration "0s" must be positive`},
		{"an unknown block", apps + "server {\n}\n", nil, nil, "Unsupported block type"},
		{"not HCL", "repository {", nil, nil, "-config: "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := tt.args
			if tt.file != "" {
				args = append([]string{"-config", writeFile(t, "nops.hcl", tt.file)}, args...)
			}
			_, err := Load(args, envOf(tt.env), io.Discard)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want one containing %q", err, tt.want)
			}
		})
	}
}

func TestLoadListenFromTheFile(t *testing.T) {
	file := writeFile(t, "nops.hcl", `
repository "apps" {
  url = "https://git.example.com/ops/apps.git"
}

listen {
  read_header_timeout = "2s"
  write_timeout       = "5m"
}
`)
	c, err := Load([]string{"-config", file}, envOf(nil), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	// The attributes the block leaves out keep their defaults.
	got := [4]time.Duration{c.ReadHeaderTimeout, c.ReadTimeout, c.WriteTimeout, c.IdleTimeout}
	want := [4]time.Duration{2 * time.Second, time.Minute, 5 * time.Minute, 2 * time.Minute}
	if got != want {
		t.Errorf("timeouts = %v, want %v", got, want)
	}
}

func TestLoadListenInvalid(t *testing.T) {
	apps := `repository "apps" {
  url = "https://git.example.com/ops/apps.git"
}
`
	listen := func(body string) string { return apps + "listen {\n" + body + "\n}\n" }
	tests := []struct {
		name, file, want string
	}{
		{"not a duration", listen(`read_timeout = "soon"`), "listen: read_timeout:"},
		{"zero", listen(`idle_timeout = "0s"`), `listen: idle_timeout: duration "0s" must be positive`},
		{"negative", listen(`write_timeout = "-1m"`), `listen: write_timeout: duration "-1m" must be positive`},
		{"a number", listen(`read_header_timeout = 10`), "read_header_timeout"},
		{"the address belongs to a flag", listen(`addr = ":9090"`), "Unsupported argument"},
		{"two blocks", listen("") + listen(""), "Duplicate listen block"},
		{"tls with no files", listen("tls {\n}"), `Missing required argument`},
		{"tls with a certificate alone", listen("tls {\ncert_file = \"nops.crt\"\n}"), `"key_file" is required`},
		{"tls with a key alone", listen("tls {\nkey_file = \"nops.key\"\n}"), `"cert_file" is required`},
		{"two tls blocks", listen("tls {\ncert_file = \"a\"\nkey_file = \"b\"\n}\ntls {\ncert_file = \"a\"\nkey_file = \"b\"\n}"), "Duplicate tls block"},
		{"a certificate that is not there", listen(`tls {
  cert_file = "/nonexistent/nops.crt"
  key_file  = "/nonexistent/nops.key"
}`), "listen: tls: cert_file:"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load([]string{"-config", writeFile(t, "nops.hcl", tt.file)}, envOf(nil), io.Discard)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want one containing %q", err, tt.want)
			}
		})
	}
}

func TestLoadListenTLS(t *testing.T) {
	noPublicURL := envOf(map[string]string{"NOPS_PUBLIC_URL": ""}) // so the URL is derived
	cert, key := writeFile(t, "nops.crt", "cert"), writeFile(t, "nops.key", "key")
	file := writeFile(t, "nops.hcl", `
repository "apps" {
  url = "https://git.example.com/ops/apps.git"
}

listen {
  tls {
    cert_file = "`+cert+`"
    key_file  = "`+key+`"
  }
}
`)
	c, err := Load([]string{"-config", file, "-listen-addr", "127.0.0.1:9443"}, noPublicURL, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.TLSCertFile != cert || c.TLSKeyFile != key {
		t.Errorf("TLS files = %q, %q; want %q, %q", c.TLSCertFile, c.TLSKeyFile, cert, key)
	}
	if c.PublicURL != "https://127.0.0.1:9443" {
		t.Errorf("public URL = %q, want the https one", c.PublicURL)
	}

	// Without the block, Nops serves plain HTTP and the URL says so.
	plain, err := Load([]string{"-config", writeFile(t, "plain.hcl", "repository \"apps\" {\n  url = \"https://git.example.com/ops/apps.git\"\n}\n")}, noPublicURL, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if plain.TLSCertFile != "" || plain.TLSKeyFile != "" || plain.PublicURL != "http://localhost:8080" {
		t.Errorf("without a tls block: files %q, %q, public URL %q", plain.TLSCertFile, plain.TLSKeyFile, plain.PublicURL)
	}
}

func TestLoadNomadFromTheFile(t *testing.T) {
	file := writeFile(t, "nops.hcl", `
repository "apps" {
  url = "https://git.example.com/ops/apps.git"
}

nomad {
  timeout = "2m"
}
`)
	c, err := Load([]string{"-config", file}, envOf(nil), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.NomadTimeout != 2*time.Minute {
		t.Errorf("NomadTimeout = %v, want 2m", c.NomadTimeout)
	}
}

func TestLoadNomadInvalid(t *testing.T) {
	apps := `repository "apps" {
  url = "https://git.example.com/ops/apps.git"
}
`
	nomad := func(body string) string { return apps + "nomad {\n" + body + "\n}\n" }
	tests := []struct {
		name, file, want string
	}{
		{"not a duration", nomad(`timeout = "soon"`), "nomad: timeout:"},
		{"zero", nomad(`timeout = "0s"`), `nomad: timeout: duration "0s" must be positive`},
		{"negative", nomad(`timeout = "-1m"`), `nomad: timeout: duration "-1m" must be positive`},
		{"a number", nomad(`timeout = 30`), "timeout"},
		{"the address belongs to a flag", nomad(`addr = "http://nomad:4646"`), "Unsupported argument"},
		{"two blocks", nomad("") + nomad(""), "Duplicate nomad block"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load([]string{"-config", writeFile(t, "nops.hcl", tt.file)}, envOf(nil), io.Discard)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want one containing %q", err, tt.want)
			}
		})
	}
}

// basicAuthArgs is the command line of a basic login, besides the -config file.
func basicAuthArgs(t *testing.T, file string) []string {
	t.Helper()
	usersFile := writeFile(t, "users", "alice:hash\n")
	return []string{"-config", file, "-auth-mode", "basic", "-users-file", usersFile}
}

func TestLoginLimitDefaults(t *testing.T) {
	file := writeFile(t, "nops.hcl", `
repository "apps" {
  url = "https://git.example.com/ops/apps.git"
}
`)
	c, err := Load(basicAuthArgs(t, file), rawEnvOf(nil), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.LoginMaxFailures != 10 || c.LoginFailureWindow != 15*time.Minute {
		t.Errorf("limit = %d in %v, want 10 in 15m", c.LoginMaxFailures, c.LoginFailureWindow)
	}
}

func TestLoadAuthFromTheFile(t *testing.T) {
	apps := `repository "apps" {
  url = "https://git.example.com/ops/apps.git"
}
`
	tests := []struct {
		name, block string
		wantMax     int
		wantWindow  time.Duration
	}{
		{"both", `max_failures = 3
  failure_window = "1h"`, 3, time.Hour},
		{"only the window", `failure_window = "5m"`, 10, 5 * time.Minute},
		{"only the count", `max_failures = 20`, 20, 15 * time.Minute},
		{"neither", ``, 10, 15 * time.Minute},
		{"a zero count turns the limit off", `max_failures = 0`, 0, 15 * time.Minute},
		{"a zero window turns the limit off", `failure_window = "0s"`, 10, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file := writeFile(t, "nops.hcl", apps+"auth \"basic\" {\n  type = \"basic\"\n  "+tt.block+"\n}\n")
			c, err := Load(basicAuthArgs(t, file), rawEnvOf(nil), io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			if c.LoginMaxFailures != tt.wantMax || c.LoginFailureWindow != tt.wantWindow {
				t.Errorf("limit = %d in %v, want %d in %v", c.LoginMaxFailures, c.LoginFailureWindow, tt.wantMax, tt.wantWindow)
			}
		})
	}
}

func TestLoadAuthInvalid(t *testing.T) {
	apps := `repository "apps" {
  url = "https://git.example.com/ops/apps.git"
}
`
	auth := func(label, body string) string { return apps + "auth \"" + label + "\" {\n" + body + "\n}\n" }
	basic := func(body string) string { return auth("basic", "type = \"basic\"\n"+body) }
	tests := []struct {
		name, file, want string
	}{
		{"no type", auth("basic", ""), "type"},
		{"another type", auth("basic", `type = "oidc"`), `auth: type: is "oidc"`},
		{"another label", auth("oidc", `type = "basic"`), `auth: the label is "oidc"`},
		{"two blocks", basic("") + basic(""), "only one auth block is allowed"},
		{"negative count", basic(`max_failures = -1`), "auth: max_failures: -1 must not be negative"},
		{"a count that is not a number", basic(`max_failures = "many"`), "a number is required"},
		{"not a duration", basic(`failure_window = "soon"`), "auth: failure_window:"},
		{"negative window", basic(`failure_window = "-1m"`), `auth: failure_window: duration "-1m" must not be negative`},
		{"the users file is a flag", basic(`users_file = "/secrets/users"`), "Unsupported argument"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(basicAuthArgs(t, writeFile(t, "nops.hcl", tt.file)), rawEnvOf(nil), io.Discard)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want one containing %q", err, tt.want)
			}
		})
	}
}

// The block belongs to the basic login: with OIDC it stops Nops, rather than
// sitting in the file with no effect.
func TestLoadAuthBlockWithOIDC(t *testing.T) {
	file := writeFile(t, "nops.hcl", `
repository "apps" {
  url = "https://git.example.com/ops/apps.git"
}

auth "basic" {
  type = "basic"
}
`)
	_, err := Load([]string{"-config", file}, envOf(nil), io.Discard)
	if err == nil || !strings.Contains(err.Error(), `the "basic" block is only used with -auth-mode=basic`) {
		t.Errorf("err = %v, want the block refused with -auth-mode=oidc", err)
	}
}

func TestLoadNotifyFromTheFile(t *testing.T) {
	webhookURL := writeFile(t, "webhook-url", "https://n8n.example.com/webhook/abc\n")
	webhookTok := writeFile(t, "webhook-token", "webhook-secret")
	discordURL := writeFile(t, "discord-url", "https://discord.com/api/webhooks/1/abc\n")
	teamURL := writeFile(t, "team-url", "https://hooks.slack.com/services/T/B/team")
	alertsURL := writeFile(t, "alerts-url", "https://hooks.slack.com/services/T/B/alerts")
	ntfyTok := writeFile(t, "ntfy-token", "ntfy-secret")
	gotifyTok := writeFile(t, "gotify-token", "gotify-secret")
	file := writeFile(t, "nops.hcl", `
repository "apps" {
  url = "https://git.example.com/ops/apps.git"
}

notify "webhook" {
  type       = "webhook"
  url_file   = "`+webhookURL+`"
  token_file = "`+webhookTok+`"
  timeout    = "3s"
}

notify "discord" {
  type     = "discord"
  url_file = "`+discordURL+`"
}

notify "team" {
  type     = "slack"
  url_file = "`+teamURL+`"
}

notify "alerts" {
  type     = "slack"
  url_file = "`+alertsURL+`"
}

notify "ntfy" {
  type       = "ntfy"
  url        = "https://ntfy.example.com/nops"
  token_file = "`+ntfyTok+`"
}

notify "gotify" {
  type       = "gotify"
  url        = "https://gotify.example.com/"
  token_file = "`+gotifyTok+`"
}
`)
	c, err := Load([]string{"-config", file}, envOf(nil), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	want := []Notify{
		{Name: "webhook", Type: "webhook", URL: "https://n8n.example.com/webhook/abc", Token: "webhook-secret", Timeout: 3 * time.Second},
		{Name: "discord", Type: "discord", URL: "https://discord.com/api/webhooks/1/abc", Timeout: 10 * time.Second},
		{Name: "team", Type: "slack", URL: "https://hooks.slack.com/services/T/B/team", Timeout: 10 * time.Second},
		{Name: "alerts", Type: "slack", URL: "https://hooks.slack.com/services/T/B/alerts", Timeout: 10 * time.Second},
		{Name: "ntfy", Type: "ntfy", URL: "https://ntfy.example.com/nops", Token: "ntfy-secret", Timeout: 10 * time.Second},
		{Name: "gotify", Type: "gotify", URL: "https://gotify.example.com", Token: "gotify-secret", Timeout: 10 * time.Second},
	}
	if !reflect.DeepEqual(c.Notify, want) {
		t.Errorf("Notify\n got %+v\nwant %+v", c.Notify, want)
	}

	// Without a block, no adapter is on.
	c, err = Load([]string{"-git-url", repo}, envOf(nil), io.Discard)
	if err != nil || len(c.Notify) != 0 {
		t.Errorf("without a block: Notify = %v, %v", c.Notify, err)
	}
}

func TestLoadNotifyInvalid(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing")
	empty := writeFile(t, "empty", " \n")
	notURL := writeFile(t, "not-url", "discord.com/api/webhooks/1/do-not-print")
	tok := writeFile(t, "token", "secret")
	urlFile := writeFile(t, "url", "https://hooks.slack.com/services/T/B/abc")
	apps := `repository "apps" {
  url = "https://git.example.com/ops/apps.git"
}
`
	notify := func(label, body string) string { return "notify \"" + label + "\" {\n" + body + "\n}\n" }
	slack := notify("slack", `type = "slack"
url_file = "`+urlFile+`"`)
	tests := []struct {
		name, file, want string
	}{
		{"no type", notify("slack", `url_file = "`+urlFile+`"`), `Missing required argument`},
		{"unknown type", notify("pager", `type = "pagerduty"`), `notify "pager": type: is "pagerduty", must be webhook, discord, slack, ntfy, or gotify`},
		{"a bad name", notify("my slack", `type = "slack"
url_file = "`+urlFile+`"`), `notify "my slack": the name may hold only letters, digits, - and _`},
		{"a name used twice", slack + slack, `notify "slack": is declared twice`},
		{"no url_file", notify("slack", `type = "slack"`), `notify "slack": url_file: required`},
		{"no url", notify("ntfy", `type = "ntfy"`), `notify "ntfy": url: required`},
		{"gotify without a token", notify("gotify", `type = "gotify"
url = "https://gotify.example.com"`), `notify "gotify": token_file: required`},
		{"a url for a url_file type", notify("slack", `type = "slack"
url = "https://hooks.slack.com/services/T/B/abc"`), `notify "slack": url: is not used by type "slack"`},
		{"a url_file for a url type", notify("ntfy", `type = "ntfy"
url_file = "`+urlFile+`"`), `notify "ntfy": url_file: is not used by type "ntfy"`},
		{"a token for discord", notify("discord", `type = "discord"
url_file = "`+urlFile+`"
token_file = "`+tok+`"`), `notify "discord": token_file: is not used by type "discord"`},
		{"a missing url_file", notify("slack", `type = "slack"
url_file = "`+missing+`"`), `notify "slack": url_file: open ` + missing},
		{"an empty url_file", notify("slack", `type = "slack"
url_file = "`+empty+`"`), `notify "slack": url_file: ` + empty + ` is empty`},
		{"a url_file without a URL", notify("discord", `type = "discord"
url_file = "`+notURL+`"`), `notify "discord": url_file: ` + notURL + ` does not hold an http`},
		{"a url that is not one", notify("ntfy", `type = "ntfy"
url = "ntfy.example.com/nops"`), `notify "ntfy": url: "ntfy.example.com/nops" is not an http`},
		{"a missing token_file", notify("ntfy", `type = "ntfy"
url = "https://ntfy.example.com/nops"
token_file = "`+missing+`"`), `notify "ntfy": token_file: open ` + missing},
		{"a timeout without a unit", notify("slack", `type = "slack"
url_file = "`+urlFile+`"
timeout = "10"`), `notify "slack": timeout: time: missing unit`},
		{"a zero timeout", notify("slack", `type = "slack"
url_file = "`+urlFile+`"
timeout = "0s"`), `notify "slack": timeout: duration "0s" must be positive`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load([]string{"-config", writeFile(t, "nops.hcl", apps+tt.file)}, envOf(nil), io.Discard)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want one containing %q", err, tt.want)
			}
			// A URL read from a file never shows up in an error: it may hold a token.
			if err != nil && strings.Contains(err.Error(), "do-not-print") {
				t.Errorf("err = %v, it prints the URL", err)
			}
		})
	}
}

// The -notify-* options moved into the notify block: each one, its variable,
// and the variable that held its secret's value stop Nops, naming the
// attribute that replaces it.
func TestLoadNotifyOptionsRemoved(t *testing.T) {
	tests := []struct {
		flag, valueEnv, want string
	}{
		{"notify-webhook-url-file", "NOPS_NOTIFY_WEBHOOK_URL", `url_file in a notify block of type "webhook"`},
		{"notify-webhook-token-file", "NOPS_NOTIFY_WEBHOOK_TOKEN", `token_file in a notify block of type "webhook"`},
		{"notify-discord-url-file", "NOPS_NOTIFY_DISCORD_URL", `url_file in a notify block of type "discord"`},
		{"notify-slack-url-file", "NOPS_NOTIFY_SLACK_URL", `url_file in a notify block of type "slack"`},
		{"notify-ntfy-url", "", `url in a notify block of type "ntfy"`},
		{"notify-ntfy-token-file", "NOPS_NOTIFY_NTFY_TOKEN", `token_file in a notify block of type "ntfy"`},
		{"notify-gotify-url", "", `url in a notify block of type "gotify"`},
		{"notify-gotify-token-file", "NOPS_NOTIFY_GOTIFY_TOKEN", `token_file in a notify block of type "gotify"`},
		{"notify-timeout", "", "timeout in each notify block"},
	}
	for _, tt := range tests {
		t.Run(tt.flag, func(t *testing.T) {
			vars := []string{removedOption{name: tt.flag}.env()}
			if tt.valueEnv != "" {
				vars = append(vars, tt.valueEnv)
			}
			_, err := Load([]string{"-git-url", repo, "-" + tt.flag, "x"}, envOf(nil), io.Discard)
			errs := map[string]error{"flag -" + tt.flag: err}
			for _, v := range vars {
				_, errs[v] = Load([]string{"-git-url", repo}, envOf(map[string]string{v: "x"}), io.Discard)
			}
			for source, err := range errs {
				if err == nil || !strings.Contains(err.Error(), source+" was removed") || !strings.Contains(err.Error(), tt.want) {
					t.Errorf("%s: err = %v, want it removed, replaced by %s", source, err, tt.want)
				}
			}
		})
	}
}

func TestLoadACLFromTheFile(t *testing.T) {
	bootstrap := secret.New()
	boot := writeFile(t, "bootstrap", bootstrap+"\n")
	file := writeFile(t, "nops.hcl", `
repository "apps" {
  url = "https://git.example.com/ops/apps.git"
}

acl {
  enabled              = true
  bootstrap_token_file = "`+boot+`"
}
`)
	c, err := Load([]string{"-config", file}, envOf(nil), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !c.ACL || c.ACLBootstrapTokenFile != boot || c.ACLBootstrapToken != bootstrap {
		t.Errorf("ACL = %v, file %q, token %q; want on, %q, the generated token", c.ACL, c.ACLBootstrapTokenFile, c.ACLBootstrapToken, boot)
	}

	// Without a block, or with an empty one, the ACL is off.
	for _, args := range [][]string{
		{"-git-url", repo},
		{"-git-url", repo, "-config", writeFile(t, "empty.hcl", "acl {}\n")},
	} {
		c, err := Load(args, envOf(nil), io.Discard)
		if err != nil || c.ACL || c.ACLBootstrapToken != "" {
			t.Errorf("%v: ACL = %v, token %q, %v; want it off", args, c != nil && c.ACL, c.ACLBootstrapToken, err)
		}
	}
}

func TestLoadACLInvalid(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	empty := writeFile(t, "empty", " \n")
	boot := writeFile(t, "bootstrap", secret.New())
	notGenerated := writeFile(t, "not-generated", "chosen-by-hand")
	acl := func(body string) string { return "acl {\n" + body + "\n}\n" }
	tests := []struct {
		name, file, want string
	}{
		{"enabled without a bootstrap token", acl(`enabled = true`), "acl: bootstrap_token_file: required with enabled = true"},
		{"a bootstrap token without enabled", acl(`bootstrap_token_file = "` + boot + `"`), "acl: bootstrap_token_file: is set without enabled = true"},
		{"a bootstrap token not generated", acl(`enabled = true
bootstrap_token_file = "` + notGenerated + `"`), `acl: bootstrap_token_file: the token is not a secret made by "nops secret generate"`},
		{"a missing file", acl(`enabled = true
bootstrap_token_file = "` + missing + `"`), "acl: bootstrap_token_file: open " + missing},
		{"an empty file", acl(`enabled = true
bootstrap_token_file = "` + empty + `"`), "acl: bootstrap_token_file: " + empty + " is empty"},
		{"enabled is a string", acl(`enabled = "yes"`), "enabled"},
		{"an unknown attribute", acl(`bootstrap_token = "x"`), "Unsupported argument"},
		{"two blocks", acl("") + acl(""), "Duplicate acl block"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load([]string{"-git-url", repo, "-config", writeFile(t, "nops.hcl", tt.file)}, envOf(nil), io.Discard)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want one containing %q", err, tt.want)
			}
		})
	}
}

// The -acl options moved into the acl block: each one, its variable, and the
// variable that held the bootstrap token stop Nops, naming the attribute that
// replaces it.
func TestLoadACLOptionsRemoved(t *testing.T) {
	boot := writeFile(t, "bootstrap", secret.New())
	tests := []struct {
		name string
		args []string
		env  map[string]string
		want string
	}{
		{"flag -acl", []string{"-acl"}, nil, "enabled in the acl block"},
		{"flag -acl", []string{"-acl=false"}, nil, "enabled in the acl block"},
		{"NOPS_ACL", nil, map[string]string{"NOPS_ACL": "true"}, "enabled in the acl block"},
		{"flag -acl-bootstrap-token-file", []string{"-acl-bootstrap-token-file", boot}, nil, "bootstrap_token_file in the acl block"},
		{"NOPS_ACL_BOOTSTRAP_TOKEN_FILE", nil, map[string]string{"NOPS_ACL_BOOTSTRAP_TOKEN_FILE": boot}, "bootstrap_token_file in the acl block"},
		{"NOPS_ACL_BOOTSTRAP_TOKEN", nil, map[string]string{"NOPS_ACL_BOOTSTRAP_TOKEN": secret.New()}, "bootstrap_token_file in the acl block"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(append([]string{"-git-url", repo}, tt.args...), envOf(tt.env), io.Discard)
			if err == nil || !strings.Contains(err.Error(), tt.name+" was removed") || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want %s removed, replaced by %s", err, tt.name, tt.want)
			}
		})
	}
}

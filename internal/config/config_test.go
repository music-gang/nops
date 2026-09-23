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
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"
)

const repo = "https://git.example.com/ops/jobs.git"

func envOf(m map[string]string) func(string) string {
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
		NomadNamespace:   "default",
		GitURL:           repo,
		GitBranch:        "main",
		GitUsername:      "git",
		DBPath:           "nops.db",
		ListenAddr:       ":8080",
		AuthHeader:       "Remote-User",
		NotifyTimeout:    10 * time.Second,
		GitPollInterval:  time.Minute,
		DriftInterval:    5 * time.Minute,
		EngineInterval:   5 * time.Second,
		HookPollInterval: 5 * time.Second,
		ApplyTimeout:     10 * time.Minute,
		LogLevel:         slog.LevelInfo,
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
func TestLoadEveryOption(t *testing.T) {
	ca := writeFile(t, "ca.pem", "ca")
	cert := writeFile(t, "cert.pem", "cert")
	key := writeFile(t, "key.pem", "key")
	nomadTok := writeFile(t, "nomad-token", "nomad-secret\n")
	gitTok := writeFile(t, "git-token", "  git-secret\n")
	webhookURL := writeFile(t, "webhook-url", "https://n8n.example.com/webhook/abc\n")
	webhookTok := writeFile(t, "webhook-token", "webhook-secret")
	discordURL := writeFile(t, "discord-url", "https://discord.com/api/webhooks/1/abc\n")
	slackURL := writeFile(t, "slack-url", "https://hooks.slack.com/services/T/B/abc")
	ntfyTok := writeFile(t, "ntfy-token", "ntfy-secret")
	gotifyTok := writeFile(t, "gotify-token", "gotify-secret")
	values := map[string]string{
		"nomad-addr":                "https://nomad.example.com:4646",
		"nomad-namespace":           "apps",
		"nomad-token-file":          nomadTok,
		"nomad-ca-cert":             ca,
		"nomad-client-cert":         cert,
		"nomad-client-key":          key,
		"nomad-tls-skip-verify":     "true",
		"git-url":                   repo,
		"git-branch":                "prod",
		"git-username":              "oauth2",
		"git-token-file":            gitTok,
		"db-path":                   "/var/lib/nops/nops.db",
		"listen-addr":               "127.0.0.1:9000",
		"auth-header":               "X-Forwarded-User",
		"notify-webhook-url-file":   webhookURL,
		"notify-webhook-token-file": webhookTok,
		"notify-discord-url-file":   discordURL,
		"notify-slack-url-file":     slackURL,
		"notify-ntfy-url":           "https://ntfy.example.com/nops",
		"notify-ntfy-token-file":    ntfyTok,
		"notify-gotify-url":         "https://gotify.example.com/",
		"notify-gotify-token-file":  gotifyTok,
		"notify-timeout":            "3s",
		"public-url":                "https://nops.example.com/",
		"git-poll-interval":         "2m",
		"drift-interval":            "15m",
		"engine-interval":           "7s",
		"hook-poll-interval":        "3s",
		"apply-timeout":             "1h",
		"log-level":                 "debug",
	}
	if len(values) != len(options) {
		t.Fatalf("test covers %d options, the table has %d", len(values), len(options))
	}
	want := &Config{
		NomadAddr:              "https://nomad.example.com:4646",
		NomadNamespace:         "apps",
		NomadTokenFile:         nomadTok,
		NomadToken:             "nomad-secret",
		NomadCACert:            ca,
		NomadClientCert:        cert,
		NomadClientKey:         key,
		NomadTLSSkipVerify:     true,
		GitURL:                 repo,
		GitBranch:              "prod",
		GitUsername:            "oauth2",
		GitTokenFile:           gitTok,
		GitToken:               "git-secret",
		DBPath:                 "/var/lib/nops/nops.db",
		ListenAddr:             "127.0.0.1:9000",
		AuthHeader:             "X-Forwarded-User",
		NotifyWebhookURLFile:   webhookURL,
		NotifyWebhookURL:       "https://n8n.example.com/webhook/abc",
		NotifyWebhookTokenFile: webhookTok,
		NotifyWebhookToken:     "webhook-secret",
		NotifyDiscordURLFile:   discordURL,
		NotifyDiscordURL:       "https://discord.com/api/webhooks/1/abc",
		NotifySlackURLFile:     slackURL,
		NotifySlackURL:         "https://hooks.slack.com/services/T/B/abc",
		NotifyNtfyURL:          "https://ntfy.example.com/nops",
		NotifyNtfyTokenFile:    ntfyTok,
		NotifyNtfyToken:        "ntfy-secret",
		NotifyGotifyURL:        "https://gotify.example.com",
		NotifyGotifyTokenFile:  gotifyTok,
		NotifyGotifyToken:      "gotify-secret",
		NotifyTimeout:          3 * time.Second,
		PublicURL:              "https://nops.example.com",
		GitPollInterval:        2 * time.Minute,
		DriftInterval:          15 * time.Minute,
		EngineInterval:         7 * time.Second,
		HookPollInterval:       3 * time.Second,
		ApplyTimeout:           time.Hour,
		LogLevel:               slog.LevelDebug,
	}

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
	for name, run := range map[string]func() (*Config, error){
		"flags": func() (*Config, error) { return Load(args, envOf(nil), io.Discard) },
		"env":   func() (*Config, error) { return Load(nil, envOf(env), io.Discard) },
	} {
		t.Run(name, func(t *testing.T) {
			c, err := run()
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(c, want) {
				t.Errorf("\n got %+v\nwant %+v", c, want)
			}
		})
	}
}

func TestLoadInvalid(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing")
	empty := writeFile(t, "empty", " \n")
	notURL := writeFile(t, "not-url", "discord.com/api/webhooks/1/secret-in-url")
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
		{"nomad-namespace", "", "required"},
		{"nomad-token-file", missing, "no such file"},
		{"nomad-token-file", empty, "is empty"},
		{"nomad-ca-cert", missing, "no such file"},
		{"nomad-ca-cert", dir, "is a directory"},
		{"nomad-client-cert", missing, "no such file"},
		{"nomad-client-key", missing, "no such file"},
		{"nomad-tls-skip-verify", "maybe", "invalid syntax"},
		{"git-url", "", "required"},
		{"git-branch", "", "required"},
		{"git-token-file", missing, "no such file"},
		{"git-token-file", empty, "is empty"},
		{"db-path", "", "required"},
		{"listen-addr", "8080", "invalid address"},
		{"listen-addr", "", "invalid address"},
		{"auth-header", "", "invalid header name"},
		{"auth-header", "Remote User", "invalid header name"},
		{"auth-header", "Remote:User", "invalid header name"},
		{"notify-webhook-url-file", missing, "no such file"},
		{"notify-webhook-url-file", empty, "is empty"},
		{"notify-webhook-url-file", notURL, "does not hold an http"},
		{"notify-webhook-token-file", missing, "no such file"},
		{"notify-discord-url-file", notURL, "does not hold an http"},
		{"notify-slack-url-file", notURL, "does not hold an http"},
		{"notify-ntfy-url", "ntfy.example.com/nops", "not an http"},
		{"notify-ntfy-token-file", empty, "is empty"},
		{"notify-gotify-url", "gotify", "not an http"},
		{"notify-gotify-token-file", missing, "no such file"},
		{"public-url", "nops.example.com", "not an http"},
		{"notify-timeout", "10", "missing unit"},
		{"notify-timeout", "0s", "must be positive"},
		{"git-poll-interval", "-1m", "must be positive"},
		{"drift-interval", "soon", "invalid duration"},
		{"engine-interval", "0", "must be positive"},
		{"hook-poll-interval", "", "invalid duration"},
		{"apply-timeout", "1d", "unknown unit"},
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
		{"webhook token without url", []string{"-git-url", repo, "-notify-webhook-token-file", tok}, "a webhook token is set without a webhook URL (-notify-webhook-url-file)"},
		{"ntfy token without url", []string{"-git-url", repo, "-notify-ntfy-token-file", tok}, "a ntfy token is set without a ntfy URL (-notify-ntfy-url)"},
		{"gotify token without url", []string{"-git-url", repo, "-notify-gotify-token-file", tok}, "a gotify token is set without a gotify URL (-notify-gotify-url)"},
		{"gotify url without token", []string{"-git-url", repo, "-notify-gotify-url", "https://gotify.example.com"}, "a gotify URL needs a token (-notify-gotify-token-file or NOPS_NOTIFY_GOTIFY_TOKEN)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(tt.args, envOf(nil), io.Discard)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want %q", err, tt.want)
			}
		})
	}

	// A URL read from a file never shows up in an error: it may hold a token.
	secretURL := writeFile(t, "discord-url", "discord.com/api/webhooks/1/do-not-print")
	if _, err := Load([]string{"-git-url", repo, "-notify-discord-url-file", secretURL}, envOf(nil), io.Discard); err == nil || strings.Contains(err.Error(), "do-not-print") {
		t.Errorf("err = %v: missing, or it prints the URL", err)
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

func TestLoadReportsEveryError(t *testing.T) {
	_, err := Load([]string{"-apply-timeout=0s", "-log-level=loud"}, envOf(map[string]string{"NOPS_DB_PATH": ""}), io.Discard)
	if err == nil {
		t.Fatal("no error")
	}
	for _, want := range []string{"-git-url / NOPS_GIT_URL: required", "flag -apply-timeout:", "flag -log-level:"} {
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
		Namespace: "default",
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

func TestNomadCarriesSettings(t *testing.T) {
	c := &Config{
		NomadAddr:          "https://nomad.example.com:4646",
		NomadNamespace:     "apps",
		NomadToken:         "secret",
		NomadCACert:        "/ca.pem",
		NomadClientCert:    "/cert.pem",
		NomadClientKey:     "/key.pem",
		NomadTLSSkipVerify: true,
	}
	want := &api.Config{
		Address:   "https://nomad.example.com:4646",
		Namespace: "apps",
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
		{"NOPS_NOTIFY_WEBHOOK_URL", "https://n8n.example.com/webhook/plain", func(c *Config) string { return c.NotifyWebhookURL }, nil},
		{"NOPS_NOTIFY_WEBHOOK_TOKEN", "plain-webhook-token", func(c *Config) string { return c.NotifyWebhookToken },
			map[string]string{"NOPS_NOTIFY_WEBHOOK_URL": "https://n8n.example.com/webhook/plain"}},
		{"NOPS_NOTIFY_DISCORD_URL", "https://discord.com/api/webhooks/1/plain", func(c *Config) string { return c.NotifyDiscordURL }, nil},
		{"NOPS_NOTIFY_SLACK_URL", "https://hooks.slack.com/services/plain", func(c *Config) string { return c.NotifySlackURL }, nil},
		{"NOPS_NOTIFY_NTFY_TOKEN", "plain-ntfy-token", func(c *Config) string { return c.NotifyNtfyToken },
			map[string]string{"NOPS_NOTIFY_NTFY_URL": "https://ntfy.example.com/nops"}},
		{"NOPS_NOTIFY_GOTIFY_TOKEN", "plain-gotify-token", func(c *Config) string { return c.NotifyGotifyToken },
			map[string]string{"NOPS_NOTIFY_GOTIFY_URL": "https://gotify.example.com"}},
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
		{"not a url", map[string]string{"NOPS_NOTIFY_DISCORD_URL": "discord.com/api/webhooks/1/secret"}, "NOPS_NOTIFY_DISCORD_URL: does not hold an http"},
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

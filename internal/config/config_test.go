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

// authEnv is what a valid configuration needs: the -config file with the auth
// block and a repository, and the public URL. envOf adds it under every test
// environment, unless the test sets (or blanks) the variable itself. TestMain
// sets NOPS_CONFIG.
var authEnv = map[string]string{
	"NOPS_PUBLIC_URL": "https://nops.example.com",
}

// baseConfig is the -config file of authEnv: authBlock and defaultRepo.
// authSecretFile holds the client secret.
var baseConfig, authSecretFile string

// authBlock is the auth block of a valid configuration, which authWant reads.
func authBlock() string {
	return `auth "oidc" {
  type               = "oidc"
  issuer_url         = "https://idp.example.com/application/o/nops/"
  client_id          = "nops"
  client_secret_file = "` + authSecretFile + `"
}
`
}

// defaultRepo is the repository of baseConfig, which defaultRepoWant reads.
const defaultRepo = `repository "default" {
  url = "` + repo + `"
}
`

// defaultRepoWant is c.Repositories from defaultRepo.
func defaultRepoWant() []Repository {
	return []Repository{{Name: "default", URL: repo, Branch: "main", Username: "git", Timeout: 2 * time.Minute}}
}

// authWant is c.Auth from authBlock.
func authWant() Auth {
	return Auth{
		Name: "oidc", Type: "oidc", IssuerURL: "https://idp.example.com/application/o/nops/",
		ClientID: "nops", ClientSecretFile: authSecretFile, ClientSecret: "client-secret",
	}
}

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "nops-config-test")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	authSecretFile = filepath.Join(dir, "oidc-secret")
	baseConfig = filepath.Join(dir, "nops.hcl")
	err = os.WriteFile(authSecretFile, []byte("client-secret\n"), 0o600)
	if err == nil {
		err = os.WriteFile(baseConfig, []byte(authBlock()+defaultRepo), 0o600)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	authEnv["NOPS_CONFIG"] = baseConfig
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// writeConfig writes a -config file of content after authBlock: the
// configuration is valid once content declares a repository.
func writeConfig(t *testing.T, content string) string {
	t.Helper()
	return writeFile(t, "nops.hcl", authBlock()+content)
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
	c, err := Load(nil, envOf(nil), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	want := &Config{
		NomadAddr:        "http://127.0.0.1:4646",
		NomadNamespaces:  []string{"default"},
		NomadTimeout:     30 * time.Second,
		Repositories:     defaultRepoWant(),
		ConfigFile:       baseConfig,
		DBPath:           "nops.db",
		ListenAddr:       ":8080",
		Auth:             authWant(),
		PublicURL:        "https://nops.example.com",
		GitPollInterval:  time.Minute,
		DriftInterval:    5 * time.Minute,
		EngineInterval:   5 * time.Second,
		HookPollInterval: 5 * time.Second,
		ApplyTimeout:     10 * time.Minute,
		SpecRetention:    720 * time.Hour,
		// The server's timeouts, with no listen block.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       time.Minute,
		WriteTimeout:      time.Minute,
		IdleTimeout:       2 * time.Minute,
		// A sync window is read in UTC unless the operator says otherwise.
		SyncWindowLocation: time.UTC,
		LogLevel:           slog.LevelInfo,
	}
	c.DeprecatedVariables = nil // envOf sets two: TestLoadDeprecatedVariables
	if !reflect.DeepEqual(c, want) {
		t.Errorf("defaults:\n got %+v\nwant %+v", c, want)
	}
}

func TestLoadPrecedence(t *testing.T) {
	withAddr := writeConfig(t, defaultRepo+"listen {\n  addr = \"127.0.0.1:7000\"\n}\n")
	tests := []struct {
		name   string
		config string // "" is baseConfig, with no address
		args   []string
		env    map[string]string
		want   string
	}{
		{"default", "", nil, nil, ":8080"},
		{"file over default", withAddr, nil, nil, "127.0.0.1:7000"},
		{"env over file", withAddr, nil, map[string]string{"NOPS_LISTEN_ADDR": ":9000"}, ":9000"},
		{"flag over env", withAddr, []string{"-listen-addr", ":9100"}, map[string]string{"NOPS_LISTEN_ADDR": ":9000"}, ":9100"},
		{"empty env is unset", withAddr, nil, map[string]string{"NOPS_LISTEN_ADDR": ""}, "127.0.0.1:7000"},
		{"flag over invalid env", "", []string{"-listen-addr=:9100"}, map[string]string{"NOPS_LISTEN_ADDR": "nope"}, ":9100"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := map[string]string{}
			for k, v := range tt.env {
				env[k] = v
			}
			if tt.config != "" {
				env["NOPS_CONFIG"] = tt.config
			}
			c, err := Load(tt.args, envOf(env), io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			if c.ListenAddr != tt.want {
				t.Errorf("ListenAddr = %s, want %s", c.ListenAddr, tt.want)
			}
		})
	}

	// An invalid addr the flag overrides is not read.
	bad := writeConfig(t, defaultRepo+"listen {\n  addr = \"8080\"\n}\n")
	if _, err := Load([]string{"-config", bad, "-listen-addr", ":9100"}, envOf(nil), io.Discard); err != nil {
		t.Errorf("addr overridden by the flag: %v", err)
	}
}

// TestLoadEveryOption sets every option once through its flag and once
// through its variable, so a typo in a set function or in the env name
// shows up.
func TestLoadEveryOption(t *testing.T) {
	appsTok := writeFile(t, "apps-token", "apps-secret\n")
	bootstrap := secret.New()
	bootstrapTok := writeFile(t, "bootstrap-token", bootstrap+"\n")
	configFile := writeConfig(t, `
repository "apps" {
  url        = "https://git.example.com/ops/apps.git"
  token_file = "`+appsTok+`"
}

acl {
  enabled              = true
  bootstrap_token_file = "`+bootstrapTok+`"
}
`)

	values := map[string]string{
		"nomad-addr":  "https://nomad.example.com:4646",
		"config":      configFile,
		"db-path":     "/var/lib/nops/nops.db",
		"listen-addr": "127.0.0.1:9000",
		"public-url":  "https://nops.example.com/",
		"log-level":   "debug",
	}
	want := Config{
		NomadAddr: "https://nomad.example.com:4646",
		// The rest of the nomad block is the file's, not an option.
		NomadNamespaces: []string{"default"},
		NomadTimeout:    30 * time.Second,
		ConfigFile:      configFile,
		Repositories: []Repository{
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
		Auth:                  authWant(),
		ACL:                   true,
		ACLBootstrapTokenFile: bootstrapTok,
		ACLBootstrapToken:     bootstrap,
		PublicURL:             "https://nops.example.com",
		// So are the intervals and the time zone: the defaults stand.
		GitPollInterval:    time.Minute,
		DriftInterval:      5 * time.Minute,
		EngineInterval:     5 * time.Second,
		HookPollInterval:   5 * time.Second,
		ApplyTimeout:       10 * time.Minute,
		SpecRetention:      720 * time.Hour,
		SyncWindowLocation: time.UTC,
		LogLevel:           slog.LevelDebug,
	}

	if len(values) != len(options) {
		t.Fatalf("test covers %d options, the table has %d", len(values), len(options))
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
	// rawEnvOf, not envOf: every option is set explicitly above, the point of
	// this test.
	for name, run := range map[string]func() (*Config, error){
		"flags": func() (*Config, error) { return Load(args, rawEnvOf(nil), io.Discard) },
		"env":   func() (*Config, error) { return Load(nil, rawEnvOf(env), io.Discard) },
	} {
		t.Run(name, func(t *testing.T) {
			c, err := run()
			if err != nil {
				t.Fatal(err)
			}
			c.DeprecatedVariables = nil // TestLoadDeprecatedVariables
			if !reflect.DeepEqual(c, &want) {
				t.Errorf("\n got %+v\nwant %+v", c, &want)
			}
		})
	}
}

func TestLoadInvalid(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing")
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
		{"config", missing, "no such file"},
		{"config", dir, "is a directory"},
		{"db-path", "", "required"},
		{"listen-addr", "8080", "invalid address"},
		{"listen-addr", "", "invalid address"},
		{"public-url", "nops.example.com", "not an http"},
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
			base := map[string]string{}

			// Through the flag: the error names the flag.
			_, err := Load([]string{"-" + o.name + "=" + tt.value}, envOf(base), io.Discard)
			checkErr(t, err, "flag -"+o.name+": ", tt.want)

			// Through the variable: the error names the variable. An empty
			// variable is unset, so only a required option without a default
			// fails that way.
			env := map[string]string{}
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
// (typically local users on a personal machine) no longer has to type
// something there just to satisfy a required flag. The default applies to an
// OIDC login too, as here (see publicURLDefault's doc comment on why that is
// still the operator's call to override for a real provider).
func TestLoadPublicURLDefault(t *testing.T) {
	loginArgs := func(extra ...string) []string {
		return append([]string{"-config", baseConfig}, extra...)
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
			args := loginArgs()
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
	c, err := Load(loginArgs("-listen-addr", "0.0.0.0:9090", "-public-url", "https://nops.example.com"), rawEnvOf(nil), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.PublicURL != "https://nops.example.com" {
		t.Errorf("public URL = %q, want the explicit value kept", c.PublicURL)
	}
}

// TestLoadBasePath covers BasePath, derived from -public-url's own path
// component: there is no separate flag (docs/running-nops.md#under-a-sub-path).
func TestLoadBasePath(t *testing.T) {
	args := func(publicURL string) []string {
		return []string{"-config", baseConfig, "-public-url", publicURL}
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
	c, err := Load([]string{"-config", baseConfig}, rawEnvOf(nil), io.Discard)
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

// A git token goes only over https; without one, any URL is accepted: a public
// repository needs no auth. The errors are in TestLoadRepositoriesInvalid.
func TestLoadRepositoryURLs(t *testing.T) {
	tok := writeFile(t, "token", "secret")
	for name, body := range map[string]string{
		"ssh url without token":  `url = "git@git.example.com:ops/jobs.git"`,
		"uppercase https, token": `url = "HTTPS://git.example.com/jobs.git"` + "\n" + `token_file = "` + tok + `"`,
	} {
		file := writeConfig(t, "repository \"apps\" {\n"+body+"\n}\n")
		if _, err := Load([]string{"-config", file}, envOf(nil), io.Discard); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// The options that are gone stop Nops, and say what replaces them.
func TestLoadRemovedOptions(t *testing.T) {
	full := map[string]string{}

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
		_, err := Load(args, envOf(env), io.Discard)
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
		_, err := Load(args, envOf(env), io.Discard)
		if err == nil || !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), "was removed") || !strings.Contains(err.Error(), "metrics capability") {
			t.Errorf("%s: err = %v, want it removed, with what replaces it", name, err)
		}
	}

}

func TestLoadReportsEveryError(t *testing.T) {
	_, err := Load([]string{"-config", writeConfig(t, `apply_timeout = "0s"`+"\n"), "-listen-addr=8080", "-log-level=loud"}, envOf(nil), io.Discard)
	if err == nil {
		t.Fatal("no error")
	}
	for _, want := range []string{"a repository block in -config is required", "apply_timeout:", "flag -listen-addr:", "flag -log-level:"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestLoadArguments(t *testing.T) {
	env := envOf(map[string]string{})

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
	file := map[string]bool{"config": true, "db-path": true}
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
}

func TestOptionsTable(t *testing.T) {
	seen := make(map[string]bool)
	for _, o := range options {
		if o.name == "" || o.usage == "" || o.set == nil || o.replacedBy == "" {
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

	c, err := Load(nil, envOf(nil), io.Discard)
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

// TestLoadSyncWindowTimeZone: a sync window is read in UTC unless the operator
// names a zone, and what is named is an IANA zone that Load resolves, so a
// typo stops the start rather than shifting every window.
func TestLoadSyncWindowTimeZone(t *testing.T) {
	c, err := Load(nil, envOf(nil), io.Discard)
	if err != nil || c.SyncWindowLocation != time.UTC {
		t.Fatalf("default = %v, %v, want UTC", c.SyncWindowLocation, err)
	}
	if _, err := time.LoadLocation("Europe/Rome"); err != nil {
		t.Skipf("no tz database: %v", err)
	}
	c, err = Load([]string{"-config", writeConfig(t, defaultRepo+`sync_window_time_zone = "Europe/Rome"`+"\n")}, envOf(nil), io.Discard)
	if err != nil || c.SyncWindowLocation.String() != "Europe/Rome" {
		t.Errorf("sync_window_time_zone Europe/Rome = %v, %v", c.SyncWindowLocation, err)
	}
}

func TestLoadRepositoriesFromTheFile(t *testing.T) {
	tok := writeFile(t, "apps-token", "apps-secret\n")
	file := writeConfig(t, `
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
	file := writeConfig(t, `
nomad {
  namespaces = ["default", "labs"]
}

repository "apps" {
  url  = "https://git.example.com/ops/apps.git"
  jobs = ["labs/billing-api", "lab*/billing-api-*", "crm"]
}

repository "infra" {
  url = "https://git.example.com/ops/infra.git"
}
`)
	c, err := Load([]string{"-config", file}, envOf(nil), io.Discard)
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
	empty := writeFile(t, "empty", " \n")
	block := func(name, body string) string { return "repository \"" + name + "\" {\n" + body + "\n}\n" }
	apps := block("apps", `url = "https://git.example.com/ops/apps.git"`)
	tests := []struct {
		name string
		file string // "" for no -config
		args []string
		env  map[string]string
		want string
	}{
		{"no -config", "", nil, map[string]string{"NOPS_CONFIG": ""}, "a repository block in -config is required"},
		{"no repository", " ", nil, nil, "a repository block in -config is required"},
		{"a name twice", apps + apps, nil, nil, `repository "apps" is declared twice`},
		{"a bad name", block("my apps", `url = "https://git.example.com/x.git"`), nil, nil, "the name may hold only letters, digits, - and _"},
		{"no url", block("apps", `branch = "main"`), nil, nil, `Missing required argument`},
		{"an empty url", block("apps", `url = ""`), nil, nil, `repository "apps": url: required`},
		{"an empty branch", block("apps", `url = "https://git.example.com/x.git"`+"\n"+`branch = ""`), nil, nil, `repository "apps": branch: required`},
		{"a path outside", block("apps", `url = "https://git.example.com/x.git"`+"\n"+`path = "../x"`), nil, nil, `repository "apps": path: "../x" must be a relative path`},
		{"a missing token file", block("apps", `url = "https://git.example.com/x.git"`+"\n"+`token_file = "`+filepath.Join(dir, "missing")+`"`), nil, nil, `repository "apps": token_file: open`},
		{"an empty token file", block("apps", `url = "https://git.example.com/x.git"`+"\n"+`token_file = "`+empty+`"`), nil, nil, `repository "apps": token_file: ` + empty + ` is empty`},
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
				args = append([]string{"-config", writeConfig(t, tt.file)}, args...)
			}
			_, err := Load(args, envOf(tt.env), io.Discard)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want one containing %q", err, tt.want)
			}
		})
	}
}

func TestLoadListenFromTheFile(t *testing.T) {
	file := writeConfig(t, `
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
		{"an address without a port", listen(`addr = "8080"`), `listen: addr: invalid address "8080"`},
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
			_, err := Load([]string{"-config", writeConfig(t, tt.file)}, envOf(nil), io.Discard)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want one containing %q", err, tt.want)
			}
		})
	}
}

func TestLoadListenTLS(t *testing.T) {
	noPublicURL := envOf(map[string]string{"NOPS_PUBLIC_URL": ""}) // so the URL is derived
	cert, key := writeFile(t, "nops.crt", "cert"), writeFile(t, "nops.key", "key")
	file := writeConfig(t, `
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
	plain, err := Load([]string{"-config", writeConfig(t, "repository \"apps\" {\n  url = \"https://git.example.com/ops/apps.git\"\n}\n")}, noPublicURL, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if plain.TLSCertFile != "" || plain.TLSKeyFile != "" || plain.PublicURL != "http://localhost:8080" {
		t.Errorf("without a tls block: files %q, %q, public URL %q", plain.TLSCertFile, plain.TLSKeyFile, plain.PublicURL)
	}
}

func TestLoadNomadFromTheFile(t *testing.T) {
	ca := writeFile(t, "ca.pem", "ca")
	cert := writeFile(t, "cert.pem", "cert")
	key := writeFile(t, "key.pem", "key")
	tok := writeFile(t, "nomad-token", "  nomad-secret\n")
	file := writeConfig(t, `
repository "apps" {
  url = "https://git.example.com/ops/apps.git"
}

nomad {
  addr            = "https://nomad.example.com:4646"
  namespaces      = ["apps", "infra", "apps"]
  token_file      = "`+tok+`"
  ui_url          = "https://infra.example.com/nomad/"
  ca_cert         = "`+ca+`"
  client_cert     = "`+cert+`"
  client_key      = "`+key+`"
  tls_skip_verify = true
  timeout         = "2m"
}
`)
	c, err := Load([]string{"-config", file}, envOf(nil), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	got := &api.Config{Address: c.NomadAddr, SecretID: c.NomadToken, TLSConfig: &api.TLSConfig{
		CACert: c.NomadCACert, ClientCert: c.NomadClientCert, ClientKey: c.NomadClientKey, Insecure: c.NomadTLSSkipVerify,
	}}
	want := &api.Config{Address: "https://nomad.example.com:4646", SecretID: "nomad-secret", TLSConfig: &api.TLSConfig{
		CACert: ca, ClientCert: cert, ClientKey: key, Insecure: true,
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Nomad client settings = %+v, want %+v", got, want)
	}
	// A repeated namespace is kept once, the trailing slash of ui_url goes and
	// its sub path stays.
	if !reflect.DeepEqual(c.NomadNamespaces, []string{"apps", "infra"}) || c.NomadTokenFile != tok ||
		c.NomadUIURL != "https://infra.example.com/nomad" || c.NomadTimeout != 2*time.Minute {
		t.Errorf("namespaces %q, token file %q, UI URL %q, timeout %v", c.NomadNamespaces, c.NomadTokenFile, c.NomadUIURL, c.NomadTimeout)
	}

	// Without ui_url there is no link: it is never taken from addr, the
	// address Nops uses, not one a browser reaches.
	c, err = Load([]string{"-config", writeConfig(t, `
repository "apps" {
  url = "https://git.example.com/ops/apps.git"
}

nomad {
  addr = "http://nomad.service.consul:4646"
}
`)}, envOf(nil), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.NomadUIURL != "" || !reflect.DeepEqual(c.NomadNamespaces, []string{"default"}) || c.NomadToken != "" {
		t.Errorf("UI URL %q, namespaces %q, token %q; want no link, the default namespace, no token", c.NomadUIURL, c.NomadNamespaces, c.NomadToken)
	}
}

// The address of the nomad block yields to -nomad-addr and to its variable.
func TestLoadNomadAddrPrecedence(t *testing.T) {
	file := writeConfig(t, `
repository "apps" {
  url = "https://git.example.com/ops/apps.git"
}

nomad {
  addr = "http://from-file:4646"
}
`)
	tests := []struct {
		name string
		args []string
		env  map[string]string
		want string
	}{
		{"the file over the default", nil, nil, "http://from-file:4646"},
		{"the variable over the file", nil, map[string]string{"NOPS_NOMAD_ADDR": "http://from-env:4646"}, "http://from-env:4646"},
		{"the flag over the variable", []string{"-nomad-addr", "http://from-flag:4646"}, map[string]string{"NOPS_NOMAD_ADDR": "http://from-env:4646"}, "http://from-flag:4646"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := Load(append([]string{"-config", file}, tt.args...), envOf(tt.env), io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			if c.NomadAddr != tt.want {
				t.Errorf("NomadAddr = %q, want %q", c.NomadAddr, tt.want)
			}
		})
	}

	// An invalid addr the flag overrides is not read.
	bad := writeConfig(t, defaultRepo+"nomad {\n  addr = \"nomad:4646\"\n}\n")
	if _, err := Load([]string{"-config", bad, "-nomad-addr", "http://from-flag:4646"}, envOf(nil), io.Discard); err != nil {
		t.Errorf("addr overridden by the flag: %v", err)
	}
}

func TestLoadNomadInvalid(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing")
	empty := writeFile(t, "empty", " \n")
	cert := writeFile(t, "cert.pem", "cert")
	apps := `repository "apps" {
  url = "https://git.example.com/ops/apps.git"
}
`
	nomad := func(body string) string { return apps + "nomad {\n" + body + "\n}\n" }
	tests := []struct {
		name, file, want string
	}{
		{"addr without a scheme", nomad(`addr = "nomad.example.com:4646"`), `nomad: addr: "nomad.example.com:4646" is not an http`},
		{"addr not http", nomad(`addr = "ftp://nomad"`), "nomad: addr: \"ftp://nomad\" is not an http"},
		{"addr empty", nomad(`addr = ""`), "nomad: addr: \"\" is not an http"},
		{"no namespace", nomad(`namespaces = []`), "nomad: namespaces: is empty"},
		{"an empty namespace", nomad(`namespaces = ["apps", " "]`), `nomad: namespaces: " " is not a namespace name`},
		{"namespaces a string", nomad(`namespaces = "apps"`), "namespaces"},
		{"token_file missing", nomad(`token_file = "` + missing + `"`), "nomad: token_file: open " + missing},
		{"token_file empty", nomad(`token_file = "` + empty + `"`), "nomad: token_file: " + empty + " is empty"},
		{"ui_url not a URL", nomad(`ui_url = "/ui"`), `nomad: ui_url: "/ui" is not an http`},
		{"ca_cert missing", nomad(`ca_cert = "` + missing + `"`), "nomad: ca_cert: open " + missing},
		{"ca_cert a directory", nomad(`ca_cert = "` + dir + `"`), "nomad: ca_cert: " + dir + " is a directory"},
		{"client_cert without client_key", nomad(`client_cert = "` + cert + `"`), "nomad: client_cert: must be set together with client_key"},
		{"client_key without client_cert", nomad(`client_key = "` + cert + `"`), "nomad: client_cert: must be set together with client_key"},
		{"tls_skip_verify a string", nomad(`tls_skip_verify = "maybe"`), "tls_skip_verify"},
		{"timeout not a duration", nomad(`timeout = "soon"`), "nomad: timeout:"},
		{"timeout zero", nomad(`timeout = "0s"`), `nomad: timeout: duration "0s" must be positive`},
		{"timeout negative", nomad(`timeout = "-1m"`), `nomad: timeout: duration "-1m" must be positive`},
		{"timeout a number", nomad(`timeout = 30`), "timeout"},
		{"the token itself", nomad(`token = "secret"`), "Unsupported argument"},
		{"two blocks", nomad("") + nomad(""), "Duplicate nomad block"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load([]string{"-config", writeConfig(t, tt.file)}, envOf(nil), io.Discard)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want one containing %q", err, tt.want)
			}
		})
	}
}

// The -nomad-* options but -nomad-addr moved into the nomad block: each one,
// its variable, and the variables that held the token or a single namespace
// stop Nops, naming the attribute that replaces it.
func TestLoadNomadOptionsRemoved(t *testing.T) {
	file := writeFile(t, "file", "x")
	tests := []struct {
		name string
		args []string
		env  map[string]string
		want string
	}{
		{"flag -nomad-namespaces", []string{"-nomad-namespaces", "apps"}, nil, "namespaces in the nomad block"},
		{"NOPS_NOMAD_NAMESPACES", nil, map[string]string{"NOPS_NOMAD_NAMESPACES": "apps"}, "namespaces in the nomad block"},
		{"flag -nomad-namespace", []string{"-nomad-namespace", "apps"}, nil, "namespaces in the nomad block"},
		{"NOPS_NOMAD_NAMESPACE", nil, map[string]string{"NOPS_NOMAD_NAMESPACE": "apps"}, "namespaces in the nomad block"},
		{"flag -nomad-token-file", []string{"-nomad-token-file", file}, nil, "token_file in the nomad block"},
		{"NOPS_NOMAD_TOKEN_FILE", nil, map[string]string{"NOPS_NOMAD_TOKEN_FILE": file}, "token_file in the nomad block"},
		{"NOPS_NOMAD_TOKEN", nil, map[string]string{"NOPS_NOMAD_TOKEN": "secret"}, "token_file in the nomad block"},
		{"flag -nomad-ui-url", []string{"-nomad-ui-url", "https://nomad.example.com"}, nil, "ui_url in the nomad block"},
		{"NOPS_NOMAD_UI_URL", nil, map[string]string{"NOPS_NOMAD_UI_URL": "https://nomad.example.com"}, "ui_url in the nomad block"},
		{"flag -nomad-ca-cert", []string{"-nomad-ca-cert", file}, nil, "ca_cert in the nomad block"},
		{"NOPS_NOMAD_CA_CERT", nil, map[string]string{"NOPS_NOMAD_CA_CERT": file}, "ca_cert in the nomad block"},
		{"flag -nomad-client-cert", []string{"-nomad-client-cert", file}, nil, "client_cert in the nomad block"},
		{"NOPS_NOMAD_CLIENT_CERT", nil, map[string]string{"NOPS_NOMAD_CLIENT_CERT": file}, "client_cert in the nomad block"},
		{"flag -nomad-client-key", []string{"-nomad-client-key", file}, nil, "client_key in the nomad block"},
		{"NOPS_NOMAD_CLIENT_KEY", nil, map[string]string{"NOPS_NOMAD_CLIENT_KEY": file}, "client_key in the nomad block"},
		{"flag -nomad-tls-skip-verify", []string{"-nomad-tls-skip-verify"}, nil, "tls_skip_verify in the nomad block"},
		{"flag -nomad-tls-skip-verify", []string{"-nomad-tls-skip-verify=false"}, nil, "tls_skip_verify in the nomad block"},
		{"NOPS_NOMAD_TLS_SKIP_VERIFY", nil, map[string]string{"NOPS_NOMAD_TLS_SKIP_VERIFY": "true"}, "tls_skip_verify in the nomad block"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(tt.args, envOf(tt.env), io.Discard)
			if err == nil || !strings.Contains(err.Error(), tt.name+" was removed") || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want %s removed, replaced by %s", err, tt.name, tt.want)
			}
		})
	}
}

// authApps is the repository of the auth block tests, whose files hold no
// other auth block than the test's.
const authApps = `repository "apps" {
  url = "https://git.example.com/ops/apps.git"
}
`

func TestLoadAuthFromTheFile(t *testing.T) {
	secretFile := writeFile(t, "oidc-secret", "  s3cret\n")
	usersFile := writeFile(t, "users", "alice:hash\n")
	groupsFile := writeFile(t, "groups", "ops: alice\n")
	tests := []struct {
		name, block string
		want        Auth
	}{
		{"oidc, the issuer kept as given", `auth "authentik" {
  type               = "oidc"
  issuer_url         = "https://idp.example.com/application/o/nops/"
  client_id          = "nops"
  client_secret_file = "` + secretFile + `"
}`, Auth{Name: "authentik", Type: "oidc", IssuerURL: "https://idp.example.com/application/o/nops/", ClientID: "nops", ClientSecretFile: secretFile, ClientSecret: "s3cret"}},
		{"basic, with the defaults", `auth "local" {
  type       = "basic"
  users_file = "` + usersFile + `"
}`, Auth{Name: "local", Type: "basic", UsersFile: usersFile, MaxFailures: 10, FailureWindow: 15 * time.Minute}},
		{"basic, every attribute", `auth "basic" {
  type           = "basic"
  users_file     = "` + usersFile + `"
  groups_file    = "` + groupsFile + `"
  max_failures   = 3
  failure_window = "1h"
}`, Auth{Name: "basic", Type: "basic", UsersFile: usersFile, GroupsFile: groupsFile, MaxFailures: 3, FailureWindow: time.Hour}},
		{"a zero count and window turn the limit off", `auth "basic" {
  type           = "basic"
  users_file     = "` + usersFile + `"
  max_failures   = 0
  failure_window = "0s"
}`, Auth{Name: "basic", Type: "basic", UsersFile: usersFile}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file := writeFile(t, "nops.hcl", authApps+tt.block+"\n")
			c, err := Load([]string{"-config", file}, rawEnvOf(nil), io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(c.Auth, tt.want) {
				t.Errorf("auth:\n got %+v\nwant %+v", c.Auth, tt.want)
			}
		})
	}
}

func TestLoadAuthInvalid(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing")
	empty := writeFile(t, "empty", " \n")
	secretFile := writeFile(t, "oidc-secret", "s3cret")
	usersFile := writeFile(t, "users", "alice:hash\n")
	auth := func(label, body string) string { return "auth \"" + label + "\" {\n" + body + "\n}\n" }
	oidc := func(body string) string {
		return auth("oidc", `type = "oidc"
issuer_url = "https://idp.example.com/"
client_id = "nops"
client_secret_file = "`+secretFile+`"
`+body)
	}
	basic := func(body string) string {
		return auth("basic", "type = \"basic\"\nusers_file = \""+usersFile+"\"\n"+body)
	}
	tests := []struct {
		name, file, want string
	}{
		{"no auth block", "", "an auth block in -config is required"},
		{"two blocks", basic("") + oidc(""), "only one auth block is allowed"},
		{"a bad name", auth("my login", `type = "basic"`+"\n"+`users_file = "`+usersFile+`"`), `auth "my login": the name may hold only letters, digits, - and _`},
		{"no type", auth("basic", ""), "type"},
		{"an unknown type", auth("ldap", `type = "ldap"`), `auth "ldap": type: is "ldap", must be oidc or basic`},
		{"oidc without an issuer", auth("oidc", `type = "oidc"`+"\n"+`client_id = "nops"`+"\n"+`client_secret_file = "`+secretFile+`"`), `auth "oidc": issuer_url: required`},
		{"oidc without a client ID", auth("oidc", `type = "oidc"`+"\n"+`issuer_url = "https://idp.example.com/"`+"\n"+`client_secret_file = "`+secretFile+`"`), `auth "oidc": client_id: required`},
		{"oidc without a client secret", auth("oidc", `type = "oidc"`+"\n"+`issuer_url = "https://idp.example.com/"`+"\n"+`client_id = "nops"`), `auth "oidc": client_secret_file: required`},
		{"an issuer that is not a URL", auth("oidc", `type = "oidc"`+"\n"+`issuer_url = "idp.example.com"`+"\n"+`client_id = "nops"`+"\n"+`client_secret_file = "`+secretFile+`"`), `auth "oidc": issuer_url: "idp.example.com" is not an http`},
		{"a missing client secret file", auth("oidc", `type = "oidc"`+"\n"+`issuer_url = "https://idp.example.com/"`+"\n"+`client_id = "nops"`+"\n"+`client_secret_file = "`+missing+`"`), `auth "oidc": client_secret_file: open`},
		{"an empty client secret file", auth("oidc", `type = "oidc"`+"\n"+`issuer_url = "https://idp.example.com/"`+"\n"+`client_id = "nops"`+"\n"+`client_secret_file = "`+empty+`"`), `auth "oidc": client_secret_file: ` + empty + ` is empty`},
		{"oidc with a users file", oidc(`users_file = "` + usersFile + `"`), `auth "oidc": users_file: is not used by type "oidc"`},
		{"oidc with a limit", oidc(`max_failures = 3`), `auth "oidc": max_failures: is not used by type "oidc"`},
		{"oidc with a window", oidc(`failure_window = "1h"`), `auth "oidc": failure_window: is not used by type "oidc"`},
		{"basic without a users file", auth("basic", `type = "basic"`), `auth "basic": users_file: required`},
		{"a missing users file", auth("basic", `type = "basic"`+"\n"+`users_file = "`+missing+`"`), `auth "basic": users_file: open`},
		{"a groups file that is a directory", basic(`groups_file = "` + dir + `"`), `auth "basic": groups_file: ` + dir + ` is a directory`},
		{"basic with a client ID", basic(`client_id = "nops"`), `auth "basic": client_id: is not used by type "basic"`},
		{"negative count", basic(`max_failures = -1`), `auth "basic": max_failures: -1 must not be negative`},
		{"a count that is not a number", basic(`max_failures = "many"`), "a number is required"},
		{"not a duration", basic(`failure_window = "soon"`), `auth "basic": failure_window:`},
		{"negative window", basic(`failure_window = "-1m"`), `auth "basic": failure_window: duration "-1m" must not be negative`},
		{"an unknown attribute", basic(`password = "x"`), "Unsupported argument"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load([]string{"-config", writeFile(t, "nops.hcl", authApps+tt.file)}, rawEnvOf(nil), io.Discard)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want one containing %q", err, tt.want)
			}
		})
	}

	// Without -config at all, the error says where the block goes.
	_, err := Load(nil, rawEnvOf(nil), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "an auth block in -config is required") {
		t.Errorf("no -config: err = %v, want the auth block required", err)
	}
	// A -config that cannot be read is reported once, not as a missing block.
	_, err = Load([]string{"-config", writeFile(t, "bad.hcl", "auth {")}, rawEnvOf(nil), io.Discard)
	if err == nil || strings.Contains(err.Error(), "an auth block in -config is required") {
		t.Errorf("unreadable -config: err = %v, want only the parse error", err)
	}
}

// The login options are gone: Nops refuses them and names the attribute of
// the auth block that replaces each.
func TestLoadAuthOptionsRemoved(t *testing.T) {
	for _, tt := range []struct {
		flag, value, env, want string
	}{
		{"auth-mode", "basic", "NOPS_AUTH_MODE", "type in the auth block"},
		{"oidc-issuer-url", "https://idp.example.com/", "NOPS_OIDC_ISSUER_URL", `issuer_url in an auth block of type "oidc"`},
		{"oidc-client-id", "nops", "NOPS_OIDC_CLIENT_ID", `client_id in an auth block of type "oidc"`},
		{"oidc-client-secret-file", "/secrets/oidc", "NOPS_OIDC_CLIENT_SECRET_FILE", `client_secret_file in an auth block of type "oidc"`},
		{"users-file", "/secrets/users", "NOPS_USERS_FILE", `users_file in an auth block of type "basic"`},
		{"groups-file", "/secrets/groups", "NOPS_GROUPS_FILE", `groups_file in an auth block of type "basic"`},
	} {
		_, err := Load([]string{"-" + tt.flag, tt.value}, envOf(nil), io.Discard)
		if err == nil || !strings.Contains(err.Error(), "flag -"+tt.flag+" was removed: "+tt.want) {
			t.Errorf("-%s: err = %v, want it removed for %q", tt.flag, err, tt.want)
		}
		_, err = Load(nil, envOf(map[string]string{tt.env: tt.value}), io.Discard)
		if err == nil || !strings.Contains(err.Error(), tt.env+" was removed: "+tt.want) {
			t.Errorf("%s: err = %v, want it removed for %q", tt.env, err, tt.want)
		}
	}
	_, err := Load(nil, envOf(map[string]string{"NOPS_OIDC_CLIENT_SECRET": "s3cret"}), io.Discard)
	if err == nil || !strings.Contains(err.Error(), `NOPS_OIDC_CLIENT_SECRET was removed: client_secret_file in an auth block of type "oidc"`) {
		t.Errorf("NOPS_OIDC_CLIENT_SECRET: err = %v, want it removed", err)
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
	file := writeConfig(t, `
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
	c, err = Load(nil, envOf(nil), io.Discard)
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
			_, err := Load([]string{"-config", writeConfig(t, apps+tt.file)}, envOf(nil), io.Discard)
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
			_, err := Load([]string{"-" + tt.flag, "x"}, envOf(nil), io.Discard)
			errs := map[string]error{"flag -" + tt.flag: err}
			for _, v := range vars {
				_, errs[v] = Load(nil, envOf(map[string]string{v: "x"}), io.Discard)
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
	file := writeConfig(t, `
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
		nil,
		{"-config", writeConfig(t, defaultRepo+"acl {}\n")},
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
			_, err := Load([]string{"-config", writeConfig(t, defaultRepo+tt.file)}, envOf(nil), io.Discard)
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
			_, err := Load(tt.args, envOf(tt.env), io.Discard)
			if err == nil || !strings.Contains(err.Error(), tt.name+" was removed") || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want %s removed, replaced by %s", err, tt.name, tt.want)
			}
		})
	}
}

// The -git-* options but -git-poll-interval moved into the repository block:
// each one, its variable, and the variable that held the token stop Nops,
// naming the attribute that replaces it.
func TestLoadGitOptionsRemoved(t *testing.T) {
	tok := writeFile(t, "token", "secret")
	tests := []struct {
		flag, value, valueEnv, want string
	}{
		{"git-url", repo, "", "url in a repository block"},
		{"git-branch", "prod", "", "branch in a repository block"},
		{"git-path", "jobs", "", "path in a repository block"},
		{"git-username", "oauth2", "", "username in a repository block"},
		{"git-token-file", tok, "NOPS_GIT_TOKEN", "token_file in a repository block"},
		{"git-timeout", "1m", "", "timeout in a repository block"},
	}
	for _, tt := range tests {
		t.Run(tt.flag, func(t *testing.T) {
			vars := []string{removedOption{name: tt.flag}.env()}
			if tt.valueEnv != "" {
				vars = append(vars, tt.valueEnv)
			}
			_, err := Load([]string{"-" + tt.flag, tt.value}, envOf(nil), io.Discard)
			errs := map[string]error{"flag -" + tt.flag: err}
			for _, v := range vars {
				_, errs[v] = Load(nil, envOf(map[string]string{v: tt.value}), io.Discard)
			}
			for source, err := range errs {
				if err == nil || !strings.Contains(err.Error(), source+" was removed: "+tt.want) || !strings.Contains(err.Error(), `name it "default"`) {
					t.Errorf("%s: err = %v, want it removed, replaced by %s", source, err, tt.want)
				}
			}
		})
	}
}

func TestLoadTopLevelFromTheFile(t *testing.T) {
	secretFile := writeFile(t, "webhook-secret", "webhook-shared-secret\n")
	file := writeConfig(t, defaultRepo+`
log_level             = "debug"
db_path               = "/var/lib/nops/nops.db"
public_url            = "https://nops.example.com/nops/"
git_poll_interval     = "2m"
drift_interval        = "15m"
engine_interval       = "7s"
hook_poll_interval    = "3s"
apply_timeout         = "1h"
spec_retention        = "2160h"
sync_window_time_zone = "UTC"
webhook_secret_file   = "`+secretFile+`"
`)
	c, err := Load(nil, envOf(map[string]string{"NOPS_CONFIG": file, "NOPS_PUBLIC_URL": ""}), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	got := []any{c.LogLevel, c.DBPath, c.PublicURL, c.BasePath, c.GitPollInterval, c.DriftInterval, c.EngineInterval,
		c.HookPollInterval, c.ApplyTimeout, c.SpecRetention, c.SyncWindowLocation, c.WebhookSecretFile, c.WebhookSecret}
	want := []any{slog.LevelDebug, "/var/lib/nops/nops.db", "https://nops.example.com/nops", "/nops", 2 * time.Minute, 15 * time.Minute, 7 * time.Second,
		3 * time.Second, time.Hour, 2160 * time.Hour, time.UTC, secretFile, "webhook-shared-secret"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("\n got %v\nwant %v", got, want)
	}
}

// spec_retention = "0s" keeps the specs forever.
func TestLoadSpecRetentionOff(t *testing.T) {
	file := writeConfig(t, defaultRepo+`spec_retention = "0s"`+"\n")
	c, err := Load([]string{"-config", file}, envOf(map[string]string{"NOPS_PUBLIC_URL": ""}), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.SpecRetention != 0 {
		t.Errorf("spec retention = %v, want 0", c.SpecRetention)
	}
}

// The attributes of the flags that stay yield to the flag and to its
// variable; an invalid one that the flag overrides is not read.
func TestLoadTopLevelPrecedence(t *testing.T) {
	file := writeConfig(t, defaultRepo+`
log_level  = "warn"
db_path    = "from-file.db"
public_url = "https://from-file.example.com"
`)
	read := func(c *Config) [3]string { return [3]string{c.LogLevel.String(), c.DBPath, c.PublicURL} }
	tests := []struct {
		name string
		args []string
		env  map[string]string
		want [3]string
	}{
		{"the file over the default", nil, nil, [3]string{"WARN", "from-file.db", "https://from-file.example.com"}},
		{"the variable over the file", nil, map[string]string{"NOPS_LOG_LEVEL": "error", "NOPS_DB_PATH": "from-env.db", "NOPS_PUBLIC_URL": "https://from-env.example.com"},
			[3]string{"ERROR", "from-env.db", "https://from-env.example.com"}},
		{"the flag over the variable", []string{"-log-level", "debug", "-db-path", "from-flag.db", "-public-url", "https://from-flag.example.com"},
			map[string]string{"NOPS_LOG_LEVEL": "error", "NOPS_DB_PATH": "from-env.db", "NOPS_PUBLIC_URL": "https://from-env.example.com"},
			[3]string{"DEBUG", "from-flag.db", "https://from-flag.example.com"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := map[string]string{"NOPS_CONFIG": file, "NOPS_PUBLIC_URL": ""}
			for k, v := range tt.env {
				env[k] = v
			}
			c, err := Load(tt.args, envOf(env), io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			if got := read(c); got != tt.want {
				t.Errorf("log level, db path, public URL = %q, want %q", got, tt.want)
			}
		})
	}

	bad := writeConfig(t, defaultRepo+`
log_level  = "loud"
db_path    = ""
public_url = "nops.example.com"
`)
	if _, err := Load([]string{"-config", bad, "-log-level", "info", "-db-path", "nops.db", "-public-url", "https://nops.example.com"}, envOf(nil), io.Discard); err != nil {
		t.Errorf("attributes overridden by the flags: %v", err)
	}
}

// Every NOPS_* variable set is deprecated, also when its flag wins, and names
// what replaces it.
func TestLoadDeprecatedVariables(t *testing.T) {
	file := writeConfig(t, defaultRepo)
	env := map[string]string{
		"NOPS_CONFIG": file, "NOPS_PUBLIC_URL": "https://nops.example.com", "NOPS_NOMAD_ADDR": "https://nomad.example.com:4646",
		"NOPS_DB_PATH": "env.db", "NOPS_LISTEN_ADDR": ":9090", "NOPS_LOG_LEVEL": "warn",
	}
	c, err := Load([]string{"-db-path", "flag.db"}, rawEnvOf(env), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	want := []DeprecatedVariable{
		{"NOPS_NOMAD_ADDR", "addr in the nomad block of -config (docs/configuration.md#nomad)"},
		{"NOPS_CONFIG", "the -config flag"},
		{"NOPS_DB_PATH", "db_path at the top level of -config (docs/configuration.md#storage-intervals-and-timeouts)"},
		{"NOPS_LISTEN_ADDR", "addr in the listen block of -config (docs/configuration.md#listen)"},
		{"NOPS_PUBLIC_URL", "public_url at the top level of -config (docs/configuration.md#dashboard)"},
		{"NOPS_LOG_LEVEL", "log_level at the top level of -config (docs/configuration.md#storage-intervals-and-timeouts)"},
	}
	if !reflect.DeepEqual(c.DeprecatedVariables, want) {
		t.Errorf("deprecated variables = %q, want %q", c.DeprecatedVariables, want)
	}

	c, err = Load([]string{"-config", file}, rawEnvOf(map[string]string{"NOPS_DB_PATH": ""}), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.DeprecatedVariables != nil {
		t.Errorf("without variables, deprecated variables = %q, want none", c.DeprecatedVariables)
	}
}

func TestLoadTopLevelInvalid(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	empty := writeFile(t, "empty", " \n")
	tests := []struct {
		attr, value, want string
	}{
		{"log_level", `"verbose"`, "log_level: slog: level string"},
		{"db_path", `""`, "db_path: required"},
		{"public_url", `"nops.example.com"`, `public_url: "nops.example.com" is not an http`},
		{"git_poll_interval", `"-1m"`, `git_poll_interval: duration "-1m" must be positive`},
		{"drift_interval", `"soon"`, "drift_interval: time: invalid duration"},
		{"engine_interval", `"0s"`, `engine_interval: duration "0s" must be positive`},
		{"hook_poll_interval", `""`, "hook_poll_interval: time: invalid duration"},
		{"apply_timeout", `"1d"`, "apply_timeout: time: unknown unit"},
		{"apply_timeout", `600`, "apply_timeout"},
		{"spec_retention", `"-1h"`, `spec_retention: duration "-1h" must not be negative`},
		{"spec_retention", `"30d"`, "spec_retention: time: unknown unit"},
		{"sync_window_time_zone", `"Mars/Olympus"`, "sync_window_time_zone: unknown time zone"},
		{"sync_window_time_zone", `"Local"`, "sync_window_time_zone: \"Local\" is not an IANA time zone"},
		{"webhook_secret_file", `"` + missing + `"`, "webhook_secret_file: open " + missing},
		{"webhook_secret_file", `"` + empty + `"`, "webhook_secret_file: " + empty + " is empty"},
	}
	for _, tt := range tests {
		t.Run(tt.attr+"="+tt.value, func(t *testing.T) {
			file := writeConfig(t, defaultRepo+tt.attr+" = "+tt.value+"\n")
			_, err := Load([]string{"-config", file}, envOf(map[string]string{"NOPS_PUBLIC_URL": ""}), io.Discard)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want one containing %q", err, tt.want)
			}
		})
	}
}

// The options without a flag that stays moved to the top level of the file:
// each one, its variable, and the variable that held the webhook secret stop
// Nops, naming the attribute that replaces it.
func TestLoadTopLevelOptionsRemoved(t *testing.T) {
	secretFile := writeFile(t, "webhook-secret", "secret")
	tests := []struct {
		flag, value, valueEnv, want string
	}{
		{"webhook-secret-file", secretFile, "NOPS_WEBHOOK_SECRET", "webhook_secret_file at the top level of -config (docs/configuration.md#dashboard)"},
		{"git-poll-interval", "1m", "", "git_poll_interval at the top level"},
		{"drift-interval", "5m", "", "drift_interval at the top level"},
		{"engine-interval", "5s", "", "engine_interval at the top level"},
		{"hook-poll-interval", "5s", "", "hook_poll_interval at the top level"},
		{"apply-timeout", "10m", "", "apply_timeout at the top level"},
		{"sync-window-time-zone", "UTC", "", "sync_window_time_zone at the top level of -config (docs/configuration.md#storage-intervals-and-timeouts)"},
	}
	for _, tt := range tests {
		t.Run(tt.flag, func(t *testing.T) {
			vars := []string{removedOption{name: tt.flag}.env()}
			if tt.valueEnv != "" {
				vars = append(vars, tt.valueEnv)
			}
			_, err := Load([]string{"-" + tt.flag, tt.value}, envOf(nil), io.Discard)
			errs := map[string]error{"flag -" + tt.flag: err}
			for _, v := range vars {
				_, errs[v] = Load(nil, envOf(map[string]string{v: tt.value}), io.Discard)
			}
			for source, err := range errs {
				if err == nil || !strings.Contains(err.Error(), source+" was removed: "+tt.want) {
					t.Errorf("%s: err = %v, want it removed, replaced by %s", source, err, tt.want)
				}
			}
		})
	}
}

// Validate opens none of the files the configuration names: their paths are
// on the server's machine, and none of these exists.
func TestValidateReadsNoNamedFile(t *testing.T) {
	file := writeFile(t, "nops.hcl", `webhook_secret_file = "/missing/webhook"

listen {
  tls {
    cert_file = "/missing/nops.crt"
    key_file  = "/missing/nops.key"
  }
}

nomad {
  token_file  = "/missing/nomad-token"
  ca_cert     = "/missing/ca.pem"
  client_cert = "/missing/cli.pem"
  client_key  = "/missing/cli-key.pem"
}

repository "apps" {
  url        = "https://git.example.com/ops/apps.git"
  token_file = "/missing/git-token"
}

auth "basic" {
  type        = "basic"
  users_file  = "/missing/users"
  groups_file = "/missing/groups"
}

acl {
  enabled              = true
  bootstrap_token_file = "/missing/bootstrap"
}

notify "slack" {
  type     = "slack"
  url_file = "/missing/slack-url"
}

notify "gotify" {
  type       = "gotify"
  url        = "https://gotify.example.com"
  token_file = "/missing/gotify-token"
}
`)
	if err := Validate(file); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// An invalid file gets the messages Load gives without flags or variables.
func TestValidateGivesTheMessagesOfLoad(t *testing.T) {
	file := writeConfig(t, `apply_timeout = "0s"
listen {
  addr = "8080"
}
nomad {
  addr = "nomad:4646"
}
repository "a b" {
  url = "`+repo+`"
}
`)
	_, want := Load([]string{"-config", file}, rawEnvOf(nil), io.Discard)
	if want == nil {
		t.Fatal("Load: no error")
	}
	got := Validate(file)
	if got == nil || got.Error() != want.Error() {
		t.Errorf("Validate: %v\nwant the error of Load: %v", got, want)
	}
}

// Every attribute is checked, also one a flag would override.
func TestValidateChecksWhatAFlagOverrides(t *testing.T) {
	file := writeConfig(t, defaultRepo+`log_level = "loud"
listen {
  addr = "8080"
}
`)
	if _, err := Load([]string{"-config", file, "-listen-addr", ":9090", "-log-level", "info"}, envOf(nil), io.Discard); err != nil {
		t.Fatalf("Load with the flags: %v", err)
	}
	err := Validate(file)
	checkErr(t, err, "-config "+file+": ", "log_level:")
	checkErr(t, err, "-config "+file+": ", "listen: addr:")
}

// A file attribute only counts as set, so the rules between attributes still
// apply.
func TestValidateKeepsTheRulesBetweenAttributes(t *testing.T) {
	for _, tc := range []struct {
		name, content, want string
	}{
		{"git token over http", `repository "a" {
  url        = "http://git.example.com/a.git"
  token_file = "/missing/token"
}
`, "a git token needs an https:// url"},
		{"tls without a key", defaultRepo + `listen {
  tls {
    cert_file = "/missing/nops.crt"
    key_file  = ""
  }
}
`, "listen: tls: key_file: required"},
		{"client cert without a key", defaultRepo + `nomad {
  client_cert = "/missing/cli.pem"
}
`, "must be set together with client_key"},
		{"acl without a token", defaultRepo + `acl {
  enabled = true
}
`, "required with enabled = true"},
		{"token without the acl", defaultRepo + `acl {
  bootstrap_token_file = "/missing/bootstrap"
}
`, "is set without enabled = true"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(writeConfig(t, tc.content))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Validate: %v, want an error with %q", err, tc.want)
			}
		})
	}
}

func TestValidateMissingFile(t *testing.T) {
	if err := Validate(filepath.Join(t.TempDir(), "nops.hcl")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Validate: %v, want a missing file", err)
	}
}

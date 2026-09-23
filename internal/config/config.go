// Package config reads the nops settings from flags and NOPS_* environment
// variables and validates them.
//
// Every option has a flag and an environment variable (NOPS_ followed by the
// flag name in upper case, with dashes as underscores). A flag wins over the
// variable, the variable over the default. nops reads nothing else: the
// standard NOMAD_* variables are ignored. The reference for users is
// docs/configuration.md: keep it aligned with the options table here.
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/nomad/api"
)

// Config holds every setting nops needs. Load fills and validates it.
type Config struct {
	NomadAddr          string
	NomadNamespace     string
	NomadTokenFile     string
	NomadToken         string // content of NomadTokenFile, read by Load
	NomadCACert        string // paths to PEM files
	NomadClientCert    string
	NomadClientKey     string
	NomadTLSSkipVerify bool

	GitURL       string
	GitBranch    string
	GitUsername  string
	GitTokenFile string
	GitToken     string // content of GitTokenFile, read by Load

	DBPath     string
	ListenAddr string
	AuthHeader string

	// Notification adapters: each one is on when its URL is set. URLs that
	// carry a token and every token are read from files by Load.
	NotifyWebhookURLFile   string
	NotifyWebhookURL       string
	NotifyWebhookTokenFile string
	NotifyWebhookToken     string
	NotifyDiscordURLFile   string
	NotifyDiscordURL       string
	NotifySlackURLFile     string
	NotifySlackURL         string
	NotifyNtfyURL          string
	NotifyNtfyTokenFile    string
	NotifyNtfyToken        string
	NotifyGotifyURL        string
	NotifyGotifyTokenFile  string
	NotifyGotifyToken      string
	NotifyTimeout          time.Duration

	PublicURL string // external URL of the dashboard, without trailing slash; may be empty

	GitPollInterval  time.Duration
	DriftInterval    time.Duration
	EngineInterval   time.Duration
	HookPollInterval time.Duration
	ApplyTimeout     time.Duration

	LogLevel slog.Level
}

type option struct {
	name    string // flag name; the env var is derived from it
	def     string
	usage   string
	boolean bool
	set     func(c *Config, v string) error
}

func (o option) env() string {
	return "NOPS_" + strings.ToUpper(strings.ReplaceAll(o.name, "-", "_"))
}

var options = []option{
	{name: "nomad-addr", def: "http://127.0.0.1:4646", usage: "Nomad HTTP API address",
		set: func(c *Config, v string) (err error) { c.NomadAddr, err = httpURL(v); return }},
	{name: "nomad-namespace", def: api.DefaultNamespace, usage: "Nomad namespace of the managed jobs and hooks",
		set: func(c *Config, v string) (err error) { c.NomadNamespace, err = required(v); return }},
	{name: "nomad-token-file", usage: "file holding the Nomad ACL token (empty: no token)",
		set: func(c *Config, v string) (err error) {
			c.NomadTokenFile = v
			c.NomadToken, err = secretFile(v)
			return
		}},
	{name: "nomad-ca-cert", usage: "PEM file of the CA that signed the Nomad server certificate",
		set: func(c *Config, v string) (err error) { c.NomadCACert, err = readableFile(v); return }},
	{name: "nomad-client-cert", usage: "PEM client certificate for Nomad mTLS (with -nomad-client-key)",
		set: func(c *Config, v string) (err error) { c.NomadClientCert, err = readableFile(v); return }},
	{name: "nomad-client-key", usage: "PEM client key for Nomad mTLS (with -nomad-client-cert)",
		set: func(c *Config, v string) (err error) { c.NomadClientKey, err = readableFile(v); return }},
	{name: "nomad-tls-skip-verify", def: "false", boolean: true, usage: "do not verify the Nomad server certificate (insecure)",
		set: func(c *Config, v string) (err error) { c.NomadTLSSkipVerify, err = strconv.ParseBool(v); return }},

	{name: "git-url", usage: "URL of the git repository holding the jobs (required)",
		set: func(c *Config, v string) (err error) { c.GitURL, err = required(v); return }},
	{name: "git-branch", def: "main", usage: "branch to read",
		set: func(c *Config, v string) (err error) { c.GitBranch, err = required(v); return }},
	{name: "git-username", def: "git", usage: "username sent with the git token",
		set: func(c *Config, v string) error { c.GitUsername = v; return nil }},
	{name: "git-token-file", usage: "file holding the git HTTPS token (empty: public repository)",
		set: func(c *Config, v string) (err error) {
			c.GitTokenFile = v
			c.GitToken, err = secretFile(v)
			return
		}},

	{name: "db-path", def: "nops.db", usage: "SQLite database file",
		set: func(c *Config, v string) (err error) { c.DBPath, err = required(v); return }},
	{name: "listen-addr", def: ":8080", usage: "address of the dashboard and the git webhook",
		set: func(c *Config, v string) error {
			if _, _, err := net.SplitHostPort(v); err != nil {
				return fmt.Errorf("invalid address %q: %w", v, err)
			}
			c.ListenAddr = v
			return nil
		}},
	{name: "auth-header", def: "Remote-User", usage: "request header carrying the user authenticated by the reverse proxy",
		set: func(c *Config, v string) error {
			if !validHeaderName(v) {
				return fmt.Errorf("invalid header name %q", v)
			}
			c.AuthHeader = v
			return nil
		}},

	{name: "notify-webhook-url-file", usage: "file holding the URL that receives notifications as a generic JSON POST (empty: off)",
		set: func(c *Config, v string) (err error) {
			c.NotifyWebhookURLFile = v
			c.NotifyWebhookURL, err = urlFile(v)
			return
		}},
	{name: "notify-webhook-token-file", usage: "file holding a token sent to the webhook as Authorization: Bearer",
		set: func(c *Config, v string) (err error) {
			c.NotifyWebhookTokenFile = v
			c.NotifyWebhookToken, err = secretFile(v)
			return
		}},
	{name: "notify-discord-url-file", usage: "file holding the Discord webhook URL (empty: off)",
		set: func(c *Config, v string) (err error) {
			c.NotifyDiscordURLFile = v
			c.NotifyDiscordURL, err = urlFile(v)
			return
		}},
	{name: "notify-slack-url-file", usage: "file holding the Slack incoming webhook URL (empty: off)",
		set: func(c *Config, v string) (err error) {
			c.NotifySlackURLFile = v
			c.NotifySlackURL, err = urlFile(v)
			return
		}},
	{name: "notify-ntfy-url", usage: "ntfy topic URL, e.g. https://ntfy.example.com/nops (empty: off)",
		set: func(c *Config, v string) (err error) { c.NotifyNtfyURL, err = optionalURL(v); return }},
	{name: "notify-ntfy-token-file", usage: "file holding the ntfy access token",
		set: func(c *Config, v string) (err error) {
			c.NotifyNtfyTokenFile = v
			c.NotifyNtfyToken, err = secretFile(v)
			return
		}},
	{name: "notify-gotify-url", usage: "Gotify server URL; messages go to <url>/message (empty: off)",
		set: func(c *Config, v string) (err error) { c.NotifyGotifyURL, err = optionalURL(v); return }},
	{name: "notify-gotify-token-file", usage: "file holding the Gotify application token (required with -notify-gotify-url)",
		set: func(c *Config, v string) (err error) {
			c.NotifyGotifyTokenFile = v
			c.NotifyGotifyToken, err = secretFile(v)
			return
		}},
	{name: "notify-timeout", def: "10s", usage: "timeout of a notification request",
		set: func(c *Config, v string) (err error) { c.NotifyTimeout, err = positiveDuration(v); return }},
	{name: "public-url", usage: "external URL of the dashboard, used for links in notifications (empty: no links)",
		set: func(c *Config, v string) (err error) { c.PublicURL, err = optionalURL(v); return }},

	{name: "git-poll-interval", def: "1m", usage: "how often the repository is fetched",
		set: func(c *Config, v string) (err error) { c.GitPollInterval, err = positiveDuration(v); return }},
	{name: "drift-interval", def: "5m", usage: "how often Nomad is checked for drift without a new commit",
		set: func(c *Config, v string) (err error) { c.DriftInterval, err = positiveDuration(v); return }},
	{name: "engine-interval", def: "5s", usage: "how often the engine advances active deployments",
		set: func(c *Config, v string) (err error) { c.EngineInterval, err = positiveDuration(v); return }},
	{name: "hook-poll-interval", def: "5s", usage: "how often a running hook is checked",
		set: func(c *Config, v string) (err error) { c.HookPollInterval, err = positiveDuration(v); return }},
	{name: "apply-timeout", def: "10m", usage: "how long an apply may wait for the Nomad deployment to succeed",
		set: func(c *Config, v string) (err error) { c.ApplyTimeout, err = positiveDuration(v); return }},

	{name: "log-level", def: "info", usage: "log level: debug, info, warn or error",
		set: func(c *Config, v string) error { return c.LogLevel.UnmarshalText([]byte(v)) }},
}

// value is the flag.Value of every option: it only records the raw string,
// parsing happens once the source (flag, env or default) is known.
type value struct {
	s       string
	boolean bool
}

func (v *value) String() string     { return v.s }
func (v *value) Set(s string) error { v.s = s; return nil }
func (v *value) IsBoolFlag() bool   { return v.boolean }

// Load builds the configuration from the command-line arguments (without the
// program name) and the environment, read through getenv. Usage and flag
// errors are written to out. With -h it returns flag.ErrHelp. Every invalid
// value is reported, each prefixed with where it came from.
func Load(args []string, getenv func(string) string, out io.Writer) (*Config, error) {
	fs := flag.NewFlagSet("nops", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.Usage = func() { usage(out) }
	vals := make([]*value, len(options))
	for i, o := range options {
		vals[i] = &value{boolean: o.boolean}
		fs.Var(vals[i], o.name, o.usage)
	}
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected argument %q: nops takes only flags", fs.Arg(0))
	}
	set := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	c := &Config{}
	var errs []error
	for i, o := range options {
		v, source := o.def, "-"+o.name+" / "+o.env()
		if set[o.name] {
			v, source = vals[i].s, "flag -"+o.name
		} else if e := getenv(o.env()); e != "" {
			v, source = e, o.env()
		}
		if err := o.set(c, v); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", source, err))
		}
	}
	errs = append(errs, resolveSecretValues(c, getenv)...)
	errs = append(errs, c.check()...)
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return c, nil
}

// secretValue lets a secret be set with a plain, env-only variable when no
// file is given: a file (from a flag or its NOPS_<NAME>_FILE variable) always
// wins over the literal value in envVar. There is no flag for the literal
// form, so a secret can never reach the command line: only an env var is
// this permissive, since it never shows in `ps` or `/proc/<pid>/cmdline`
// (see docs/configuration.md#secrets-without-a-file).
type secretValue struct {
	envVar string // always the _FILE option's variable with _FILE dropped
	isURL  bool   // Discord/Slack/webhook URLs are validated, but never echoed
	get    func(*Config) string
	set    func(*Config, string)
}

var secretValues = []secretValue{
	{envVar: "NOPS_GIT_TOKEN",
		get: func(c *Config) string { return c.GitToken }, set: func(c *Config, v string) { c.GitToken = v }},
	{envVar: "NOPS_NOMAD_TOKEN",
		get: func(c *Config) string { return c.NomadToken }, set: func(c *Config, v string) { c.NomadToken = v }},
	{envVar: "NOPS_NOTIFY_WEBHOOK_URL", isURL: true,
		get: func(c *Config) string { return c.NotifyWebhookURL }, set: func(c *Config, v string) { c.NotifyWebhookURL = v }},
	{envVar: "NOPS_NOTIFY_WEBHOOK_TOKEN",
		get: func(c *Config) string { return c.NotifyWebhookToken }, set: func(c *Config, v string) { c.NotifyWebhookToken = v }},
	{envVar: "NOPS_NOTIFY_DISCORD_URL", isURL: true,
		get: func(c *Config) string { return c.NotifyDiscordURL }, set: func(c *Config, v string) { c.NotifyDiscordURL = v }},
	{envVar: "NOPS_NOTIFY_SLACK_URL", isURL: true,
		get: func(c *Config) string { return c.NotifySlackURL }, set: func(c *Config, v string) { c.NotifySlackURL = v }},
	{envVar: "NOPS_NOTIFY_NTFY_TOKEN",
		get: func(c *Config) string { return c.NotifyNtfyToken }, set: func(c *Config, v string) { c.NotifyNtfyToken = v }},
	{envVar: "NOPS_NOTIFY_GOTIFY_TOKEN",
		get: func(c *Config) string { return c.NotifyGotifyToken }, set: func(c *Config, v string) { c.NotifyGotifyToken = v }},
}

// resolveSecretValues fills every secret still empty after the flag/env-file
// pass from its plain env var, in place.
func resolveSecretValues(c *Config, getenv func(string) string) []error {
	var errs []error
	for _, sv := range secretValues {
		if sv.get(c) != "" {
			continue // a file already provided it
		}
		raw := getenv(sv.envVar)
		if raw == "" {
			continue // not set: the secret stays off
		}
		v := strings.TrimSpace(raw)
		if v == "" {
			errs = append(errs, fmt.Errorf("%s: is empty", sv.envVar))
			continue
		}
		if sv.isURL {
			if _, err := httpURL(v); err != nil {
				errs = append(errs, fmt.Errorf("%s: does not hold an http:// or https:// URL with a host", sv.envVar))
				continue
			}
		}
		sv.set(c, v)
	}
	return errs
}

// check validates the rules that involve more than one option.
func (c *Config) check() []error {
	var errs []error
	if c.GitToken != "" {
		if c.GitURL != "" && !strings.HasPrefix(strings.ToLower(c.GitURL), "https://") {
			errs = append(errs, fmt.Errorf("a git token needs an https:// -git-url, got %q", c.GitURL))
		}
		if c.GitUsername == "" {
			errs = append(errs, errors.New("a git token needs a non-empty -git-username"))
		}
	}
	for _, t := range []struct{ token, url, name, urlOpt string }{
		{c.NotifyWebhookToken, c.NotifyWebhookURL, "webhook", "-notify-webhook-url-file"},
		{c.NotifyNtfyToken, c.NotifyNtfyURL, "ntfy", "-notify-ntfy-url"},
		{c.NotifyGotifyToken, c.NotifyGotifyURL, "gotify", "-notify-gotify-url"},
	} {
		if t.token != "" && t.url == "" {
			errs = append(errs, fmt.Errorf("a %s token is set without a %s URL (%s)", t.name, t.name, t.urlOpt))
		}
	}
	if c.NotifyGotifyURL != "" && c.NotifyGotifyToken == "" {
		errs = append(errs, errors.New("a gotify URL needs a token (-notify-gotify-token-file or NOPS_NOTIFY_GOTIFY_TOKEN)"))
	}
	if (c.NomadClientCert == "") != (c.NomadClientKey == "") {
		errs = append(errs, errors.New("-nomad-client-cert and -nomad-client-key must be set together"))
	}
	return errs
}

// Nomad returns the Nomad client configuration. It is built field by field,
// not from api.DefaultConfig, so no NOMAD_* variable of the process reaches
// it: when nops runs as a Nomad job, Nomad sets NOMAD_NAMESPACE (and
// NOMAD_TOKEN with workload identity) in its environment.
func (c *Config) Nomad() *api.Config {
	return &api.Config{
		Address:   c.NomadAddr,
		Namespace: c.NomadNamespace,
		SecretID:  c.NomadToken,
		TLSConfig: &api.TLSConfig{
			CACert:     c.NomadCACert,
			ClientCert: c.NomadClientCert,
			ClientKey:  c.NomadClientKey,
			Insecure:   c.NomadTLSSkipVerify,
		},
	}
}

func usage(out io.Writer) {
	fmt.Fprintln(out, "Usage: nops [flags]")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Every flag can be set with its environment variable instead; the flag wins.")
	fmt.Fprintln(out)
	for _, o := range options {
		def := ""
		if o.def != "" {
			def = fmt.Sprintf(" (default %q)", o.def)
		}
		fmt.Fprintf(out, "  -%s, %s%s\n\t%s\n", o.name, o.env(), def, o.usage)
	}
}

func required(v string) (string, error) {
	if v == "" {
		return "", errors.New("required")
	}
	return v, nil
}

func positiveDuration(v string) (time.Duration, error) {
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, err
	}
	if d <= 0 {
		return 0, fmt.Errorf("duration %q must be positive", v)
	}
	return d, nil
}

func httpURL(v string) (string, error) {
	u, err := url.Parse(v)
	if err != nil {
		return "", err
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("%q is not an http:// or https:// URL with a host", v)
	}
	return v, nil
}

// optionalURL accepts an empty value or an http(s) URL with a host, and drops
// a trailing slash so paths can be appended.
func optionalURL(v string) (string, error) {
	if v == "" {
		return "", nil
	}
	u, err := httpURL(v)
	return strings.TrimRight(u, "/"), err
}

// urlFile reads a URL that carries a secret (a Discord or Slack webhook
// holds its token) from a file. Errors name the file, never the URL.
func urlFile(path string) (string, error) {
	v, err := secretFile(path)
	if err != nil || v == "" {
		return "", err
	}
	if _, err := httpURL(v); err != nil {
		return "", fmt.Errorf("%s does not hold an http:// or https:// URL with a host", path)
	}
	return v, nil
}

// readableFile checks that a path, if set, is a file that can be opened.
func readableFile(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", err
	}
	if fi.IsDir() {
		return "", fmt.Errorf("%s is a directory", path)
	}
	return path, nil
}

// secretFile reads a secret from a file, so that it never appears on the
// command line or in the job spec. Surrounding whitespace (the trailing
// newline of a rendered template) is dropped.
func secretFile(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return s, nil
}

// validHeaderName reports whether v is an HTTP token (RFC 9110, section 5.1).
func validHeaderName(v string) bool {
	if v == "" {
		return false
	}
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("!#$%&'*+-.^_`|~", r):
		default:
			return false
		}
	}
	return true
}

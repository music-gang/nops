// Package config reads the nops settings from flags, NOPS_* environment
// variables and the HCL file of -config, and validates them.
//
// Every option has a flag and an environment variable (NOPS_ followed by the
// flag name in upper case, with dashes as underscores). A flag wins over the
// variable, the variable over the default. The file holds only what a flag
// cannot: the repository, listen, nomad and auth blocks. nops reads nothing else:
// the standard NOMAD_* variables are ignored. The reference for users is docs/configuration.md:
// keep it aligned with the options table and the file's schema here.
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
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	// The zone database is embedded: a sync window is read in a named time zone
	// and the image has no /usr/share/zoneinfo to look it up in.
	_ "time/tzdata"

	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/hashicorp/nomad/api"
	"github.com/music-gang/nops/internal/secret"
)

// DefaultRepository is the name of the repository the -git-* options describe.
const DefaultRepository = "default"

// The default timeouts of the dashboard's server, set by the listen block.
const (
	defaultReadHeaderTimeout = 10 * time.Second
	defaultReadTimeout       = time.Minute
	defaultWriteTimeout      = time.Minute
	defaultIdleTimeout       = 2 * time.Minute
)

// The default deadlines of the calls to Nomad (the nomad block) and of a poll
// of a repository (the repository block, -git-timeout for the default one).
const (
	defaultNomadTimeout      = 30 * time.Second
	defaultRepositoryTimeout = 2 * time.Minute
)

// The default limit on failed logins with a username and a password, set by
// the auth block: this many failures lock the username for the window.
const (
	defaultLoginMaxFailures   = 10
	defaultLoginFailureWindow = 15 * time.Minute
)

// Config holds every setting nops needs. Load fills and validates it.
type Config struct {
	NomadAddr string
	// NomadUIURL is the address a person opens Nomad at in a browser, without
	// trailing slash; "" means the dashboard links to no Nomad page. It is never
	// derived from NomadAddr, the address nops itself uses.
	NomadUIURL         string
	NomadNamespaces    []string // the namespaces nops manages, in the order given, without duplicates
	NomadTokenFile     string
	NomadToken         string // content of NomadTokenFile, read by Load
	NomadCACert        string // paths to PEM files
	NomadClientCert    string
	NomadClientKey     string
	NomadTLSSkipVerify bool
	// NomadTimeout is how long a request to Nomad may take, from the nomad
	// block of the -config file.
	NomadTimeout time.Duration

	// The -git-* options: the repository named DefaultRepository, when GitURL
	// is set.
	GitURL       string
	GitBranch    string
	GitPath      string // subdirectory holding the job files; "" is the root
	GitUsername  string
	GitTokenFile string
	GitToken     string // content of GitTokenFile, read by Load
	GitTimeout   time.Duration

	// ConfigFile is the HCL file of -config, "" without one.
	ConfigFile string
	// Repositories are the repositories nops reads: DefaultRepository first
	// when GitURL is set, then the file's repository blocks in their order.
	// Load makes sure there is at least one.
	Repositories []Repository

	DBPath     string
	ListenAddr string

	// The timeouts of the dashboard's server, from the listen block of the
	// -config file. WriteTimeout also bounds each block of a backup download.
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration

	// The PEM files of the certificate and the key, from the tls block of the
	// listen block: both set, Nops serves HTTPS itself; both empty, plain HTTP.
	TLSCertFile string
	TLSKeyFile  string

	// The limit on failed logins with a username and a password, from the auth
	// block of the -config file (docs/configuration.md#local-users--auth-modebasic).
	// A zero in either turns it off.
	LoginMaxFailures   int
	LoginFailureWindow time.Duration

	// AuthMode picks the dashboard's login backend, "oidc" or "basic"
	// (docs/dashboard.md#authentication): mutually exclusive, so only the
	// fields of the chosen one are used, and check() requires the other's
	// to be unset.
	AuthMode string

	// OIDC login of the dashboard (docs/dashboard.md#authentication). The
	// client secret is read from a file by Load, or from its plain env var.
	OIDCIssuerURL        string // exactly as the provider announces it: a trailing slash matters
	OIDCClientID         string
	OIDCClientSecretFile string
	OIDCClientSecret     string

	// UsersFile is the local-users login's "username:bcrypt-hash" file
	// (docs/dashboard.md#local-users--auth-modebasic), used only with AuthMode "basic".
	UsersFile string

	// GroupsFile is the optional "group: user1 user2" file of the local-users
	// login (docs/dashboard.md#local-users--auth-modebasic), used only with AuthMode "basic".
	GroupsFile string

	// WebhookSecret authenticates the git forge's push webhook
	// (docs/running-nops.md#the-git-webhook): its HMAC for GitHub and Gitea, its
	// literal value for GitLab's token header. Empty disables the endpoint.
	WebhookSecretFile string
	WebhookSecret     string

	// ACL turns the access control on (docs/acl.md). The bootstrap token is
	// its management token, read from a file at every start.
	ACL                   bool
	ACLBootstrapTokenFile string
	ACLBootstrapToken     string

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

	// PublicURL is the dashboard's external URL, without trailing slash. If
	// not set explicitly, check() derives it from ListenAddr.
	PublicURL string
	// BasePath is the sub path the dashboard is served under (e.g. "/nops"),
	// read by check() from PublicURL's own path component: "" when it has
	// none. See docs/running-nops.md#under-a-sub-path.
	BasePath string

	GitPollInterval  time.Duration
	DriftInterval    time.Duration
	EngineInterval   time.Duration
	HookPollInterval time.Duration
	ApplyTimeout     time.Duration

	// SyncWindowLocation is the time zone the sync windows of the jobs
	// (nops_sync_window) are read in; UTC unless -sync-window-time-zone says
	// otherwise. See docs/meta-keys.md.
	SyncWindowLocation *time.Location

	LogLevel slog.Level
}

// Repository is one repository of jobs.
type Repository struct {
	Name      string
	URL       string
	Branch    string
	Path      string // subdirectory holding the job files; "" is the root
	Username  string
	TokenFile string
	Token     string // content of TokenFile, read by Load
	// Timeout is how long a poll of the repository may take, the clone included.
	Timeout time.Duration
	// Jobs are the "<namespace>/<job ID>" patterns of the jobs the repository
	// may declare, path.Match syntax; nil is any job.
	Jobs []string
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
	{name: "nomad-ui-url", usage: "address a person opens the Nomad UI at in a browser, e.g. https://nomad.example.com (a sub path is kept): the dashboard links to Nomad from it; unset: no link, it is never taken from -nomad-addr",
		set: func(c *Config, v string) (err error) { c.NomadUIURL, err = optionalURL(v); return }},
	{name: "nomad-namespaces", def: api.DefaultNamespace, usage: "comma-separated Nomad namespaces of the managed jobs and hooks",
		set: func(c *Config, v string) (err error) { c.NomadNamespaces, err = namespaceList(v); return }},
	{name: "nomad-token-file", usage: "`file` holding the Nomad ACL token (empty: no token)",
		set: func(c *Config, v string) (err error) {
			c.NomadTokenFile = v
			c.NomadToken, err = secretFile(v)
			return
		}},
	{name: "nomad-ca-cert", usage: "PEM `file` of the CA that signed the Nomad server certificate",
		set: func(c *Config, v string) (err error) { c.NomadCACert, err = readableFile(v); return }},
	{name: "nomad-client-cert", usage: "PEM `file` of the client certificate for Nomad mTLS (with -nomad-client-key)",
		set: func(c *Config, v string) (err error) { c.NomadClientCert, err = readableFile(v); return }},
	{name: "nomad-client-key", usage: "PEM `file` of the client key for Nomad mTLS (with -nomad-client-cert)",
		set: func(c *Config, v string) (err error) { c.NomadClientKey, err = readableFile(v); return }},
	{name: "nomad-tls-skip-verify", def: "false", boolean: true, usage: "do not verify the Nomad server certificate (insecure)",
		set: func(c *Config, v string) (err error) { c.NomadTLSSkipVerify, err = strconv.ParseBool(v); return }},

	{name: "config", usage: "HCL `file` of the options a flag can't hold",
		set: func(c *Config, v string) (err error) { c.ConfigFile, err = readableFile(v); return }},
	{name: "git-url", usage: "URL of the git repository named default (required without a repository block in -config)",
		set: func(c *Config, v string) error { c.GitURL = v; return nil }},
	{name: "git-branch", def: "main", usage: "branch to read",
		set: func(c *Config, v string) (err error) { c.GitBranch, err = required(v); return }},
	{name: "git-path", usage: "subdirectory of the repository holding the job files (empty: root)",
		set: func(c *Config, v string) (err error) { c.GitPath, err = gitPath(v); return }},
	{name: "git-username", def: "git", usage: "username sent with the git token",
		set: func(c *Config, v string) error { c.GitUsername = v; return nil }},
	{name: "git-token-file", usage: "`file` holding the git HTTPS token (empty: public repository)",
		set: func(c *Config, v string) (err error) {
			c.GitTokenFile = v
			c.GitToken, err = secretFile(v)
			return
		}},
	{name: "git-timeout", def: "2m", usage: "how long a poll of the default repository may take, the clone included",
		set: func(c *Config, v string) (err error) { c.GitTimeout, err = positiveDuration(v); return }},

	{name: "db-path", def: "nops.db", usage: "SQLite database `file`",
		set: func(c *Config, v string) (err error) { c.DBPath, err = required(v); return }},
	{name: "listen-addr", def: ":8080", usage: "address of the dashboard and the git webhook",
		set: func(c *Config, v string) error {
			if _, _, err := net.SplitHostPort(v); err != nil {
				return fmt.Errorf("invalid address %q: %w", v, err)
			}
			c.ListenAddr = v
			return nil
		}},
	{name: "auth-mode", usage: "how the dashboard logs people in: oidc or basic (required)",
		set: func(c *Config, v string) error {
			switch v {
			case "":
				return errors.New("required")
			case "oidc", "basic":
				c.AuthMode = v
				return nil
			default:
				return fmt.Errorf("must be %q or %q, not %q", "oidc", "basic", v)
			}
		}},
	{name: "oidc-issuer-url", usage: "issuer URL of the OIDC provider, exactly as it announces it (required with -auth-mode=oidc)",
		set: func(c *Config, v string) (err error) {
			if v == "" {
				return nil
			}
			// Not optionalURL: unlike other URLs, a trailing slash here is
			// significant (Authentik's issuer has one) and must be kept.
			c.OIDCIssuerURL, err = httpURL(v)
			return
		}},
	{name: "oidc-client-id", usage: "OIDC client ID of nops (required with -auth-mode=oidc)",
		set: func(c *Config, v string) error { c.OIDCClientID = v; return nil }},
	{name: "oidc-client-secret-file", usage: "`file` holding the OIDC client secret (required with -auth-mode=oidc)",
		set: func(c *Config, v string) (err error) {
			c.OIDCClientSecretFile = v
			c.OIDCClientSecret, err = secretFile(v)
			return
		}},
	{name: "users-file", usage: "`file` holding \"username:bcrypt-hash\" lines for the dashboard login (required with -auth-mode=basic)",
		set: func(c *Config, v string) (err error) { c.UsersFile, err = readableFile(v); return }},
	{name: "groups-file", usage: "`file` holding \"group: user1 user2\" lines, the groups of the local users for binding rules (optional, only with -auth-mode=basic)",
		set: func(c *Config, v string) (err error) { c.GroupsFile, err = readableFile(v); return }},

	{name: "webhook-secret-file", usage: "`file` holding the git forge's webhook secret (empty: the git webhook endpoint is off)",
		set: func(c *Config, v string) (err error) {
			c.WebhookSecretFile = v
			c.WebhookSecret, err = secretFile(v)
			return
		}},

	{name: "acl", def: "false", boolean: true, usage: "turn on the access control: tokens and ACL policies decide what each request may do (needs -acl-bootstrap-token-file)",
		set: func(c *Config, v string) (err error) { c.ACL, err = strconv.ParseBool(v); return }},
	{name: "acl-bootstrap-token-file", usage: "`file` holding the bootstrap token, a management token made by \"nops secret generate\" (required with -acl)",
		set: func(c *Config, v string) (err error) {
			c.ACLBootstrapTokenFile = v
			c.ACLBootstrapToken, err = secretFile(v)
			return
		}},

	{name: "notify-webhook-url-file", usage: "`file` holding the URL that receives notifications as a generic JSON POST (empty: off)",
		set: func(c *Config, v string) (err error) {
			c.NotifyWebhookURLFile = v
			c.NotifyWebhookURL, err = urlFile(v)
			return
		}},
	{name: "notify-webhook-token-file", usage: "`file` holding a token sent to the webhook as Authorization: Bearer",
		set: func(c *Config, v string) (err error) {
			c.NotifyWebhookTokenFile = v
			c.NotifyWebhookToken, err = secretFile(v)
			return
		}},
	{name: "notify-discord-url-file", usage: "`file` holding the Discord webhook URL (empty: off)",
		set: func(c *Config, v string) (err error) {
			c.NotifyDiscordURLFile = v
			c.NotifyDiscordURL, err = urlFile(v)
			return
		}},
	{name: "notify-slack-url-file", usage: "`file` holding the Slack incoming webhook URL (empty: off)",
		set: func(c *Config, v string) (err error) {
			c.NotifySlackURLFile = v
			c.NotifySlackURL, err = urlFile(v)
			return
		}},
	{name: "notify-ntfy-url", usage: "ntfy topic URL, e.g. https://ntfy.example.com/nops (empty: off)",
		set: func(c *Config, v string) (err error) { c.NotifyNtfyURL, err = optionalURL(v); return }},
	{name: "notify-ntfy-token-file", usage: "`file` holding the ntfy access token",
		set: func(c *Config, v string) (err error) {
			c.NotifyNtfyTokenFile = v
			c.NotifyNtfyToken, err = secretFile(v)
			return
		}},
	{name: "notify-gotify-url", usage: "Gotify server URL; messages go to <url>/message (empty: off)",
		set: func(c *Config, v string) (err error) { c.NotifyGotifyURL, err = optionalURL(v); return }},
	{name: "notify-gotify-token-file", usage: "`file` holding the Gotify application token (required with -notify-gotify-url)",
		set: func(c *Config, v string) (err error) {
			c.NotifyGotifyTokenFile = v
			c.NotifyGotifyToken, err = secretFile(v)
			return
		}},
	{name: "notify-timeout", def: "10s", usage: "timeout of a notification request",
		set: func(c *Config, v string) (err error) { c.NotifyTimeout, err = positiveDuration(v); return }},
	{name: "public-url", usage: "external URL of the dashboard: the OIDC redirect URL and the links in notifications are built on it, and its own path (if any) becomes the dashboard's base path (default: derived from -listen-addr)",
		set: func(c *Config, v string) (err error) {
			c.PublicURL, err = optionalURL(v)
			return
		}},

	{name: "git-poll-interval", def: "1m", usage: "how often the repository is fetched",
		set: func(c *Config, v string) (err error) { c.GitPollInterval, err = positiveDuration(v); return }},
	{name: "drift-interval", def: "5m", usage: "how often Nomad is checked for drift without a new commit",
		set: func(c *Config, v string) (err error) { c.DriftInterval, err = positiveDuration(v); return }},
	{name: "engine-interval", def: "5s", usage: "how often the engine advances active deployments",
		set: func(c *Config, v string) (err error) { c.EngineInterval, err = positiveDuration(v); return }},
	{name: "hook-poll-interval", def: "5s", usage: "how often a running hook is checked",
		set: func(c *Config, v string) (err error) { c.HookPollInterval, err = positiveDuration(v); return }},
	{name: "apply-timeout", def: "10m", usage: "how long an apply may take, from its start: to register the job and to wait until it is healthy",
		set: func(c *Config, v string) (err error) { c.ApplyTimeout, err = positiveDuration(v); return }},
	{name: "sync-window-time-zone", def: "UTC", usage: "IANA time zone (Europe/Rome, America/New_York, UTC) the sync windows of the jobs (nops_sync_window) are read in",
		set: func(c *Config, v string) (err error) { c.SyncWindowLocation, err = timeZone(v); return }},

	{name: "log-level", def: "info", usage: "log level: debug, info, warn or error",
		set: func(c *Config, v string) error { return c.LogLevel.UnmarshalText([]byte(v)) }},
}

// removedOption is an option that no longer exists. It still parses, so that
// Nops refuses to start with it and says what replaces it, instead of the
// bare "flag provided but not defined".
type removedOption struct {
	name, replacedBy string
	// valueEnv is the plain variable that held the secret of a -<name>-file
	// option, removed with it.
	valueEnv string
}

func (o removedOption) env() string { return option{name: o.name}.env() }

var removedOptions = []removedOption{
	{name: "oidc-allowed-users", replacedBy: "limit who can log in at the identity provider, or with binding rules and -acl (docs/acl.md#binding-rules)"},
	{name: "oidc-allowed-groups", replacedBy: "limit who can log in at the identity provider, or with binding rules and -acl (docs/acl.md#binding-rules)"},
	{name: "metrics-token-file", valueEnv: "NOPS_METRICS_TOKEN", replacedBy: "give Prometheus a token with the metrics capability (docs/metrics.md#scraping)"},
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

// FlagSet is the flags of "nops serve", for what only reads them, such as the
// shell completion. The usage of a flag that takes a file quotes its value
// name, `file`, as the flag package reads it.
func FlagSet(out io.Writer) *flag.FlagSet {
	fs, _ := newFlagSet(out)
	return fs
}

func newFlagSet(out io.Writer) (*flag.FlagSet, []*value) {
	fs := flag.NewFlagSet("nops serve", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.Usage = func() { usage(out) }
	vals := make([]*value, len(options))
	for i, o := range options {
		vals[i] = &value{boolean: o.boolean}
		fs.Var(vals[i], o.name, o.usage)
	}
	return fs, vals
}

// Load builds the configuration from the command-line arguments (without
// "nops serve") and the environment, read through getenv. Usage and flag errors are
// written to out. With -h it returns flag.ErrHelp before any option is
// validated. Every invalid value is reported, each prefixed with where it
// came from.
func Load(args []string, getenv func(string) string, out io.Writer) (*Config, error) {
	fs, vals := newFlagSet(out)
	for _, o := range removedOptions {
		fs.Var(&value{}, o.name, "removed")
	}
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected argument %q: nops serve takes only flags", fs.Arg(0))
	}
	set := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	c := &Config{
		NomadTimeout:      defaultNomadTimeout,
		ReadHeaderTimeout: defaultReadHeaderTimeout,
		ReadTimeout:       defaultReadTimeout,
		WriteTimeout:      defaultWriteTimeout,
		IdleTimeout:       defaultIdleTimeout,

		LoginMaxFailures:   defaultLoginMaxFailures,
		LoginFailureWindow: defaultLoginFailureWindow,
	}
	var errs []error
	explicit := make(map[string]bool) // the options given by flag or variable
	for i, o := range options {
		v, source := o.def, "-"+o.name+" / "+o.env()
		if set[o.name] {
			v, source = vals[i].s, "flag -"+o.name
			explicit[o.name] = true
		} else if e := getenv(o.env()); e != "" {
			v, source = e, o.env()
			explicit[o.name] = true
		}
		if err := o.set(c, v); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", source, err))
		}
	}
	for _, o := range removedOptions {
		switch {
		case set[o.name]:
			errs = append(errs, fmt.Errorf("flag -%s was removed: %s", o.name, o.replacedBy))
		case getenv(o.env()) != "":
			errs = append(errs, fmt.Errorf("%s was removed: %s", o.env(), o.replacedBy))
		case o.valueEnv != "" && getenv(o.valueEnv) != "":
			errs = append(errs, fmt.Errorf("%s was removed: %s", o.valueEnv, o.replacedBy))
		}
	}
	if getenv("NOPS_NOMAD_NAMESPACE") != "" {
		errs = append(errs, errors.New("NOPS_NOMAD_NAMESPACE was replaced by NOPS_NOMAD_NAMESPACES (a comma-separated list)"))
	}
	errs = append(errs, resolveSecretValues(c, getenv)...)
	fc, fileErr := readConfigFile(c.ConfigFile)
	errs = append(errs, c.loadRepositories(explicit, fc, fileErr)...)
	errs = append(errs, c.loadListen(fc)...)
	errs = append(errs, c.loadNomad(fc)...)
	errs = append(errs, c.loadAuth(fc)...)
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
// (see docs/configuration.md#secrets).
type secretValue struct {
	envVar string // always the _FILE option's variable with _FILE dropped
	isURL  bool   // Discord/Slack/webhook URLs are validated, but never echoed
	get    func(*Config) string
	set    func(*Config, string)
}

var secretValues = []secretValue{
	{envVar: "NOPS_OIDC_CLIENT_SECRET",
		get: func(c *Config) string { return c.OIDCClientSecret }, set: func(c *Config, v string) { c.OIDCClientSecret = v }},
	{envVar: "NOPS_GIT_TOKEN",
		get: func(c *Config) string { return c.GitToken }, set: func(c *Config, v string) { c.GitToken = v }},
	{envVar: "NOPS_NOMAD_TOKEN",
		get: func(c *Config) string { return c.NomadToken }, set: func(c *Config, v string) { c.NomadToken = v }},
	{envVar: "NOPS_WEBHOOK_SECRET",
		get: func(c *Config) string { return c.WebhookSecret }, set: func(c *Config, v string) { c.WebhookSecret = v }},
	{envVar: "NOPS_ACL_BOOTSTRAP_TOKEN",
		get: func(c *Config) string { return c.ACLBootstrapToken }, set: func(c *Config, v string) { c.ACLBootstrapToken = v }},
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

// check validates the rules that involve more than one option, and fills
// the one default that depends on another option's value (-public-url on
// -listen-addr).
func (c *Config) check() []error {
	var errs []error
	if c.PublicURL == "" {
		c.PublicURL = publicURLDefault(c.ListenAddr, c.TLSCertFile != "")
	}
	c.BasePath = basePathOf(c.PublicURL)
	for _, r := range c.Repositories {
		if r.Token == "" {
			continue
		}
		url, username := "-git-url", "-git-username"
		if r.Name != DefaultRepository || c.GitURL == "" { // a repository block
			url, username = fmt.Sprintf("url in repository %q", r.Name), fmt.Sprintf("username in repository %q", r.Name)
		}
		if r.URL != "" && !strings.HasPrefix(strings.ToLower(r.URL), "https://") {
			errs = append(errs, fmt.Errorf("a git token needs an https:// %s, got %q", url, r.URL))
		}
		if r.Username == "" {
			errs = append(errs, fmt.Errorf("a git token needs a non-empty %s", username))
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
	oidcSet := c.OIDCIssuerURL != "" || c.OIDCClientID != "" || c.OIDCClientSecret != ""
	switch c.AuthMode {
	case "oidc":
		if c.OIDCIssuerURL == "" {
			errs = append(errs, errors.New("-oidc-issuer-url is required with -auth-mode=oidc"))
		}
		if c.OIDCClientID == "" {
			errs = append(errs, errors.New("-oidc-client-id is required with -auth-mode=oidc"))
		}
		if c.OIDCClientSecret == "" {
			errs = append(errs, errors.New("the OIDC client secret is required with -auth-mode=oidc (-oidc-client-secret-file or NOPS_OIDC_CLIENT_SECRET)"))
		}
		if c.UsersFile != "" {
			errs = append(errs, errors.New("-users-file is only used with -auth-mode=basic"))
		}
		if c.GroupsFile != "" {
			errs = append(errs, errors.New("-groups-file is only used with -auth-mode=basic"))
		}
	case "basic":
		if c.UsersFile == "" {
			errs = append(errs, errors.New("-users-file is required with -auth-mode=basic"))
		}
		if oidcSet {
			errs = append(errs, errors.New("the -oidc-* options are only used with -auth-mode=oidc"))
		}
	}
	switch {
	case c.ACL && c.ACLBootstrapToken == "":
		errs = append(errs, errors.New("-acl needs a bootstrap token (-acl-bootstrap-token-file or NOPS_ACL_BOOTSTRAP_TOKEN), made by \"nops secret generate\""))
	case !c.ACL && c.ACLBootstrapToken != "":
		errs = append(errs, errors.New("a bootstrap token is set without -acl"))
	case c.ACL && !secret.Valid(c.ACLBootstrapToken):
		errs = append(errs, errors.New("the bootstrap token is not a secret made by \"nops secret generate\""))
	}
	if (c.NomadClientCert == "") != (c.NomadClientKey == "") {
		errs = append(errs, errors.New("-nomad-client-cert and -nomad-client-key must be set together"))
	}
	return errs
}

// repositoryName is what a repository block's label may be: it names the
// repository on the dashboard, in a metric label and in a notification.
var repositoryName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// fileConfig is the schema of the -config file.
type fileConfig struct {
	Repositories []fileRepository `hcl:"repository,block"`
	Listen       *fileListen      `hcl:"listen,block"`
	Nomad        *fileNomad       `hcl:"nomad,block"`
	Auth         []fileAuth       `hcl:"auth,block"`
}

type fileAuth struct {
	Name          string  `hcl:"name,label"`
	Type          string  `hcl:"type"`
	MaxFailures   *int    `hcl:"max_failures,optional"`
	FailureWindow *string `hcl:"failure_window,optional"`
}

type fileNomad struct {
	Timeout *string `hcl:"timeout,optional"`
}

type fileListen struct {
	ReadHeaderTimeout *string  `hcl:"read_header_timeout,optional"`
	ReadTimeout       *string  `hcl:"read_timeout,optional"`
	WriteTimeout      *string  `hcl:"write_timeout,optional"`
	IdleTimeout       *string  `hcl:"idle_timeout,optional"`
	TLS               *fileTLS `hcl:"tls,block"`
}

// fileTLS has both files required, so a block with one alone or neither is a
// decoding error.
type fileTLS struct {
	CertFile string `hcl:"cert_file"`
	KeyFile  string `hcl:"key_file"`
}

type fileRepository struct {
	Name      string    `hcl:"name,label"`
	URL       string    `hcl:"url"`
	Branch    *string   `hcl:"branch,optional"`
	Path      string    `hcl:"path,optional"`
	Username  *string   `hcl:"username,optional"`
	TokenFile string    `hcl:"token_file,optional"`
	Jobs      *[]string `hcl:"jobs,optional"`
	Timeout   *string   `hcl:"timeout,optional"`
}

// loadRepositories fills c.Repositories from the -git-* options and the
// repository blocks of c.ConfigFile. explicit tells the -git-* options given
// by flag or variable from their defaults: without -git-url they describe no
// repository, so one given is an error rather than ignored. fc is the decoded
// -config file, nil without one, and fileErr why it could not be read.
func (c *Config) loadRepositories(explicit map[string]bool, fc *fileConfig, fileErr error) []error {
	var errs []error
	if c.GitURL != "" {
		c.Repositories = append(c.Repositories, Repository{
			Name: DefaultRepository, URL: c.GitURL, Branch: c.GitBranch, Path: c.GitPath,
			Username: c.GitUsername, TokenFile: c.GitTokenFile, Token: c.GitToken,
			Timeout: c.GitTimeout,
		})
	} else {
		for _, name := range []string{"git-branch", "git-path", "git-username", "git-token-file", "git-timeout"} {
			if explicit[name] {
				errs = append(errs, fmt.Errorf("-%s is set without -git-url", name))
			}
		}
		if c.GitToken != "" && !explicit["git-token-file"] {
			errs = append(errs, errors.New("NOPS_GIT_TOKEN is set without -git-url"))
		}
	}
	if fileErr != nil {
		errs = append(errs, fileErr)
	}
	if fc != nil {
		repos, repoErrs := repositoriesOf(fc, c.ConfigFile, c.NomadNamespaces)
		c.Repositories = append(c.Repositories, repos...)
		errs = append(errs, repoErrs...)
	}

	seen := make(map[string]bool, len(c.Repositories))
	for _, r := range c.Repositories {
		switch {
		case seen[r.Name] && r.Name == DefaultRepository && c.GitURL != "":
			errs = append(errs, fmt.Errorf("-config %s: repository %q is the one of -git-url: set one or the other", c.ConfigFile, r.Name))
		case seen[r.Name]:
			errs = append(errs, fmt.Errorf("-config %s: repository %q is declared twice", c.ConfigFile, r.Name))
		}
		seen[r.Name] = true
	}
	if len(c.Repositories) == 0 && len(errs) == 0 {
		errs = append(errs, errors.New("-git-url or a repository block in -config is required"))
	}
	return errs
}

// readConfigFile parses and decodes the -config file at file; "" is no file,
// and gives nil.
func readConfigFile(file string) (*fileConfig, error) {
	if file == "" {
		return nil, nil
	}
	f, diags := hclparse.NewParser().ParseHCLFile(file)
	if diags.HasErrors() {
		return nil, fmt.Errorf("-config: %w", diags)
	}
	var fc fileConfig
	if diags := gohcl.DecodeBody(f.Body, nil, &fc); diags.HasErrors() {
		return nil, fmt.Errorf("-config: %w", diags)
	}
	return &fc, nil
}

// loadListen sets the timeouts and the TLS files of the listen block of fc,
// when it has one. Every error names the file and the attribute.
func (c *Config) loadListen(fc *fileConfig) []error {
	if fc == nil || fc.Listen == nil {
		return nil
	}
	var errs []error
	for _, a := range []struct {
		name string
		in   *string
		out  *time.Duration
	}{
		{"read_header_timeout", fc.Listen.ReadHeaderTimeout, &c.ReadHeaderTimeout},
		{"read_timeout", fc.Listen.ReadTimeout, &c.ReadTimeout},
		{"write_timeout", fc.Listen.WriteTimeout, &c.WriteTimeout},
		{"idle_timeout", fc.Listen.IdleTimeout, &c.IdleTimeout},
	} {
		if a.in == nil {
			continue
		}
		d, err := positiveDuration(*a.in)
		if err != nil {
			errs = append(errs, fmt.Errorf("-config %s: listen: %s: %w", c.ConfigFile, a.name, err))
			continue
		}
		*a.out = d
	}
	if t := fc.Listen.TLS; t != nil {
		for _, f := range []struct {
			name string
			in   string
			out  *string
		}{
			{"cert_file", t.CertFile, &c.TLSCertFile},
			{"key_file", t.KeyFile, &c.TLSKeyFile},
		} {
			if f.in == "" {
				errs = append(errs, fmt.Errorf("-config %s: listen: tls: %s: required", c.ConfigFile, f.name))
				continue
			}
			file, err := readableFile(f.in)
			if err != nil {
				errs = append(errs, fmt.Errorf("-config %s: listen: tls: %s: %w", c.ConfigFile, f.name, err))
				continue
			}
			*f.out = file
		}
	}
	return errs
}

// loadNomad sets the timeout of the nomad block of fc, when it has one. An
// error names the file and the attribute.
func (c *Config) loadNomad(fc *fileConfig) []error {
	if fc == nil || fc.Nomad == nil || fc.Nomad.Timeout == nil {
		return nil
	}
	d, err := positiveDuration(*fc.Nomad.Timeout)
	if err != nil {
		return []error{fmt.Errorf("-config %s: nomad: timeout: %w", c.ConfigFile, err)}
	}
	c.NomadTimeout = d
	return nil
}

// loadAuth sets the limit on failed logins from the auth block of fc, when it
// has one. The method is named basic, as a binding rule names it, and its
// backend too: another name, a second block or one with -auth-mode=oidc is an
// error. Every error names the file and the attribute.
func (c *Config) loadAuth(fc *fileConfig) []error {
	if fc == nil || len(fc.Auth) == 0 {
		return nil
	}
	fail := func(format string, a ...any) []error {
		return []error{fmt.Errorf("-config %s: auth: %s", c.ConfigFile, fmt.Sprintf(format, a...))}
	}
	if len(fc.Auth) > 1 {
		return fail("only one auth block is allowed")
	}
	b := fc.Auth[0]
	switch {
	case b.Name != "basic":
		return fail(`the label is %q, only "basic" is known`, b.Name)
	case b.Type != "basic":
		return fail(`type: is %q, only "basic" is known`, b.Type)
	case c.AuthMode != "basic":
		return fail(`the "basic" block is only used with -auth-mode=basic`)
	}
	var errs []error
	if b.MaxFailures != nil {
		if *b.MaxFailures < 0 {
			errs = append(errs, fmt.Errorf("-config %s: auth: max_failures: %d must not be negative", c.ConfigFile, *b.MaxFailures))
		} else {
			c.LoginMaxFailures = *b.MaxFailures
		}
	}
	if b.FailureWindow != nil {
		d, err := nonNegativeDuration(*b.FailureWindow)
		if err != nil {
			errs = append(errs, fmt.Errorf("-config %s: auth: failure_window: %w", c.ConfigFile, err))
		} else {
			c.LoginFailureWindow = d
		}
	}
	return errs
}

// repositoriesOf reads the repository blocks of fc, the decoded -config file
// at file. Every error names the file, and the block it is about. namespaces
// are the managed ones, which the namespace of a jobs pattern must match.
func repositoriesOf(fc *fileConfig, file string, namespaces []string) ([]Repository, []error) {
	var (
		repos []Repository
		errs  []error
	)
	for _, b := range fc.Repositories {
		r := Repository{Name: b.Name, Branch: "main", Username: "git", TokenFile: b.TokenFile, Timeout: defaultRepositoryTimeout}
		var blockErrs []error
		fail := func(attr string, err error) {
			blockErrs = append(blockErrs, fmt.Errorf("-config %s: repository %q: %s: %w", file, b.Name, attr, err))
		}
		if !repositoryName.MatchString(b.Name) {
			blockErrs = append(blockErrs, fmt.Errorf("-config %s: repository %q: the name may hold only letters, digits, - and _", file, b.Name))
		}
		var err error
		if r.URL, err = required(b.URL); err != nil {
			fail("url", err)
		}
		if b.Branch != nil {
			if r.Branch, err = required(*b.Branch); err != nil {
				fail("branch", err)
			}
		}
		if r.Path, err = gitPath(b.Path); err != nil {
			fail("path", err)
		}
		if b.Username != nil {
			r.Username = *b.Username
		}
		if r.Token, err = secretFile(b.TokenFile); err != nil {
			fail("token_file", err)
		}
		if b.Jobs != nil {
			if r.Jobs, err = jobPatterns(*b.Jobs, namespaces); err != nil {
				fail("jobs", err)
			}
		}
		if b.Timeout != nil {
			if r.Timeout, err = positiveDuration(*b.Timeout); err != nil {
				fail("timeout", err)
			}
		}
		if len(blockErrs) > 0 {
			errs = append(errs, blockErrs...)
			continue
		}
		repos = append(repos, r)
	}
	return repos, errs
}

// jobPatterns checks the jobs of a repository block and gives each pattern its
// namespace: an entry without a "/" is a job ID in the default namespace. The
// namespace of an entry must match a managed one, so a typo stops the start
// instead of refusing every job.
func jobPatterns(list, namespaces []string) ([]string, error) {
	if len(list) == 0 {
		return nil, errors.New("is empty: remove the block to stop reading the repository")
	}
	out := make([]string, len(list))
	for i, p := range list {
		if !strings.Contains(p, "/") {
			p = api.DefaultNamespace + "/" + p
		}
		ns, id, _ := strings.Cut(p, "/")
		if ns == "" || id == "" {
			return nil, fmt.Errorf("%q must be <namespace>/<job ID>", list[i])
		}
		if _, err := path.Match(p, ""); err != nil {
			return nil, fmt.Errorf("%q: %w", list[i], err)
		}
		if !slices.ContainsFunc(namespaces, func(managed string) bool { ok, _ := path.Match(ns, managed); return ok }) {
			return nil, fmt.Errorf("%q matches no managed namespace (%s)", list[i], strings.Join(namespaces, ", "))
		}
		out[i] = p
	}
	return out, nil
}

// Nomad returns the Nomad client configuration. It is built field by field,
// not from api.DefaultConfig, so no NOMAD_* variable of the process reaches
// it: when nops runs as a Nomad job, Nomad sets NOMAD_NAMESPACE (and
// NOMAD_TOKEN with workload identity) in its environment. It carries no
// namespace: nomadx sets one on every request.
func (c *Config) Nomad() *api.Config {
	return &api.Config{
		Address:  c.NomadAddr,
		SecretID: c.NomadToken,
		TLSConfig: &api.TLSConfig{
			CACert:     c.NomadCACert,
			ClientCert: c.NomadClientCert,
			ClientKey:  c.NomadClientKey,
			Insecure:   c.NomadTLSSkipVerify,
		},
	}
}

func usage(out io.Writer) {
	fmt.Fprintln(out, "Usage: nops serve [flags]")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Every flag can be set with its environment variable instead; the flag wins.")
	fmt.Fprintln(out)
	for _, o := range options {
		def := ""
		if o.def != "" {
			def = fmt.Sprintf(" (default %q)", o.def)
		}
		fmt.Fprintf(out, "  -%s, %s%s\n\t%s\n", o.name, o.env(), def, strings.ReplaceAll(o.usage, "`", ""))
	}
}

func required(v string) (string, error) {
	if v == "" {
		return "", errors.New("required")
	}
	return v, nil
}

// gitPath validates -git-path: a relative path inside the repository
// (gitwatch joins it to the tree it reads), or empty for the root.
func gitPath(v string) (string, error) {
	if v == "" {
		return "", nil
	}
	clean := path.Clean(v)
	if clean == "." {
		return "", nil
	}
	if strings.HasPrefix(clean, "/") || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("%q must be a relative path inside the repository", v)
	}
	return clean, nil
}

// csvList splits a comma-separated list, dropping blanks around and between
// the items. An empty value is an empty (nil) list.
func csvList(v string) []string {
	var out []string
	for _, item := range strings.Split(v, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

// namespaceList parses -nomad-namespaces: a non-empty comma-separated list,
// with a repeated name kept once (the order of the first occurrence stands).
func namespaceList(v string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, ns := range csvList(v) {
		if !seen[ns] {
			seen[ns] = true
			out = append(out, ns)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("required")
	}
	return out, nil
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

// nonNegativeDuration is positiveDuration with zero allowed, for a limit that
// zero turns off.
func nonNegativeDuration(v string) (time.Duration, error) {
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, err
	}
	if d < 0 {
		return 0, fmt.Errorf("duration %q must not be negative", v)
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

// timeZone loads an IANA time zone by name. "Local" is refused: what the
// machine is set to is not something a config file should depend on, and a
// container usually has none.
func timeZone(v string) (*time.Location, error) {
	if v == "" || v == "Local" {
		return nil, fmt.Errorf("%q is not an IANA time zone name (Europe/Rome, America/New_York, UTC)", v)
	}
	loc, err := time.LoadLocation(v)
	if err != nil {
		return nil, fmt.Errorf("unknown time zone %q (want an IANA name such as Europe/Rome or UTC): %w", v, err)
	}
	return loc, nil
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

// publicURLDefault derives a default -public-url from -listen-addr, for an
// operator with no reverse proxy in front who would otherwise have nothing
// meaningful to put there (typically -auth-mode=basic on a personal
// machine, though the default applies to -auth-mode=oidc too: it is wrong
// for a real OIDC provider, same as forgetting -public-url used to be, just
// no longer a hard stop at startup). A specific host is kept as is; an
// address with no host, or the wildcard 0.0.0.0/[::] (listen on every
// interface), becomes localhost, since that says nothing about which
// interface a browser should use. listenAddr is assumed already validated
// by the -listen-addr option (net.SplitHostPort); an invalid one yields "",
// same as no default, since that failure is already reported on its own. The
// scheme is https when Nops serves TLS itself (tls).
func publicURLDefault(listenAddr string, tls bool) string {
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return ""
	}
	switch host {
	case "", "0.0.0.0", "::":
		host = "localhost"
	}
	scheme := "http"
	if tls {
		scheme = "https"
	}
	return scheme + "://" + net.JoinHostPort(host, port)
}

// basePathOf reads the dashboard's base path from -public-url's own path
// component: a public URL of "https://host/nops" mounts the dashboard under
// /nops, exactly what a reverse proxy must forward to it unstripped
// (docs/running-nops.md#under-a-sub-path). publicURL is assumed already validated as
// an http(s) URL by the -public-url option; a parse failure here is
// unreachable and just yields no base path, same as none set.
func basePathOf(publicURL string) string {
	u, err := url.Parse(publicURL)
	if err != nil {
		return ""
	}
	p := path.Clean(u.Path)
	if p == "." || p == "/" {
		return ""
	}
	return p
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
	f, err := os.Open(path) // #nosec G304 -- the path is an operator's flag or environment variable
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
	b, err := os.ReadFile(path) // #nosec G304 -- the path is an operator's flag or environment variable
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return s, nil
}

// Package config reads the nops settings from flags, NOPS_* environment
// variables and the HCL file of -config, and validates them.
//
// Every option is in the file. A few also have a flag and an environment
// variable (NOPS_ followed by the flag name in upper case, with dashes as
// underscores): a flag wins over the variable, the variable over the file, the
// file over the default. nops reads nothing else: the standard NOMAD_*
// variables are ignored. The reference for users is docs/configuration.md:
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

// The default timeouts of the dashboard's server, set by the listen block.
const (
	defaultReadHeaderTimeout = 10 * time.Second
	defaultReadTimeout       = time.Minute
	defaultWriteTimeout      = time.Minute
	defaultIdleTimeout       = 2 * time.Minute
)

// defaultNomadAddr is the address of Nomad's HTTP API on the local agent.
const defaultNomadAddr = "http://127.0.0.1:4646"

// The default deadlines of the calls to Nomad (the nomad block) and of a poll
// of a repository (the repository block).
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

// defaultNotifyTimeout is how long a notification request may take, unless
// its notify block says otherwise.
const defaultNotifyTimeout = 10 * time.Second

// The default intervals of the loops and timeout of an apply, set by the
// top-level attributes of the file.
const (
	defaultGitPollInterval  = time.Minute
	defaultDriftInterval    = 5 * time.Minute
	defaultEngineInterval   = 5 * time.Second
	defaultHookPollInterval = 5 * time.Second
	defaultApplyTimeout     = 10 * time.Minute
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
	// NomadTimeout is how long a request to Nomad may take.
	NomadTimeout time.Duration

	// ConfigFile is the HCL file of -config, "" without one.
	ConfigFile string
	// Repositories are the repositories nops reads, the repository blocks of
	// the -config file in their order. Load makes sure there is at least one.
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

	// Auth is the dashboard's auth method, the auth block of the -config file
	// (docs/configuration.md#authentication).
	Auth Auth

	// WebhookSecret authenticates the git forge's push webhook
	// (docs/running-nops.md#the-git-webhook): its HMAC for GitHub and Gitea, its
	// literal value for GitLab's token header. Empty disables the endpoint.
	WebhookSecretFile string
	WebhookSecret     string

	// ACL turns the access control on (docs/acl.md), from the acl block of the
	// -config file. The bootstrap token is its management token, read from a
	// file at every start.
	ACL                   bool
	ACLBootstrapTokenFile string
	ACLBootstrapToken     string

	// Notify are the notification adapters, the notify blocks of the -config
	// file in their order.
	Notify []Notify

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
	// (nops_sync_window) are read in; UTC unless sync_window_time_zone says
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

// Auth is the auth method of the dashboard, the auth block of the -config
// file. Only the fields of its Type are set.
type Auth struct {
	// Name is the block's label: the name binding rules give the method.
	Name string
	Type string // oidc or basic

	// The OIDC login (type oidc).
	IssuerURL        string // exactly as the provider announces it: a trailing slash matters
	ClientID         string
	ClientSecretFile string
	ClientSecret     string // content of ClientSecretFile

	// The local users (type basic): the "username:bcrypt-hash" file, the
	// optional "group: user1 user2" file, and the limit on failed logins, which
	// a zero in either turns off (docs/dashboard.md#local-users).
	UsersFile     string
	GroupsFile    string
	MaxFailures   int
	FailureWindow time.Duration
}

// Notify is one notification adapter, a notify block of the -config file.
type Notify struct {
	Name    string
	Type    string // webhook, discord, slack, ntfy or gotify
	URL     string // read from url_file for webhook, discord and slack: it may hold a token
	Token   string // content of token_file
	Timeout time.Duration
}

type option struct {
	name  string // flag name; the env var is derived from it
	def   string
	usage string
	set   func(c *Config, v string) error
}

func (o option) env() string {
	return "NOPS_" + strings.ToUpper(strings.ReplaceAll(o.name, "-", "_"))
}

var options = []option{
	{name: "nomad-addr", def: defaultNomadAddr, usage: "Nomad HTTP API address (wins over addr in the nomad block of -config)",
		set: func(c *Config, v string) (err error) { c.NomadAddr, err = httpURL(v); return }},

	{name: "config", usage: "HCL `file` of the options",
		set: func(c *Config, v string) (err error) { c.ConfigFile, err = readableFile(v); return }},

	{name: "db-path", def: "nops.db", usage: "SQLite database `file` (wins over db_path in -config)",
		set: func(c *Config, v string) (err error) { c.DBPath, err = required(v); return }},
	{name: "listen-addr", def: ":8080", usage: "address of the dashboard and the git webhook (wins over addr in the listen block of -config)",
		set: (*Config).setListenAddr},

	{name: "public-url", usage: "external URL of the dashboard: the OIDC redirect URL and the links in notifications are built on it, and its own path (if any) becomes the dashboard's base path (default: derived from -listen-addr; wins over public_url in -config)",
		set: func(c *Config, v string) (err error) {
			c.PublicURL, err = optionalURL(v)
			return
		}},

	{name: "log-level", def: "info", usage: "log level: debug, info, warn or error (wins over log_level in -config)",
		set: func(c *Config, v string) error { return c.LogLevel.UnmarshalText([]byte(v)) }},
}

// setListenAddr sets the address of the dashboard's server, a host and a port.
func (c *Config) setListenAddr(v string) error {
	if _, _, err := net.SplitHostPort(v); err != nil {
		return fmt.Errorf("invalid address %q: %w", v, err)
	}
	c.ListenAddr = v
	return nil
}

// removedOption is an option that no longer exists. It still parses, so that
// Nops refuses to start with it and says what replaces it, instead of the
// bare "flag provided but not defined".
type removedOption struct {
	name, replacedBy string
	boolean          bool // parsed as a boolean flag, so it needs no value
	// valueEnv is the plain variable that held the secret of a -<name>-file
	// option, removed with it.
	valueEnv string
}

func (o removedOption) env() string { return option{name: o.name}.env() }

var removedOptions = []removedOption{
	{name: "oidc-allowed-users", replacedBy: "limit who can log in at the identity provider, or with binding rules and the ACL on (docs/acl.md#binding-rules)"},
	{name: "oidc-allowed-groups", replacedBy: "limit who can log in at the identity provider, or with binding rules and the ACL on (docs/acl.md#binding-rules)"},
	{name: "metrics-token-file", valueEnv: "NOPS_METRICS_TOKEN", replacedBy: "give Prometheus a token with the metrics capability (docs/metrics.md#scraping)"},
	{name: "notify-webhook-url-file", valueEnv: "NOPS_NOTIFY_WEBHOOK_URL", replacedBy: notifyReplacement("url_file", "webhook")},
	{name: "notify-webhook-token-file", valueEnv: "NOPS_NOTIFY_WEBHOOK_TOKEN", replacedBy: notifyReplacement("token_file", "webhook")},
	{name: "notify-discord-url-file", valueEnv: "NOPS_NOTIFY_DISCORD_URL", replacedBy: notifyReplacement("url_file", "discord")},
	{name: "notify-slack-url-file", valueEnv: "NOPS_NOTIFY_SLACK_URL", replacedBy: notifyReplacement("url_file", "slack")},
	{name: "notify-ntfy-url", replacedBy: notifyReplacement("url", "ntfy")},
	{name: "notify-ntfy-token-file", valueEnv: "NOPS_NOTIFY_NTFY_TOKEN", replacedBy: notifyReplacement("token_file", "ntfy")},
	{name: "notify-gotify-url", replacedBy: notifyReplacement("url", "gotify")},
	{name: "notify-gotify-token-file", valueEnv: "NOPS_NOTIFY_GOTIFY_TOKEN", replacedBy: notifyReplacement("token_file", "gotify")},
	{name: "notify-timeout", replacedBy: "timeout in each notify block of -config (docs/configuration.md#notifications)"},
	{name: "acl", boolean: true, replacedBy: "enabled in the acl block of -config (docs/configuration.md#access-control)"},
	{name: "acl-bootstrap-token-file", valueEnv: "NOPS_ACL_BOOTSTRAP_TOKEN", replacedBy: "bootstrap_token_file in the acl block of -config (docs/configuration.md#access-control)"},
	{name: "auth-mode", replacedBy: "type in the auth block of -config (docs/configuration.md#authentication)"},
	{name: "oidc-issuer-url", replacedBy: authReplacement("issuer_url", "oidc")},
	{name: "oidc-client-id", replacedBy: authReplacement("client_id", "oidc")},
	{name: "oidc-client-secret-file", valueEnv: "NOPS_OIDC_CLIENT_SECRET", replacedBy: authReplacement("client_secret_file", "oidc")},
	{name: "users-file", replacedBy: authReplacement("users_file", "basic")},
	{name: "groups-file", replacedBy: authReplacement("groups_file", "basic")},
	{name: "nomad-namespace", replacedBy: nomadReplacement("namespaces")},
	{name: "nomad-namespaces", replacedBy: nomadReplacement("namespaces")},
	{name: "nomad-token-file", valueEnv: "NOPS_NOMAD_TOKEN", replacedBy: nomadReplacement("token_file")},
	{name: "nomad-ui-url", replacedBy: nomadReplacement("ui_url")},
	{name: "nomad-ca-cert", replacedBy: nomadReplacement("ca_cert")},
	{name: "nomad-client-cert", replacedBy: nomadReplacement("client_cert")},
	{name: "nomad-client-key", replacedBy: nomadReplacement("client_key")},
	{name: "nomad-tls-skip-verify", boolean: true, replacedBy: nomadReplacement("tls_skip_verify")},
	{name: "git-url", replacedBy: repositoryReplacement("url")},
	{name: "git-branch", replacedBy: repositoryReplacement("branch")},
	{name: "git-path", replacedBy: repositoryReplacement("path")},
	{name: "git-username", replacedBy: repositoryReplacement("username")},
	{name: "git-token-file", valueEnv: "NOPS_GIT_TOKEN", replacedBy: repositoryReplacement("token_file")},
	{name: "git-timeout", replacedBy: repositoryReplacement("timeout")},
	{name: "webhook-secret-file", valueEnv: "NOPS_WEBHOOK_SECRET", replacedBy: topLevelReplacement("webhook_secret_file", "dashboard")},
	{name: "git-poll-interval", replacedBy: topLevelReplacement("git_poll_interval", "storage-intervals-and-timeouts")},
	{name: "drift-interval", replacedBy: topLevelReplacement("drift_interval", "storage-intervals-and-timeouts")},
	{name: "engine-interval", replacedBy: topLevelReplacement("engine_interval", "storage-intervals-and-timeouts")},
	{name: "hook-poll-interval", replacedBy: topLevelReplacement("hook_poll_interval", "storage-intervals-and-timeouts")},
	{name: "apply-timeout", replacedBy: topLevelReplacement("apply_timeout", "storage-intervals-and-timeouts")},
	{name: "sync-window-time-zone", replacedBy: topLevelReplacement("sync_window_time_zone", "storage-intervals-and-timeouts")},
}

// topLevelReplacement says which top-level attribute of the file replaces a
// removed option, and the section of docs/configuration.md that holds it.
func topLevelReplacement(attr, section string) string {
	return attr + " at the top level of -config (docs/configuration.md#" + section + ")"
}

// repositoryReplacement says which attribute of a repository block replaces a
// removed -git-* option.
func repositoryReplacement(attr string) string {
	return attr + ` in a repository block of -config: name it "default" to keep the repository's name (docs/configuration.md#repositories)`
}

// nomadReplacement says which attribute of the nomad block replaces a removed
// -nomad-* option.
func nomadReplacement(attr string) string {
	return attr + " in the nomad block of -config (docs/configuration.md#nomad)"
}

// authReplacement says which attribute of the auth block replaces a removed
// login option.
func authReplacement(attr, typ string) string {
	return fmt.Sprintf("%s in an auth block of type %q in -config (docs/configuration.md#authentication)", attr, typ)
}

// notifyReplacement says which attribute of a notify block replaces a removed
// -notify-* option.
func notifyReplacement(attr, typ string) string {
	return fmt.Sprintf("%s in a notify block of type %q in -config (docs/configuration.md#notifications)", attr, typ)
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
		vals[i] = &value{}
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
		fs.Var(&value{boolean: o.boolean}, o.name, "removed")
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
		NomadNamespaces:    []string{api.DefaultNamespace},
		NomadTimeout:       defaultNomadTimeout,
		ReadHeaderTimeout:  defaultReadHeaderTimeout,
		ReadTimeout:        defaultReadTimeout,
		WriteTimeout:       defaultWriteTimeout,
		IdleTimeout:        defaultIdleTimeout,
		GitPollInterval:    defaultGitPollInterval,
		DriftInterval:      defaultDriftInterval,
		EngineInterval:     defaultEngineInterval,
		HookPollInterval:   defaultHookPollInterval,
		ApplyTimeout:       defaultApplyTimeout,
		SyncWindowLocation: time.UTC,
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
	fc, fileErr := readConfigFile(c.ConfigFile)
	errs = append(errs, c.loadTopLevel(fc, explicit)...)
	// The nomad block first: a repository's jobs must match its namespaces.
	errs = append(errs, c.loadNomad(fc, explicit)...)
	errs = append(errs, c.loadRepositories(fc, fileErr)...)
	errs = append(errs, c.loadListen(fc, explicit)...)
	errs = append(errs, c.loadAuth(fc, fileErr)...)
	errs = append(errs, c.loadACL(fc)...)
	errs = append(errs, c.loadNotify(fc)...)
	errs = append(errs, c.check()...)
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return c, nil
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
		if r.URL != "" && !strings.HasPrefix(strings.ToLower(r.URL), "https://") {
			errs = append(errs, fmt.Errorf("a git token needs an https:// url in repository %q, got %q", r.Name, r.URL))
		}
		if r.Username == "" {
			errs = append(errs, fmt.Errorf("a git token needs a non-empty username in repository %q", r.Name))
		}
	}
	return errs
}

// repositoryName is what a repository block's label may be: it names the
// repository on the dashboard, in a metric label and in a notification.
var repositoryName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// fileConfig is the schema of the -config file.
type fileConfig struct {
	LogLevel           *string `hcl:"log_level,optional"`
	DBPath             *string `hcl:"db_path,optional"`
	PublicURL          *string `hcl:"public_url,optional"`
	GitPollInterval    *string `hcl:"git_poll_interval,optional"`
	DriftInterval      *string `hcl:"drift_interval,optional"`
	EngineInterval     *string `hcl:"engine_interval,optional"`
	HookPollInterval   *string `hcl:"hook_poll_interval,optional"`
	ApplyTimeout       *string `hcl:"apply_timeout,optional"`
	SyncWindowTimeZone *string `hcl:"sync_window_time_zone,optional"`
	WebhookSecretFile  string  `hcl:"webhook_secret_file,optional"`

	Repositories []fileRepository `hcl:"repository,block"`
	Listen       *fileListen      `hcl:"listen,block"`
	Nomad        *fileNomad       `hcl:"nomad,block"`
	Auth         []fileAuth       `hcl:"auth,block"`
	ACL          *fileACL         `hcl:"acl,block"`
	Notify       []fileNotify     `hcl:"notify,block"`
}

type fileNotify struct {
	Name      string  `hcl:"name,label"`
	Type      string  `hcl:"type"`
	URLFile   *string `hcl:"url_file,optional"`
	URL       *string `hcl:"url,optional"`
	TokenFile *string `hcl:"token_file,optional"`
	Timeout   *string `hcl:"timeout,optional"`
}

type fileACL struct {
	Enabled            bool   `hcl:"enabled,optional"`
	BootstrapTokenFile string `hcl:"bootstrap_token_file,optional"`
}

type fileAuth struct {
	Name             string  `hcl:"name,label"`
	Type             string  `hcl:"type"`
	IssuerURL        *string `hcl:"issuer_url,optional"`
	ClientID         *string `hcl:"client_id,optional"`
	ClientSecretFile *string `hcl:"client_secret_file,optional"`
	UsersFile        *string `hcl:"users_file,optional"`
	GroupsFile       *string `hcl:"groups_file,optional"`
	MaxFailures      *int    `hcl:"max_failures,optional"`
	FailureWindow    *string `hcl:"failure_window,optional"`
}

type fileNomad struct {
	Addr          *string   `hcl:"addr,optional"`
	Namespaces    *[]string `hcl:"namespaces,optional"`
	TokenFile     string    `hcl:"token_file,optional"`
	UIURL         string    `hcl:"ui_url,optional"`
	CACert        string    `hcl:"ca_cert,optional"`
	ClientCert    string    `hcl:"client_cert,optional"`
	ClientKey     string    `hcl:"client_key,optional"`
	TLSSkipVerify bool      `hcl:"tls_skip_verify,optional"`
	Timeout       *string   `hcl:"timeout,optional"`
}

type fileListen struct {
	Addr              *string  `hcl:"addr,optional"`
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

// loadRepositories fills c.Repositories from the repository blocks of fc, the
// decoded -config file, nil without one. fileErr is why the file could not be
// read: it is reported here, once.
func (c *Config) loadRepositories(fc *fileConfig, fileErr error) []error {
	var errs []error
	if fileErr != nil {
		errs = append(errs, fileErr)
	}
	if fc != nil {
		repos, repoErrs := repositoriesOf(fc, c.ConfigFile, c.NomadNamespaces)
		c.Repositories = repos
		errs = append(errs, repoErrs...)
	}

	seen := make(map[string]bool, len(c.Repositories))
	for _, r := range c.Repositories {
		if seen[r.Name] {
			errs = append(errs, fmt.Errorf("-config %s: repository %q is declared twice", c.ConfigFile, r.Name))
		}
		seen[r.Name] = true
	}
	if len(c.Repositories) == 0 && len(errs) == 0 {
		errs = append(errs, errors.New("a repository block in -config is required (docs/configuration.md#repositories)"))
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

// loadTopLevel reads the top-level attributes of fc, when there is a file. Of
// those with a flag, an attribute counts only when its option is not in
// explicit, the options given by flag or variable. Every error names the file
// and the attribute.
func (c *Config) loadTopLevel(fc *fileConfig, explicit map[string]bool) []error {
	if fc == nil {
		return nil
	}
	var errs []error
	fail := func(attr string, err error) {
		errs = append(errs, fmt.Errorf("-config %s: %s: %w", c.ConfigFile, attr, err))
	}
	var err error
	if fc.LogLevel != nil && !explicit["log-level"] {
		if err := c.LogLevel.UnmarshalText([]byte(*fc.LogLevel)); err != nil {
			fail("log_level", err)
		}
	}
	if fc.DBPath != nil && !explicit["db-path"] {
		if c.DBPath, err = required(*fc.DBPath); err != nil {
			fail("db_path", err)
		}
	}
	if fc.PublicURL != nil && !explicit["public-url"] {
		if c.PublicURL, err = optionalURL(*fc.PublicURL); err != nil {
			fail("public_url", err)
		}
	}
	for _, a := range []struct {
		name string
		in   *string
		out  *time.Duration
	}{
		{"git_poll_interval", fc.GitPollInterval, &c.GitPollInterval},
		{"drift_interval", fc.DriftInterval, &c.DriftInterval},
		{"engine_interval", fc.EngineInterval, &c.EngineInterval},
		{"hook_poll_interval", fc.HookPollInterval, &c.HookPollInterval},
		{"apply_timeout", fc.ApplyTimeout, &c.ApplyTimeout},
	} {
		if a.in == nil {
			continue
		}
		if *a.out, err = positiveDuration(*a.in); err != nil {
			fail(a.name, err)
		}
	}
	if fc.SyncWindowTimeZone != nil {
		if c.SyncWindowLocation, err = timeZone(*fc.SyncWindowTimeZone); err != nil {
			fail("sync_window_time_zone", err)
		}
	}
	c.WebhookSecretFile = fc.WebhookSecretFile
	if c.WebhookSecret, err = secretFile(fc.WebhookSecretFile); err != nil {
		fail("webhook_secret_file", err)
	}
	return errs
}

// loadListen sets the address, the timeouts and the TLS files of the listen
// block of fc, when it has one. Its addr counts only when -listen-addr is not
// in explicit, the options given by flag or variable. Every error names the
// file and the attribute.
func (c *Config) loadListen(fc *fileConfig, explicit map[string]bool) []error {
	if fc == nil || fc.Listen == nil {
		return nil
	}
	var errs []error
	if a := fc.Listen.Addr; a != nil && !explicit["listen-addr"] {
		if err := c.setListenAddr(*a); err != nil {
			errs = append(errs, fmt.Errorf("-config %s: listen: addr: %w", c.ConfigFile, err))
		}
	}
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

// loadNomad reads the nomad block of fc, when it has one. Its addr counts
// only when -nomad-addr is not in explicit, the options given by flag or
// variable. Every error names the file and the attribute.
func (c *Config) loadNomad(fc *fileConfig, explicit map[string]bool) []error {
	if fc == nil || fc.Nomad == nil {
		return nil
	}
	b := fc.Nomad
	var errs []error
	fail := func(attr string, err error) {
		errs = append(errs, fmt.Errorf("-config %s: nomad: %s: %w", c.ConfigFile, attr, err))
	}
	var err error
	if b.Addr != nil && !explicit["nomad-addr"] {
		if c.NomadAddr, err = httpURL(*b.Addr); err != nil {
			fail("addr", err)
		}
	}
	if b.Namespaces != nil {
		// Kept as they were on an error, so the jobs of a repository block
		// are not reported against an empty list too.
		if ns, err := namespaceList(*b.Namespaces); err != nil {
			fail("namespaces", err)
		} else {
			c.NomadNamespaces = ns
		}
	}
	c.NomadTokenFile = b.TokenFile
	if c.NomadToken, err = secretFile(b.TokenFile); err != nil {
		fail("token_file", err)
	}
	if c.NomadUIURL, err = optionalURL(b.UIURL); err != nil {
		fail("ui_url", err)
	}
	for _, f := range []struct {
		name string
		in   string
		out  *string
	}{
		{"ca_cert", b.CACert, &c.NomadCACert},
		{"client_cert", b.ClientCert, &c.NomadClientCert},
		{"client_key", b.ClientKey, &c.NomadClientKey},
	} {
		if *f.out, err = readableFile(f.in); err != nil {
			fail(f.name, err)
		}
	}
	if (b.ClientCert == "") != (b.ClientKey == "") {
		fail("client_cert", errors.New("must be set together with client_key"))
	}
	c.NomadTLSSkipVerify = b.TLSSkipVerify
	if b.Timeout != nil {
		if c.NomadTimeout, err = positiveDuration(*b.Timeout); err != nil {
			fail("timeout", err)
		}
	}
	return errs
}

// authAttributes are the attributes each type of auth block takes besides
// type, and whether each is required.
var authAttributes = map[string]map[string]bool{
	"oidc":  {"issuer_url": true, "client_id": true, "client_secret_file": true},
	"basic": {"users_file": true, "groups_file": false, "max_failures": false, "failure_window": false},
}

// loadAuth reads the auth block of fc into c.Auth. Nops needs exactly one:
// none, a second one, an unknown type, a missing attribute or one the type
// doesn't take is an error that names the file and the block. fileErr is why
// the -config file could not be read, already reported.
func (c *Config) loadAuth(fc *fileConfig, fileErr error) []error {
	if fileErr != nil {
		return nil
	}
	if fc == nil || len(fc.Auth) == 0 {
		return []error{errors.New("an auth block in -config is required (docs/configuration.md#authentication)")}
	}
	if len(fc.Auth) > 1 {
		return []error{fmt.Errorf("-config %s: only one auth block is allowed", c.ConfigFile)}
	}
	b := fc.Auth[0]
	var errs []error
	fail := func(format string, a ...any) {
		errs = append(errs, fmt.Errorf("-config %s: auth %q: %s", c.ConfigFile, b.Name, fmt.Sprintf(format, a...)))
	}
	if !repositoryName.MatchString(b.Name) {
		fail("the name may hold only letters, digits, - and _")
	}
	takes, ok := authAttributes[b.Type]
	if !ok {
		fail("type: is %q, must be oidc or basic", b.Type)
		return errs
	}
	a := Auth{Name: b.Name, Type: b.Type}
	if b.Type == "basic" {
		a.MaxFailures, a.FailureWindow = defaultLoginMaxFailures, defaultLoginFailureWindow
	}
	for _, f := range []struct {
		name string
		in   *string
		read func(string) (string, error)
		out  *string
	}{
		// Not optionalURL: unlike other URLs, a trailing slash in the issuer
		// is significant (Authentik's has one) and must be kept.
		{"issuer_url", b.IssuerURL, httpURL, &a.IssuerURL},
		{"client_id", b.ClientID, required, &a.ClientID},
		{"client_secret_file", b.ClientSecretFile, secretFile, &a.ClientSecret},
		{"users_file", b.UsersFile, readableFile, &a.UsersFile},
		{"groups_file", b.GroupsFile, readableFile, &a.GroupsFile},
	} {
		required, taken := takes[f.name]
		switch {
		case f.in != nil && !taken:
			fail("%s: is not used by type %q", f.name, b.Type)
			continue
		case f.in == nil || *f.in == "":
			if required {
				fail("%s: required", f.name)
			}
			continue
		}
		v, err := f.read(*f.in)
		if err != nil {
			fail("%s: %v", f.name, err)
			continue
		}
		*f.out = v
	}
	if b.ClientSecretFile != nil {
		a.ClientSecretFile = *b.ClientSecretFile
	}
	// The limit on failed logins: a zero turns it off.
	if b.MaxFailures != nil {
		_, taken := takes["max_failures"]
		switch {
		case !taken:
			fail("max_failures: is not used by type %q", b.Type)
		case *b.MaxFailures < 0:
			fail("max_failures: %d must not be negative", *b.MaxFailures)
		default:
			a.MaxFailures = *b.MaxFailures
		}
	}
	if b.FailureWindow != nil {
		_, taken := takes["failure_window"]
		if !taken {
			fail("failure_window: is not used by type %q", b.Type)
		} else if d, err := nonNegativeDuration(*b.FailureWindow); err != nil {
			fail("failure_window: %v", err)
		} else {
			a.FailureWindow = d
		}
	}
	if len(errs) == 0 {
		c.Auth = a
	}
	return errs
}

// loadACL turns the ACL on from the acl block of fc, when it has one, and reads
// its bootstrap token. The token is required with enabled and refused without
// it, so the ACL is never half on. An error names the file and the attribute.
func (c *Config) loadACL(fc *fileConfig) []error {
	if fc == nil || fc.ACL == nil {
		return nil
	}
	b := fc.ACL
	fail := func(format string, a ...any) []error {
		return []error{fmt.Errorf("-config %s: acl: bootstrap_token_file: %s", c.ConfigFile, fmt.Sprintf(format, a...))}
	}
	tok, err := secretFile(b.BootstrapTokenFile)
	switch {
	case err != nil:
		return fail("%v", err)
	case b.Enabled && tok == "":
		return fail(`required with enabled = true: make the token with "nops secret generate"`)
	case !b.Enabled && tok != "":
		return fail("is set without enabled = true")
	case b.Enabled && !secret.Valid(tok):
		return fail(`the token is not a secret made by "nops secret generate"`)
	}
	c.ACL, c.ACLBootstrapTokenFile, c.ACLBootstrapToken = b.Enabled, b.BootstrapTokenFile, tok
	return nil
}

// notifyAttributes are the attributes each type of notify block takes besides
// type and timeout, and whether each is required.
var notifyAttributes = map[string]map[string]bool{
	"webhook": {"url_file": true, "token_file": false},
	"discord": {"url_file": true},
	"slack":   {"url_file": true},
	"ntfy":    {"url": true, "token_file": false},
	"gotify":  {"url": true, "token_file": true},
}

// loadNotify reads the notify blocks of fc into c.Notify, in their order. A
// name used twice, an unknown type, a missing attribute or one the type
// doesn't take is an error that names the file and the block.
func (c *Config) loadNotify(fc *fileConfig) []error {
	if fc == nil {
		return nil
	}
	var errs []error
	seen := make(map[string]bool, len(fc.Notify))
	for _, b := range fc.Notify {
		fail := func(format string, a ...any) {
			errs = append(errs, fmt.Errorf("-config %s: notify %q: %s", c.ConfigFile, b.Name, fmt.Sprintf(format, a...)))
		}
		if !repositoryName.MatchString(b.Name) {
			fail("the name may hold only letters, digits, - and _")
		}
		if seen[b.Name] {
			fail("is declared twice")
		}
		seen[b.Name] = true
		takes, ok := notifyAttributes[b.Type]
		if !ok {
			fail("type: is %q, must be webhook, discord, slack, ntfy, or gotify", b.Type)
			continue
		}
		n := Notify{Name: b.Name, Type: b.Type, Timeout: defaultNotifyTimeout}
		var blockErr bool
		for _, a := range []struct {
			name string
			in   *string
			read func(string) (string, error)
			out  *string
		}{
			{"url_file", b.URLFile, urlFile, &n.URL},
			{"url", b.URL, func(v string) (string, error) { return optionalURL(v) }, &n.URL},
			{"token_file", b.TokenFile, secretFile, &n.Token},
		} {
			required, taken := takes[a.name]
			switch {
			case a.in != nil && !taken:
				fail("%s: is not used by type %q", a.name, b.Type)
				blockErr = true
				continue
			case a.in == nil || *a.in == "":
				if required {
					fail("%s: required", a.name)
					blockErr = true
				}
				continue
			}
			v, err := a.read(*a.in)
			if err != nil {
				fail("%s: %v", a.name, err)
				blockErr = true
				continue
			}
			*a.out = v
		}
		if b.Timeout != nil {
			d, err := positiveDuration(*b.Timeout)
			if err != nil {
				fail("timeout: %v", err)
				blockErr = true
			}
			n.Timeout = d
		}
		if !blockErr {
			c.Notify = append(c.Notify, n)
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

// gitPath validates the path of a repository block: a relative path inside
// the repository (gitwatch joins it to the tree it reads), or empty for the
// root.
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

// namespaceList checks the namespaces of the nomad block: a non-empty list
// with no empty name, a repeated name kept once (the order of the first
// occurrence stands).
func namespaceList(list []string) ([]string, error) {
	if len(list) == 0 {
		return nil, errors.New("is empty: Nops needs a namespace to manage")
	}
	var out []string
	seen := map[string]bool{}
	for _, ns := range list {
		if strings.TrimSpace(ns) == "" {
			return nil, fmt.Errorf("%q is not a namespace name", ns)
		}
		if !seen[ns] {
			seen[ns] = true
			out = append(out, ns)
		}
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
// meaningful to put there (typically local users on a personal
// machine, though the default applies to an OIDC login too: it is wrong
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

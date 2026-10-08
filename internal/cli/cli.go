// Package cli is the command line of Nops (docs/cli.md): every command of the
// nops binary except "serve", which main runs. Most are a client of the API:
// they read the URL and the token from their flags or from the environment,
// call the API of docs/api.md, and print text, or the API's own JSON with
// -json. An offline command needs neither.
package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/music-gang/nops/internal/version"
)

// Variables are the environment variables the client reads. The server reads
// every flag as NOPS_<FLAG> too, so none of its flags may take one of these
// names (a test in internal/config keeps it so).
var Variables = []string{"NOPS_ADDR", "NOPS_TOKEN", "NOPS_TOKEN_FILE", "NOPS_NAMESPACE"}

// Exit codes of Run.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

// usageError is a mistake in how the command was called, as opposed to a
// request that failed.
type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

func usageErrorf(format string, a ...any) error { return usageError{fmt.Sprintf(format, a...)} }

// errAborted is the answer "no" to a question.
var errAborted = errors.New("aborted: nothing was changed")

// call is one command being run.
type call struct {
	ctx         context.Context
	client      *client
	arg         string // the first argument: a deployment ID, a job ID, a name
	args        []string
	namespace   string
	asJSON, yes bool
	reason      string
	flags       aclFlags
	in          *bufio.Reader
	out, errOut io.Writer
}

type command struct {
	name string // one word, or up to three ("secret generate", "acl token create")
	arg  string // the placeholders of the arguments it takes, such as "", "<id>", "<job>" or "<name> <file>"
	help string
	yes  bool // takes -yes
	why  bool // takes -reason
	// flags adds the flags that only this command takes.
	flags func(fs *flag.FlagSet, k *call)
	// offline runs without the API: no -addr, no token and no -json.
	offline bool
	run     func(*call) error
}

// takesJob is whether the command names a job, and so takes -namespace.
func (c command) takesJob() bool { return c.arg == "<job>" }

// Run runs the command in args (the arguments after the program name) and
// returns the exit code. getenv reads the environment; stdin answers the
// question of approve and deploy-now.
func Run(ctx context.Context, args []string, getenv func(string) string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return exitUsage
	}
	switch name := args[0]; {
	case name == "-version" || name == "--version":
		fmt.Fprintln(stdout, version.String())
		return exitOK
	case name == "-h" || name == "-help" || name == "--help" || name == "help":
		usage(stdout)
		return exitOK
	case strings.HasPrefix(name, "-"):
		fmt.Fprintf(stderr, "nops: unknown flag %s: the flags of the server go after \"nops serve\"\n", name)
		return exitUsage
	}
	cmd, rest := find(args)
	if cmd == nil {
		for n := min(len(args), 2); n > 0; n-- {
			if subs := subcommands(args[:n]); len(subs) > 0 {
				fmt.Fprintf(stderr, "nops: %s takes a subcommand: %s\n", strings.Join(args[:n], " "), strings.Join(subs, ", "))
				return exitUsage
			}
		}
		fmt.Fprintf(stderr, "nops: unknown command %q\n\n", args[0])
		usage(stderr)
		return exitUsage
	}

	err := cmd.exec(ctx, rest, getenv, stdin, stdout, stderr)
	var ue usageError
	switch {
	case err == nil, errors.Is(err, flag.ErrHelp):
		return exitOK
	case errors.As(err, &ue):
		fmt.Fprintf(stderr, "nops %s: %v\n", cmd.name, err)
		return exitUsage
	default:
		fmt.Fprintf(stderr, "nops %s: %v\n", cmd.name, err)
		return exitError
	}
}

func (c *command) exec(ctx context.Context, args []string, getenv func(string) string, stdin io.Reader, stdout, stderr io.Writer) error {
	var s settings
	k := &call{ctx: ctx, in: bufio.NewReader(stdin), out: stdout, errOut: stderr}
	fs := flag.NewFlagSet("nops "+c.name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	if !c.offline {
		fs.StringVar(&s.addr, "addr", "", "URL of Nops (NOPS_ADDR)")
		fs.StringVar(&s.tokenFile, "token-file", "", "file holding the API token (NOPS_TOKEN_FILE, or NOPS_TOKEN)")
		fs.BoolVar(&k.asJSON, "json", false, "print the answer of the API as JSON")
	}
	if c.takesJob() {
		fs.StringVar(&s.namespace, "namespace", "", `namespace of the job (NOPS_NAMESPACE, default "default")`)
	}
	if c.yes {
		fs.BoolVar(&k.yes, "yes", false, "do not ask for confirmation")
	}
	if c.why {
		fs.StringVar(&k.reason, "reason", "", "why, up to 500 bytes")
	}
	if c.flags != nil {
		c.flags(fs, k)
	}
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: nops %s [flags]%s\n\n%s\n\n", c.name, strings.TrimRight(" "+c.arg, " "), c.help)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return usageError{"see nops " + c.name + " -h"}
	}
	want := len(strings.Fields(c.arg))
	if fs.NArg() != want {
		hint := ""
		for _, a := range fs.Args() {
			if strings.HasPrefix(a, "-") {
				hint = ": put the flags before the argument"
			}
		}
		if want == 0 {
			return usageErrorf("takes no argument%s", hint)
		}
		if want == 1 {
			return usageErrorf("takes one argument, %s%s", c.arg, hint)
		}
		return usageErrorf("takes %d arguments, %s%s", want, c.arg, hint)
	}
	k.args = fs.Args()
	if want > 0 {
		k.arg = k.args[0]
	}
	k.namespace = first(s.namespace, getenv("NOPS_NAMESPACE"), "default")

	if !c.offline {
		cl, err := newClient(s, getenv)
		if err != nil {
			return err
		}
		k.client = cl
	}
	return c.run(k)
}

// find is the command args start with, matched on its words, and the arguments
// after them.
func find(args []string) (*command, []string) {
	for i := range commands {
		words := strings.Fields(commands[i].name)
		if len(args) >= len(words) && strings.Join(args[:len(words)], " ") == commands[i].name {
			return &commands[i], args[len(words):]
		}
	}
	return nil, nil
}

// subcommands are the words that follow group in the names of the commands,
// such as "generate" after "secret" or "policy" and "token" after "acl", or
// none if group is not the start of a longer name.
func subcommands(group []string) []string {
	var subs []string
	for _, c := range commands {
		words := strings.Fields(c.name)
		if len(words) > len(group) && slices.Equal(words[:len(group)], group) && !slices.Contains(subs, words[len(group)]) {
			subs = append(subs, words[len(group)])
		}
	}
	return subs
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "Usage: nops <command> [flags] [argument]")
	fmt.Fprintln(w, "       nops serve [flags]")
	fmt.Fprintln(w, "       nops -version")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "serve runs the server. The other commands call its API, except the ones that")
	fmt.Fprintln(w, "say they run offline: set NOPS_ADDR and NOPS_TOKEN_FILE (or NOPS_TOKEN).")
	fmt.Fprintln(w, "\"nops <command> -h\" lists their flags.")
	fmt.Fprintln(w)
	for _, c := range commands {
		fmt.Fprintf(w, "  %-34s %s\n", strings.TrimRight(c.name+" "+c.arg, " "), c.help)
	}
}

// writeJSON prints an answer of the API as it is, indented.
func writeJSON(w io.Writer, raw []byte) {
	var b bytes.Buffer
	if err := json.Indent(&b, raw, "", "  "); err != nil {
		b.Reset()
		b.Write(raw)
	}
	fmt.Fprintln(w, strings.TrimSpace(b.String()))
}

// confirm asks the question on stderr, unless -yes, and reads the answer from
// stdin. Anything but y or yes is a no, an empty stdin included.
func (k *call) confirm(question string) error {
	if k.yes {
		return nil
	}
	fmt.Fprintf(k.errOut, "%s [y/N] ", question)
	line, _ := k.in.ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return nil
	}
	if line == "" {
		fmt.Fprintln(k.errOut)
	}
	return errAborted
}

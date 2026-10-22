package cli

import (
	"flag"
	"fmt"
	"net/url"
	"strings"
)

// listFlag is a flag of a list (docs/cli.md#lists) and the parameter of the
// API it sets.
type listFlag struct {
	name, param, usage string
	deployments, jobs  bool // the lists that take it
}

var listFlagTable = []listFlag{
	{"per-page", "per_page", "rows per page: 25, 50 or 100 (default 50)", true, true},
	{"page-token", "next_token", "where the page starts: the token the previous page printed", true, true},
	{"search", "q", "part of the job, the namespace, or of the commit or the file", true, true},
	{"state", "state", "a state, or active, for deployments; a sync state for jobs", true, true},
	{"namespace", "namespace", "the namespace (it doesn't read NOPS_NAMESPACE: without it, every namespace)", true, true},
	{"job", "job", "the job", true, false},
	{"repository", "repository", "the repository the job file comes from", true, true},
	{"policy", "policy", "auto, approval or none", true, true},
	{"decided-by", "decided_by", "who approved or rejected", true, false},
	{"since", "since", "created from a date (2026-10-01, UTC) or an RFC 3339 time", true, false},
	{"until", "until", "created up to a date (UTC, included) or an RFC 3339 time", true, false},
}

// listArgs are the flags of a list a command was given.
type listArgs struct {
	fs       *flag.FlagSet
	values   map[string]*string
	warnings bool
}

// listFlags adds the flags of a list of deployments, or of jobs.
func listFlags(jobs bool) func(fs *flag.FlagSet, k *call) {
	return func(fs *flag.FlagSet, k *call) {
		k.list = listArgs{fs: fs, values: map[string]*string{}}
		for _, f := range listFlagTable {
			if (jobs && f.jobs) || (!jobs && f.deployments) {
				k.list.values[f.name] = fs.String(f.name, "", f.usage)
			}
		}
		if jobs {
			fs.BoolVar(&k.list.warnings, "warnings", false, "only the jobs with meta warnings")
		}
	}
}

// path is the request of the list at base, with the parameters of the flags
// that were set.
func (l listArgs) path(base string) string {
	v := url.Values{}
	for _, f := range listFlagTable {
		if p, ok := l.values[f.name]; ok && *p != "" {
			v.Set(f.param, *p)
		}
	}
	if l.warnings {
		v.Set("warnings", "true")
	}
	if len(v) == 0 {
		return base
	}
	return base + "?" + v.Encode()
}

// next is the command of the page after this one: the same flags, with the
// token of the answer.
func (l listArgs) next(command, token string) string {
	parts := []string{"nops", command}
	l.fs.Visit(func(f *flag.Flag) {
		switch {
		case f.Name == "page-token":
		case f.Name == "warnings":
			parts = append(parts, "-warnings")
		case l.values[f.Name] != nil:
			parts = append(parts, "-"+f.Name, quote(f.Value.String()))
		}
	})
	return strings.Join(append(parts, "-page-token", quote(token)), " ")
}

// quote makes s one word of a POSIX shell.
func quote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./:=@+,", r))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// printNext says how to get the next page, on the error output so a pipe keeps
// only the rows; nothing on the last page.
func (k *call) printNext(command, token string) {
	if token != "" && !k.asJSON {
		fmt.Fprintf(k.errOut, "\nNext page: %s\n", k.list.next(command, token))
	}
}

// Package acl parses ACL policies and decides what a token may do
// (docs/acl.md). An ACL policy is never called a bare "policy": that word is
// the job's auto, approval or none.
package acl

import (
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

// Capability is one thing an ACL policy allows.
type Capability string

// The capabilities of a namespace.
const (
	Read      Capability = "read"
	Approve   Capability = "approve" // approve and reject
	Promote   Capability = "promote"
	Retry     Capability = "retry"
	DeployNow Capability = "deploy-now"
	Pause     Capability = "pause" // pause and resume
)

// The capabilities that belong to no namespace. Fetch asks for a git poll,
// Backup takes a copy of the database and Metrics scrapes /metrics. Pause,
// which names the capability of a namespace too, is also a global one when it
// stands outside the namespace blocks: it pauses and resumes every job.
const (
	Fetch   Capability = "fetch"
	Backup  Capability = "backup"
	Metrics Capability = "metrics"
)

// globalCapabilities are the ones the top-level list of a policy takes.
var globalCapabilities = []Capability{Fetch, Pause, Backup, Metrics}

// namespaceCapabilities is what the shorthand "write" stands for. A new
// capability of a namespace joins it, so a list written by hand never gains one.
var namespaceCapabilities = []Capability{Read, Approve, Promote, Retry, DeployNow, Pause}

// The shorthands of a namespace block.
const (
	shorthandRead  = "read"
	shorthandWrite = "write"
	shorthandDeny  = "deny"
)

// labelPattern is what a namespace block may be named: a Nomad namespace name,
// or a glob of one.
var labelPattern = regexp.MustCompile(`^[A-Za-z0-9*-]+$`)

// Rule is a namespace block of an ACL policy once parsed.
type Rule struct {
	Label string // a namespace name or a glob, such as "prod", "pr*" or "*"
	Deny  bool
	Caps  []Capability
}

// Policy is an ACL policy once parsed.
type Policy struct {
	Rules  []Rule
	Global []Capability
}

type file struct {
	Namespaces   []namespaceBlock `hcl:"namespace,block"`
	Capabilities []string         `hcl:"capabilities,optional"`
}

type namespaceBlock struct {
	Label        string   `hcl:"name,label"`
	Policy       string   `hcl:"policy,optional"`
	Capabilities []string `hcl:"capabilities,optional"`
}

// Parse reads the rules of an ACL policy and checks them. The error says what
// is wrong and where, and Nops saves nothing.
func Parse(rules string) (*Policy, error) {
	f, diags := hclsyntax.ParseConfig([]byte(rules), "acl policy", hcl.InitialPos)
	if diags.HasErrors() {
		return nil, diagError(diags)
	}
	var in file
	if diags := gohcl.DecodeBody(f.Body, nil, &in); diags.HasErrors() {
		return nil, diagError(diags)
	}

	p := &Policy{}
	seen := map[string]bool{}
	for _, b := range in.Namespaces {
		if !labelPattern.MatchString(b.Label) {
			return nil, fmt.Errorf("namespace %q: a name has letters, digits, - and *", b.Label)
		}
		if seen[b.Label] {
			return nil, fmt.Errorf("namespace %q appears twice", b.Label)
		}
		seen[b.Label] = true
		r, err := rule(b)
		if err != nil {
			return nil, err
		}
		p.Rules = append(p.Rules, r)
	}
	for _, c := range in.Capabilities {
		if !slices.Contains(globalCapabilities, Capability(c)) {
			return nil, fmt.Errorf("capability %q: the ones outside a namespace are %s", c, listOf(globalCapabilities))
		}
		if !slices.Contains(p.Global, Capability(c)) {
			p.Global = append(p.Global, Capability(c))
		}
	}
	return p, nil
}

func rule(b namespaceBlock) (Rule, error) {
	r := Rule{Label: b.Label}
	if b.Policy == "" && len(b.Capabilities) == 0 {
		return r, fmt.Errorf("namespace %q: set policy or capabilities", b.Label)
	}
	switch b.Policy {
	case "":
	case shorthandRead:
		r.add(Read)
	case shorthandWrite:
		r.add(namespaceCapabilities...)
	case shorthandDeny:
		if len(b.Capabilities) > 0 {
			return r, fmt.Errorf("namespace %q: deny allows no capabilities", b.Label)
		}
		r.Deny = true
	default:
		return r, fmt.Errorf("namespace %q: policy %q is not %s, %s or %s", b.Label, b.Policy, shorthandRead, shorthandWrite, shorthandDeny)
	}
	for _, c := range b.Capabilities {
		if !slices.Contains(namespaceCapabilities, Capability(c)) {
			return r, fmt.Errorf("namespace %q: capability %q is not one of %s", b.Label, c, listOf(namespaceCapabilities))
		}
		r.add(Capability(c))
	}
	return r, nil
}

func (r *Rule) add(cs ...Capability) {
	for _, c := range cs {
		if !slices.Contains(r.Caps, c) {
			r.Caps = append(r.Caps, c)
		}
	}
}

func listOf(cs []Capability) string {
	s := make([]string, len(cs))
	for i, c := range cs {
		s[i] = string(c)
	}
	return strings.Join(s, ", ")
}

func diagError(diags hcl.Diagnostics) error {
	d := diags[0]
	if d.Subject == nil {
		return fmt.Errorf("%s: %s", d.Summary, d.Detail)
	}
	return fmt.Errorf("line %d: %s: %s", d.Subject.Start.Line, d.Summary, d.Detail)
}

// ACL is what one token may do: all of its ACL policies, combined.
type ACL struct {
	management bool
	policies   []*Policy
}

// Management allows everything.
func Management() *ACL { return &ACL{management: true} }

// New combines ACL policies. With none, it allows nothing.
func New(policies ...*Policy) *ACL { return &ACL{policies: policies} }

// Allow reports whether the ACL grants c on the namespace. It looks at the
// most specific block that matches the namespace in any ACL policy, so "prod"
// comes before "pr*" and "pr*" before "*". If one of them denies, the answer
// is no; otherwise the capabilities of all of them add up.
func (a *ACL) Allow(namespace string, c Capability) bool {
	if a.management {
		return true
	}
	best := -1
	var rules []Rule
	for _, p := range a.policies {
		for _, r := range p.Rules {
			ok, _ := path.Match(r.Label, namespace)
			if !ok {
				continue
			}
			switch s := specificity(r.Label); {
			case s > best:
				best, rules = s, []Rule{r}
			case s == best:
				rules = append(rules, r)
			}
		}
	}
	allowed := false
	for _, r := range rules {
		if r.Deny {
			return false
		}
		allowed = allowed || slices.Contains(r.Caps, c)
	}
	return allowed
}

// Global reports whether the ACL grants a capability that belongs to no namespace.
func (a *ACL) Global(c Capability) bool {
	if a.management {
		return true
	}
	for _, p := range a.policies {
		if slices.Contains(p.Global, c) {
			return true
		}
	}
	return false
}

// Management reports whether the ACL allows everything.
func (a *ACL) Management() bool { return a.management }

// specificity orders the labels that match one namespace: an exact name beats
// any glob, and a glob beats another by the characters it spells out.
func specificity(label string) int {
	literal := len(label) - strings.Count(label, "*")
	if !strings.Contains(label, "*") {
		return 1<<20 + literal
	}
	return literal
}

// Scope is where an ACL grants a capability, in a form a query can test on
// each row (docs/api.md#lists): the blocks of every ACL policy, grouped by
// specificity, read the way Allow reads them. A list is never built first,
// so a page costs the rows it walks, not the namespaces that exist.
type Scope struct {
	All    bool    // a management ACL: every namespace
	Levels []Level // most specific first
}

// Level is the blocks of one specificity. The first level with a block that
// matches a namespace decides for it: no if one of its matching blocks denies,
// yes if one grants the capability.
type Level struct {
	Labels []string // every block of the level
	Deny   []string // the blocks that deny
	Grant  []string // the blocks that grant the capability
}

// Scope returns where the ACL grants c.
func (a *ACL) Scope(c Capability) Scope {
	if a.management {
		return Scope{All: true}
	}
	byLevel := map[int]*Level{}
	for _, p := range a.policies {
		for _, r := range p.Rules {
			s := specificity(r.Label)
			l := byLevel[s]
			if l == nil {
				l = &Level{}
				byLevel[s] = l
			}
			l.Labels = append(l.Labels, r.Label)
			switch {
			case r.Deny:
				l.Deny = append(l.Deny, r.Label)
			case slices.Contains(r.Caps, c):
				l.Grant = append(l.Grant, r.Label)
			}
		}
	}
	keys := make([]int, 0, len(byLevel))
	for s := range byLevel {
		keys = append(keys, s)
	}
	slices.Sort(keys)
	var sc Scope
	for _, s := range slices.Backward(keys) {
		l := byLevel[s]
		for _, labels := range []*[]string{&l.Labels, &l.Deny, &l.Grant} {
			slices.Sort(*labels)
			*labels = slices.Compact(*labels)
		}
		sc.Levels = append(sc.Levels, *l)
	}
	return sc
}

// Match reports whether the scope holds the namespace. A query that tests the
// scope gives the same answer: a block's name has no character a glob of
// SQLite reads differently from path.Match.
func (s Scope) Match(namespace string) bool {
	if s.All {
		return true
	}
	matches := func(labels []string) bool {
		return slices.ContainsFunc(labels, func(l string) bool {
			ok, _ := path.Match(l, namespace)
			return ok
		})
	}
	for _, l := range s.Levels {
		if matches(l.Labels) {
			return !matches(l.Deny) && matches(l.Grant)
		}
	}
	return false
}

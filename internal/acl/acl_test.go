package acl

import (
	"slices"
	"strings"
	"testing"
)

func mustParse(t *testing.T, rules string) *Policy {
	t.Helper()
	p, err := Parse(rules)
	if err != nil {
		t.Fatalf("Parse(%q): %v", rules, err)
	}
	return p
}

func TestParseShorthands(t *testing.T) {
	p := mustParse(t, `
namespace "a" { policy = "read" }
namespace "b" { policy = "write" }
namespace "c" { policy = "deny" }
namespace "d" {
  policy       = "read"
  capabilities = ["approve", "read"]
}
capabilities = ["fetch"]
`)
	got := map[string]Rule{}
	for _, r := range p.Rules {
		got[r.Label] = r
	}
	if !slices.Equal(got["a"].Caps, []Capability{Read}) {
		t.Errorf("read = %v, want [read]", got["a"].Caps)
	}
	for _, c := range namespaceCapabilities {
		if !slices.Contains(got["b"].Caps, c) {
			t.Errorf("write lacks %s", c)
		}
	}
	if !got["c"].Deny || len(got["c"].Caps) != 0 {
		t.Errorf("deny = %+v, want deny and no capabilities", got["c"])
	}
	if !slices.Equal(got["d"].Caps, []Capability{Read, Approve}) {
		t.Errorf("read plus approve = %v, want [read approve] with no repeat", got["d"].Caps)
	}
	if !slices.Equal(p.Global, []Capability{Fetch}) {
		t.Errorf("global = %v, want [fetch]", p.Global)
	}
}

func TestParseRefusesWhatIsWrong(t *testing.T) {
	for name, tc := range map[string]struct{ rules, want string }{
		"not HCL":                     {`namespace {{`, "line 1"},
		"unknown block":               {`job "x" {}`, "job"},
		"unknown policy":              {`namespace "a" { policy = "admin" }`, `"admin"`},
		"unknown capability":          {`namespace "a" { capabilities = ["submit"] }`, `"submit"`},
		"fetch in a namespace":        {`namespace "a" { capabilities = ["fetch"] }`, `"fetch"`},
		"a namespace capability up":   {`capabilities = ["read"]`, `"read"`},
		"empty block":                 {`namespace "a" {}`, "set policy or capabilities"},
		"deny with capabilities":      {"namespace \"a\" {\n policy = \"deny\"\n capabilities = [\"read\"]\n}", "deny allows no capabilities"},
		"the same namespace twice":    {"namespace \"a\" { policy = \"read\" }\nnamespace \"a\" { policy = \"write\" }", "twice"},
		"a name with a slash":         {`namespace "a/b" { policy = "read" }`, "letters, digits"},
		"a name with a space":         {`namespace "a b" { policy = "read" }`, "letters, digits"},
		"a name that is empty":        {`namespace "" { policy = "read" }`, "letters, digits"},
		"capabilities that are a map": {`namespace "a" { capabilities = {} }`, "list of string"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse(tc.rules)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Parse(%q) error = %v, want one that mentions %q", tc.rules, err, tc.want)
			}
		})
	}
}

func TestEmptyRulesGrantNothing(t *testing.T) {
	a := New(mustParse(t, ``))
	if a.Allow("default", Read) || a.Global(Fetch) {
		t.Error("an ACL policy without rules granted something")
	}
}

func TestAllowTakesTheMostSpecificBlock(t *testing.T) {
	a := New(mustParse(t, `
namespace "*"    { policy = "write" }
namespace "pr*"  { policy = "read" }
namespace "prod" { capabilities = ["approve"] }
`))
	for _, tc := range []struct {
		ns   string
		c    Capability
		want bool
	}{
		{"prod", Approve, true},
		{"prod", Read, false}, // the exact block hides the wider ones
		{"prod", Pause, false},
		{"prom", Read, true},
		{"prom", Approve, false}, // pr* is read only
		{"staging", Approve, true},
		{"staging", Pause, true},
	} {
		if got := a.Allow(tc.ns, tc.c); got != tc.want {
			t.Errorf("Allow(%q, %s) = %v, want %v", tc.ns, tc.c, got, tc.want)
		}
	}
}

func TestAGlobWithMoreLiteralCharactersWins(t *testing.T) {
	a := New(mustParse(t, `
namespace "p*"   { policy = "write" }
namespace "pro*" { policy = "read" }
`))
	if a.Allow("prod", Approve) {
		t.Error(`"pro*" should beat "p*" for prod, and it only reads`)
	}
	if !a.Allow("play", Approve) {
		t.Error(`"p*" should apply to play`)
	}
}

func TestDenyWinsAcrossACLPolicies(t *testing.T) {
	writer := mustParse(t, `namespace "prod" { policy = "write" }`)
	denier := mustParse(t, `namespace "prod" { policy = "deny" }`)
	a := New(writer, denier)
	for _, c := range namespaceCapabilities {
		if a.Allow("prod", c) {
			t.Errorf("a deny and a write on prod allowed %s", c)
		}
	}
}

func TestADenyOnAWiderBlockDoesNotReachAnExactOne(t *testing.T) {
	a := New(mustParse(t, `
namespace "*"    { policy = "deny" }
namespace "prod" { policy = "read" }
`))
	if !a.Allow("prod", Read) {
		t.Error("the exact block for prod should beat the deny on *")
	}
	if a.Allow("dev", Read) {
		t.Error("dev is denied by *")
	}
}

func TestCapabilitiesAddUpAcrossACLPolicies(t *testing.T) {
	a := New(
		mustParse(t, `namespace "prod" { capabilities = ["read"] }`),
		mustParse(t, `namespace "prod" { capabilities = ["approve"] }
capabilities = ["fetch"]`),
	)
	if !a.Allow("prod", Read) || !a.Allow("prod", Approve) {
		t.Error("read and approve should add up")
	}
	if a.Allow("prod", Pause) {
		t.Error("pause was never granted")
	}
	if !a.Global(Fetch) {
		t.Error("fetch should be granted")
	}
}

func TestNoACLPoliciesGrantNothing(t *testing.T) {
	a := New()
	if a.Allow("default", Read) || a.Global(Fetch) || a.Management() {
		t.Error("a token without ACL policies allowed something")
	}
}

func TestManagementAllowsEverything(t *testing.T) {
	a := Management()
	if !a.Allow("anything", Approve) || !a.Global(Fetch) || !a.Management() {
		t.Error("a management ACL refused something")
	}
}

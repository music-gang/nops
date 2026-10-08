package acl

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/hashicorp/go-bexpr"
	"github.com/hashicorp/go-bexpr/grammar"
)

// What a binding rule binds.
const (
	BindPolicy     = "policy"     // an ACL policy, by name
	BindManagement = "management" // a management token
)

// Claims are what an auth method says about the person who logged in. A
// selector reads them as value.username, value.sub and list.groups.
type Claims struct {
	Username string
	Sub      string
	Groups   []string
}

// selectorTarget is what a selector is evaluated on. It is a struct, not a
// map, so a claim that does not exist is an error and not a quiet "false".
type selectorTarget struct {
	Value struct {
		Username string `bexpr:"username"`
		Sub      string `bexpr:"sub"`
	} `bexpr:"value"`
	List struct {
		Groups []string `bexpr:"groups"`
	} `bexpr:"list"`
}

func (c Claims) target() selectorTarget {
	var t selectorTarget
	t.Value.Username, t.Value.Sub, t.List.Groups = c.Username, c.Sub, c.Groups
	return t
}

// claimPaths are the claims a selector may read.
var claimPaths = [][]string{{"value", "username"}, {"value", "sub"}, {"list", "groups"}}

// ValidSelector checks a selector: it parses, reads only the claims above, and
// evaluates without error. An empty one is valid: it matches every login of
// its auth method.
func ValidSelector(selector string) error {
	if strings.TrimSpace(selector) == "" {
		return nil
	}
	ast, err := grammar.Parse("", []byte(selector))
	if err != nil {
		return fmt.Errorf("selector: %w", err)
	}
	if err := checkClaims(ast.(grammar.Expression)); err != nil {
		return fmt.Errorf("selector: %w", err)
	}
	// Evaluated on a login that has every claim, so that a claim used as the
	// wrong kind (a list compared to a string) is refused too.
	if _, err := matches(selector, Claims{Username: "u", Sub: "s", Groups: []string{"g"}}); err != nil {
		return fmt.Errorf("selector: %w", err)
	}
	return nil
}

// checkClaims refuses a selector that reads anything but the claims. A
// collection expression is checked on the list it walks, not on its body.
func checkClaims(e grammar.Expression) error {
	var sel *grammar.Selector
	switch n := e.(type) {
	case *grammar.UnaryExpression:
		return checkClaims(n.Operand)
	case *grammar.BinaryExpression:
		if err := checkClaims(n.Left); err != nil {
			return err
		}
		return checkClaims(n.Right)
	case *grammar.MatchExpression:
		sel = &n.Selector
	case *grammar.CollectionExpression:
		sel = &n.Selector
	default:
		return nil
	}
	if !slices.ContainsFunc(claimPaths, func(p []string) bool { return slices.Equal(p, sel.Path) }) {
		return fmt.Errorf("%q is not a claim: use value.username, value.sub or list.groups", sel.String())
	}
	return nil
}

func matches(selector string, c Claims) (bool, error) {
	eval, err := bexpr.CreateEvaluator(selector)
	if err != nil {
		return false, err
	}
	return eval.Evaluate(c.target())
}

// Binding is a binding rule as Bind reads it.
type Binding struct {
	ID         string
	AuthMethod string
	Selector   string
	Type       string // BindPolicy or BindManagement
	Name       string // the ACL policy a BindPolicy rule binds
}

// Granted is what the binding rules that match a login add up to.
type Granted struct {
	Management bool
	Policies   []string // sorted, without repeats
}

// Any reports whether a rule matched: a login that no rule matches gets no token.
func (g Granted) Any() bool { return g.Management || len(g.Policies) > 0 }

// Bind adds up the rules of the auth method that match the claims. A rule whose
// selector cannot be evaluated binds nothing and is reported in the error;
// the others still apply.
func Bind(method string, claims Claims, rules []Binding) (Granted, error) {
	var g Granted
	var errs []error
	for _, r := range rules {
		if r.AuthMethod != method {
			continue
		}
		ok := true
		if strings.TrimSpace(r.Selector) != "" {
			var err error
			if ok, err = matches(r.Selector, claims); err != nil {
				errs = append(errs, fmt.Errorf("binding rule %s: %w", r.ID, err))
				continue
			}
		}
		if !ok {
			continue
		}
		switch r.Type {
		case BindManagement:
			g.Management = true
		case BindPolicy:
			g.Policies = append(g.Policies, r.Name)
		}
	}
	slices.Sort(g.Policies)
	g.Policies = slices.Compact(g.Policies)
	return g, errors.Join(errs...)
}

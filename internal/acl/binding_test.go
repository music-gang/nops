package acl

import (
	"reflect"
	"testing"
)

func TestValidSelector(t *testing.T) {
	for _, sel := range []string{
		"",
		"  ",
		`value.username == "alice"`,
		`value.sub == "8f1c"`,
		`"ops" in list.groups`,
		`value.username == "alice" and "ops" in list.groups`,
	} {
		if err := ValidSelector(sel); err != nil {
			t.Errorf("ValidSelector(%q) = %v, want nil", sel, err)
		}
	}
	for _, sel := range []string{
		`value.nope == "alice"`,    // a claim that does not exist
		`username == "alice"`,      // not under value
		`value.username ==`,        // not an expression
		`list.groups == "ops"`,     // a list is not a string
		`value.username in "ops"`,  // a string is not a list
		`"ops" in list.groupz`,     // a mistyped claim
		`value.username == "a" or`, // cut short
	} {
		if err := ValidSelector(sel); err == nil {
			t.Errorf("ValidSelector(%q) = nil, want an error", sel)
		}
	}
}

func TestBind(t *testing.T) {
	alice := Claims{Username: "alice", Sub: "sub-1", Groups: []string{"ops", "dev"}}
	rules := []Binding{
		{ID: "r1", AuthMethod: "oidc", Selector: `"ops" in list.groups`, Type: BindPolicy, Name: "operator"},
		{ID: "r2", AuthMethod: "oidc", Selector: `value.username == "bob"`, Type: BindPolicy, Name: "bobs"},
		{ID: "r3", AuthMethod: "oidc", Selector: `value.sub == "sub-1"`, Type: BindPolicy, Name: "personal"},
		{ID: "r4", AuthMethod: "basic", Selector: "", Type: BindManagement},
		{ID: "r5", AuthMethod: "oidc", Selector: `"dev" in list.groups`, Type: BindPolicy, Name: "operator"},
	}

	t.Run("the rules that match add up", func(t *testing.T) {
		g, err := Bind("oidc", alice, rules)
		if err != nil {
			t.Fatal(err)
		}
		if g.Management || !reflect.DeepEqual(g.Policies, []string{"operator", "personal"}) {
			t.Errorf("granted = %+v, want operator and personal", g)
		}
	})

	t.Run("a rule of another auth method does not apply", func(t *testing.T) {
		g, _ := Bind("basic", alice, rules)
		if !g.Management || len(g.Policies) != 0 {
			t.Errorf("granted = %+v, want management only", g)
		}
	})

	t.Run("an empty selector matches every login of the method", func(t *testing.T) {
		g, _ := Bind("basic", Claims{Username: "anyone"}, rules)
		if !g.Management || !g.Any() {
			t.Errorf("granted = %+v, want management", g)
		}
	})

	t.Run("a login no rule matches gets nothing", func(t *testing.T) {
		g, err := Bind("oidc", Claims{Username: "carol", Sub: "sub-3"}, rules)
		if err != nil || g.Any() {
			t.Errorf("granted = %+v, err = %v, want nothing", g, err)
		}
	})

	t.Run("management is added to the policies, not instead of them", func(t *testing.T) {
		g, _ := Bind("oidc", alice, append(rules, Binding{ID: "r6", AuthMethod: "oidc", Type: BindManagement}))
		if !g.Management || len(g.Policies) != 2 {
			t.Errorf("granted = %+v, want management and two policies", g)
		}
	})

	t.Run("a selector that cannot be evaluated binds nothing and is reported", func(t *testing.T) {
		broken := append([]Binding{{ID: "bad", AuthMethod: "oidc", Selector: `value.nope == "x"`, Type: BindManagement}}, rules...)
		g, err := Bind("oidc", alice, broken)
		if err == nil {
			t.Error("the broken rule was not reported")
		}
		if g.Management || len(g.Policies) != 2 {
			t.Errorf("granted = %+v, want the other rules to apply and not management", g)
		}
	})
}

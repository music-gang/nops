package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// The commands under "nops acl" (docs/cli.md) call /api/acl/ (docs/api.md).

// aclFlags are the flags of the commands that make an ACL policy, a binding
// rule or a token.
type aclFlags struct {
	description string
	name, kind  string
	policies    stringList
	expires     string

	authMethod, selector, bindType, bindName string

	creator, creatorIdentity string
}

// stringList is a flag that can be given more than once.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }

func (l *stringList) Set(v string) error {
	*l = append(*l, v)
	return nil
}

func policyFlags(fs *flag.FlagSet, k *call) {
	fs.StringVar(&k.flags.description, "description", "", "what the ACL policy is for")
}

func tokenFlags(fs *flag.FlagSet, k *call) {
	fs.StringVar(&k.flags.name, "name", "", "name of the token (required)")
	fs.StringVar(&k.flags.kind, "type", "client", "client, or management for a token that can do everything")
	fs.Var(&k.flags.policies, "policy", "ACL policy a client token carries (repeat it for more)")
	fs.StringVar(&k.flags.expires, "expires", "", "how long it lasts, such as 720h (default: it never expires)")
}

func revokeCreatedFlags(fs *flag.FlagSet, k *call) {
	fs.StringVar(&k.flags.creator, "creator", "", "accessor ID of the token that created them, bootstrap for the bootstrap token")
	fs.StringVar(&k.flags.creatorIdentity, "creator-identity", "", "identity of the person who created them")
}

func bindingRuleFlags(fs *flag.FlagSet, k *call) {
	fs.StringVar(&k.flags.description, "description", "", "what the binding rule is for")
	fs.StringVar(&k.flags.authMethod, "auth-method", "", "oidc or basic (required)")
	fs.StringVar(&k.flags.selector, "selector", "", "a go-bexpr expression on value.username, value.sub and list.groups (default: every login of the auth method)")
	fs.StringVar(&k.flags.bindType, "bind-type", "", "policy, or management for everything (required)")
	fs.StringVar(&k.flags.bindName, "bind-name", "", "the ACL policy a policy rule binds")
}

func policyPath(k *call) string { return "/api/acl/policies/" + url.PathEscape(k.arg) }
func tokenPath(k *call) string  { return "/api/acl/tokens/" + url.PathEscape(k.arg) }
func bindingRulePath(k *call) string {
	return "/api/acl/binding-rules/" + url.PathEscape(k.arg)
}

// What the API answers, as far as the text output reads it.

type aclPolicy struct {
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Rules       string    `json:"rules"`
	ModifiedAt  time.Time `json:"modified_at"`
}

type bindingRule struct {
	ID          string    `json:"id"`
	Description string    `json:"description"`
	AuthMethod  string    `json:"auth_method"`
	Selector    string    `json:"selector"`
	BindType    string    `json:"bind_type"`
	BindName    string    `json:"bind_name"`
	ModifiedAt  time.Time `json:"modified_at"`
}

// binds says what a binding rule gives.
func (b bindingRule) binds() string {
	if b.BindType == "management" {
		return "management"
	}
	return "ACL policy " + b.BindName
}

type aclToken struct {
	AccessorID      string     `json:"accessor_id"`
	Name            string     `json:"name"`
	Type            string     `json:"type"`
	Origin          string     `json:"origin"`
	Identity        string     `json:"identity"`
	Policies        []string   `json:"policies"`
	CreatedAt       time.Time  `json:"created_at"`
	ExpiresAt       *time.Time `json:"expires_at"`
	CreatorName     string     `json:"creator_name"`
	CreatorIdentity string     `json:"creator_identity"`
	Secret          string     `json:"secret"`
}

// show prints the answer as JSON with -json, and as text otherwise.
func show(k *call, raw []byte, text func(io.Writer)) {
	if k.asJSON {
		writeJSON(k.out, raw)
		return
	}
	text(k.out)
}

func runPolicyList(k *call) error {
	var r struct {
		Policies []aclPolicy `json:"acl_policies"`
	}
	raw, err := k.client.get(k.ctx, "/api/acl/policies", &r)
	if err != nil {
		return err
	}
	show(k, raw, func(w io.Writer) {
		if len(r.Policies) == 0 {
			fmt.Fprintln(w, "No ACL policies.")
			return
		}
		t := table(w)
		fmt.Fprintln(t, "NAME\tDESCRIPTION\tMODIFIED")
		for _, p := range r.Policies {
			fmt.Fprintf(t, "%s\t%s\t%s\n", p.Name, dash(p.Description), stamp(p.ModifiedAt))
		}
		t.Flush()
	})
	return nil
}

func runPolicyInfo(k *call) error {
	var p aclPolicy
	raw, err := k.client.get(k.ctx, policyPath(k), &p)
	if err != nil {
		return err
	}
	show(k, raw, func(w io.Writer) { writePolicy(w, p) })
	return nil
}

func writePolicy(w io.Writer, p aclPolicy) {
	fmt.Fprintf(w, "Name:        %s\nDescription: %s\nModified:    %s\n\n%s\n", p.Name, dash(p.Description), stamp(p.ModifiedAt), strings.TrimRight(p.Rules, "\n"))
}

// runPolicyApply sends the rules of a file. Rules that do not parse come back
// as the error of the API, with the line, and nothing is saved.
func runPolicyApply(k *call) error {
	rules, err := os.ReadFile(k.args[1])
	if err != nil {
		return fmt.Errorf("read the rules: %w", err)
	}
	body := map[string]string{"rules": string(rules)}
	if k.flags.description != "" {
		body["description"] = k.flags.description
	}
	raw, err := k.client.do(k.ctx, http.MethodPut, policyPath(k), body)
	if err != nil {
		return err
	}
	show(k, raw, func(w io.Writer) { fmt.Fprintf(w, "saved %s\n", k.arg) })
	return nil
}

func runBindingRuleList(k *call) error {
	var r struct {
		Rules []bindingRule `json:"binding_rules"`
	}
	raw, err := k.client.get(k.ctx, "/api/acl/binding-rules", &r)
	if err != nil {
		return err
	}
	show(k, raw, func(w io.Writer) {
		if len(r.Rules) == 0 {
			fmt.Fprintln(w, "No binding rules.")
			return
		}
		t := table(w)
		fmt.Fprintln(t, "ID\tAUTH METHOD\tSELECTOR\tBINDS\tMODIFIED")
		for _, b := range r.Rules {
			fmt.Fprintf(t, "%s\t%s\t%s\t%s\t%s\n", b.ID, b.AuthMethod, dash(b.Selector), b.binds(), stamp(b.ModifiedAt))
		}
		t.Flush()
	})
	return nil
}

func runBindingRuleInfo(k *call) error {
	var b bindingRule
	raw, err := k.client.get(k.ctx, bindingRulePath(k), &b)
	if err != nil {
		return err
	}
	show(k, raw, func(w io.Writer) { writeBindingRule(w, b) })
	return nil
}

func writeBindingRule(w io.Writer, b bindingRule) {
	fmt.Fprintf(w, "ID:          %s\nDescription: %s\nAuth method: %s\nSelector:    %s\nBinds:       %s\nModified:    %s\n",
		b.ID, dash(b.Description), b.AuthMethod, dash(b.Selector), b.binds(), stamp(b.ModifiedAt))
}

// bindingRuleBody is the rule the flags describe. Update replaces the whole
// rule, so it takes the same flags as create.
func bindingRuleBody(k *call) (map[string]string, error) {
	f := k.flags
	if f.authMethod == "" || f.bindType == "" {
		return nil, usageErrorf("-auth-method and -bind-type are required")
	}
	return map[string]string{
		"description": f.description, "auth_method": f.authMethod, "selector": f.selector,
		"bind_type": f.bindType, "bind_name": f.bindName,
	}, nil
}

func runBindingRuleCreate(k *call) error {
	body, err := bindingRuleBody(k)
	if err != nil {
		return err
	}
	raw, err := k.client.do(k.ctx, http.MethodPost, "/api/acl/binding-rules", body)
	if err != nil {
		return err
	}
	var b bindingRule
	if err := json.Unmarshal(raw, &b); err != nil {
		return fmt.Errorf("the answer is not what the API sends: %w", err)
	}
	show(k, raw, func(w io.Writer) { writeBindingRule(w, b) })
	return nil
}

func runBindingRuleUpdate(k *call) error {
	body, err := bindingRuleBody(k)
	if err != nil {
		return err
	}
	raw, err := k.client.do(k.ctx, http.MethodPut, bindingRulePath(k), body)
	if err != nil {
		return err
	}
	show(k, raw, func(w io.Writer) { fmt.Fprintf(w, "saved %s\n", k.arg) })
	return nil
}

func runTokenList(k *call) error {
	var r struct {
		Tokens []aclToken `json:"tokens"`
	}
	raw, err := k.client.get(k.ctx, "/api/acl/tokens", &r)
	if err != nil {
		return err
	}
	show(k, raw, func(w io.Writer) {
		if len(r.Tokens) == 0 {
			fmt.Fprintln(w, "No tokens.")
			return
		}
		t := table(w)
		fmt.Fprintln(t, "ACCESSOR ID\tNAME\tTYPE\tORIGIN\tACL POLICIES\tEXPIRES")
		for _, tok := range r.Tokens {
			fmt.Fprintf(t, "%s\t%s\t%s\t%s\t%s\t%s\n", tok.AccessorID, tok.Name, tok.Type, tok.Origin, dash(strings.Join(tok.Policies, ",")), expiry(tok))
		}
		t.Flush()
	})
	return nil
}

func expiry(t aclToken) string {
	if t.ExpiresAt == nil {
		return "never"
	}
	return stamp(*t.ExpiresAt)
}

func runTokenInfo(k *call) error {
	var t aclToken
	raw, err := k.client.get(k.ctx, tokenPath(k), &t)
	if err != nil {
		return err
	}
	show(k, raw, func(w io.Writer) { writeToken(w, t) })
	return nil
}

func runTokenSelf(k *call) error {
	var t aclToken
	raw, err := k.client.get(k.ctx, "/api/acl/token/self", &t)
	if err != nil {
		return err
	}
	show(k, raw, func(w io.Writer) { writeToken(w, t) })
	return nil
}

func writeToken(w io.Writer, t aclToken) {
	fmt.Fprintf(w, "Accessor ID:  %s\nName:         %s\nType:         %s\nACL policies: %s\nCreated:      %s\nExpires:      %s\n",
		t.AccessorID, t.Name, t.Type, dash(strings.Join(t.Policies, ", ")), stamp(t.CreatedAt), expiry(t))
	if t.Identity != "" {
		fmt.Fprintf(w, "Identity:     %s\n", t.Identity)
	}
	if t.CreatorName != "" && t.CreatorIdentity != "" && t.CreatorIdentity != t.Identity {
		fmt.Fprintf(w, "Created by:   %s (%s)\n", t.CreatorName, t.CreatorIdentity)
	} else if t.CreatorName != "" {
		fmt.Fprintf(w, "Created by:   %s\n", t.CreatorName)
	}
}

// runRevokeSessions revokes every session of the person named by identity.
func runRevokeSessions(k *call) error {
	if err := k.confirm(fmt.Sprintf("Revoke every session of %s?", k.arg)); err != nil {
		return err
	}
	return revokeTokens(k, "/api/acl/tokens/revoke-sessions", map[string]string{"identity": k.arg})
}

// runRevokeCreated revokes every token a person or a token created, and the
// tokens those created.
func runRevokeCreated(k *call) error {
	if (k.flags.creator == "") == (k.flags.creatorIdentity == "") {
		return usageErrorf("set one of -creator and -creator-identity")
	}
	body, who := map[string]string{"creator_accessor_id": k.flags.creator}, k.flags.creator
	if k.flags.creatorIdentity != "" {
		body, who = map[string]string{"creator_identity": k.flags.creatorIdentity}, k.flags.creatorIdentity
	}
	if err := k.confirm(fmt.Sprintf("Revoke every token %s created, and the tokens those created?", who)); err != nil {
		return err
	}
	return revokeTokens(k, "/api/acl/tokens/revoke-created", body)
}

// revokeTokens asks the API for a revocation in one action and prints what it
// revoked.
func revokeTokens(k *call, path string, body map[string]string) error {
	raw, err := k.client.do(k.ctx, http.MethodPost, path, body)
	if err != nil {
		return err
	}
	var r struct {
		Revoked []string `json:"revoked"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return fmt.Errorf("the answer is not what the API sends: %w", err)
	}
	show(k, raw, func(w io.Writer) {
		if len(r.Revoked) == 0 {
			fmt.Fprintln(w, "No token to revoke.")
		}
		for _, id := range r.Revoked {
			fmt.Fprintln(w, "revoked", id)
		}
	})
	return nil
}

// runTokenCreate prints the secret on stdout, the only time Nops has it; the
// warning goes to stderr so a pipe keeps only the answer.
func runTokenCreate(k *call) error {
	if k.flags.name == "" {
		return usageErrorf("-name is required")
	}
	body := map[string]any{"name": k.flags.name, "type": k.flags.kind}
	if len(k.flags.policies) > 0 {
		body["policies"] = []string(k.flags.policies)
	}
	if k.flags.expires != "" {
		body["expires_in"] = k.flags.expires
	}
	raw, err := k.client.do(k.ctx, http.MethodPost, "/api/acl/tokens", body)
	if err != nil {
		return err
	}
	var t aclToken
	if err := json.Unmarshal(raw, &t); err != nil {
		return fmt.Errorf("the answer is not what the API sends: %w", err)
	}
	if k.asJSON {
		writeJSON(k.out, raw)
		return nil
	}
	writeToken(k.out, t)
	fmt.Fprintf(k.out, "Secret:       %s\n", t.Secret)
	fmt.Fprintln(k.errOut, "Copy the secret now: Nops keeps only its hash and does not show it again.")
	return nil
}

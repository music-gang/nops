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

// aclFlags are the flags of the commands that make an ACL policy or a token.
type aclFlags struct {
	description string
	name, kind  string
	policies    stringList
	expires     string
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

func policyPath(k *call) string { return "/api/acl/policies/" + url.PathEscape(k.arg) }
func tokenPath(k *call) string  { return "/api/acl/tokens/" + url.PathEscape(k.arg) }

// What the API answers, as far as the text output reads it.

type aclPolicy struct {
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Rules       string    `json:"rules"`
	ModifiedAt  time.Time `json:"modified_at"`
}

type aclToken struct {
	AccessorID  string     `json:"accessor_id"`
	Name        string     `json:"name"`
	Type        string     `json:"type"`
	Policies    []string   `json:"policies"`
	CreatedAt   time.Time  `json:"created_at"`
	ExpiresAt   *time.Time `json:"expires_at"`
	CreatorName string     `json:"creator_name"`
	Secret      string     `json:"secret"`
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
		fmt.Fprintln(t, "ACCESSOR ID\tNAME\tTYPE\tACL POLICIES\tEXPIRES")
		for _, tok := range r.Tokens {
			fmt.Fprintf(t, "%s\t%s\t%s\t%s\t%s\n", tok.AccessorID, tok.Name, tok.Type, dash(strings.Join(tok.Policies, ",")), expiry(tok))
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
	if t.CreatorName != "" {
		fmt.Fprintf(w, "Created by:   %s\n", t.CreatorName)
	}
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

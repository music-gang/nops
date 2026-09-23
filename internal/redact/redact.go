// Package redact removes secret values from a Nomad plan diff before it is
// saved to SQLite or shown in the dashboard.
//
// The rules work on the names Nomad gives to the diff fields (verified on
// Nomad 2.0.3, see testdata/plan_diff.json). They are documented in
// docs/dashboard.md#secret-redaction: keep the two aligned.
package redact

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/hashicorp/nomad/api"
)

// Marker replaces a secret value. An empty value stays empty, so an added or
// removed secret still reads as added or removed.
const Marker = "<redacted>"

// secretWords are matched against field and object names, lowercased and
// with '-' and '_' removed: "api_key", "api-key" and "APIKey" are one name.
var secretWords = []string{
	"password", "token", "secret", "auth", "credential", "privatekey", "apikey",
}

// urlCredentials matches the user:password part of a URL anywhere in a value,
// e.g. an artifact source "https://user:token@host/repo".
var urlCredentials = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://)[^/@\s:]+:[^/@\s]+@`)

// Diff returns the JSON of a redacted copy of d; d is not modified. The field
// names and the change types stay, so a reviewer sees which secret changed
// and how, but not its value. A nil diff gives "null".
func Diff(d *api.JobDiff) ([]byte, error) {
	// A JSON round trip is the deep copy: the result is JSON anyway.
	raw, err := json.Marshal(d)
	if err != nil {
		return nil, fmt.Errorf("redact plan diff: %w", err)
	}
	var c *api.JobDiff
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("redact plan diff: %w", err)
	}
	if c != nil {
		fields(c.Fields, false)
		objects(c.Objects, false)
		for _, tg := range c.TaskGroups {
			if tg == nil {
				continue
			}
			fields(tg.Fields, false)
			objects(tg.Objects, false)
			for _, t := range tg.Tasks {
				if t == nil {
					continue
				}
				fields(t.Fields, false)
				objects(t.Objects, false)
			}
		}
	}
	// No HTML escaping, so the stored JSON reads "<redacted>" rather than
	// "<redacted>". The dashboard escapes when it renders.
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(c); err != nil {
		return nil, fmt.Errorf("redact plan diff: %w", err)
	}
	return bytes.TrimSuffix(out.Bytes(), []byte("\n")), nil
}

// objects redacts every field below an object whose name is a secret (the
// docker "auth" block) or that holds HTTP headers (a service check "Header").
func objects(objs []*api.ObjectDiff, all bool) {
	for _, o := range objs {
		if o == nil {
			continue
		}
		sub := all || secretName(o.Name) || o.Name == "Header"
		fields(o.Fields, sub)
		objects(o.Objects, sub)
	}
}

func fields(fs []*api.FieldDiff, all bool) {
	for _, f := range fs {
		if f == nil {
			continue
		}
		if all || secretField(f.Name) {
			f.Old, f.New = mask(f.Old), mask(f.New)
			continue
		}
		f.Old = urlCredentials.ReplaceAllString(f.Old, "${1}"+Marker+"@")
		f.New = urlCredentials.ReplaceAllString(f.New, "${1}"+Marker+"@")
	}
}

// secretField reports whether a field holds a secret: an environment
// variable, a template body, an artifact header, or a secret-looking name.
func secretField(name string) bool {
	return strings.HasPrefix(name, "Env[") ||
		name == "EmbeddedTmpl" ||
		strings.HasPrefix(name, "GetterHeaders[") ||
		secretName(name)
}

func secretName(name string) bool {
	n := strings.NewReplacer("-", "", "_", "").Replace(strings.ToLower(name))
	for _, w := range secretWords {
		if strings.Contains(n, w) {
			return true
		}
	}
	return false
}

func mask(v string) string {
	if v == "" {
		return ""
	}
	return Marker
}

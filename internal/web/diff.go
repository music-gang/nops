package web

import (
	"encoding/json"
	"fmt"
	"html/template"
	"strings"

	"github.com/hashicorp/nomad/api"

	"github.com/music-gang/nops/internal/redact"
)

// templateFuncs are the functions every template can call. Kept small on
// purpose: most of the view logic lives in Go (render.go), not in the
// templates.
var templateFuncs = template.FuncMap{
	"lower":          strings.ToLower,
	"diffCell":       diffCell,
	"stateLabel":     stateLabel,
	"stateClass":     stateClass,
	"hookStateLabel": hookStateLabel,
	"hookStateClass": hookStateClass,
	"isTerminal":     isTerminal,
	"shortCommit":    shortCommit,
}

// parseDiff decodes a stored plan_diff column (already redacted by
// internal/redact before it reached SQLite: docs/dashboard.md#secret-redaction)
// back into the Nomad diff it was. An empty string (no drift) gives a nil
// diff, which the "diff" template renders as "No changes."
func parseDiff(raw string) (*api.JobDiff, error) {
	if raw == "" {
		return nil, nil
	}
	var d api.JobDiff
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		return nil, fmt.Errorf("parse plan diff: %w", err)
	}
	return &d, nil
}

// diffCell renders one Old/New diff cell. A redacted value becomes a pill so
// it reads as "a secret changed here", not as literal text; an empty value
// becomes a muted dash instead of a blank cell. Values are escaped by hand
// with html/template's own escaper because the function returns
// template.HTML, which html/template otherwise trusts verbatim.
func diffCell(v string) template.HTML {
	switch v {
	case redact.Marker:
		return template.HTML(`<span class="chip-redacted">` + template.HTMLEscapeString(v) + `</span>`)
	case "":
		return template.HTML(`<span class="diff-empty">&mdash;</span>`)
	default:
		return template.HTML(template.HTMLEscapeString(v))
	}
}

// shortCommit shortens a git commit SHA to the 7 characters people
// recognize; anything shorter (or not a commit at all) is returned as is.
func shortCommit(sha string) string {
	if len(sha) <= 7 {
		return sha
	}
	return sha[:7]
}

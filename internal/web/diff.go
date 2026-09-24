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
	"short":          short,
	"asset":          asset,
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

// short shortens an identifier (a spec hash, an evaluation ID) to n
// characters; the full value goes in the hover title next to it.
func short(n int, s string) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// shortCommit shortens a git commit SHA to the 7 characters people
// recognize; anything shorter (or not a commit at all) is returned as is.
func shortCommit(sha string) string {
	if len(sha) <= 7 {
		return sha
	}
	return sha[:7]
}

// -- diff summary ---------------------------------------------------------

// diffSummary is what a plan diff comes to at a glance, above the diff itself:
// how many fields it adds, edits and deletes, and where.
type diffSummary struct {
	Added, Edited, Deleted int
	Places                 []diffPlace
}

// diffPlace is one part of the job that changes: the job itself, a task
// group or a task, with the number of field changes directly inside it.
type diffPlace struct {
	Kind    string // "job", "group" or "task"
	Name    string // "" for the job; "group/task" for a task
	Type    string // the change type Nomad gives it: Added, Edited, Deleted
	Changes int
}

// Total is the number of field changes.
func (s diffSummary) Total() int { return s.Added + s.Edited + s.Deleted }

// summarize walks a plan diff. A nil diff (no drift) is the zero summary. A
// redacted value counts like any other: it changed.
func summarize(d *api.JobDiff) diffSummary {
	var s diffSummary
	if d == nil {
		return s
	}
	if n := s.count(d.Fields, d.Objects); n > 0 {
		s.Places = append(s.Places, diffPlace{Kind: "job", Type: d.Type, Changes: n})
	}
	for _, g := range d.TaskGroups {
		if g == nil {
			continue
		}
		n := s.count(g.Fields, g.Objects)
		if n > 0 || g.Type == "Added" || g.Type == "Deleted" {
			s.Places = append(s.Places, diffPlace{Kind: "group", Name: g.Name, Type: g.Type, Changes: n})
		}
		for _, t := range g.Tasks {
			if t == nil {
				continue
			}
			n := s.count(t.Fields, t.Objects)
			if n > 0 || t.Type == "Added" || t.Type == "Deleted" {
				s.Places = append(s.Places, diffPlace{Kind: "task", Name: g.Name + "/" + t.Name, Type: t.Type, Changes: n})
			}
		}
	}
	return s
}

// count adds the changed fields of a level (and of the objects below it) to
// the summary and returns how many there were.
func (s *diffSummary) count(fields []*api.FieldDiff, objects []*api.ObjectDiff) int {
	n := 0
	for _, f := range fields {
		if f == nil {
			continue
		}
		switch f.Type {
		case "Added":
			s.Added++
		case "Edited":
			s.Edited++
		case "Deleted":
			s.Deleted++
		default:
			continue
		}
		n++
	}
	for _, o := range objects {
		if o != nil {
			n += s.count(o.Fields, o.Objects)
		}
	}
	return n
}

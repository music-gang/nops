package nomadx

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hashicorp/nomad/api"
)

// ParseDiff decodes a stored plan_diff column (already redacted by
// internal/redact before it reached SQLite: docs/dashboard.md#secret-redaction)
// back into the Nomad diff it was. An empty string (no drift) gives a nil
// diff, which the dashboard renders as "No changes."
func ParseDiff(raw string) (*api.JobDiff, error) {
	if raw == "" {
		return nil, nil
	}
	var d api.JobDiff
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		return nil, fmt.Errorf("parse plan diff: %w", err)
	}
	return &d, nil
}

// DiffSummary is what a plan diff comes to at a glance, above the diff itself:
// how many fields it adds, edits and deletes, and where.
type DiffSummary struct {
	Added, Edited, Deleted int
	Places                 []DiffPlace
}

// DiffPlace is one part of the job that changes: the job itself, a task
// group or a task, with the number of field changes directly inside it.
type DiffPlace struct {
	Kind    string // "job", "group" or "task"
	Name    string // "" for the job; "group/task" for a task
	Type    string // the change type Nomad gives it: Added, Edited, Deleted
	Changes int
}

// Total is the number of field changes.
func (s DiffSummary) Total() int { return s.Added + s.Edited + s.Deleted }

// Line is the summary in one line, such as "2 groups, 3 tasks changed": the
// task groups and tasks that change, and "job" for a change of the job
// itself. It is "" when nothing changes.
func (s DiffSummary) Line() string {
	var job bool
	var groups, tasks int
	for _, p := range s.Places {
		switch p.Kind {
		case "job":
			job = true
		case "group":
			groups++
		case "task":
			tasks++
		}
	}
	var parts []string
	if job {
		parts = append(parts, "job")
	}
	if groups > 0 {
		parts = append(parts, plural(groups, "group"))
	}
	if tasks > 0 {
		parts = append(parts, plural(tasks, "task"))
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, ", ") + " changed"
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// Summarize walks a plan diff. A nil diff (no drift) is the zero summary. A
// redacted value counts like any other: it changed.
func Summarize(d *api.JobDiff) DiffSummary {
	var s DiffSummary
	if d == nil {
		return s
	}
	if n := s.count(d.Fields, d.Objects); n > 0 {
		s.Places = append(s.Places, DiffPlace{Kind: "job", Type: d.Type, Changes: n})
	}
	for _, g := range d.TaskGroups {
		if g == nil {
			continue
		}
		n := s.count(g.Fields, g.Objects)
		if n > 0 || g.Type == "Added" || g.Type == "Deleted" {
			s.Places = append(s.Places, DiffPlace{Kind: "group", Name: g.Name, Type: g.Type, Changes: n})
		}
		for _, t := range g.Tasks {
			if t == nil {
				continue
			}
			n := s.count(t.Fields, t.Objects)
			if n > 0 || t.Type == "Added" || t.Type == "Deleted" {
				s.Places = append(s.Places, DiffPlace{Kind: "task", Name: g.Name + "/" + t.Name, Type: t.Type, Changes: n})
			}
		}
	}
	return s
}

// count adds the changed fields of a level (and of the objects below it) to
// the summary and returns how many there were.
func (s *DiffSummary) count(fields []*api.FieldDiff, objects []*api.ObjectDiff) int {
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

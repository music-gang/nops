package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/hashicorp/nomad/api"

	"github.com/music-gang/nops/internal/nomadx"
)

// writeDiff prints the plan diff the API sends, as the dashboard shows it: what
// is added (+), deleted (-) or edited (~) in the job, its groups and its
// tasks, and nothing that stays as it is. A redacted value is printed as the
// marker the API holds.
func writeDiff(w io.Writer, raw json.RawMessage) error {
	d, err := nomadx.ParseDiff(string(raw))
	if err != nil {
		return err
	}
	s := nomadx.Summarize(d)
	if d == nil || s.Total() == 0 && s.Line() == "" {
		fmt.Fprintln(w, "No changes.")
		return nil
	}
	fmt.Fprintf(w, "%s (%d added, %d edited, %d deleted)\n\n", s.Line(), s.Added, s.Edited, s.Deleted)

	fmt.Fprintf(w, "Job %q\n", d.ID)
	writeNode(w, 1, d.Fields, d.Objects)
	for _, g := range d.TaskGroups {
		if g == nil || g.Type == "None" {
			continue
		}
		fmt.Fprintf(w, "  Group %q (%s)\n", g.Name, g.Type)
		writeNode(w, 2, g.Fields, g.Objects)
		for _, t := range g.Tasks {
			if t == nil || t.Type == "None" {
				continue
			}
			fmt.Fprintf(w, "    Task %q (%s)\n", t.Name, t.Type)
			writeNode(w, 3, t.Fields, t.Objects)
		}
	}
	return nil
}

// writeNode prints the changed fields of one level and the objects below it,
// indented by depth.
func writeNode(w io.Writer, depth int, fields []*api.FieldDiff, objects []*api.ObjectDiff) {
	pad := strings.Repeat("  ", depth)
	for _, f := range fields {
		if f == nil {
			continue
		}
		switch f.Type {
		case "Added":
			fmt.Fprintf(w, "%s+ %s: %s\n", pad, f.Name, f.New)
		case "Deleted":
			fmt.Fprintf(w, "%s- %s: %s\n", pad, f.Name, f.Old)
		case "Edited":
			fmt.Fprintf(w, "%s~ %s: %s => %s\n", pad, f.Name, f.Old, f.New)
		}
	}
	for _, o := range objects {
		if o == nil || o.Type == "None" {
			continue
		}
		fmt.Fprintf(w, "%s%s (%s)\n", pad, o.Name, o.Type)
		writeNode(w, depth+1, o.Fields, o.Objects)
	}
}

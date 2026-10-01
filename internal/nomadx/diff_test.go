package nomadx

import (
	"testing"

	"github.com/hashicorp/nomad/api"
)

// sampleDiff changes one job field and, inside task group g / task t, two more.
func sampleDiff() *api.JobDiff {
	return &api.JobDiff{
		Type:   "Edited",
		Fields: []*api.FieldDiff{{Type: "Edited", Name: "Priority", Old: "50", New: "60"}},
		TaskGroups: []*api.TaskGroupDiff{{
			Type: "Edited", Name: "g",
			Tasks: []*api.TaskDiff{{
				Type: "Edited", Name: "t",
				Fields:  []*api.FieldDiff{{Type: "Added", Name: "Env[FOO]", Old: "", New: "<redacted>"}},
				Objects: []*api.ObjectDiff{{Type: "Edited", Name: "Resources", Fields: []*api.FieldDiff{{Type: "Deleted", Name: "CPU", Old: "100"}}}},
			}},
		}},
	}
}

func TestSummarize(t *testing.T) {
	if s := Summarize(nil); s.Total() != 0 || len(s.Places) != 0 {
		t.Errorf("Summarize(nil) = %+v, want nothing", s)
	}
	s := Summarize(sampleDiff())
	if s.Added != 1 || s.Edited != 1 || s.Deleted != 1 || s.Total() != 3 {
		t.Errorf("counts = +%d ~%d -%d, want one each", s.Added, s.Edited, s.Deleted)
	}
	// The group has no field of its own and is only Edited because a task in it
	// is: it is not a place by itself.
	want := []DiffPlace{
		{Kind: "job", Type: "Edited", Changes: 1},
		{Kind: "task", Name: "g/t", Type: "Edited", Changes: 2},
	}
	if len(s.Places) != len(want) {
		t.Fatalf("places = %+v, want %+v", s.Places, want)
	}
	for i := range want {
		if s.Places[i] != want[i] {
			t.Errorf("place %d = %+v, want %+v", i, s.Places[i], want[i])
		}
	}

	// A whole task group or task that appears or goes counts as a place even
	// with no field of its own, and an unchanged field counts for nothing.
	added := Summarize(&api.JobDiff{TaskGroups: []*api.TaskGroupDiff{
		{Type: "Added", Name: "new", Fields: []*api.FieldDiff{{Type: "None", Name: "Count"}}},
		{Type: "Deleted", Name: "old", Tasks: []*api.TaskDiff{{Type: "Deleted", Name: "t"}}},
	}})
	if added.Total() != 0 || len(added.Places) != 3 {
		t.Errorf("summary of added and deleted groups = %+v, want 3 places and no field", added)
	}
}

func TestDiffSummaryLine(t *testing.T) {
	for name, tt := range map[string]struct {
		diff *api.JobDiff
		want string
	}{
		"no drift":      {nil, ""},
		"job and task":  {sampleDiff(), "job, 1 task changed"},
		"groups, tasks": {twoGroupsThreeTasks(), "2 groups, 3 tasks changed"},
		"one group": {&api.JobDiff{TaskGroups: []*api.TaskGroupDiff{
			{Type: "Added", Name: "new"},
		}}, "1 group changed"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := Summarize(tt.diff).Line(); got != tt.want {
				t.Errorf("Line() = %q, want %q", got, tt.want)
			}
		})
	}
}

func twoGroupsThreeTasks() *api.JobDiff {
	edit := []*api.FieldDiff{{Type: "Edited", Name: "Count"}}
	task := func(name string) *api.TaskDiff { return &api.TaskDiff{Type: "Edited", Name: name, Fields: edit} }
	return &api.JobDiff{TaskGroups: []*api.TaskGroupDiff{
		{Type: "Edited", Name: "a", Fields: edit, Tasks: []*api.TaskDiff{task("x"), task("y")}},
		{Type: "Edited", Name: "b", Fields: edit, Tasks: []*api.TaskDiff{task("z")}},
	}}
}

func TestParseDiff(t *testing.T) {
	if d, err := ParseDiff(""); d != nil || err != nil {
		t.Errorf("ParseDiff(\"\") = %v, %v, want nil, nil", d, err)
	}
	d, err := ParseDiff(`{"Type":"Edited","ID":"web"}`)
	if err != nil || d.Type != "Edited" || d.ID != "web" {
		t.Errorf("ParseDiff = %+v, %v", d, err)
	}
	if _, err := ParseDiff("{"); err == nil {
		t.Error("ParseDiff accepted invalid JSON")
	}
}

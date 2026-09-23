package hooks

import (
	"reflect"
	"strings"
	"testing"

	"github.com/hashicorp/nomad/api"
)

func dockerTask(name, image string) *api.Task {
	return &api.Task{Name: name, Driver: "docker", Config: map[string]any{"image": image}}
}

func targetJob(id string, groups ...*api.TaskGroup) *api.Job {
	return &api.Job{ID: &id, TaskGroups: groups}
}

func group(name string, tasks ...*api.Task) *api.TaskGroup {
	return &api.TaskGroup{Name: &name, Tasks: tasks}
}

func TestBuildMeta(t *testing.T) {
	target := targetJob("api",
		group("web",
			dockerTask("api", "reg/api:2"),
			dockerTask("side-car.v2", "reg/side:1"),
			&api.Task{Name: "raw", Driver: "raw_exec", Config: map[string]any{"command": "true"}},
			&api.Task{Name: "noimage", Driver: "docker", Config: map[string]any{"image": 5}},
		),
	)
	got, err := BuildMeta("01DEP", "abc123", "pre", target)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"nops_deployment_id":     "01DEP",
		"nops_job_id":            "api",
		"nops_commit":            "abc123",
		"nops_phase":             "pre",
		"nops_image_api":         "reg/api:2",
		"nops_image_side_car_v2": "reg/side:1",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("meta = %v\nwant   %v", got, want)
	}
}

func TestBuildMetaCollisions(t *testing.T) {
	same := targetJob("api",
		group("a", dockerTask("web-x", "img:1")),
		group("b", dockerTask("web_x", "img:1")),
	)
	got, err := BuildMeta("d", "c", "post", same)
	if err != nil || got["nops_image_web_x"] != "img:1" {
		t.Errorf("same image: %v, %v", got, err)
	}

	diff := targetJob("api",
		group("a", dockerTask("web-x", "img:1")),
		group("b", dockerTask("web_x", "img:2")),
	)
	_, err = BuildMeta("d", "c", "post", diff)
	if err == nil {
		t.Fatal("different images under one key: want an error")
	}
	for _, s := range []string{"nops_image_web_x", "a/web-x", "b/web_x", "img:1", "img:2"} {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("error %q does not mention %q", err, s)
		}
	}
}

func TestBuildMetaNoTarget(t *testing.T) {
	if _, err := BuildMeta("d", "c", "pre", nil); err == nil {
		t.Error("nil target: want an error")
	}
	if _, err := BuildMeta("d", "c", "pre", &api.Job{}); err == nil {
		t.Error("target without ID: want an error")
	}
}

func parameterized(required, optional []string) *api.Job {
	return &api.Job{ParameterizedJob: &api.ParameterizedJobConfig{MetaRequired: required, MetaOptional: optional}}
}

func TestDispatchMeta(t *testing.T) {
	all := map[string]string{
		"nops_deployment_id": "d1",
		"nops_job_id":        "api",
		"nops_phase":         "pre",
		"nops_image_api":     "img",
	}
	tests := []struct {
		name    string
		parent  *api.Job
		want    map[string]string
		wantErr string
	}{
		{
			name:   "only declared keys",
			parent: parameterized([]string{"nops_deployment_id"}, []string{"nops_phase"}),
			want:   map[string]string{"nops_deployment_id": "d1", "nops_phase": "pre"},
		},
		{
			name:   "optional key nops cannot provide is skipped",
			parent: parameterized([]string{"nops_deployment_id"}, []string{"nops_image_other", "nops_image_api"}),
			want:   map[string]string{"nops_deployment_id": "d1", "nops_image_api": "img"},
		},
		{
			name:   "no declared meta",
			parent: parameterized(nil, nil),
			want:   map[string]string{},
		},
		{
			name:    "required key missing",
			parent:  parameterized([]string{"nops_deployment_id", "nops_image_web", "custom"}, nil),
			wantErr: "[custom nops_image_web]",
		},
		{
			name:    "not parameterized",
			parent:  &api.Job{},
			wantErr: "not parameterized",
		},
		{
			name:    "nil parent",
			wantErr: "not parameterized",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DispatchMeta(tc.parent, all)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("meta = %v, want %v", got, tc.want)
			}
		})
	}
}

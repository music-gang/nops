package hooks

import (
	"strings"
	"testing"

	"github.com/hashicorp/nomad/api"

	"github.com/music-gang/nops/internal/nomadx"
)

func childWithStatus(status string) *api.Job { return &api.Job{Status: &status} }

func alloc(client, desired string) nomadx.Alloc {
	return nomadx.Alloc{ID: "abcdef123456", ClientStatus: client, DesiredStatus: desired}
}

func TestEvaluate(t *testing.T) {
	failed := alloc("failed", "run")
	failed.Failure = "task t: Exit Code: 1"
	nilStatus := &api.Job{}

	tests := []struct {
		name    string
		child   *api.Job
		allocs  []nomadx.Alloc
		want    verdict
		wantMsg string
	}{
		{"nothing yet", childWithStatus("pending"), nil, verdictRunning, ""},
		{"pending allocation", childWithStatus("pending"), []nomadx.Alloc{alloc("pending", "run")}, verdictRunning, ""},
		{"running allocation", childWithStatus("running"), []nomadx.Alloc{alloc("running", "run")}, verdictRunning, ""},
		{"child without status", nilStatus, []nomadx.Alloc{alloc("complete", "run")}, verdictRunning, ""},
		{"complete but child not dead yet", childWithStatus("running"), []nomadx.Alloc{alloc("complete", "run")}, verdictRunning, ""},
		{"one group complete, another pending", childWithStatus("running"),
			[]nomadx.Alloc{alloc("complete", "run"), alloc("pending", "run")}, verdictRunning, ""},
		{"all complete", childWithStatus("dead"), []nomadx.Alloc{alloc("complete", "run")}, verdictSucceeded, ""},
		{"several complete", childWithStatus("dead"),
			[]nomadx.Alloc{alloc("complete", "run"), alloc("complete", "run")}, verdictSucceeded, ""},
		{"dead with a non-terminal allocation", childWithStatus("dead"),
			[]nomadx.Alloc{alloc("complete", "run"), alloc("running", "run")}, verdictRunning, ""},
		{"failed", childWithStatus("dead"), []nomadx.Alloc{failed}, verdictFailed, "task t: Exit Code: 1"},
		{"failed while child still running", childWithStatus("running"), []nomadx.Alloc{failed}, verdictFailed, "abcdef12"},
		{"failed without details", childWithStatus("dead"), []nomadx.Alloc{alloc("failed", "run")}, verdictFailed, "no details"},
		{"lost", childWithStatus("running"), []nomadx.Alloc{alloc("lost", "stop")}, verdictFailed, "lost"},
		{"one complete, one failed", childWithStatus("dead"),
			[]nomadx.Alloc{alloc("complete", "run"), failed}, verdictFailed, "Exit Code: 1"},
		{"stopped from outside", childWithStatus("dead"), []nomadx.Alloc{alloc("complete", "stop")}, verdictStopped, "stopped from outside"},
		{"evicted", childWithStatus("dead"), []nomadx.Alloc{alloc("complete", "evict")}, verdictStopped, "evict"},
		{"dead without any allocation", childWithStatus("dead"), nil, verdictFailed, "before any allocation"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, msg := evaluate(tc.child, tc.allocs)
			if got != tc.want {
				t.Fatalf("verdict = %d, want %d (msg %q)", got, tc.want, msg)
			}
			if !strings.Contains(msg, tc.wantMsg) {
				t.Errorf("msg = %q, want it to contain %q", msg, tc.wantMsg)
			}
		})
	}
}

func TestShortID(t *testing.T) {
	if shortID("0123456789") != "01234567" || shortID("abc") != "abc" {
		t.Error("shortID")
	}
}

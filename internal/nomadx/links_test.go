package nomadx

import "testing"

func TestUIJobURL(t *testing.T) {
	for name, tt := range map[string]struct{ ui, ns, job, want string }{
		"plain":            {"https://nomad.example.com", "default", "web", "https://nomad.example.com/ui/jobs/web@default"},
		"sub path":         {"https://infra.example.com/nomad", "default", "web", "https://infra.example.com/nomad/ui/jobs/web@default"},
		"other namespace":  {"https://nomad.example.com", "apps", "api", "https://nomad.example.com/ui/jobs/api@apps"},
		"dispatched child": {"https://nomad.example.com", "default", "hook/dispatch-1-ab", "https://nomad.example.com/ui/jobs/hook%2Fdispatch-1-ab@default"},
		"no UI address":    {"", "default", "web", ""},
	} {
		t.Run(name, func(t *testing.T) {
			if got := UIJobURL(tt.ui, tt.ns, tt.job); got != tt.want {
				t.Errorf("UIJobURL = %q, want %q", got, tt.want)
			}
		})
	}
}

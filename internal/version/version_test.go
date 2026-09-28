package version

import (
	"runtime/debug"
	"testing"
)

func TestFromBuildInfo(t *testing.T) {
	rev := debug.BuildSetting{Key: "vcs.revision", Value: "fb0c48c383418492326a63d726d7996e750575a6"}
	tests := []struct {
		name string
		info *debug.BuildInfo
		want string
	}{
		{"no build info", nil, "devel"},
		{"go install of a tag", &debug.BuildInfo{Main: debug.Module{Version: "v0.1.0"}}, "v0.1.0"},
		{"go build in a checkout", &debug.BuildInfo{Main: debug.Module{Version: "v0.0.0-20260925193735-fb0c48c38341+dirty"}},
			"v0.0.0-20260925193735-fb0c48c38341+dirty"},
		{"no module version, a commit", &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}, Settings: []debug.BuildSetting{rev}},
			"devel+fb0c48c38341"},
		{"nothing recorded", &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}}, "devel"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := fromBuildInfo(tt.info); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestStringPrefersTheLinkTimeVersion(t *testing.T) {
	old := version
	t.Cleanup(func() { version = old })
	version = "v1.2.3"
	if got := String(); got != "v1.2.3" {
		t.Errorf("got %q, want v1.2.3", got)
	}
}

// Package version reports which build of nops is running. A release sets
// the version at link time from its git tag (see docs/development.md#releasing):
//
//	go build -ldflags "-X github.com/music-gang/nops/internal/version.version=v0.1.0" ./cmd/nops
//
// A build without that flag falls back to what the Go toolchain recorded.
package version

import "runtime/debug"

// version is set by the linker (-X) for a release build; empty otherwise.
var version string

// String returns the version of this binary: the release tag set at link
// time, else the module version the Go toolchain recorded (a
// `go install ...@vX.Y.Z` gives the tag, a `go build` in a checkout a
// pseudo-version such as v0.0.0-20260925193735-fb0c48c38341, with "+dirty"
// for uncommitted changes), else "devel+<commit>", else "devel".
func String() string {
	if version != "" {
		return version
	}
	info, _ := debug.ReadBuildInfo()
	return fromBuildInfo(info)
}

// fromBuildInfo is String without the link-time version, split out so the
// fallback can be tested with a BuildInfo of its own.
func fromBuildInfo(info *debug.BuildInfo) string {
	if info == nil {
		return "devel"
	}
	if v := info.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" && s.Value != "" {
			return "devel+" + s.Value[:min(12, len(s.Value))]
		}
	}
	return "devel"
}

// Package buildinfo reports the program version.
package buildinfo

import "runtime/debug"

// Version is set at build time for releases:
//
//	go build -ldflags "-X github.com/ngh1105/Devin2API/internal/buildinfo.Version=v1.2.3"
var Version = "dev"

// String returns Version, or for development builds "dev" plus the VCS
// revision Go embedded at build time (when built from a git checkout).
func String() string {
	if Version != "dev" {
		return Version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return Version
	}
	var rev, dirty string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			if len(s.Value) > 7 {
				rev = s.Value[:7]
			}
		case "vcs.modified":
			if s.Value == "true" {
				dirty = "+dirty"
			}
		}
	}
	if rev == "" {
		return Version
	}
	return Version + " (" + rev + dirty + ")"
}

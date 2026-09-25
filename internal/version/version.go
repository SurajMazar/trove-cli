// Package version exposes build-time metadata, set via -ldflags:
//
//	-X github.com/SurajMazar/trove-cli/internal/version.Version=v1.2.3
//	-X github.com/SurajMazar/trove-cli/internal/version.Commit=abc123
//	-X github.com/SurajMazar/trove-cli/internal/version.Date=2024-01-01T00:00:00Z
package version

import (
	"runtime"
	"runtime/debug"
)

var (
	Version = "dev"
	Commit  = ""
	Date    = ""
)

// Info is the full build description.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildDate string `json:"build_date"`
	GoVersion string `json:"go_version"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
}

// Get returns build information, falling back to VCS data embedded by the Go
// toolchain when ldflags were not supplied (e.g. `go install`).
func Get() Info {
	info := Info{
		Version: Version, Commit: Commit, BuildDate: Date,
		GoVersion: runtime.Version(), OS: runtime.GOOS, Arch: runtime.GOARCH,
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		if info.Version == "dev" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			info.Version = bi.Main.Version
		}
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				if info.Commit == "" {
					info.Commit = s.Value
				}
			case "vcs.time":
				if info.BuildDate == "" {
					info.BuildDate = s.Value
				}
			}
		}
	}
	if info.Commit == "" {
		info.Commit = "unknown"
	}
	if info.BuildDate == "" {
		info.BuildDate = "unknown"
	}
	return info
}

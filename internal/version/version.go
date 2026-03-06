package version

import (
	"fmt"
	"runtime/debug"
)

// Version is set via ldflags at build time. Defaults to "dev".
var Version = "dev"

// Info returns version string with commit hash fallback for dev builds.
func Info() string {
	if Version != "dev" {
		return Version
	}

	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" && len(s.Value) >= 7 {
				return fmt.Sprintf("dev (commit: %s)", s.Value[:7])
			}
		}
	}

	return "dev"
}

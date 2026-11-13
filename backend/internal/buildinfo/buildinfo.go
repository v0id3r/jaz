package buildinfo

import "runtime/debug"

// version is set via -ldflags at build time (frontend/scripts/build-backend.ts).
// Go stamps vcs.revision natively for builds from a VCS checkout (see -buildvcs).
var (
	version = "dev"
	commit  = vcsRevision()
)

func Version() string { return version }
func Commit() string  { return commit }

func vcsRevision() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" {
				return setting.Value
			}
		}
	}
	return "unknown"
}

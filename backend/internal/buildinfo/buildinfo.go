package buildinfo

import "runtime/debug"

// version is set via -ldflags at build time (frontend/scripts/build-backend.ts).
// Native VCS stamping covers plain `go build` in regular clones; git worktrees
// carry no stamp, so the ldflag stays the primary source there.
var (
	version      = "dev"
	ldflagCommit string

	commit = resolveCommit()
)

func Version() string { return version }
func Commit() string  { return commit }

func resolveCommit() string {
	if ldflagCommit != "" {
		return ldflagCommit
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" {
				return setting.Value
			}
		}
	}
	return "unknown"
}

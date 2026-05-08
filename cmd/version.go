package cmd

import (
	"fmt"
	"io"
	"runtime"
	"runtime/debug"

	"github.com/spf13/cobra"
)

// version is the user-facing release tag (e.g. "v0.1.0"). Injected at link
// time by release.yml via `-ldflags -X 'github.com/p5n-dev/forge/cmd.version=...'`.
// Local `go build` leaves it as "dev".
var version = "dev"

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the forge version",
	Run: func(cmd *cobra.Command, args []string) {
		printVersion(cmd.OutOrStdout())
	},
}

func printVersion(w io.Writer) {
	commit, buildTime, modified := vcsInfo()

	// Errors writing to a cobra-managed writer have nowhere useful to go;
	// swallow them via a local helper so the body stays readable.
	p := func(format string, args ...any) {
		_, _ = fmt.Fprintf(w, format, args...)
	}

	p("forge:\n")
	p(" Version:    %s\n", version)
	if commit != "" {
		mod := ""
		if modified {
			mod = " (modified)"
		}
		p(" Git commit: %s%s\n", commit, mod)
	}
	if buildTime != "" {
		p(" Built:      %s\n", buildTime)
	}
	p(" Go version: %s\n", runtime.Version())
	p(" OS/Arch:    %s/%s\n", runtime.GOOS, runtime.GOARCH)
}

// vcsInfo pulls git revision, commit time, and dirty-tree flag out of the
// build info Go embeds when -buildvcs=true (default since 1.18). Returns
// empty strings when the binary was built outside a git checkout, or with
// -buildvcs=false. Commit is truncated to 12 chars to match Docker's style.
func vcsInfo() (commit, buildTime string, modified bool) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "", "", false
	}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			commit = s.Value
		case "vcs.time":
			buildTime = s.Value
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	if len(commit) > 12 {
		commit = commit[:12]
	}
	return commit, buildTime, modified
}

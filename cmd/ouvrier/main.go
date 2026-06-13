package main

import (
	"context"
	"os"
	"runtime/debug"

	"github.com/ArnaudGuiovanna/ouvrier/internal/cli"
)

// version is overridden at build time for release binaries via
//
//	-ldflags "-X main.version=v0.3.0"
//
// When it is left empty (a plain `go build`/`go install`), resolveVersion
// recovers the version from the module build info instead, so
// `go install github.com/ArnaudGuiovanna/ouvrier/cmd/ouvrier@v0.3.0` reports
// v0.3.0 and a build from a checkout reports its VCS revision.
var version = ""

func main() {
	app := cli.New(resolveVersion(version))
	if err := app.Run(context.Background(), os.Args[1:]); err != nil {
		os.Exit(1)
	}
}

// resolveVersion prefers an ldflags-injected version, then the module version
// recorded by `go install module@version`, then the VCS revision embedded in a
// checkout build, and finally "dev".
func resolveVersion(ldflagsVersion string) string {
	if ldflagsVersion != "" {
		return ldflagsVersion
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	// Set for `go install path@version` (e.g. "v0.3.0"); "(devel)" for a build
	// from a local checkout, where we fall through to the VCS revision.
	if v := info.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	var revision, modified string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			modified = s.Value
		}
	}
	if revision != "" {
		short := revision
		if len(short) > 12 {
			short = short[:12]
		}
		v := "devel-" + short
		if modified == "true" {
			v += "-dirty"
		}
		return v
	}
	return "dev"
}

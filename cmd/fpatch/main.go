// Command fpatch is an exact-match, atomic, all-or-nothing file patcher for
// agents and scripts. See the README for the spec format.
package main

import (
	"os"
	"runtime/debug"
	"strings"

	"github.com/baditaflorin/fpatch/internal/cli"
)

// version is set at build time (-ldflags "-X main.version=0.1.0"); a binary
// built with "go install ...@vX" falls back to the module version.
var version = ""

func main() {
	os.Exit(cli.Run(os.Args[1:], cli.Env{
		Stdin:           os.Stdin,
		Stdout:          os.Stdout,
		Stderr:          os.Stderr,
		StdinIsTerminal: isTerminal(os.Stdin),
		Version:         versionString(),
	}))
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// versionString is the bare version number, without a leading "v", exactly as
// it appears in release archive names.
func versionString() string {
	v := version
	if v == "" {
		if bi, ok := debug.ReadBuildInfo(); ok {
			v = bi.Main.Version
		}
	}
	v = strings.TrimPrefix(v, "v")
	if v == "" || v == "(devel)" {
		return "dev"
	}
	return v
}

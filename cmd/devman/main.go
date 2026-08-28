// Command devman is the DevMan command line.
//
// It is intentionally a thin shell: everything the CLI does lives in
// internal/cli, which talks to the daemon over the same HTTP API the GUI and the
// Codex skill use. That keeps the CLI from becoming a second implementation of
// DevMan's behaviour.
package main

import (
	"os"

	"github.com/devman-project/devman/internal/cli"
	"github.com/devman-project/devman/internal/platform"
)

// Version is set at build time with -ldflags "-X main.Version=...".
var Version = "0.1.0-dev"

func main() {
	// The console is the user's, not ours: switch it to UTF-8 for the duration
	// of the command and hand it back as it was found.
	restore := platform.UseUTF8Console()
	code := cli.Main(Version, os.Args[1:])
	restore()
	os.Exit(code)
}

// Command dsky-agent provisions a freshly imaged machine at its first sign-in:
// drivers, consumer-app removal and the operator's programs, from a manifest
// DSKY stages beside it. It replaces the scripts DSKY used to generate per
// build, which failed on the machine in ways that could not be tested before
// they were written.
package main

import (
	"os"

	"github.com/uplinkresearch/dsky/internal/agent"
)

func main() {
	// On Windows this program is linked for the GUI subsystem, so that a
	// double-clicked payload does not put a console window on the desktop for
	// the length of the run. First, then, find the console it was started
	// from, if it was started from one: this is a command line tool too, and
	// verify, scan and a quiet apply all have something to say.
	agent.UseParentConsole()
	if err := agent.Main(os.Args[1:]); err != nil {
		agent.SayFatal(err)
		os.Exit(1)
	}
}

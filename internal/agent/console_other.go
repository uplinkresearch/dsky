//go:build !windows

package agent

import (
	"fmt"
	"os"
)

// Off Windows there is nothing to attach to and nothing to hide: a program
// started from a terminal has its terminal, and one started any other way
// never had a window of its own to begin with.

// UseParentConsole does nothing here.
func UseParentConsole() {}

// SayFatal writes to standard error, which is where it has always gone.
func SayFatal(err error) { fmt.Fprintln(os.Stderr, "dsky-agent:", err) }

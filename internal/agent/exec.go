package agent

import (
	"context"
	"os/exec"
	"strings"
	"time"

	"github.com/uplinkresearch/dsky/internal/hidewin"
)

// result is what running one program produced.
type result struct {
	Code   int
	Out    string
	Err    error
	Timing time.Duration
}

// ok reports whether the program ran and returned success. It is deliberately
// never the only test of whether a step worked: a driver pack that printed its
// usage text and did nothing also returned 0.
func (r result) ok() bool { return r.Err == nil && r.Code == 0 }

// runner runs a program. It is a variable so tests can watch what the agent
// would run without a Windows machine.
var runner = runReal

// command is how the agent starts a program.
//
// The window matters as much as the deadline. The agent is linked for the GUI
// subsystem, so it has no console of its own, and Windows gives a console
// program started by a program without one a new console of its own --
// visible. Without this, a run would flash up a window for every winget
// package, every pnputil sweep and every PowerShell script, on a desktop
// somebody is sitting at. The output still comes back: only the window is
// suppressed, and elsewhere this does nothing.
func command(ctx context.Context, name string, args ...string) *exec.Cmd {
	return hidewin.Cmd(exec.CommandContext(ctx, name, args...))
}

func runReal(ctx context.Context, name string, args ...string) result {
	start := time.Now()
	cmd := command(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	r := result{Out: string(out), Timing: time.Since(start)}
	if err != nil {
		var ee *exec.ExitError
		if errorsAs(err, &ee) {
			r.Code = signedExit(ee.ExitCode())
		} else {
			r.Err = err
			r.Code = -1
		}
	}
	return r
}

// signedExit reads a Windows exit code the way the program that set it meant
// it. Windows hands back an unsigned 32-bit value, and Go passes it through,
// so winget's 0x8A150056 arrives as 2316632150 rather than -1978335146.
// Everything that documents these codes -- winget, Microsoft, PowerShell's
// own $LASTEXITCODE -- writes them signed.
//
// Watched in the VM: the agent compared the documented signed value against
// the unsigned one it was given, never recognised "this installer refuses to
// run as an administrator", and so never tried the standard-user install
// that exists for exactly that case. Spotify failed three identical attempts
// and was reported, correctly but unhelpfully, as uninstallable.
func signedExit(code int) int {
	if code > 0x7FFFFFFF {
		return code - 0x100000000
	}
	return code
}

// run executes a program with a deadline.
func run(timeout time.Duration, name string, args ...string) result {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	r := runner(ctx, name, args...)
	if ctx.Err() != nil && r.Err == nil {
		r.Err = ctx.Err()
	}
	return r
}

// trimOut shortens command output for a log line; the full text goes to the
// human log separately.
func trimOut(s string) string {
	s = strings.TrimSpace(s)
	const max = 300
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}

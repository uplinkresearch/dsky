// Package elevate relaunches the flash worker with the privileges raw
// device access needs, per platform.
package elevate

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"golang.org/x/sys/windows"

	"github.com/uplinkresearch/dsky/internal/hidewin"
)

// IsElevated reports whether this process already has administrator rights.
func IsElevated() bool {
	return windows.GetCurrentProcessToken().IsElevated()
}

// RunElevated relaunches this executable with args through UAC (one prompt),
// waits, and returns the exit code. Uses PowerShell's Start-Process -Verb
// RunAs, present on every supported Windows.
func RunElevated(args []string) (int, error) {
	exe, err := os.Executable()
	if err != nil {
		return -1, err
	}
	return RunElevatedExe(exe, args)
}

// RunElevatedExe is RunElevated for a program other than this one. The UAC
// prompt names the program being started, so what goes here decides what the
// person is asked to trust: a payload picks its own unpacked agent, which is
// the binary we sign, rather than the file it happened to arrive in, which
// nobody can sign because it is built on the operator's machine.
func RunElevatedExe(exe string, args []string) (int, error) {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = "'" + strings.ReplaceAll(a, "'", "''") + "'"
	}
	script := fmt.Sprintf(
		`$p = Start-Process -FilePath '%s' -ArgumentList @(%s) -Verb RunAs -Wait -PassThru -WindowStyle Hidden; exit $p.ExitCode`,
		strings.ReplaceAll(exe, "'", "''"), strings.Join(quoted, ","))
	cmd := hidewin.Cmd(exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script))
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), nil
	}
	if err != nil {
		return -1, fmt.Errorf("elevate: UAC relaunch failed (prompt declined?): %w", err)
	}
	return 0, nil
}

// OpensEachDisk is false: Windows elevates a worker once through UAC.
func OpensEachDisk() bool { return false }

// Hint tells the user how to elevate manually.
func Hint() string { return "approve the UAC prompt, or run from an Administrator terminal" }

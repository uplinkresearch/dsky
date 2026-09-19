package helpers

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Telling somebody to install PowerShell when PowerShell is on their PATH
// sends them round in a circle. It happened: mise had installed 7.6.6 and set
// no default version, so its shim answered every call with an error, and DSKY
// said "install it with your package manager, e.g. mise use -g powershell" to
// somebody who had already done exactly that.
//
// The shim's own complaint is the instruction — mise names the command to run
// — so the message carries it rather than replacing it with a guess.
func TestAPowerShellThatWillNotRunIsNotReportedAsMissing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has PowerShell; this is about the other two")
	}
	dir := t.TempDir()
	fake := filepath.Join(dir, "pwsh")
	// A shim with no version chosen, as mise writes it.
	script := "#!/bin/sh\n" +
		"echo 'mise ERROR No version is set for shim: pwsh' >&2\n" +
		"echo 'Set a global default version with one of the following:' >&2\n" +
		"echo 'mise use -g powershell@7.6.6' >&2\n" +
		"echo 'mise ERROR Version: 2026.9.4 linux-x64' >&2\n" +
		"exit 1\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DSKY_PWSH", fake)
	t.Setenv("PATH", dir)
	// Otherwise the real PowerShell a version manager installed under this
	// account is found, and the test proves nothing.
	t.Setenv("HOME", t.TempDir())

	_, err := powershellBinary(context.Background())
	if err == nil {
		t.Fatal("a pwsh that exits 1 was accepted as PowerShell 7")
	}
	msg := err.Error()
	t.Log("message a person sees: " + msg)
	// It must name the thing that is there...
	if !strings.Contains(msg, fake) {
		t.Errorf("the message does not say which pwsh it tried:\n%s", msg)
	}
	// ...pass on what it said, which is where the fix is...
	if !strings.Contains(msg, "No version is set for shim") {
		t.Errorf("the message drops the reason it failed:\n%s", msg)
	}
	// ...and not send somebody to install what they already have.
	if strings.Contains(msg, "install it with your package manager") {
		t.Errorf("the message still says to install it:\n%s", msg)
	}
	// The way out that needs no PowerShell at all is still offered.
	if !strings.Contains(msg, "--iso") {
		t.Errorf("the message does not offer the ISO route:\n%s", msg)
	}
}

// With no pwsh anywhere, the old message is the right one: there really is
// nothing installed, and saying how to install it is the help.
func TestNoPowerShellAtAllStillSaysHowToInstallIt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has PowerShell")
	}
	t.Setenv("DSKY_PWSH", "")
	t.Setenv("PATH", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	_, err := powershellBinary(context.Background())
	if err == nil {
		t.Fatal("PowerShell was found where none exists")
	}
	if !strings.Contains(err.Error(), "https://aka.ms/powershell") {
		t.Errorf("no way to get it:\n%s", err)
	}
	if strings.Contains(err.Error(), "does not run") {
		t.Errorf("it blamed a pwsh that is not there:\n%s", err)
	}
}

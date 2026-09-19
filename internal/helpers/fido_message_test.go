package helpers

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
)

// Telling somebody to install PowerShell when PowerShell is on their PATH
// sends them round in a circle. It happened: mise had installed 7.6.6 and set
// no default version, so its shim answered every call with an error, and DSKY
// said "install it with your package manager, e.g. mise use -g powershell" to
// somebody who had already done exactly that.
//
// This reads the message rather than the search. The search depends on what is
// installed on the machine running the test — the first version of this test
// passed here and failed on two CI runners, because both have PowerShell in a
// place no environment variable can hide.
func TestTheMessageTellsMissingApartFromBroken(t *testing.T) {
	broken := noPowerShellError("/home/someone/.local/share/mise/shims/pwsh",
		"No version is set for shim: pwsh").Error()

	// It names the thing that is there...
	if !strings.Contains(broken, "mise/shims/pwsh") {
		t.Errorf("the message does not say which pwsh it tried:\n%s", broken)
	}
	// ...passes on what that thing said, which is where the fix is...
	if !strings.Contains(broken, "No version is set for shim") {
		t.Errorf("the message drops the reason it failed:\n%s", broken)
	}
	// ...and does not send somebody to install what they already have.
	if strings.Contains(broken, "install it with your package manager") {
		t.Errorf("the message still says to install it:\n%s", broken)
	}

	// With nothing anywhere, saying how to install it IS the help.
	missing := noPowerShellError("", "").Error()
	if !strings.Contains(missing, "https://aka.ms/powershell") {
		t.Errorf("no way to get it:\n%s", missing)
	}
	if strings.Contains(missing, "does not run") {
		t.Errorf("it blamed a pwsh that is not there:\n%s", missing)
	}

	// Both offer the way out that needs no PowerShell at all.
	for _, m := range []string{broken, missing} {
		if !strings.Contains(m, "--iso") {
			t.Errorf("the message does not offer the ISO route:\n%s", m)
		}
	}
}

// The failing tool's own words are the instruction, so the useful line has to
// survive and the noise around it has to go. This is mise's actual output.
func TestTheReasonIsTakenFromWhateverTheToolSaid(t *testing.T) {
	mise := "mise ERROR No version is set for shim: pwsh\n" +
		"Set a global default version with one of the following:\n" +
		"mise use -g powershell@7.6.6\n" +
		"mise ERROR Version: 2026.9.4 linux-x64 (2026-09-09)\n" +
		"mise ERROR Run with --verbose or MISE_VERBOSE=1 for more information\n"
	got := whyItFailed(&exec.ExitError{Stderr: []byte(mise)}, "")
	if got != "No version is set for shim: pwsh" {
		t.Errorf("first useful line = %q", got)
	}

	// Some tools say nothing on stderr and print to stdout instead.
	if got := whyItFailed(errors.New("exit status 1"), "pwsh: command not found\n"); got != "pwsh: command not found" {
		t.Errorf("stdout fallback = %q", got)
	}
	// Nothing to pass on is fine; the message simply leaves it out.
	if got := whyItFailed(errors.New("exit status 1"), ""); got != "" {
		t.Errorf("empty = %q", got)
	}
	// A wall of text is trimmed rather than pasted into a terminal message.
	long := strings.Repeat("x", 400)
	if got := whyItFailed(&exec.ExitError{Stderr: []byte(long)}, ""); len(got) > 130 {
		t.Errorf("a %d-character line reached the message", len(got))
	}
}

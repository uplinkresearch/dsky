package agent

import (
	"os"
	"testing"
)

// The agent's tests run on the machine that runs them, and on Windows much of
// the agent is real there: it opens windows, sweeps desktops, registers
// scheduled tasks and writes to Winlogon. None of that may happen to a CI
// runner or a developer's own machine because somebody ran `go test`.
//
// It did. On the Windows runner Apply opened a full-screen status window and
// waited for Finish until the ten-minute test timeout, and a second run
// counted the resume task's schtasks calls as work it should not have done.
// Neither showed up on Linux or macOS, where those calls do nothing -- the
// tests passed on exactly the platforms where the code under test is a no-op.
// Worse, the desktop sweep would have deleted the shortcuts of whoever ran the
// tests on Windows.
//
// So every test starts with stand-ins for all of it. A test that wants to
// watch one of these calls installs its own fake over the stand-in, and puts
// the stand-in back when it is done.
func TestMain(m *testing.M) {
	openScreenFn = func(string, bool) *screen { return nil }
	desktopDirsFn = func() []string { return nil }
	ensureResumeFn = func(*Agent) error { return nil }
	clearResumeFn = func(*Agent) {}
	disarmAutoLogonFn = func(*Agent) {}
	restartFn = func(*Agent, string) error { return nil }
	keepAwakeFn = func() func() { return func() {} }
	isElevatedFn = func() bool { return true }
	exitFn = func(int) {}
	verifySignatureFn = func(string) (string, string, error) { return "NotSigned", "", nil }
	os.Exit(m.Run())
}

// If a future change calls the real thing directly instead of through these,
// this is where it will be noticed first: on the machine running the tests,
// nothing real is reachable.
func TestTestsNeverTouchTheRealMachine(t *testing.T) {
	if openScreenFn("test", true) != nil {
		t.Error("the tests can open a real status window")
	}
	if dirs := desktopDirsFn(); len(dirs) != 0 {
		t.Errorf("the tests would sweep real desktops: %v", dirs)
	}
}

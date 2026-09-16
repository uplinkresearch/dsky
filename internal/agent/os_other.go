//go:build !windows

package agent

import (
	"errors"
	"os"
	"path/filepath"
)

// The agent only ever runs on Windows. These stubs exist so the package
// builds and its logic can be tested on the machine DSKY is developed on.

func machineModel() (vendor, model string) { return "", "" }

// ensureResume, clearResume and restart carry the run across a restart on
// Windows. Here they do nothing and report success, so the run's shape can be
// tested without a Windows machine.
func (a *Agent) ensureResume() error { return nil }

func (a *Agent) clearResume() {}

func (a *Agent) disarmAutoLogon() {}

func (a *Agent) restart(reason string) error { return nil }

func (a *Agent) runAsSignedInUser(exe string, args []string) (int, error) {
	return 0, errors.New("standard-user installs are a Windows feature")
}

func (a *Agent) setPolicy(p policy) error {
	return errors.New("the registry is a Windows feature")
}

func (a *Agent) removeAppx(prefixes []string) {
	a.J.Info(stepDebloat, "not running on Windows, nothing to remove")
}

// desktopDirs finds nothing to tidy anywhere but Windows.
func desktopDirs() []string { return nil }

// keepAwake has nothing to hold off anywhere but Windows.
func keepAwake() (release func()) { return func() {} }

// verifySignature is Windows' to answer.
func verifySignature(file string) (status, subject string, err error) {
	return "", "", errors.New("Authenticode signatures are a Windows feature")
}

// isElevated is root, where root exists; the agent only provisions Windows.
func isElevated() bool { return os.Geteuid() == 0 }

// payloadRoot keeps payloads in the temporary directory off Windows.
func payloadRoot() string { return filepath.Join(os.TempDir(), "dsky-payloads") }

package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stageInstaller writes a file onto a stand-in for the stick and returns what
// it hashes to, which is what the build would have recorded for it.
func stageInstaller(t *testing.T, dir, name, contents string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(contents))
	return hex.EncodeToString(sum[:])
}

// installerRun records what the agent would have handed to Windows, so a test
// can tell "refused" from "ran and failed" -- which the log alone cannot,
// because a refusal and a bad exit code both end as a failure in the record.
func installerRun(t *testing.T, dir string, in Installer) (ran bool, log string) {
	t.Helper()
	j, err := OpenJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	swap(t, &runner, func(_ context.Context, name string, args ...string) result {
		ran = true
		return result{Code: 0}
	})
	a := &Agent{Dir: dir, Manifest: &Manifest{Recipe: "bench"}, J: j}
	a.runInstaller(in)
	j.Close()
	b, err := os.ReadFile(filepath.Join(dir, LogName))
	if err != nil {
		t.Fatal(err)
	}
	return ran, string(b)
}

// The gap this closes: between the build finishing and first boot running,
// the file at a given name on the stick is whatever is at that name on the
// stick. Nothing downstream noticed a substitution, and Verify reported the
// install as a success because it looked for the filename in the log.
func TestAnInstallerThatIsNotWhatTheBuildStagedIsNotRun(t *testing.T) {
	dir := t.TempDir()
	stageInstaller(t, dir, "agent.msi", "something else entirely")
	built := sha256.Sum256([]byte("the installer the operator added"))

	ran, log := installerRun(t, dir, Installer{
		File: "agent.msi", MSI: true, SHA256: hex.EncodeToString(built[:]),
	})
	if ran {
		t.Fatal("the agent handed a substituted installer to Windows as Administrator")
	}
	if !strings.Contains(log, "not installing it") {
		t.Errorf("the log does not say the file was refused:\n%s", log)
	}
	if !strings.Contains(log, "rebuild the stick") {
		t.Errorf("the log does not tell the technician what to do about it:\n%s", log)
	}
}

// The ordinary case, which has to keep working: the file is the file.
func TestAnInstallerTheBuildStagedRuns(t *testing.T) {
	dir := t.TempDir()
	sum := stageInstaller(t, dir, "agent.msi", "the installer the operator added")

	ran, log := installerRun(t, dir, Installer{File: "agent.msi", MSI: true, SHA256: sum})
	if !ran {
		t.Fatalf("an untouched installer was refused:\n%s", log)
	}
	if !strings.Contains(log, "installed agent.msi") {
		t.Errorf("the install was not recorded:\n%s", log)
	}
}

// Media built before the manifest carried hashes has none to check. Those
// sticks are in vans and drawers now, and refusing them over a check their
// build never made would strand machines that have always worked. It runs,
// and the log says it was not checked, which is the honest line.
func TestOlderMediaWithNoRecordedHashStillInstalls(t *testing.T) {
	dir := t.TempDir()
	stageInstaller(t, dir, "agent.msi", "the installer the operator added")

	ran, log := installerRun(t, dir, Installer{File: "agent.msi", MSI: true})
	if !ran {
		t.Fatalf("media built before hashes existed was refused:\n%s", log)
	}
	if !strings.Contains(log, "run unchecked") {
		t.Errorf("the log does not admit the file was not checked:\n%s", log)
	}
}

// A refusal has to read as a refusal in the report somebody runs at the
// machine, not as one more program that did not install.
func TestVerifySaysWhenAnInstallerWasRefused(t *testing.T) {
	dir := t.TempDir()
	stageInstaller(t, dir, "agent.msi", "something else entirely")
	built := sha256.Sum256([]byte("the installer the operator added"))
	in := Installer{File: "agent.msi", MSI: true, SHA256: hex.EncodeToString(built[:])}
	installerRun(t, dir, in)

	m := &Manifest{Version: ManifestVersion, Recipe: "bench", Steps: []string{"apps"},
		Apps: &Apps{Installers: []Installer{in}}}
	if err := m.Save(filepath.Join(dir, ManifestName)); err != nil {
		t.Fatal(err)
	}
	checks, err := Verify(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range checks {
		if c.What == "installer agent.msi" {
			if c.OK {
				t.Error("a refused installer is reported as installed")
			}
			if !strings.Contains(c.Note, "not the one the build staged") {
				t.Errorf("the report does not say why: %q", c.Note)
			}
			return
		}
	}
	t.Error("the report says nothing about the installer at all")
}

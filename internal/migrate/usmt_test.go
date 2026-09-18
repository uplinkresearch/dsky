package migrate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeUSMT is a USMT that records what it would have run. The real tools come
// from the Windows ADK and are not on a build machine, but the arguments are
// the part worth testing: they decide whose files move and whether the store
// is readable by anybody who finds it.
func fakeUSMT(t *testing.T) (*USMT, *[]string) {
	t.Helper()
	dir := t.TempDir()
	for _, f := range []string{"scanstate.exe", "loadstate.exe", "MigApp.xml", "MigDocs.xml"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	u, err := NewUSMT(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	u.run = func(_ context.Context, name string, args ...string) (int, string, error) {
		got = append([]string{filepath.Base(name)}, args...)
		return 0, "", nil
	}
	return u, &got
}

func usmtPlan() *Manifest {
	return &Manifest{
		Source: Source{Hostname: "PC01"},
		Data: Data{
			Strategy: DataUSMT, StorePath: `\\fs01\migration$\PC01`,
			Users: []string{`LAB\reception`, `LAB\hygienist`},
		},
	}
}

// Everybody is excluded and then the plan's people are named. Copying a
// profile nobody asked for is a privacy problem, not a bonus.
func TestCaptureCopiesOnlyThePeopleThePlanNames(t *testing.T) {
	u, got := fakeUSMT(t)
	if err := u.Capture(context.Background(), usmtPlan(), "", os.Stderr); err != nil {
		t.Fatal(err)
	}
	line := strings.Join(*got, " ")
	if !strings.HasPrefix(line, "scanstate.exe") {
		t.Fatalf("ran the wrong tool: %s", line)
	}
	if !strings.Contains(line, `\\fs01\migration$\PC01`) {
		t.Errorf("no store: %s", line)
	}
	if !strings.Contains(line, "/ue:*") {
		t.Error("everybody was not excluded first, so a profile nobody asked for could be copied")
	}
	for _, who := range []string{`/ui:LAB\reception`, `/ui:LAB\hygienist`} {
		if !strings.Contains(line, who) {
			t.Errorf("%s is in the plan and not on the command line: %s", who, line)
		}
	}
	for _, x := range []string{"MigApp.xml", "MigDocs.xml"} {
		if !strings.Contains(line, x) {
			t.Errorf("%s was not included: %s", x, line)
		}
	}
	// Restoring does not re-state who: the store holds only them already.
	u2, got2 := fakeUSMT(t)
	if err := u2.Restore(context.Background(), usmtPlan(), "", os.Stderr); err != nil {
		t.Fatal(err)
	}
	if line := strings.Join(*got2, " "); !strings.HasPrefix(line, "loadstate.exe") || strings.Contains(line, "/ue:") {
		t.Errorf("restore: %s", line)
	}
}

// The key never appears on a command line. Anybody on the machine can read one
// out of the process list, and this one unlocks every document the old PC had.
func TestTheStoreKeyIsNeverOnACommandLine(t *testing.T) {
	m := usmtPlan()
	m.Data.Encrypted = true
	u, got := fakeUSMT(t)
	if err := u.Capture(context.Background(), m, "correct horse battery staple", os.Stderr); err != nil {
		t.Fatal(err)
	}
	line := strings.Join(*got, " ")
	if strings.Contains(line, "correct horse") {
		t.Fatalf("the key is on the command line: %s", line)
	}
	if !strings.Contains(line, "/encrypt") || !strings.Contains(line, "/keyfile:") {
		t.Errorf("the store was not encrypted: %s", line)
	}
	// And the key file does not outlive the run.
	var keyfile string
	for _, a := range *got {
		if strings.HasPrefix(a, "/keyfile:") {
			keyfile = strings.TrimPrefix(a, "/keyfile:")
		}
	}
	if keyfile == "" {
		t.Fatal("no key file")
	}
	if _, err := os.Stat(keyfile); err == nil {
		t.Errorf("the key file is still on disk at %s", keyfile)
	}
}

// An encrypted store with no key stops before anything runs, rather than
// producing a store nobody can open.
func TestAnEncryptedStoreWithoutItsKeyStops(t *testing.T) {
	m := usmtPlan()
	m.Data.Encrypted = true
	u, got := fakeUSMT(t)
	err := u.Capture(context.Background(), m, "", os.Stderr)
	if err == nil || !strings.Contains(err.Error(), "key") {
		t.Fatalf("err = %v", err)
	}
	if len(*got) != 0 {
		t.Errorf("it ran anyway: %v", *got)
	}
}

// USMT is not shipped, so being told where it is comes before everything else
// -- including before an old PC is wiped on the assumption its files moved.
func TestUSMTHasToBeSuppliedAndIsNamedWhenItIsNot(t *testing.T) {
	if _, err := NewUSMT(""); err == nil || !strings.Contains(err.Error(), "ADK") {
		t.Errorf("no --usmt: %v", err)
	}
	if _, err := NewUSMT(t.TempDir()); err == nil || !strings.Contains(err.Error(), "scanstate.exe") {
		t.Errorf("empty folder: %v", err)
	}
}

// A plan that moves files another way is not quietly run through USMT.
func TestAPlanThatDoesNotUseUSMTIsRefused(t *testing.T) {
	m := usmtPlan()
	m.Data.Strategy = DataKFM
	u, _ := fakeUSMT(t)
	if err := u.Capture(context.Background(), m, "", os.Stderr); err == nil {
		t.Error("a OneDrive plan was captured with USMT")
	}
}

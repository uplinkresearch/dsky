package workspace

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func scaffolded(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "ws")
	if err := Scaffold(dir, "Test Org"); err != nil {
		t.Fatal(err)
	}
	return dir
}

func read(t *testing.T, dir string, parts ...string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(append([]string{dir}, parts...)...))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestUnattendTemplateIsASCII: the answer file is parsed by Windows Setup in
// WinPE, on hardware and in locales nobody here controls. It was pure ASCII
// before domain join was added, there is no reason for it not to stay that
// way, and a stray character in a comment is an entirely self-inflicted risk.
func TestUnattendTemplateIsASCII(t *testing.T) {
	b := read(t, scaffolded(t), "templates", "autounattend.xml.tmpl")
	for i := 0; i < len(b); i++ {
		if b[i] > 127 {
			lo, hi := max(0, i-50), min(len(b), i+50)
			t.Fatalf("byte %d is non-ASCII (0x%02X) — near: %q", i, b[i], b[lo:hi])
		}
	}
}

// TestScaffoldRefusesToOverwrite: init creates fresh workspaces. Clobbering
// somebody's recipes and their gitignored secrets file would be unrecoverable.
func TestScaffoldRefusesToOverwrite(t *testing.T) {
	dir := scaffolded(t)
	err := Scaffold(dir, "Test Org")
	if err == nil {
		t.Fatal("scaffolding over an existing workspace was allowed")
	}
	if !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Errorf("unhelpful refusal: %v", err)
	}
}

// TestScaffoldKeepsSecretsOutOfGit: vars.local.yaml holds the domain-join
// password and the autoinstall hash. It is only safe to tell people to put
// secrets there because it is gitignored from the start.
func TestScaffoldKeepsSecretsOutOfGit(t *testing.T) {
	dir := scaffolded(t)
	if got := read(t, dir, ".gitignore"); !strings.Contains(got, "vars.local.yaml") {
		t.Errorf("vars.local.yaml is not gitignored:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "vars.local.yaml")); err != nil {
		t.Errorf("no vars.local.yaml to put secrets in: %v", err)
	}
}

// TestScaffoldedUnattendHasBothJoinPaths: a workspace created today must be
// able to do a domain join. A template without these blocks silently ignores
// windows.domain — which is exactly what the build-time check exists to catch,
// and what a fresh workspace must never trip.
func TestScaffoldedUnattendHasBothJoinPaths(t *testing.T) {
	got := read(t, scaffolded(t), "templates", "autounattend.xml.tmpl")
	for _, want := range []string{
		`name="Microsoft-Windows-UnattendedJoin"`,
		"<JoinDomain>", "<AccountData>",
		`{{if eq .Vars.domain_mode "offline"}}`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("a fresh workspace cannot do a domain join: missing %s", want)
		}
	}
	// The join belongs to specialize; the same element in offlineServicing is
	// a differently-named thing and putting it in the wrong pass does nothing.
	spec := got[strings.Index(got, `<settings pass="specialize">`):]
	spec = spec[:strings.Index(spec, "</settings>")]
	if !strings.Contains(spec, "Microsoft-Windows-UnattendedJoin") {
		t.Error("the join component is not in the specialize pass")
	}
}

// vars.local.yaml is the one file in a workspace whose purpose is holding
// secrets — the domain password, the administrator's, the Wi-Fi passphrase.
// It was written 0644 like the README, so on any machine with more than one
// account every one of them could read it. Gitignored keeps it out of a
// repository and does nothing about the machine it sits on.
func TestTheSecretsFileIsNotWorldReadable(t *testing.T) {
	// Windows does not decide who can read a file from the mode bits; it
	// uses the folder's access rules, and Go reports 0666 for anything
	// writable there whatever it was created with. So there is nothing for
	// this to check on Windows -- which is the same reason the fix itself
	// only means anything on Linux and macOS. Checking it anyway is testing
	// the machine the test runs on rather than the thing being tested, and
	// it turned CI red on one of three runners for a file that was written
	// exactly as asked.
	if runtime.GOOS == "windows" {
		t.Skip("file modes do not control access on Windows; the folder's rules do")
	}
	dir := t.TempDir()
	if err := Scaffold(dir, "Test Org"); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(dir, SecretsFile))
	if err != nil {
		t.Fatal(err)
	}
	if mode := st.Mode().Perm(); mode != 0o600 {
		t.Errorf("%s is mode %04o, want 0600 — anybody else on this machine can read the passwords", SecretsFile, mode)
	}
	// Everything else is meant to be read, committed and shared, and stays
	// as it was: a workspace somebody clones should not arrive with a
	// README nobody else can open.
	for _, rel := range []string{"workspace.yaml", "README.md", "recipes/example-win11.yaml"} {
		st, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		if mode := st.Mode().Perm(); mode != 0o644 {
			t.Errorf("%s is mode %04o, want 0644", rel, mode)
		}
	}
}

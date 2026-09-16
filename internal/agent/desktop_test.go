package agent

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// Shortcuts go; everything else on the desktop stays. Somebody's own file
// saved to the desktop is not ours to delete, and a machine that eats a
// document is a much worse machine than one with a Chrome icon on it.
func TestOnlyShortcutsAreSweptOff(t *testing.T) {
	dir := t.TempDir()
	keep := []string{"Budget.xlsx", "notes.txt", "photo.png", "shortcuts.txt"}
	gone := []string{"Google Chrome.lnk", "Zoom.LNK", "Acrobat Reader.url", "Teams.website"}
	for _, n := range append(append([]string{}, keep...), gone...) {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A folder called something.lnk is still a folder.
	if err := os.Mkdir(filepath.Join(dir, "Projects.lnk"), 0o755); err != nil {
		t.Fatal(err)
	}

	removed, stuck := removeShortcuts([]string{dir})
	if removed != len(gone) {
		t.Errorf("removed %d shortcut(s), want %d", removed, len(gone))
	}
	if len(stuck) != 0 {
		t.Errorf("could not remove: %v", stuck)
	}

	left, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range left {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	want := append(append([]string{}, keep...), "Projects.lnk")
	sort.Strings(want)
	if len(names) != len(want) {
		t.Fatalf("left on the desktop: %v, want %v", names, want)
	}
	for i := range names {
		if names[i] != want[i] {
			t.Errorf("left on the desktop: %v, want %v", names, want)
			break
		}
	}
}

// Every desktop on the machine, including ones that are not there.
func TestSweepsEveryDesktopItIsGiven(t *testing.T) {
	root := t.TempDir()
	public := filepath.Join(root, "Public", "Desktop")
	user := filepath.Join(root, "user", "Desktop")
	def := filepath.Join(root, "Default", "Desktop")
	for _, d := range []string{public, user, def} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "App.lnk"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	missing := filepath.Join(root, "nobody", "Desktop")

	removed, stuck := removeShortcuts([]string{public, user, def, missing})
	if removed != 3 {
		t.Errorf("removed %d, want one from each of the three desktops", removed)
	}
	if len(stuck) != 0 {
		t.Errorf("could not remove: %v", stuck)
	}
	// The Default profile matters most: a shortcut left there is copied to
	// the desktop of every account made later.
	if _, err := os.Stat(filepath.Join(def, "App.lnk")); !os.IsNotExist(err) {
		t.Error("the Default profile's desktop was not swept")
	}
}

// A machine with nothing on its desktops says nothing about it.
func TestNothingToSweepIsSilent(t *testing.T) {
	dir := t.TempDir()
	a, _ := newAgent(t, &Manifest{Version: ManifestVersion})
	removed, stuck := removeShortcuts([]string{dir})
	if removed != 0 || len(stuck) != 0 {
		t.Fatalf("removed %d, stuck %v", removed, stuck)
	}
	a.tidyDesktop() // no desktops in tests (see TestMain); must not panic or log noise
}

// Edge is not allowed to put its icon back the next time it updates itself.
func TestEdgeIsToldNotToPutItsIconBack(t *testing.T) {
	var found bool
	for _, p := range policiesFor("standard") {
		if p.Name == "CreateDesktopShortcut" && p.DWord == 0 {
			found = true
		}
	}
	if !found {
		t.Error("nothing stops Edge recreating its desktop shortcut at its next update")
	}
}

// A setting Windows always refuses is not a problem worth reporting to the
// person at the machine; it is a line that should not be in the list.
func TestNoSettingWindowsAlwaysRefuses(t *testing.T) {
	for _, preset := range []string{"standard", "aggressive"} {
		for _, p := range policiesFor(preset) {
			if p.Name == "TaskbarDa" {
				t.Errorf("%s still sets TaskbarDa, which 24H2 refuses to everybody", preset)
			}
		}
	}
}

// Spotify installs as the signed-in user and puts its icon on the desktop
// after it has exited, so a machine swept twice still had one. The desktop is
// kept clear while the finish screen is up.
func TestAnIconAddedAfterTheSweepIsStillRemoved(t *testing.T) {
	dir := t.TempDir()
	prev := desktopDirsFn
	desktopDirsFn = func() []string { return []string{dir} }
	t.Cleanup(func() { desktopDirsFn = prev })

	prevEvery := desktopSweepEvery
	desktopSweepEvery = 50 * time.Millisecond
	t.Cleanup(func() { desktopSweepEvery = prevEvery })

	a, logDir := newAgent(t, &Manifest{Version: ManifestVersion})
	done := make(chan struct{})
	go a.keepDesktopClear(done)

	// The installer drops its icon a moment after everything else finished.
	late := filepath.Join(dir, "Spotify.lnk")
	if err := os.WriteFile(late, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 300; i++ {
		if _, err := os.Stat(late); os.IsNotExist(err) {
			close(done)
			if !strings.Contains(logText(t, logDir), "after it finished") {
				t.Error("the late removal is not in the log")
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	close(done)
	t.Fatal("an icon added after the last sweep was left on the desktop")
}

// The machine says what a refused installer actually is, rather than leaving
// somebody to look up 1620.
func TestARefusedInstallerIsExplained(t *testing.T) {
	dir := t.TempDir()
	for _, c := range []struct{ name, head, want string }{
		{"prog.msi", "MZ\x90\x00", "Windows program"},
		{"zipped.msi", "PK\x03\x04", "zip file"},
		{"page.msi", "<!DOCTYPE html>", "web page"},
		{"empty.msi", "", "empty"},
	} {
		p := filepath.Join(dir, c.name)
		if err := os.WriteFile(p, []byte(c.head), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := installerLooksLike(p); !strings.Contains(got, c.want) {
			t.Errorf("%s: %q does not mention %q", c.name, got, c.want)
		}
	}
}

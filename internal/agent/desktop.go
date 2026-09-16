package agent

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// No desktop shortcuts, ever.
//
// Installers scatter them: Chrome, Reader, Zoom, Teams and every vendor
// utility drop an icon on the desktop, and some drop one on the Default
// profile's desktop so that every account made later gets it too. A machine
// handed over by DSKY has a clean desktop, whatever the installers wanted.
//
// This is a sweep rather than a set of switches. Every installer spells "no
// shortcut please" differently, many ignore it, and winget passes nothing
// through reliably -- so the shortcuts are taken off afterwards, which works
// the same for a winget package, an operator's own MSI and a driver bundle's
// leftovers.

const stepDesktop = "desktop"

// shortcutExt is what counts as a shortcut. Anything else on the desktop was
// put there by a person and is left alone.
func shortcutExt(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".lnk", ".url", ".website":
		return true
	}
	return false
}

// removeShortcuts clears every shortcut out of dirs, and says what it could
// not remove. A directory that is not there is not a problem: not every
// machine has every profile.
func removeShortcuts(dirs []string, keep map[string]bool) (removed int, stuck []string) {
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !shortcutExt(e.Name()) {
				continue
			}
			p := filepath.Join(dir, e.Name())
			if keep[strings.ToLower(p)] {
				continue // the machine owner's, not ours
			}
			if err := os.Remove(p); err != nil {
				stuck = append(stuck, e.Name())
				continue
			}
			removed++
		}
	}
	return removed, stuck
}

// desktopDirsFn finds the desktops to sweep, as a variable so the package's
// tests sweep their own temporary folders. The real one, run by `go test` on a
// Windows machine, would delete that developer's own shortcuts.
var desktopDirsFn = desktopDirs

// tidyDesktop is run after the programs go in, and again at the end of the
// whole run: an install that finishes in the signed-in user's session can put
// an icon there after the step that started it has been recorded as done.
func (a *Agent) tidyDesktop() {
	dirs := desktopDirsFn()
	if len(dirs) == 0 {
		return
	}
	removed, stuck := removeShortcuts(dirs, a.ownShortcuts)
	if removed > 0 {
		a.J.Info(stepDesktop, "removed %d desktop shortcut(s)", removed)
		refreshDesktop(dirs)
	}
	if len(stuck) > 0 {
		a.J.Fail(stepDesktop, "could not remove %d desktop shortcut(s): %s",
			len(stuck), strings.Join(stuck, ", "))
	}
}

// desktopSweepEvery is how often the desktop is swept while the finish screen
// is up. A variable so the tests do not wait ten seconds to watch it work.
var desktopSweepEvery = 10 * time.Second

// keepDesktopClear sweeps every few seconds until done is closed.
//
// Some installers put their icon there after they have exited. Spotify -- which
// installs as the signed-in user, because it refuses an administrator -- left
// one on a real machine that had been swept twice already. So the desktop is
// kept clear while the finish screen is up, which costs nothing: the work is
// over and the agent is only waiting for somebody to press a button.
func (a *Agent) keepDesktopClear(done <-chan struct{}) {
	if len(desktopDirsFn()) == 0 {
		return
	}
	t := time.NewTicker(desktopSweepEvery)
	defer t.Stop()
	deadline := time.After(15 * time.Minute)
	for {
		select {
		case <-done:
			return
		case <-deadline:
			return
		case <-t.C:
			if dirs := desktopDirsFn(); true {
				if removed, _ := removeShortcuts(dirs, a.ownShortcuts); removed > 0 {
					a.J.Info(stepDesktop, "removed %d desktop shortcut(s) an installer added after it finished", removed)
					refreshDesktop(dirs)
				}
			}
		}
	}
}

// currentShortcuts lists every shortcut on every desktop, as full paths.
func currentShortcuts() []string {
	var out []string
	for _, dir := range desktopDirsFn() {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() && shortcutExt(e.Name()) {
				out = append(out, filepath.Join(dir, e.Name()))
			}
		}
	}
	return out
}

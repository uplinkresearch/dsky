package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Check is one thing the build promised, and what the machine actually shows.
type Check struct {
	What string
	OK   bool
	Note string
}

// Verify compares the machine against the manifest that built it and returns
// what it found. It reads and reports; it changes nothing.
//
// This is the cheapest tool in the set. Every failure in the first watched
// install would have shown here in seconds: no driver files unpacked, a
// program list that never ran, a first-boot log that stopped early.
func Verify(dir string) ([]Check, error) {
	m, err := LoadManifest(filepath.Join(dir, ManifestName))
	if err != nil {
		return nil, err
	}
	var out []Check
	add := func(what string, ok bool, note string) {
		out = append(out, Check{What: what, OK: ok, Note: note})
	}

	// Did first boot run at all, and did it reach the end?
	logPath := filepath.Join(dir, LogName)
	logBytes, logErr := os.ReadFile(logPath)
	log := string(logBytes)
	switch {
	case logErr != nil:
		add("first boot ran", false, "there is no "+LogName+" on this machine")
	case strings.Contains(log, "agent done"):
		// "first-boot agent done" in logs written before payloads existed.
		add("first boot ran", true, "finished")
	default:
		add("first boot ran", false, "the log stops before the end; it may still be running")
	}

	// Each step the manifest asked for should be recorded as finished.
	state := LoadState(dir)
	for _, step := range m.Steps {
		add("step "+step, state.Finished(step), "")
	}

	// Drivers: the machine should hold driver files, and pnputil should have
	// taken them. A pack that unpacked nothing is the failure that started
	// all of this.
	driversDir := filepath.Join(dir, "Drivers")
	for _, ex := range m.Drivers.Extracts {
		n := countINF(filepath.Join(driversDir, ex.Dir))
		add("driver pack "+ex.File, n > 0, fmt.Sprintf("%d driver file(s) unpacked", n))
	}
	for _, c := range m.Drivers.Cabs {
		n := countINF(filepath.Join(driversDir, c.Dir))
		add("driver pack "+c.File, n > 0, fmt.Sprintf("%d driver file(s) unpacked", n))
	}
	if m.Drivers.Sweep {
		installed := strings.Contains(log, "pnputil added")
		add("drivers installed", installed, firstLineContaining(log, "pnputil added"))
	}

	// Programs: the log names each one as it lands, so the check does not
	// have to guess at install locations that differ per package.
	if m.Apps != nil {
		for _, id := range m.Apps.Winget {
			ok := strings.Contains(log, "installed "+id) || strings.Contains(log, id+" is already present")
			note := ""
			if !ok {
				note = "not recorded as installed"
			}
			add("program "+id, ok, note)
		}
		for _, in := range m.Apps.Installers {
			// An installer the agent refused reads as "not installed" here,
			// which is true but not the useful sentence: somebody looking at
			// this wants to know the file on the media was not the file the
			// build staged, because that is a stick to destroy rather than a
			// program to install again.
			note := ""
			if strings.Contains(log, in.File+refusedPhrase) {
				note = "refused: the file on the media is not the one the build staged"
			}
			add("installer "+in.File, strings.Contains(log, "installed "+in.File), note)
		}
	}

	if m.Debloat != nil {
		add("consumer apps removed", strings.Contains(log, "consumer app(s)"),
			firstLineContaining(log, "consumer app(s)"))
	}

	// The desktop, read now rather than from the log: shortcuts are what
	// installers do behind the agent's back, so this is worth checking
	// against the machine as it stands, whenever somebody asks.
	if dirs := desktopDirsFn(); len(dirs) > 0 {
		var left []string
		for _, d := range dirs {
			entries, err := os.ReadDir(d)
			if err != nil {
				continue
			}
			for _, e := range entries {
				if !e.IsDir() && shortcutExt(e.Name()) {
					left = append(left, e.Name())
				}
			}
		}
		note := "no shortcuts on any desktop"
		if len(left) > 0 {
			note = strings.Join(left, ", ")
		}
		add("desktop is clear", len(left) == 0, note)
	}
	return out, nil
}

func firstLineContaining(s, want string) string {
	for _, ln := range strings.Split(s, "\n") {
		if strings.Contains(ln, want) {
			return strings.TrimSpace(ln)
		}
	}
	return ""
}

// Report prints the checks the way somebody standing at the machine wants to
// read them, and reports whether everything matched.
func Report(checks []Check, w *os.File) bool {
	allOK := true
	for _, c := range checks {
		mark := " ok "
		if !c.OK {
			mark = "FAIL"
			allOK = false
		}
		line := fmt.Sprintf("  [%s] %s", mark, c.What)
		if c.Note != "" {
			line += " - " + c.Note
		}
		fmt.Fprintln(w, line)
	}
	return allOK
}

package oscatalog

import (
	"strings"
	"testing"
)

// Walk the whole catalog rather than the entries somebody remembered. Each of
// the three defects this type exists to prevent was a missing branch, and a
// missing branch is invisible to a test that only asks about the entries it
// already knows.
func TestEveryEntryDescribesItself(t *testing.T) {
	for _, e := range Builtin() {
		for _, opts := range []Options{
			{},
			{Apps: []string{"vlc"}},
			{Apps: []string{"docker", "dsky"}},
			{ThirdPartyDrivers: true},
		} {
			p := e.InstallPlan(opts)

			// It says its own name, never the family of whichever answer file
			// is involved. A Fedora stick announced "Ubuntu" for a release.
			if p.OSName != e.Name {
				t.Errorf("%s: plan calls itself %q", e.ID, p.OSName)
			}
			for _, line := range []string{p.InstallerLine(), p.StopsForLine()} {
				for _, other := range []string{"Ubuntu", "Fedora"} {
					if strings.Contains(line, other) && !strings.Contains(e.Name, other) {
						t.Errorf("%s: says %q in %q", e.ID, other, line)
					}
				}
			}

			// Automation and erasing travel together. "Boots the installer —
			// nothing set in advance" was said of a kickstart carrying
			// clearpart --all, which is the sentence that matters most on the
			// page and was the one that was wrong.
			if (p.Automation != BootsInstaller) != p.ErasesDisk {
				t.Errorf("%s: automation %v but ErasesDisk=%v", e.ID, p.Automation, p.ErasesDisk)
			}
			if p.ErasesDisk && !strings.Contains(p.InstallerLine(), "erase") {
				t.Errorf("%s: erases the disk and does not say so: %q", e.ID, p.InstallerLine())
			}
			if !p.ErasesDisk && strings.Contains(p.InstallerLine(), "erase") {
				t.Errorf("%s: threatens the disk without answers: %q", e.ID, p.InstallerLine())
			}

			// Nothing is promised that the entry cannot deliver.
			if p.Any() && !e.ProgramsSupported() {
				t.Errorf("%s: programs planned for an entry that takes none", e.ID)
			}
			if p.ThirdPartyDrivers && !e.ThirdPartyDriversSupported() {
				t.Errorf("%s: drivers planned for an entry that ignores them", e.ID)
			}
			if p.Any() && len(opts.Apps) == 0 {
				t.Errorf("%s: programs appeared from nowhere", e.ID)
			}
		}
	}
}

// The packages named must be the ones that installer will fetch. `docker` is
// docker.io on Ubuntu and moby-engine on Fedora; resolving both through
// Ubuntu's table is how a Fedora stick came to promise docker.io.
func TestPlanNamesThePackagesThatInstallerWillFetch(t *testing.T) {
	cases := []struct {
		id      string
		want    []string
		wantNot []string
	}{
		{"fedora-44-server", []string{"from Fedora", "moby-engine"}, []string{"from Ubuntu", "docker.io", "snap"}},
		{"ubuntu-26.04-server", []string{"from Ubuntu", "docker.io"}, []string{"from Fedora", "moby-engine"}},
	}
	for _, c := range cases {
		e, ok := Get(c.id)
		if !ok {
			t.Fatalf("%s missing", c.id)
		}
		var b strings.Builder
		for _, g := range e.InstallPlan(Options{Apps: []string{"docker", "dsky"}}).Programs {
			b.WriteString(g.Where + ": " + strings.Join(g.Names, ",") + "\n")
		}
		got := b.String()
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: missing %q in:\n%s", c.id, w, got)
			}
		}
		for _, w := range c.wantNot {
			if strings.Contains(got, w) {
				t.Errorf("%s: should not say %q in:\n%s", c.id, w, got)
			}
		}
		// A program installed from a release was resolved and never shown, so
		// `--apps dsky` listed nothing and installed something.
		if !strings.Contains(got, "published release, on first boot: dsky") {
			t.Errorf("%s: the release install is invisible:\n%s", c.id, got)
		}
	}
}

// The three sentences the surfaces share, pinned per automation.
func TestTheSentencesMatchTheAutomation(t *testing.T) {
	cases := []struct {
		id, opts, installer, stops string
	}{
		{"fedora-44-server", "vlc", "Installs by itself and erases the computer's disk", "No questions except the account"},
		{"ubuntu-26.04-server", "vlc", "Installs by itself and erases the computer's disk", "No questions except the account"},
		{"ubuntu-26.04-desktop", "vlc", "Waits for Install on the review screen", "The installer shows its review screen"},
		{"fedora-44-server", "", "Boots the installer", ""},
	}
	for _, c := range cases {
		e, ok := Get(c.id)
		if !ok {
			t.Fatalf("%s missing", c.id)
		}
		var apps []string
		if c.opts != "" {
			apps = strings.Split(c.opts, ",")
		}
		p := e.InstallPlan(Options{Apps: apps})
		if !strings.Contains(p.InstallerLine(), c.installer) {
			t.Errorf("%s(%q): installer line %q", c.id, c.opts, p.InstallerLine())
		}
		if c.stops == "" {
			if p.StopsForLine() != "" {
				t.Errorf("%s(%q): stops for %q with no answers", c.id, c.opts, p.StopsForLine())
			}
			continue
		}
		if !strings.Contains(p.StopsForLine(), c.stops) {
			t.Errorf("%s(%q): stops-for line %q", c.id, c.opts, p.StopsForLine())
		}
		if note := p.PickerNote(); !strings.Contains(note, e.Name) || !strings.Contains(note, c.stops) {
			t.Errorf("%s(%q): picker note %q", c.id, c.opts, note)
		}
	}
}

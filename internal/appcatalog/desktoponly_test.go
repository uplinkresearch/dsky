package appcatalog

import (
	"strings"
	"testing"
)

// Flathub is a desktop application store. Anything whose only Linux source is
// Flathub therefore wants a graphical session, and a server must not be
// offered it -- so rather than trusting whoever adds the next program to
// remember, the rule is checked here.
func TestFlathubProgramsAreMarkedDesktop(t *testing.T) {
	for _, a := range builtin {
		if u := a.Ubuntu; u != nil && u.Flatpak != "" && u.Apt == "" && u.Snap == "" && u.Repo == "" && u.Release == "" {
			if !a.Desktop {
				t.Errorf("%s comes only from Flathub and is not marked Desktop", a.ID)
			}
		}
		if f := a.Fedora; f != nil && f.Flatpak != "" && f.Dnf == "" && f.Repo == "" && f.Release == "" {
			if !a.Desktop {
				t.Errorf("%s comes only from Flathub on Fedora and is not marked Desktop", a.ID)
			}
		}
	}
}

// The point of keeping a picker on servers at all is the programs that belong
// there. If this list ever comes back empty on a server, the feature has
// quietly become "no programs on servers", which is a different decision from
// the one that was made.
func TestServersStillHaveProgramsWorthPicking(t *testing.T) {
	want := []string{"git", "docker", "nodejs", "python", "wireguard", "nmap", "dsky"}
	for _, id := range want {
		a, ok := Get(id)
		if !ok {
			t.Errorf("%s is not in the catalog at all", id)
			continue
		}
		if a.Desktop {
			t.Errorf("%s is marked as needing a desktop; it is a command-line program", id)
		}
		if a.Ubuntu == nil {
			t.Errorf("%s is not offered on Ubuntu", id)
		}
	}

	var left int
	for _, a := range builtin {
		if a.Ubuntu != nil && !a.Desktop {
			left++
		}
	}
	if left < 8 {
		t.Errorf("only %d programs left for an Ubuntu server; the picker has become pointless there", left)
	}
	t.Logf("%d of %d Ubuntu programs are offered on a server", left, countUbuntu())
}

func countUbuntu() int {
	var n int
	for _, a := range builtin {
		if a.Ubuntu != nil {
			n++
		}
	}
	return n
}

// The label is how somebody finds out before they pick, rather than after.
func TestDesktopProgramsSaySo(t *testing.T) {
	a, ok := Get("vlc")
	if !ok {
		t.Skip("no vlc in the catalog")
	}
	if !a.Desktop {
		t.Fatal("vlc is not marked as needing a desktop")
	}
	if !strings.Contains(strings.Join(a.Labels(), "; "), "needs a desktop") {
		t.Errorf("vlc's labels do not mention it: %v", a.Labels())
	}
}

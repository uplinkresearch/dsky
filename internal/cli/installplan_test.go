package cli

import (
	"strings"
	"testing"

	"github.com/uplinkresearch/dsky/internal/oscatalog"
)

// The plan is the last thing shown before the disk is erased, so every line of
// it has to be about the machine in front of the person reading it.
func TestProgramPlanNamesTheRightOSAndPackages(t *testing.T) {
	cases := []struct {
		id      string
		apps    []string
		want    []string
		wantNot []string
	}{{
		// `docker` is docker.io on Ubuntu and moby-engine on Fedora. This
		// resolved everything through Ubuntu's table under the word "Ubuntu",
		// so a Fedora stick named an OS it would not install and a package it
		// would never fetch.
		id:      "fedora-44-server",
		apps:    []string{"docker", "vlc"},
		want:    []string{"install Fedora 44 Server", "from Fedora:", "moby-engine"},
		wantNot: []string{"Ubuntu", "docker.io", "snap:"},
	}, {
		id:      "ubuntu-26.04-server",
		apps:    []string{"docker", "vlc"},
		want:    []string{"install Ubuntu 26.04 LTS Server", "from Ubuntu:", "docker.io"},
		wantNot: []string{"Fedora", "moby-engine", "review screen"},
	}, {
		// A release-installed program was resolved and never printed, so this
		// listed no programs at all and then installed one.
		id:   "ubuntu-26.04-server",
		apps: []string{"dsky"},
		want: []string{"published release, on first boot: dsky"},
	}, {
		id:      "fedora-44-server",
		apps:    []string{"dsky"},
		want:    []string{"published release, on first boot: dsky"},
		wantNot: []string{"Ubuntu"},
	}, {
		// Only Ubuntu's desktop installer stops at a review screen; the
		// kickstart asks nothing but the account.
		id:   "ubuntu-26.04-desktop",
		apps: []string{"vlc"},
		want: []string{"review screen"},
	}}

	for _, c := range cases {
		e, ok := oscatalog.Get(c.id)
		if !ok {
			t.Fatalf("%s missing from the catalog", c.id)
		}
		var b strings.Builder
		if err := printLinuxProgramPlan(&b, e, c.apps, ""); err != nil {
			t.Fatalf("%s %v: %v", c.id, c.apps, err)
		}
		got := b.String()
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s --apps %s: missing %q in:\n%s", c.id, strings.Join(c.apps, ","), w, got)
			}
		}
		for _, w := range c.wantNot {
			if strings.Contains(got, w) {
				t.Errorf("%s --apps %s: should not say %q in:\n%s", c.id, strings.Join(c.apps, ","), w, got)
			}
		}
	}
}

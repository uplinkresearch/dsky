package cli

import (
	"strings"
	"testing"

	"github.com/uplinkresearch/dsky/internal/migrate"
)

// A domain machine will not build without a password for the account it signs
// itself in as, and the refusal has to say how to give one -- a message that
// only says "no" sends somebody to put it on the command line, which is the
// one place it must not go.
func TestADomainBuildNeedsAnAdminPassword(t *testing.T) {
	domained := &migrate.Manifest{
		Identity: migrate.Identity{DomainFQDN: "lab.dsky.local"},
		Target:   migrate.Target{LocalAdmin: "uplink"},
	}
	_, err := adminPassword(domained, false, "/plans/PC01.json")
	if err == nil {
		t.Fatal("a domain build was allowed with a passwordless administrator")
	}
	for _, want := range []string{"lab.dsky.local", "uplink", "--admin-password-stdin",
		"DSKY_ADMIN_PASSWORD", "shell history", "PC01.json"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal never mentions %q:\n%s", want, err)
		}
	}

	// The environment is the other way in.
	t.Setenv("DSKY_ADMIN_PASSWORD", "from-the-env")
	got, err := adminPassword(domained, false, "/plans/PC01.json")
	if err != nil || got != "from-the-env" {
		t.Errorf("env: %q %v", got, err)
	}
}

// A standalone machine keeps the behaviour it has always had: no password, no
// question. It is a local account on a box somebody is standing in front of.
func TestAStandaloneBuildStillNeedsNothing(t *testing.T) {
	m := &migrate.Manifest{Target: migrate.Target{LocalAdmin: "uplink"}}
	got, err := adminPassword(m, false, "plan.json")
	if err != nil || got != "" {
		t.Errorf("standalone: %q %v", got, err)
	}
}

package migrate

import (
	"context"
	"errors"
	"testing"

	"github.com/uplinkresearch/dsky/internal/appcatalog"
)

// The packages a migration adds of its own — the runtimes nobody picks from a
// list — are checked against winget-pkgs the same way the app picker's are,
// on the weekly catalog-health run. A renamed package is otherwise invisible
// until a rebuilt machine comes up without its C++ runtime and the software
// that needed it fails at first boot.
func TestBuiltinTableIDsLive(t *testing.T) {
	if testing.Short() {
		t.Skip("live: checks winget-pkgs over the network")
	}
	for _, p := range prereqPackages {
		got, err := appcatalog.LookupWinget(context.Background(), p.ref)
		switch {
		case errors.Is(err, appcatalog.ErrWingetUnchecked):
			// GitHub's hourly limit, or no network: nothing is known about
			// this id either way, and calling that a bad id turns the test
			// red for a reason that has nothing to do with the table.
			t.Skipf("winget's package list could not be reached: %v", err)
		case err != nil:
			t.Errorf("%s (%s): %v", p.name, p.ref, err)
		case got != p.ref:
			t.Errorf("%s: winget-pkgs spells it %q, we say %q", p.name, got, p.ref)
		}
	}
}

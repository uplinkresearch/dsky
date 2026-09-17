package migrate

import (
	"context"
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
		case err != nil:
			t.Errorf("%s (%s): %v", p.name, p.ref, err)
		case got != p.ref:
			t.Errorf("%s: winget-pkgs spells it %q, we say %q", p.name, got, p.ref)
		}
	}
}

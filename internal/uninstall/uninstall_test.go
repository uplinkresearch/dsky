package uninstall

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPlanNeverIncludesWorkspaces is the safety property. A workspace is
// someone's git repository of recipes and org configuration, kept wherever
// they keep code — including, plausibly, right next to the binary. Nothing
// in the plan may ever point at one.
func TestPlanNeverIncludesWorkspaces(t *testing.T) {
	// A workspace in the most awkward place: inside the home directory, and
	// sharing a name prefix with things we do remove.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	decoys := []string{
		filepath.Join(home, "code", "dsky-workspace"),
		filepath.Join(home, "dsky"),
		filepath.Join(home, ".local", "share", "dsky-workspace"),
	}

	plan, err := Build()
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range plan.Items {
		for _, d := range decoys {
			if filepath.Clean(it.Path) == filepath.Clean(d) {
				t.Errorf("plan would remove %s, which is a workspace-shaped path", it.Path)
			}
			// A prefix match is the real danger: removing a parent takes the
			// workspace with it.
			if d != it.Path && sameDir(d, it.Path) {
				t.Errorf("plan item %s contains %s — removing it would take a workspace", it.Path, d)
			}
		}
	}
}

// TestPlanItemsAreOurs checks every path is one this tool actually creates,
// so a future edit cannot quietly widen the blast radius.
func TestPlanItemsAreOurs(t *testing.T) {
	plan, err := Build()
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range plan.Items {
		base := strings.ToLower(filepath.Base(it.Path))
		dir := strings.ToLower(it.Path)
		ours := strings.Contains(base, "dsky") || strings.Contains(base, "compose") ||
			strings.Contains(dir, "dsky")
		if !ours {
			t.Errorf("plan includes %s, which is not obviously ours", it.Path)
		}
		if it.Kind == "" {
			t.Errorf("%s has no kind, so callers cannot gate it", it.Path)
		}
		if it.What == "" {
			t.Errorf("%s has no description to show before deleting it", it.Path)
		}
	}
}

// TestLibraryIsSeparable: the library must be its own kind so it can be kept.
// Someone uninstalling to reinstall should not lose tens of gigabytes.
func TestLibraryIsSeparable(t *testing.T) {
	plan, err := Build()
	if err != nil {
		t.Fatal(err)
	}
	withLib := plan.Bytes(true)
	withoutLib := plan.Bytes(false)
	if withoutLib > withLib {
		t.Errorf("keeping the library totalled more (%d) than deleting it (%d)", withoutLib, withLib)
	}
	if plan.LibraryBytes() > 0 && withoutLib == withLib {
		t.Error("library bytes are counted even when it is being kept")
	}
	for _, it := range plan.Items {
		if it.Kind == KindLibrary && !strings.Contains(strings.ToLower(it.Path), "dsky") {
			t.Errorf("library item %s is not a DSKY path", it.Path)
		}
	}
}

// TestRunKeepsLibraryUnlessAsked builds a plan over a temporary tree and
// confirms a default uninstall leaves the library alone.
func TestRunKeepsLibraryUnlessAsked(t *testing.T) {
	dir := t.TempDir()
	prog := filepath.Join(dir, "dsky-fake")
	lib := filepath.Join(dir, "dsky-library")
	if err := os.WriteFile(prog, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lib, "blob"), []byte("big"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := &Plan{}
	p.add(Item{Path: prog, What: "program", Kind: KindProgram})
	p.add(Item{Path: lib, What: "library", Kind: KindLibrary})

	if _, errs := Run(p, false); len(errs) > 0 {
		t.Fatalf("run: %v", errs)
	}
	if _, err := os.Stat(prog); !os.IsNotExist(err) {
		t.Error("the program was not removed")
	}
	if _, err := os.Stat(lib); err != nil {
		t.Error("the library was removed without --purge")
	}

	// Now with purge.
	p2 := &Plan{}
	p2.add(Item{Path: lib, What: "library", Kind: KindLibrary})
	if _, errs := Run(p2, true); len(errs) > 0 {
		t.Fatalf("purge: %v", errs)
	}
	if _, err := os.Stat(lib); !os.IsNotExist(err) {
		t.Error("--purge did not remove the library")
	}
}

// The alias is a symlink to the program, and os.Executable() resolves through
// it, so running either marks both items as self. Keeping one pointer meant
// the second overwrote the first: `dsky uninstall` removed compose, left dsky
// on the machine, and said "Removed 5 item(s)" of six listed -- with no error,
// because nothing had tried and failed. Found by uninstalling for real and
// noticing the program still ran afterwards.
func TestEverySelfItemIsRemoved(t *testing.T) {
	dir := t.TempDir()
	prog := filepath.Join(dir, "dsky")
	alias := filepath.Join(dir, "compose")
	for _, p := range []string{prog, alias} {
		if err := os.WriteFile(p, []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	other := filepath.Join(dir, "icon.png")
	if err := os.WriteFile(other, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := &Plan{Items: []Item{
		{Path: prog, What: "the dsky program", Kind: KindProgram, Self: true},
		{Path: alias, What: "the compose alias", Kind: KindProgram, Self: true},
		{Path: other, What: "app icon", Kind: KindProgram},
	}}
	removed, errs := Run(p, false)
	if len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	if len(removed) != 3 {
		t.Errorf("removed %d of 3: %v", len(removed), removed)
	}
	for _, path := range []string{prog, alias, other} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s is still there after uninstall", filepath.Base(path))
		}
	}
}

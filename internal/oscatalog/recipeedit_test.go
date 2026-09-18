package oscatalog

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/uplinkresearch/dsky/internal/library"
	"github.com/uplinkresearch/dsky/internal/workspace"
)

func editWorkspace(t *testing.T) (*library.Library, string) {
	t.Helper()
	lib, err := library.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	wsDir := filepath.Join(t.TempDir(), "ws")
	if err := workspace.ScaffoldEmpty(wsDir, "Acme"); err != nil {
		t.Fatal(err)
	}
	return lib, wsDir
}

func loadForm(t *testing.T, wsDir, id string) (RecipeForm, error) {
	t.Helper()
	ws, err := workspace.Load(wsDir)
	if err != nil {
		t.Fatal(err)
	}
	r, err := ws.Recipe(id)
	if err != nil {
		t.Fatal(err)
	}
	return FormFromRecipe(wsDir, r)
}

// What the install dialog saved comes back as the same choices, so editing a
// recipe starts from what it does rather than from defaults.
func TestRecipeFormRoundTrip(t *testing.T) {
	lib, wsDir := editWorkspace(t)
	win, _ := Get("windows-11")
	opts := Options{Edition: "Pro", AccountMode: "oobe", Debloat: "off", BypassRequirement: true,
		Apps: []string{"brave", "winget:Some.Tool"}}
	if _, err := SaveRecipe(context.Background(), lib, wsDir, "desk", "Desk", win, opts, nil); err != nil {
		t.Fatal(err)
	}
	f, err := loadForm(t, wsDir, "desk")
	if err != nil {
		t.Fatal(err)
	}
	want := RecipeForm{OSID: "windows-11", Edition: "Pro", AccountMode: "oobe", Debloat: "off",
		BypassRequirement: true, Apps: []string{"brave", "winget:Some.Tool"}}
	if !reflect.DeepEqual(f, want) {
		t.Errorf("form\n got %+v\nwant %+v", f, want)
	}

	// Saved over with other choices: same id, new contents, no leftovers.
	opts2 := Options{Edition: "Home", AccountMode: "local", Debloat: "aggressive", Apps: []string{"vlc"}}
	if _, err := ReplaceRecipe(context.Background(), lib, wsDir, "desk", "Front desk", win, opts2, nil); err != nil {
		t.Fatal(err)
	}
	f, err = loadForm(t, wsDir, "desk")
	if err != nil || f.Edition != "Home" || f.Debloat != "aggressive" || !reflect.DeepEqual(f.Apps, []string{"vlc"}) {
		t.Errorf("after replace: %+v %v", f, err)
	}
	left, _ := filepath.Glob(filepath.Join(wsDir, "recipes", "desk.yaml*"))
	if len(left) != 1 {
		t.Errorf("recipe files after replace: %v, want only desk.yaml", left)
	}

	// A failed save puts the old recipe back.
	if _, err := ReplaceRecipe(context.Background(), lib, wsDir, "desk", "Bad", win, Options{Edition: "Nope"}, nil); err == nil {
		t.Fatal("unknown edition saved")
	}
	if f, err = loadForm(t, wsDir, "desk"); err != nil || f.Edition != "Home" {
		t.Errorf("old recipe not restored: %+v %v", f, err)
	}
}

func TestUbuntuRecipeFormAndDelete(t *testing.T) {
	lib, wsDir := editWorkspace(t)
	e, ok := Get("ubuntu-26.04-server")
	if !ok {
		t.Skip("no ubuntu-26.04-server in the catalog")
	}
	apps := []string{"vlc", "brave", "obsidian", "chrome"}
	if _, err := SaveRecipe(context.Background(), lib, wsDir, "lab", "Lab", e, Options{Apps: apps}, nil); err != nil {
		t.Fatal(err)
	}
	f, err := loadForm(t, wsDir, "lab")
	if err != nil || !reflect.DeepEqual(f.Apps, apps) {
		t.Errorf("form %+v %v, want apps %v", f, err, apps)
	}

	// Answers written before the marker existed are matched by package.
	ud := filepath.Join(wsDir, filepath.FromSlash(ubuntuUserDataFile("lab")))
	b, _ := os.ReadFile(ud)
	var kept []string
	for _, l := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(l, programsMarker) {
			kept = append(kept, l)
		}
	}
	os.WriteFile(ud, []byte(strings.Join(kept, "\n")), 0o644)
	f, err = loadForm(t, wsDir, "lab")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range apps {
		if !strings.Contains(" "+strings.Join(f.Apps, " ")+" ", " "+id+" ") {
			t.Errorf("without the marker, %s not found in %v", id, f.Apps)
		}
	}

	ws, _ := workspace.Load(wsDir)
	r, _ := ws.Recipe("lab")
	if err := DeleteRecipe(wsDir, r); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{r.Path, ud} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s still there after delete", p)
		}
	}
}

// A recipe with something the dialog has no control for is refused, so saving
// it from the dialog can't silently drop that.
func TestHandWrittenRecipeNotEditable(t *testing.T) {
	lib, wsDir := editWorkspace(t)
	win, _ := Get("windows-11")
	if _, err := SaveRecipe(context.Background(), lib, wsDir, "desk", "Desk", win, Options{}, nil); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(wsDir, "recipes", "desk.yaml")
	b, _ := os.ReadFile(p)
	edited := strings.Replace(string(b), "  debloat:\n    preset: standard\n", "  debloat:\n    preset: standard\n    keep_apps: [Microsoft.WindowsCalculator]\n", 1)
	if edited == string(b) {
		t.Fatal("test did not change the recipe")
	}
	os.WriteFile(p, []byte(edited), 0o644)
	if _, err := loadForm(t, wsDir, "desk"); err == nil || !strings.Contains(err.Error(), "bloatware") {
		t.Errorf("hand-edited recipe: %v", err)
	}
}

// The Fedora twin of TestUbuntuRecipeFormAndDelete. There wasn't one, which is
// how a kickstart recipe shipped that the dialog could save and then never
// reopen: FormFromRecipe only knew the autoinstall shape, so every Fedora
// recipe came back "not editable", the programs marker written into the
// kickstart had no reader, and deleting the recipe left the kickstart behind.
func TestFedoraRecipeFormAndDelete(t *testing.T) {
	lib, wsDir := editWorkspace(t)
	e, ok := Get("fedora-44-server")
	if !ok {
		t.Skip("no fedora-44-server in the catalog")
	}
	apps := []string{"vlc", "brave", "chrome"}
	if _, err := SaveRecipe(context.Background(), lib, wsDir, "lab", "Lab", e, Options{Apps: apps}, nil); err != nil {
		t.Fatal(err)
	}
	ks := filepath.Join(wsDir, filepath.FromSlash(fedoraKickstartFile("lab")))
	if _, err := os.Stat(ks); err != nil {
		t.Fatalf("kickstart not written: %v", err)
	}
	f, err := loadForm(t, wsDir, "lab")
	if err != nil || !reflect.DeepEqual(f.Apps, apps) {
		t.Fatalf("form %+v %v, want apps %v", f, err, apps)
	}

	// Saved over with a different list: the form follows, and the old
	// kickstart is not left beside the new one.
	if _, err := ReplaceRecipe(context.Background(), lib, wsDir, "lab", "Lab", e, Options{Apps: []string{"vlc"}}, nil); err != nil {
		t.Fatal(err)
	}
	if f, err = loadForm(t, wsDir, "lab"); err != nil || !reflect.DeepEqual(f.Apps, []string{"vlc"}) {
		t.Errorf("after replace: %+v %v", f, err)
	}
	left, _ := filepath.Glob(ks + "*")
	if len(left) != 1 {
		t.Errorf("kickstart files after replace: %v, want only the one", left)
	}

	// A kickstart with no marker is one DSKY did not write, and the program
	// list cannot be recovered from %packages -- a set expands, and one
	// package can come from several programs. Refused, not guessed at.
	b, _ := os.ReadFile(ks)
	var kept []string
	for _, l := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(l, programsMarker) {
			kept = append(kept, l)
		}
	}
	os.WriteFile(ks, []byte(strings.Join(kept, "\n")), 0o644)
	if _, err := loadForm(t, wsDir, "lab"); err == nil {
		t.Error("a kickstart with no marker was offered to the dialog")
	}
	os.WriteFile(ks, b, 0o644)

	ws, _ := workspace.Load(wsDir)
	r, _ := ws.Recipe("lab")
	if err := DeleteRecipe(wsDir, r); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{r.Path, ks} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s still there after delete", p)
		}
	}
}

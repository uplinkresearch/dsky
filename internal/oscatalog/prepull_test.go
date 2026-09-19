package oscatalog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/uplinkresearch/dsky/internal/appcatalog"
	"github.com/uplinkresearch/dsky/internal/workspace"
)

func offlineOpts() Options {
	return Options{
		Edition: "Pro", AccountMode: "local", Offline: true,
		builtAt: "2026-09-19T10:00:00Z",
		prepulled: []appcatalog.Prepulled{
			{WingetID: "Google.Chrome", Version: "153.0.8010.53", SHA256: "aaaa",
				Filename: "chrome64.msi", Format: "msi", Size: 120 << 20},
			{WingetID: "Notepad++.Notepad++", Version: "8.9.8", SHA256: "bbbb",
				Filename: "npp.exe", Format: "exe", Args: []string{"/S"}, Size: 5 << 20},
		},
	}
}

// An offline build must not also hand the machine a winget list: it would
// install every program twice, the second time over the network it has not
// got. The programs become payload -- staged, manifested and run by exactly
// the code that has been carrying an MSP's own RMM installer for a year.
func TestAnOfflineRecipeCarriesTheInstallersAndNoWingetList(t *testing.T) {
	y := recipeYAML(recipeMeta{ID: "q", Name: "Quick", Template: "autounattend.xml.tmpl"},
		Entry{ID: "windows-11", Family: Windows}, offlineOpts(), nil)
	if strings.Contains(y, "winget:") {
		t.Errorf("an offline recipe still names packages for winget:\n%s", y)
	}
	for _, want := range []string{
		"    - { ref: winget-google.chrome }",
		"      - msi: { ref: winget-google.chrome }",
		`      - exe: { ref: winget-notepad--.notepad--, args: ["/S"] }`,
		`      - { id: Google.Chrome, version: "153.0.8010.53", ref: winget-google.chrome }`,
		`    built_at: "2026-09-19T10:00:00Z"`,
	} {
		if !strings.Contains(y, want) {
			t.Errorf("the recipe has no\n  %s\nin:\n%s", want, y)
		}
	}
}

// Whatever the recipe says has to load back, or the build fails somewhere
// less helpful than here.
func TestAnOfflineRecipeLoadsBack(t *testing.T) {
	dir := t.TempDir()
	for _, sub := range []string{"templates", "manifests", "recipes", "payload"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(dir, "workspace.yaml"),
		[]byte("version: 1\norg:\n  name: T\n  id: t\n"), 0o644)
	tmpl, _ := templatesFS.ReadFile("templates/autounattend.xml.tmpl")
	os.WriteFile(filepath.Join(dir, "templates", "autounattend.xml.tmpl"), tmpl, 0o644)
	e := Entry{ID: "windows-11", Name: "Windows 11", Family: Windows, SHA256: "cccc", Filename: "w.iso", URL: "https://example.invalid/w.iso"}
	os.WriteFile(filepath.Join(dir, "manifests", "windows-11.yaml"), []byte(manifestYAML(e)), 0o644)

	opts := offlineOpts()
	if err := writePrepulledManifests(opts, func(rel string, data []byte) error {
		return os.WriteFile(filepath.Join(dir, filepath.FromSlash(rel)), data, 0o644)
	}); err != nil {
		t.Fatal(err)
	}
	y := recipeYAML(recipeMeta{ID: "windows-11", Name: "Quick", Template: "templates/autounattend.xml.tmpl"}, e, opts, nil)
	if err := os.WriteFile(filepath.Join(dir, "recipes", "windows-11.yaml"), []byte(y), 0o644); err != nil {
		t.Fatal(err)
	}
	ws, err := workspace.Load(dir)
	if err != nil {
		t.Fatalf("the workspace did not load: %v", err)
	}
	r, err := ws.Recipe("windows-11")
	if err != nil {
		t.Fatalf("the offline recipe did not load back: %v\n%s", err, y)
	}
	off := r.Windows.Apps.OfflineApps()
	if len(off) != 2 {
		t.Fatalf("the record came back as %+v", off)
	}
	if off[0].ID != "Google.Chrome" || off[0].Version != "153.0.8010.53" || off[0].Ref != "winget-google.chrome" {
		t.Errorf("first record = %+v", off[0])
	}
	if r.Windows.Apps.BuiltAt != "2026-09-19T10:00:00Z" {
		t.Errorf("the media's age was not recorded: %q", r.Windows.Apps.BuiltAt)
	}
	// And the manifests pin the library blobs, with no url: the download
	// already happened and was checked against winget's own hash.
	b, err := os.ReadFile(filepath.Join(dir, "manifests", "winget-google.chrome.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "url:") || !strings.Contains(string(b), `sha256: "aaaa"`) {
		t.Errorf("manifest = %s", b)
	}
}

// An ordinary build must be exactly what it always was.
func TestAnOnlineRecipeIsUnchanged(t *testing.T) {
	y := recipeYAML(recipeMeta{ID: "q", Name: "Quick", Template: "autounattend.xml.tmpl"},
		Entry{ID: "windows-11", Family: Windows},
		Options{Edition: "Pro", AccountMode: "local", Apps: []string{"chrome"}}, nil)
	if !strings.Contains(y, "  apps:\n    winget:\n      - Google.Chrome\n") {
		t.Errorf("a normal build changed shape:\n%s", y)
	}
	if strings.Contains(y, "offline:") || strings.Contains(y, "built_at:") {
		t.Errorf("a normal build grew an offline record:\n%s", y)
	}
}

// A saved offline recipe has to open in the install dialog like any other, or
// the only way to change one program in it is to build a new one. Opening it
// must not touch the network either: renaming a recipe should not download a
// hundred megabytes, and must not quietly swap in a newer Chrome than the
// recipe was saved with.
func TestASavedOfflineRecipeOpensInTheDialogWithoutDownloadingAnything(t *testing.T) {
	dir := t.TempDir()
	for _, sub := range []string{"templates", "manifests", "recipes", "payload"} {
		os.MkdirAll(filepath.Join(dir, sub), 0o755)
	}
	os.WriteFile(filepath.Join(dir, "workspace.yaml"),
		[]byte("version: 1\norg:\n  name: T\n  id: t\n"), 0o644)
	tmpl, _ := templatesFS.ReadFile("templates/autounattend.xml.tmpl")
	os.WriteFile(filepath.Join(dir, savedTemplate), tmpl, 0o644)
	e, ok := Get("windows-11")
	if !ok {
		t.Skip("no windows-11 in the built-in catalog")
	}
	os.WriteFile(filepath.Join(dir, "manifests", e.ID+".yaml"), []byte(manifestYAML(e)), 0o644)

	opts := offlineOpts()
	opts.defaults(e)
	if err := writePrepulledManifests(opts, func(rel string, data []byte) error {
		return os.WriteFile(filepath.Join(dir, filepath.FromSlash(rel)), data, 0o644)
	}); err != nil {
		t.Fatal(err)
	}
	meta := recipeMeta{ID: "bench", Name: "Bench", Template: savedTemplate}
	if err := os.WriteFile(filepath.Join(dir, "recipes", "bench.yaml"),
		[]byte(recipeYAML(meta, e, opts, nil)), 0o644); err != nil {
		t.Fatal(err)
	}
	ws, err := workspace.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	r, err := ws.Recipe("bench")
	if err != nil {
		t.Fatal(err)
	}
	f, err := FormFromRecipe(dir, r)
	if err != nil {
		t.Fatalf("a saved offline recipe cannot be opened: %v", err)
	}
	if !f.Offline {
		t.Error("the dialog would open it with the offline tick clear, and saving would put the machine back on the network")
	}
	if len(f.Apps) != 2 {
		t.Fatalf("programs came back as %v", f.Apps)
	}
	// Both are in the built-in list, so both come back as the ids the picker
	// shows rather than as typed winget ids.
	if f.Apps[0] != "chrome" || f.Apps[1] != "notepadplusplus" {
		t.Errorf("programs came back as %v, not the ones the picker shows", f.Apps)
	}
	// The versions must be the ones the recipe was saved with, read off the
	// disk rather than fetched.
	if len(f.prepulled) != 2 || f.prepulled[0].Version != "153.0.8010.53" || f.prepulled[0].SHA256 != "aaaa" {
		t.Errorf("read back %+v", f.prepulled)
	}
	if f.prepulled[1].Format != "exe" || len(f.prepulled[1].Args) != 1 || f.prepulled[1].Args[0] != "/S" {
		t.Errorf("the silent switches were lost: %+v", f.prepulled[1])
	}
}

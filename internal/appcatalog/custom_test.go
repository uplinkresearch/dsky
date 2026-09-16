package appcatalog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/uplinkresearch/dsky/internal/library"
	"github.com/uplinkresearch/dsky/internal/manifest"
)

// freshStore points the package at an empty store and restores the previous
// one afterwards, since the custom list is process-wide state.
func freshStore(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := LoadCustom(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		customMu.Lock()
		customs = nil
		customMu.Unlock()
	})
	return root
}

func sample(id string) Custom {
	return Custom{
		ID: id, Name: "Macula Agent", Format: "msi",
		SHA256: strings.Repeat("a", 64), Filename: "MaculaAgent.msi",
		Args: []string{"/qn", "/norestart"},
	}
}

func TestCustomRoundTrip(t *testing.T) {
	root := freshStore(t)
	if err := AddCustom(root, sample("macula"), false); err != nil {
		t.Fatal(err)
	}
	// Reloading from disk is the real test: the picker on the next run reads
	// the file, not the in-memory copy.
	customMu.Lock()
	customs = nil
	customMu.Unlock()
	if err := LoadCustom(root); err != nil {
		t.Fatal(err)
	}
	got, ok := GetCustom("macula")
	if !ok {
		t.Fatal("the added program did not survive a reload")
	}
	if got.Name != "Macula Agent" || len(got.Args) != 2 || got.Args[0] != "/qn" {
		t.Errorf("record changed across the store: %+v", got)
	}
	if got.SourceID() != "app-macula" {
		t.Errorf("source id = %q, want app-macula", got.SourceID())
	}
	if got.AddedAt.IsZero() {
		t.Error("AddedAt was not stamped")
	}
}

// TestAddRefusesSilentOverwrite: re-adding an id almost always means a new
// version of the same installer, and doing that by accident would change what
// lands on every machine built afterwards.
func TestAddRefusesSilentOverwrite(t *testing.T) {
	root := freshStore(t)
	if err := AddCustom(root, sample("macula"), false); err != nil {
		t.Fatal(err)
	}
	if err := AddCustom(root, sample("macula"), false); err == nil {
		t.Error("a second add with the same id was accepted silently")
	}
	replacement := sample("macula")
	replacement.Name = "Macula Agent v2"
	if err := AddCustom(root, replacement, true); err != nil {
		t.Fatalf("--replace was refused: %v", err)
	}
	if got, _ := GetCustom("macula"); got.Name != "Macula Agent v2" {
		t.Errorf("replace did not take: %q", got.Name)
	}
	if len(CustomApps()) != 1 {
		t.Errorf("replace duplicated the entry: %d records", len(CustomApps()))
	}
}

// TestCustomCannotShadowBuiltin: --apps takes ids, so a duplicate would be
// ambiguous and one of the two would be silently ignored.
func TestCustomCannotShadowBuiltin(t *testing.T) {
	root := freshStore(t)
	c := sample("chrome")
	if err := AddCustom(root, c, false); err == nil {
		t.Error("an id that collides with a built-in program was accepted")
	}
}

func TestCustomIDValidation(t *testing.T) {
	for _, bad := range []string{"", "Macula", "has space", "-leading", "with/slash"} {
		if err := CheckCustomID(bad, "Name", "msi"); err == nil {
			t.Errorf("id %q was accepted but cannot be a manifest id", bad)
		}
	}
	if err := CheckCustomID("macula-agent.v2", "Name", "msi"); err != nil {
		t.Errorf("a legitimate id was refused: %v", err)
	}
	if err := CheckCustomID("ok", "Name", "zip"); err == nil {
		t.Error("format zip was accepted; only msi and exe can be run at first boot")
	}
}

// TestResolveSplitsByHowTheyInstall is the distinction the build depends on:
// winget ids go in a script that needs the network, the operator's installers
// are staged onto the media and run directly.
func TestResolveSplitsByHowTheyInstall(t *testing.T) {
	root := freshStore(t)
	if err := AddCustom(root, sample("macula"), false); err != nil {
		t.Fatal(err)
	}
	winget, custom, err := Resolve([]string{"chrome", "macula", " 7zip "})
	if err != nil {
		t.Fatal(err)
	}
	if len(winget) != 2 || winget[0] != "Google.Chrome" {
		t.Errorf("winget packages = %v", winget)
	}
	if len(custom) != 1 || custom[0].ID != "macula" {
		t.Errorf("custom installers = %+v", custom)
	}
	if _, _, err := Resolve([]string{"nope"}); err == nil {
		t.Error("an unknown id was accepted — the build would silently skip a program")
	}
}

// TestCustomAppearsInPickers: adding a program is pointless if the pickers
// still show only the built-in list.
func TestCustomAppearsInPickers(t *testing.T) {
	root := freshStore(t)
	before := len(Catalog())
	if err := AddCustom(root, sample("macula"), false); err != nil {
		t.Fatal(err)
	}
	if got := len(Catalog()); got != before+1 {
		t.Errorf("catalog has %d entries, want %d", got, before+1)
	}
	a, ok := Get("macula")
	if !ok || a.Custom == nil {
		t.Fatal("the added program is not in the catalog")
	}
	if !a.InstallsOnWindows() {
		t.Error("an operator installer must count as installable, or pickers hide it")
	}
	if a.Category != CustomCategory {
		t.Errorf("category = %q, want the default %q", a.Category, CustomCategory)
	}
	var found bool
	for _, c := range Categories() {
		if c == CustomCategory {
			found = true
		}
	}
	if !found {
		t.Error("the custom category is missing, so the grouped pickers would drop it")
	}
}

// fakeStore stands in for the library: AddInstaller only needs somewhere to
// put the file, and a real library would mean copying bytes for no reason.
type fakeStore struct {
	sum    string
	stages *[]string // stages the caller was told about, when it cares
}

func (f fakeStore) ImportWithProgress(src *manifest.Source, path string,
	progress func(stage string, done, total int64)) (library.Entry, error) {
	if progress != nil {
		progress("checking "+src.Filename, 0, 2048)
		progress("checking "+src.Filename, 2048, 2048)
		progress("filing "+src.Filename, 2048, 2048)
	}
	if f.stages != nil {
		*f.stages = append(*f.stages, "imported")
	}
	return library.Entry{ID: src.ID, SHA256: f.sum, Filename: src.Filename, Size: 2048}, nil
}

// TestAddInstallerReturnsWhatWasStored: AddCustom stamps AddedAt on the stored
// record, so returning the copy that went in would hand the caller a record
// with a zero date it never had — which the portal then displays.
func TestAddInstallerReturnsWhatWasStored(t *testing.T) {
	root := freshStore(t)
	dir := t.TempDir()
	msi := filepath.Join(dir, "MaculaAgent.msi")
	if err := os.WriteFile(msi, fakeMSI("installer"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := fakeStore{sum: strings.Repeat("e", 64)}

	got, err := AddInstaller(root, store, Installer{Path: msi, ID: "macula", Args: `/qn "C:\a b"`}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.AddedAt.IsZero() {
		t.Error("the returned record has no AddedAt — a caller echoing it shows 0001-01-01")
	}
	if got.Name != "MaculaAgent.msi" {
		t.Errorf("name should default to the filename, got %q", got.Name)
	}
	if got.Format != "msi" || got.SHA256 != store.sum {
		t.Errorf("record did not pick up the import: %+v", got)
	}
	// A quoted switch carrying a path with a space must survive as one arg.
	if len(got.Args) != 2 || got.Args[1] != `C:\a b` {
		t.Errorf("args split wrongly: %q", got.Args)
	}
	if got.RunLine() != `msiexec /i "MaculaAgent.msi" /qn C:\a b` {
		t.Errorf("run line = %q", got.RunLine())
	}
}

// TestAddInstallerRefusesBeforeCopying: the copy is the expensive part, so
// everything knowable is checked first.
func TestAddInstallerRefusesBeforeCopying(t *testing.T) {
	root := freshStore(t)
	dir := t.TempDir()
	msi := filepath.Join(dir, "Thing.msi")
	os.WriteFile(msi, fakeMSI("x"), 0o644)
	txt := filepath.Join(dir, "Thing.txt")
	os.WriteFile(txt, []byte("x"), 0o644)

	// The store records every import it is asked to do, so "did the expensive
	// part run?" is answered by the store rather than by the progress
	// callback — which is only a display.
	var imports []string
	watching := fakeStore{sum: strings.Repeat("f", 64), stages: &imports}

	if _, err := AddInstaller(root, watching, Installer{Path: txt}, nil); err == nil {
		t.Error("a .txt was accepted as an installer")
	}
	if _, err := AddInstaller(root, watching, Installer{Path: msi, ID: "chrome"}, nil); err == nil {
		t.Error("an id colliding with a built-in was accepted")
	}
	if len(imports) != 0 {
		t.Error("the installer was copied before the record was found invalid")
	}

	// And a duplicate must also refuse before copying.
	if _, err := AddInstaller(root, watching, Installer{Path: msi, ID: "thing"}, nil); err != nil {
		t.Fatal(err)
	}
	if len(imports) != 1 {
		t.Fatalf("a valid add did not import: %v", imports)
	}
	if _, err := AddInstaller(root, watching, Installer{Path: msi, ID: "thing"}, nil); err == nil {
		t.Error("a duplicate id was accepted without replace")
	}
	if len(imports) != 1 {
		t.Error("a duplicate add copied the installer before refusing")
	}
}

// TestAddInstallerReportsProgress: an installer is read twice, once to hash
// and once to copy, and for a few hundred megabytes that is long enough that
// silence reads as a hang.
func TestAddInstallerReportsProgress(t *testing.T) {
	root := freshStore(t)
	dir := t.TempDir()
	msi := filepath.Join(dir, "Agent.msi")
	if err := os.WriteFile(msi, fakeMSI("installer"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stages []string
	_, err := AddInstaller(root, fakeStore{sum: strings.Repeat("a", 64)},
		Installer{Path: msi, ID: "agent"},
		func(stage string, done, total int64) { stages = append(stages, stage) })
	if err != nil {
		t.Fatal(err)
	}
	if len(stages) == 0 {
		t.Fatal("the import reported no progress at all")
	}
	joined := strings.Join(stages, " ")
	for _, want := range []string{"checking", "filing"} {
		if !strings.Contains(joined, want) {
			t.Errorf("progress never mentioned %q: %v", want, stages)
		}
	}
}

func TestRemoveCustom(t *testing.T) {
	root := freshStore(t)
	if err := AddCustom(root, sample("macula"), false); err != nil {
		t.Fatal(err)
	}
	if _, err := RemoveCustom(root, "macula"); err != nil {
		t.Fatal(err)
	}
	if _, ok := GetCustom("macula"); ok {
		t.Error("the program is still listed after removal")
	}
	if _, err := RemoveCustom(root, "macula"); err == nil {
		t.Error("removing a program that is not there should say so")
	}
}

func TestUpdateCustom(t *testing.T) {
	root := freshStore(t)
	if err := AddCustom(root, sample("macula"), false); err != nil {
		t.Fatal(err)
	}
	got, err := UpdateCustom(root, "macula", func(c *Custom) { c.Args = []string{"/quiet"} })
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Args) != 1 || got.Args[0] != "/quiet" {
		t.Errorf("args = %v", got.Args)
	}
	// The installer itself must not have to be re-imported to fix a switch.
	if got.SHA256 != strings.Repeat("a", 64) || got.Filename != "MaculaAgent.msi" {
		t.Errorf("editing switches disturbed the installer: %+v", got)
	}
	if _, err := UpdateCustom(root, "ghost", func(*Custom) {}); err == nil {
		t.Error("editing a program that is not there should say so")
	}
}

// TestCorruptStoreIsReported: silently presenting a short list as if it were
// complete is how a machine ships without its agent.
func TestCorruptStoreIsReported(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "apps.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := LoadCustom(root); err == nil {
		t.Error("a corrupt store loaded without complaint")
	}
}

// TestStoreFromNewerBuildIsRefused: reading a format we do not understand and
// then writing it back would drop whatever we failed to parse.
func TestStoreFromNewerBuildIsRefused(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "apps.json"),
		[]byte(`{"version":99,"apps":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := LoadCustom(root); err == nil {
		t.Error("a store from a newer build was accepted")
	}
}

// fakeMSI is a stand-in installer that starts the way a Windows Installer
// package does: an OLE compound file. These tests are about what AddInstaller
// stores and reports, and a file named .msi that is not one is refused before
// any of that happens -- which is the point of FormatForFile's own test.
func fakeMSI(body string) []byte {
	return append([]byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1}, body...)
}

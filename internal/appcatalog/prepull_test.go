package appcatalog

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/uplinkresearch/dsky/internal/fetch"
	"github.com/uplinkresearch/dsky/internal/library"
	"github.com/uplinkresearch/dsky/internal/manifest"
)

// fakeLibrary records what was asked for instead of downloading it.
type fakeLibrary struct {
	pulled []manifest.Source
	err    error
}

func (f *fakeLibrary) Pull(ctx context.Context, src *manifest.Source, pinTOFU bool,
	resolve library.URLResolver, progress fetch.Progress, note func(string)) (library.Entry, error) {
	if f.err != nil {
		return library.Entry{}, f.err
	}
	f.pulled = append(f.pulled, *src)
	return library.Entry{ID: src.ID, SHA256: src.SHA256, Size: 1234}, nil
}

func TestPrePullPinsTheManifestsOwnHash(t *testing.T) {
	onePackage(t, "Google.Chrome", "153.0.8010.53", `
PackageIdentifier: Google.Chrome
PackageVersion: 153.0.8010.53
InstallerType: wix
Scope: machine
Installers:
- Architecture: x64
  InstallerUrl: https://dl.google.com/chrome64.msi
  InstallerSha256: E461E0C8
`)
	lib := &fakeLibrary{}
	got, err := PrePull(context.Background(), lib, []string{"google.chrome"}, nil, nil)
	if err != nil {
		t.Fatalf("PrePull: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d programs, want 1", len(got))
	}
	p := got[0]
	if p.WingetID != "Google.Chrome" || p.Version != "153.0.8010.53" {
		t.Errorf("recorded %s %s, want the id the catch-up can upgrade later", p.WingetID, p.Version)
	}
	if p.SourceID() != "winget-google.chrome" {
		t.Errorf("source id = %q", p.SourceID())
	}
	if !p.MSI() || p.Filename != "chrome64.msi" || p.Size != 1234 {
		t.Errorf("got %+v", p)
	}
	if len(lib.pulled) != 1 {
		t.Fatalf("pulled %d files", len(lib.pulled))
	}
	// The pin is the whole point: an installer that arrives over somebody's
	// coffee-shop connection and is never checked must not reach a fleet.
	if s := lib.pulled[0]; s.SHA256 != "e461e0c8" || s.URL != "https://dl.google.com/chrome64.msi" {
		t.Errorf("pulled %+v, want the manifest's url pinned to the manifest's hash", s)
	}
	if lib.pulled[0].Kind != manifest.KindPayload {
		t.Errorf("kind = %q, want a payload so it stages like an operator's own installer", lib.pulled[0].Kind)
	}
}

// A package that cannot ride on a stick stops the build, and the message names
// every one of them -- an operator deciding what to do about it wants the whole
// list, not one package per build attempt.
func TestPrePullNamesEveryProgramItCannotTake(t *testing.T) {
	dirs := map[string][]treeEntry{
		"manifests/s":                trees("Some"),
		"manifests/s/Some":           trees("Store", "Chain"),
		"manifests/s/Some/Store":     trees("1.0"),
		"manifests/s/Some/Store/1.0": blobs("Some.Store.installer.yaml"),
		"manifests/s/Some/Chain":     trees("1.0"),
		"manifests/s/Some/Chain/1.0": blobs("Some.Chain.installer.yaml"),
	}
	files := map[string]string{
		"manifests/s/Some/Store/1.0/Some.Store.installer.yaml": `
PackageIdentifier: Some.Store
PackageVersion: "1.0"
InstallerType: msstore
Installers:
- Architecture: neutral
  InstallerUrl: https://example.invalid/x
  InstallerSha256: AA
`,
		"manifests/s/Some/Chain/1.0/Some.Chain.installer.yaml": `
PackageIdentifier: Some.Chain
PackageVersion: "1.0"
InstallerType: wix
Installers:
- Architecture: x64
  InstallerUrl: https://example.invalid/x.msi
  InstallerSha256: AA
  Dependencies:
    PackageDependencies:
    - PackageIdentifier: Microsoft.VCRedist.2015+.x64
`,
	}
	fakeManifests(t, dirs, files)
	lib := &fakeLibrary{}
	_, err := PrePull(context.Background(), lib, []string{"Some.Store", "Some.Chain"}, nil, nil)
	var pe *PrePullError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v, want a list of refusals", err)
	}
	if len(pe.Reasons) != 2 {
		t.Fatalf("reasons = %q, want both packages", pe.Reasons)
	}
	if !strings.Contains(pe.Error(), "Some.Store") || !strings.Contains(pe.Error(), "Some.Chain") {
		t.Errorf("message names only some of them: %s", pe)
	}
	if len(lib.pulled) != 0 {
		t.Error("downloaded something for a build that is about to be refused")
	}
}

// Offline or rate-limited is not the same as impossible: nothing is known
// about the package either way, so the build must stop with "try again when
// the network is back" rather than telling an operator to pick different
// programs.
func TestPrePullSeparatesCannotFromCouldNotCheck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rate limited", http.StatusForbidden)
	}))
	defer srv.Close()
	old := WingetRepoAPI
	WingetRepoAPI = srv.URL
	defer func() {
		WingetRepoAPI = old
		lookupMu.Lock()
		lookupCache = map[string]lookupResult{}
		lookupMu.Unlock()
	}()
	_, err := PrePull(context.Background(), &fakeLibrary{}, []string{"Brave.Brave"}, nil, nil)
	var pe *PrePullError
	if errors.As(err, &pe) {
		t.Fatalf("a lookup that never happened was reported as a package that can never be pre-pulled: %v", err)
	}
	if !errors.Is(err, ErrWingetUnchecked) {
		t.Fatalf("err = %v", err)
	}
}

// manyPackages wires the folder walk for several packages at once.
func manyPackages(t *testing.T, manifests map[string]string) {
	t.Helper()
	dirs := map[string][]treeEntry{}
	files := map[string]string{}
	seen := map[string][]string{}
	for id := range manifests {
		path := "manifests/" + strings.ToLower(id[:1])
		for _, part := range strings.Split(id, ".") {
			seen[path] = append(seen[path], part)
			path += "/" + part
		}
		seen[path] = append(seen[path], "1.0")
		dirs[path+"/1.0"] = blobs(id+".installer.yaml", id+".yaml")
		files[path+"/1.0/"+id+".installer.yaml"] = manifests[id]
	}
	for path, names := range seen {
		dirs[path] = trees(names...)
	}
	fakeManifests(t, dirs, files)
}

func pkgYAML(id, kind string, needs ...string) string {
	y := "PackageIdentifier: " + id + "\nPackageVersion: \"1.0\"\nInstallerType: " + kind +
		"\nInstallers:\n- Architecture: x64\n  Scope: machine\n  InstallerUrl: https://example.invalid/" +
		strings.ToLower(id) + ".msi\n  InstallerSha256: AA\n"
	if len(needs) > 0 {
		y += "  Dependencies:\n    PackageDependencies:\n"
		for _, n := range needs {
			y += "    - PackageIdentifier: " + n + "\n"
		}
	}
	return y
}

// LibreOffice does not start without a Visual C++ runtime, and on a machine
// with no network there is nothing to fetch it. So the runtime goes on the
// stick too, ahead of the program that named it.
func TestPrePullCarriesWhatAProgramNeedsFirst(t *testing.T) {
	manyPackages(t, map[string]string{
		"Some.Office":  pkgYAML("Some.Office", "wix", "Some.Runtime"),
		"Some.Editor":  pkgYAML("Some.Editor", "wix", "Some.Runtime"),
		"Some.Runtime": pkgYAML("Some.Runtime", "wix"),
	})
	lib := &fakeLibrary{}
	got, err := PrePull(context.Background(), lib, []string{"Some.Office", "Some.Editor"}, nil, nil)
	if err != nil {
		t.Fatalf("PrePull: %v", err)
	}
	var order []string
	for _, p := range got {
		order = append(order, p.WingetID)
	}
	// The runtime once, first; then the two programs that wanted it.
	want := []string{"Some.Runtime", "Some.Office", "Some.Editor"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("install order = %v, want %v", order, want)
	}
	if len(lib.pulled) != 3 {
		t.Errorf("downloaded %d files; the shared runtime should be fetched once", len(lib.pulled))
	}
}

// A chain that keeps going stops the build rather than quietly filling
// somebody's stick with a tree of packages they did not choose. The reason
// has to name what asked for it: "Some.Deep cannot be downloaded" is a puzzle
// on its own and obvious with the program that wanted it beside it.
func TestPrePullStopsFollowingAChainAndSaysWhatWantedIt(t *testing.T) {
	manyPackages(t, map[string]string{
		"Some.App":  pkgYAML("Some.App", "wix", "Some.Mid"),
		"Some.Mid":  pkgYAML("Some.Mid", "wix", "Some.Deep"),
		"Some.Deep": pkgYAML("Some.Deep", "wix", "Some.Deeper"),
	})
	_, err := PrePull(context.Background(), &fakeLibrary{}, []string{"Some.App"}, nil, nil)
	var pe *PrePullError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v, want a refusal", err)
	}
	if !strings.Contains(pe.Error(), "needed by Some.App → Some.Mid") {
		t.Errorf("the message does not say what wanted it: %s", pe)
	}
}

// A dependency that cannot itself be put on a stick refuses the program that
// needs it, named the same way.
func TestPrePullRefusesAProgramWhoseRuntimeCannotBeCarried(t *testing.T) {
	manyPackages(t, map[string]string{
		"Some.App":   pkgYAML("Some.App", "wix", "Some.Store"),
		"Some.Store": pkgYAML("Some.Store", "msstore"),
	})
	_, err := PrePull(context.Background(), &fakeLibrary{}, []string{"Some.App"}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "Some.Store (needed by Some.App)") {
		t.Fatalf("err = %v", err)
	}
}

// A Windows feature is not a download: DISM fetches it from Windows Update or
// from the installation media, and a machine with no network has neither.
func TestPrePullRefusesAWindowsFeature(t *testing.T) {
	onePackage(t, "Some.Old", "1.0", `
PackageIdentifier: Some.Old
PackageVersion: "1.0"
InstallerType: wix
Installers:
- Architecture: x64
  InstallerUrl: https://example.invalid/old.msi
  InstallerSha256: AA
  Dependencies:
    WindowsFeatures:
    - NetFx3
`)
	_, err := PrePull(context.Background(), &fakeLibrary{}, []string{"Some.Old"}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "NetFx3") {
		t.Fatalf("err = %v", err)
	}
}

// A winget id may contain a plus sign; a library manifest id may not. Real
// ones do -- Microsoft.VCRedist.2015+.x64 is what LibreOffice, KeePassXC and
// Plex all depend on -- and the first real offline build stopped dead on it.
func TestASourceIDIsSomethingTheLibraryWillAccept(t *testing.T) {
	for _, tc := range []struct{ id, want string }{
		{"Google.Chrome", "winget-google.chrome"},
		{"Microsoft.VCRedist.2015+.x64", "winget-microsoft.vcredist.2015-.x64"},
		{"Notepad++.Notepad++", "winget-notepad--.notepad--"},
	} {
		got := Prepulled{WingetID: tc.id}.SourceID()
		if got != tc.want {
			t.Errorf("%s filed as %q, want %q", tc.id, got, tc.want)
		}
		if !idRe.MatchString(got) {
			t.Errorf("%s filed as %q, which no library will accept", tc.id, got)
		}
	}
}

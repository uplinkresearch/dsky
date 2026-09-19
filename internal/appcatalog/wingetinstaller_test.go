package appcatalog

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// fakeManifests serves both halves of winget-pkgs a lookup needs: the trees
// API for the folder walk, and raw file contents for the manifests. dirs are
// folder listings, files are paths under the repository root.
func fakeManifests(t *testing.T, dirs map[string][]treeEntry, files map[string]string) {
	t.Helper()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tree, ok := dirs[strings.TrimPrefix(r.URL.Path, "/git/trees/master:")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"tree": tree})
	}))
	raw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := files[strings.TrimPrefix(r.URL.Path, "/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(body))
	}))
	t.Cleanup(api.Close)
	t.Cleanup(raw.Close)
	oldAPI, oldRaw := WingetRepoAPI, WingetRawBase
	WingetRepoAPI, WingetRawBase = api.URL, raw.URL
	t.Cleanup(func() {
		WingetRepoAPI, WingetRawBase = oldAPI, oldRaw
		lookupMu.Lock()
		lookupCache = map[string]lookupResult{}
		lookupMu.Unlock()
	})
}

func trees(names ...string) (out []treeEntry) {
	for _, n := range names {
		out = append(out, treeEntry{Path: n, Type: "tree"})
	}
	return
}

func blobs(names ...string) (out []treeEntry) {
	for _, n := range names {
		out = append(out, treeEntry{Path: n, Type: "blob"})
	}
	return
}

// onePackage wires up the folder walk for a single package so each test only
// has to write the manifest it is about.
func onePackage(t *testing.T, id, version, manifestYAML string) {
	t.Helper()
	parts := strings.Split(id, ".")
	path := "manifests/" + strings.ToLower(id[:1])
	dirs := map[string][]treeEntry{}
	for _, p := range parts {
		dirs[path] = trees(p)
		path += "/" + p
	}
	dirs[path] = trees(version)
	dirs[path+"/"+version] = blobs(id+".installer.yaml", id+".yaml")
	fakeManifests(t, dirs, map[string]string{
		path + "/" + version + "/" + id + ".installer.yaml": manifestYAML,
	})
}

// Chrome's shape: one type for the whole manifest, three architectures. x64
// wins; arm64 is left alone because an x64 MSI runs on Windows on ARM and the
// stick is built for a fleet.
func TestInstallerPrefersX64(t *testing.T) {
	onePackage(t, "Google.Chrome", "153.0.8010.53", `
PackageIdentifier: Google.Chrome
PackageVersion: 153.0.8010.53
InstallerType: wix
Scope: machine
Installers:
- Architecture: x86
  InstallerUrl: https://dl.google.com/chrome32.msi
  InstallerSha256: AAAA
- Architecture: x64
  InstallerUrl: https://dl.google.com/chrome64.msi
  InstallerSha256: BBBB
- Architecture: arm64
  InstallerUrl: https://dl.google.com/chromearm.msi
  InstallerSha256: CCCC
`)
	got, err := LookupWingetInstaller(context.Background(), "google.chrome")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got.URL != "https://dl.google.com/chrome64.msi" || got.Arch != "x64" {
		t.Errorf("picked %s (%s), want the x64 one", got.URL, got.Arch)
	}
	if got.SHA256 != "bbbb" {
		t.Errorf("sha = %q, want the x64 hash, lowercased", got.SHA256)
	}
	if !got.MSI() || len(got.Args) != 0 {
		t.Errorf("wix should be an MSI with msiexec's own switches, got msi=%v args=%v", got.MSI(), got.Args)
	}
	if got.ID != "Google.Chrome" {
		t.Errorf("id = %q, want the manifest's own spelling", got.ID)
	}
	if got.Version != "153.0.8010.53" {
		t.Errorf("version = %q", got.Version)
	}
	if got.Filename() != "chrome64.msi" {
		t.Errorf("filename = %q", got.Filename())
	}
}

// Firefox's shape: the same build published as both an MSI and an EXE. The MSI
// wins, because msiexec's /qn is the one silent switch that behaves the same
// on every machine.
func TestInstallerPrefersMSIOverEXEOfTheSameBuild(t *testing.T) {
	onePackage(t, "Mozilla.Firefox", "156.0", `
PackageIdentifier: Mozilla.Firefox
PackageVersion: "156.0"
Scope: machine
Installers:
- Architecture: x64
  InstallerType: nullsoft
  InstallerUrl: https://example.invalid/Firefox%20Setup%20156.0.exe
  InstallerSha256: DEAD
  InstallerSwitches:
    Silent: /S /PreventRebootRequired=true
- Architecture: x64
  InstallerType: wix
  InstallerUrl: https://example.invalid/firefox.msi
  InstallerSha256: BEEF
`)
	got, err := LookupWingetInstaller(context.Background(), "Mozilla.Firefox")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if !got.MSI() {
		t.Fatalf("picked %s, want the MSI", got.URL)
	}
}

// A per-installer type overrides the manifest-wide one, and stated switches
// are used verbatim rather than guessed from the type.
func TestInstallerUsesTheSwitchesTheManifestStates(t *testing.T) {
	onePackage(t, "Notepad++.Notepad++", "8.9.8", `
PackageIdentifier: Notepad++.Notepad++
PackageVersion: 8.9.8
InstallerType: exe
Installers:
- Architecture: x64
  InstallerType: nullsoft
  Scope: machine
  InstallerUrl: https://example.invalid/npp%20installer.x64.exe
  InstallerSha256: 1234
  InstallerSwitches:
    Silent: /S /D="C:\Program Files\Notepad++"
`)
	got, err := LookupWingetInstaller(context.Background(), "Notepad++.Notepad++")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	want := []string{"/S", `/D=C:\Program Files\Notepad++`}
	if !reflect.DeepEqual(got.Args, want) {
		t.Errorf("args = %q, want %q", got.Args, want)
	}
	if got.MSI() {
		t.Error("nullsoft is not an MSI")
	}
	// A space in the URL is legal and a nuisance on a stick.
	if got.Filename() != "npp-installer.x64.exe" {
		t.Errorf("filename = %q", got.Filename())
	}
}

// A type with no stated switches gets the ones every installer of that kind
// takes -- that is what recording a type is for.
func TestInstallerFillsInTheSwitchesForAKnownType(t *testing.T) {
	for _, tc := range []struct {
		kind string
		want []string
	}{
		{"inno", []string{"/VERYSILENT", "/SUPPRESSMSGBOXES", "/NORESTART", "/SP-"}},
		{"nullsoft", []string{"/S"}},
		{"burn", []string{"/quiet", "/norestart"}},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			onePackage(t, "Some.Thing", "1.0", `
PackageIdentifier: Some.Thing
PackageVersion: "1.0"
InstallerType: `+tc.kind+`
Installers:
- Architecture: x64
  Scope: machine
  InstallerUrl: https://example.invalid/setup.exe
  InstallerSha256: ABCD
`)
			got, err := LookupWingetInstaller(context.Background(), "Some.Thing")
			if err != nil {
				t.Fatalf("lookup: %v", err)
			}
			if !reflect.DeepEqual(got.Args, tc.want) {
				t.Errorf("args = %q, want %q", got.Args, tc.want)
			}
		})
	}
}

// The refusals. Each of these would otherwise produce a stick that looks
// complete and installs nothing on the machine, so they stop the build.
func TestInstallerRefusesWhatCannotRideOnAStick(t *testing.T) {
	for _, tc := range []struct {
		name     string
		manifest string
		says     string
	}{
		{"store package", `
InstallerType: msstore
Installers:
- Architecture: neutral
  InstallerUrl: https://example.invalid/x
  InstallerSha256: AA
`, "Microsoft Store"},
		{"msix", `
InstallerType: msix
Installers:
- Architecture: x64
  InstallerUrl: https://example.invalid/x.msix
  InstallerSha256: AA
`, "msix"},
		{"nested in an archive", `
InstallerType: zip
NestedInstallerType: exe
Installers:
- Architecture: x64
  InstallerUrl: https://example.invalid/x.zip
  InstallerSha256: AA
`, "zip"},
		{"a Windows feature", `
InstallerType: wix
Installers:
- Architecture: x64
  InstallerUrl: https://example.invalid/x.msi
  InstallerSha256: AA
  Dependencies:
    WindowsFeatures:
    - NetFx3
`, "NetFx3"},
		{"an exe nobody can silence", `
InstallerType: exe
Installers:
- Architecture: x64
  InstallerUrl: https://example.invalid/setup.exe
  InstallerSha256: AA
`, "without a person"},
		{"an http link", `
InstallerType: wix
Installers:
- Architecture: x64
  InstallerUrl: http://example.invalid/x.msi
  InstallerSha256: AA
`, "not https"},
		{"no installers at all", `
InstallerType: wix
Installers: []
`, "no installers"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			onePackage(t, "Some.Thing", "1.0",
				"PackageIdentifier: Some.Thing\nPackageVersion: \"1.0\"\n"+tc.manifest)
			_, err := LookupWingetInstaller(context.Background(), "Some.Thing")
			if !errors.Is(err, ErrNoOfflineInstaller) {
				t.Fatalf("err = %v, want a refusal", err)
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("%q does not say %q", err, tc.says)
			}
		})
	}
}

// Chrome refuses three times for one reason; the operator should read it once.
func TestRefusalDoesNotRepeatItselfOncePerArchitecture(t *testing.T) {
	onePackage(t, "Some.Store", "1.0", `
PackageIdentifier: Some.Store
PackageVersion: "1.0"
InstallerType: msstore
Installers:
- Architecture: x64
  InstallerUrl: https://example.invalid/a
  InstallerSha256: AA
- Architecture: x86
  InstallerUrl: https://example.invalid/b
  InstallerSha256: BB
`)
	_, err := LookupWingetInstaller(context.Background(), "Some.Store")
	if err == nil {
		t.Fatal("want a refusal")
	}
	if n := strings.Count(err.Error(), "Microsoft Store"); n != 1 {
		t.Errorf("reason appears %d times in %q, want once", n, err)
	}
}

// A CDN that serves an installer from a path of digits should still produce a
// name somebody can recognise on the stick.
func TestFilenameFallsBackToTheIDWhenTheURLHasNoUsefulName(t *testing.T) {
	w := WingetInstaller{ID: "Some.Thing", Version: "1.0", Type: "wix",
		URL: "https://example.invalid/download?id=44121"}
	if got := w.Filename(); got != "some.thing-1.0.msi" {
		t.Errorf("filename = %q", got)
	}
}

func TestSplitSwitchesKeepsQuotedPathsWhole(t *testing.T) {
	got := splitSwitches(`/S /v"/qn REBOOT=ReallySuppress" /dir='C:\Program Files\X'`)
	want := []string{"/S", "/v/qn REBOOT=ReallySuppress", `/dir=C:\Program Files\X`}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

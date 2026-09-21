package oscatalog

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/uplinkresearch/dsky/internal/appcatalog"
	"github.com/uplinkresearch/dsky/internal/buildinfo"
	"github.com/uplinkresearch/dsky/internal/library"
	"github.com/uplinkresearch/dsky/internal/recipe"
	"github.com/uplinkresearch/dsky/internal/workspace"
)

// TestSynthesizedRecipesValid builds the ephemeral workspace for every
// catalog entry and confirms the generated recipe + manifest load and
// validate — a broken template or YAML would break Quick Install silently.
func TestSynthesizedRecipesValid(t *testing.T) {
	lib, err := library.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		opts Options
	}{
		{"win-local", Options{Edition: "Pro", AccountMode: "local", Debloat: "standard"}},
		{"win-oobe-bypass", Options{Edition: "Home", AccountMode: "oobe", Debloat: "off", BypassRequirement: true}},
	}
	// Server takes the other path through recipe generation: its editions
	// name images on the media, not client SKUs.
	serverCases := []struct {
		name string
		opts Options
	}{
		{"srv-standard", Options{Edition: "Standard", AccountMode: "local"}},
		{"srv-datacenter-core", Options{Edition: "Datacenter Core", AccountMode: "local"}},
	}
	for _, e := range Catalog() {
		if e.Family != Windows {
			dir, err := scaffoldQuickWorkspace(lib, e, Options{}, nil)
			if err != nil {
				t.Fatalf("%s: scaffold: %v", e.ID, err)
			}
			assertLoads(t, dir, e.ID)
			continue
		}
		active := cases
		if e.IsWindowsServer() {
			active = serverCases
		}
		for _, c := range active {
			dir, err := scaffoldQuickWorkspace(lib, e, c.opts, nil)
			if err != nil {
				t.Fatalf("%s/%s: scaffold: %v", e.ID, c.name, err)
			}
			r := assertLoads(t, dir, e.ID)
			if r == nil {
				continue
			}
			switch {
			case e.IsWindowsServer():
				// No ei.cfg on Server media; the image index is what picks
				// the edition, and an absent one leaves Setup prompting.
				if r.Windows != nil && r.Windows.EICfg != nil {
					t.Errorf("%s/%s: ei_cfg on Server media", e.ID, c.name)
				}
				if r.Windows == nil || r.Windows.Unattend == nil ||
					r.Windows.Unattend.Vars["image_index"] == "" {
					t.Errorf("%s/%s: missing windows/unattend image_index", e.ID, c.name)
				} else if got := r.Windows.Unattend.Vars["image_index"]; got != strconv.Itoa(serverImages[c.opts.Edition]) {
					t.Errorf("%s/%s: image_index %s, want %d for %q",
						e.ID, c.name, got, serverImages[c.opts.Edition], c.opts.Edition)
				}
			case r.Windows == nil || r.Windows.EICfg == nil:
				t.Errorf("%s/%s: missing windows/ei_cfg", e.ID, c.name)
			}
			for _, f := range r.Lint() {
				if f.Severity == "error" {
					t.Errorf("%s/%s: lint error: %s", e.ID, c.name, f.Message)
				}
			}
		}
	}
}

func assertLoads(t *testing.T, dir, id string) *recipe.Recipe {
	t.Helper()
	ws, err := workspace.Load(dir)
	if err != nil {
		t.Fatalf("load workspace %s: %v", dir, err)
	}
	r, err := ws.Recipe(id)
	if err != nil {
		t.Errorf("recipe %s did not load/validate: %v", id, err)
		return nil
	}
	if _, err := ws.Source(id); err != nil {
		t.Errorf("manifest for %s missing: %v", id, err)
	}
	return r
}

// TestHardwareBlockRoundTrips is the guard on auto-detected drivers reaching
// the recipe intact: hardware IDs carry backslashes (PCI\VEN_...), so a
// quoting slip in the generated YAML would either fail to parse or silently
// mangle the ID and match no driver pack.
func TestHardwareBlockRoundTrips(t *testing.T) {
	lib, err := library.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e, ok := Get("windows-11")
	if !ok {
		t.Fatal("windows-11 missing from the catalog")
	}
	hw := []recipe.HardwareSpec{
		{Vendor: "dell", Model: "OptiPlex 7010", OS: "win11"},
		{HWIDs: []string{`PCI\VEN_10DE&DEV_2B85`, `PCI\VEN_8086&DEV_15F3`}, OS: "win11"},
	}
	dir, err := scaffoldQuickWorkspace(lib, e, Options{Edition: "Pro"}, hw)
	if err != nil {
		t.Fatalf("scaffold: %v", err)
	}
	r := assertLoads(t, dir, e.ID)
	if r == nil || r.Windows == nil {
		t.Fatal("recipe did not load")
	}
	got := r.Windows.Hardware
	if len(got) != 2 {
		t.Fatalf("got %d hardware entries, want 2: %+v", len(got), got)
	}
	if got[0].Vendor != "dell" || got[0].Model != "OptiPlex 7010" {
		t.Errorf("vendor entry round-tripped as %+v", got[0])
	}
	want := []string{`PCI\VEN_10DE&DEV_2B85`, `PCI\VEN_8086&DEV_15F3`}
	if len(got[1].HWIDs) != len(want) {
		t.Fatalf("got %d hwids, want %d: %q", len(got[1].HWIDs), len(want), got[1].HWIDs)
	}
	for i, id := range want {
		if got[1].HWIDs[i] != id {
			t.Errorf("hwid %d round-tripped as %q, want %q", i, got[1].HWIDs[i], id)
		}
	}
	for _, f := range r.Lint() {
		if f.Severity == "error" {
			t.Errorf("lint error: %s", f.Message)
		}
	}
	// A machine whose catalogs covered nothing must leave the block out
	// entirely rather than emit an empty `hardware:` key.
	bare, err := scaffoldQuickWorkspace(lib, e, Options{Edition: "Pro"}, nil)
	if err != nil {
		t.Fatalf("scaffold without hardware: %v", err)
	}
	if rb := assertLoads(t, bare, e.ID); rb != nil && rb.Windows != nil && len(rb.Windows.Hardware) != 0 {
		t.Errorf("expected no hardware entries, got %+v", rb.Windows.Hardware)
	}
}

// TestAppsReachTheRecipe checks the program picker's ids survive into the
// synthesized recipe as winget package ids with a step to run them — and that
// asking for a program that does not exist fails the build rather than
// quietly producing media without it.
func TestAppsReachTheRecipe(t *testing.T) {
	lib, err := library.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e, ok := Get("windows-11")
	if !ok {
		t.Fatal("windows-11 missing from the catalog")
	}
	// A typed winget package rides the same path as a listed program.
	opts := Options{Edition: "Pro", Apps: []string{"chrome", "7zip", "winget:Mozilla.Firefox.ESR"}}
	dir, err := scaffoldQuickWorkspace(lib, e, opts, nil)
	if err != nil {
		t.Fatalf("scaffold: %v", err)
	}
	r := assertLoads(t, dir, e.ID)
	if r == nil || r.Windows == nil {
		t.Fatal("recipe did not load")
	}
	if !r.Windows.Apps.Enabled() {
		t.Fatal("windows.apps is empty")
	}
	want := []string{"Google.Chrome", "7zip.7zip", "Mozilla.Firefox.ESR"}
	got := r.Windows.Apps.Winget
	if len(got) != len(want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("package %d = %q, want %q", i, got[i], want[i])
		}
	}
	hasApps := false
	for _, s := range r.Windows.Firstboot.Steps {
		if s.Apps {
			hasApps = true
		}
	}
	if !hasApps {
		t.Error("recipe has apps but no `apps` firstboot step — they would never install")
	}
	if ps := recipe.GenerateAppsPS(r); !strings.Contains(ps, "'Mozilla.Firefox.ESR',") {
		t.Errorf("apps.ps1 does not install the typed package:\n%s", ps)
	}
	for _, f := range r.Lint() {
		if f.Severity == "error" {
			t.Errorf("lint error: %s", f.Message)
		}
	}

	// No apps selected must leave the block (and the step) out entirely.
	bare, err := scaffoldQuickWorkspace(lib, e, Options{Edition: "Pro"}, nil)
	if err != nil {
		t.Fatalf("scaffold without apps: %v", err)
	}
	if rb := assertLoads(t, bare, e.ID); rb != nil && rb.Windows.Apps.Enabled() {
		t.Error("expected no apps block")
	}
}

// TestCatalogEntriesWellFormed guards the hand-maintained OS list: a typo in
// an id, a missing filename, or an unpinned image would only surface as a
// confusing failure partway through someone's install.
func TestCatalogEntriesWellFormed(t *testing.T) {
	idRe := regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
	seen := map[string]bool{}
	for _, e := range Catalog() {
		if !idRe.MatchString(e.ID) {
			t.Errorf("%q is not a usable id (lowercase, digits, dot, dash, underscore)", e.ID)
		}
		if seen[e.ID] {
			t.Errorf("duplicate catalog id %q", e.ID)
		}
		seen[e.ID] = true
		if e.Name == "" || e.Notes == "" {
			t.Errorf("%s: needs a name and notes — they are all the picker shows", e.ID)
		}
		// An import-only entry is a pointer to a vendor portal, not a download:
		// the pinning rules below are about fetching, so they do not apply. What
		// it must have instead is checked by TestImportOnlyEntriesAreFeatureGated.
		if e.ImportOnly() {
			continue
		}
		switch e.Family {
		case Windows:
			if e.Provider != "fido" || e.Fido == nil {
				t.Errorf("%s: Windows entries resolve their ISO through Fido", e.ID)
			}
			if len(e.Editions) == 0 {
				t.Errorf("%s: Windows entries need editions (the key table is keyed by them)", e.ID)
			}
		case Linux:
			if e.URL == "" || e.Filename == "" {
				t.Errorf("%s: Linux entries need a url and a filename", e.ID)
			}
			// Unpinned would mean downloading gigabytes and trusting whatever
			// arrived. Immutable artifacts carry a hash; rolling ones resolve
			// theirs from the vendor's checksum file at pull time.
			if e.SHA256 == "" && e.ChecksumsURL == "" {
				t.Errorf("%s: unpinned — needs either sha256 or a checksums_url", e.ID)
			}
			if e.SHA256 != "" && len(e.SHA256) != 64 {
				t.Errorf("%s: sha256 is %d characters, want 64", e.ID, len(e.SHA256))
			}
		default:
			t.Errorf("%s: unknown family %q", e.ID, e.Family)
		}
		if !strings.HasPrefix(e.URL, "https://") && e.URL != "" {
			t.Errorf("%s: url must be https", e.ID)
		}
	}
}

// TestResolveChecksumLive exercises the rolling-image path against the real
// checksum file: the filename has to still match a line in it, which is
// exactly what silently rots when a distro changes its layout. Network, so
// it is skipped under -short (which is what CI runs).
func TestResolveChecksumLive(t *testing.T) {
	if testing.Short() {
		t.Skip("network test")
	}
	for _, e := range Catalog() {
		if e.ChecksumsURL == "" {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		sum, err := resolveChecksum(ctx, e)
		cancel()
		if err != nil {
			t.Errorf("%s: %v", e.ID, err)
			continue
		}
		if len(sum) != 64 {
			t.Errorf("%s: resolved checksum %q is not a sha256", e.ID, sum)
		}
		t.Logf("%s -> %s", e.ID, sum)
	}
}

// TestCustomAppReachesTheMedia is the whole point of operator-supplied
// installers: the file has to be staged onto the stick AND run at first boot.
// Either half alone is useless — a staged file nobody runs, or a run step
// pointing at a file that was never copied, which fails the build outright.
func TestCustomAppReachesTheMedia(t *testing.T) {
	root := t.TempDir()
	if err := appcatalog.LoadCustom(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = appcatalog.LoadCustom(t.TempDir()) })
	agent := appcatalog.Custom{
		ID: "macula", Name: "Macula Agent", Format: "msi",
		SHA256: strings.Repeat("d", 64), Filename: "MaculaAgent.msi",
		Args: []string{"/qn", "/norestart"},
	}
	if err := appcatalog.AddCustom(root, agent, false); err != nil {
		t.Fatal(err)
	}

	lib, err := library.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	win, ok := Get("windows-11")
	if !ok {
		t.Skip("no windows entry in this catalog")
	}
	dir, err := scaffoldQuickWorkspace(lib, win, Options{Apps: []string{"macula", "chrome"}}, nil)
	if err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "recipes", win.ID+".yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"payload:",
		"{ ref: app-macula }",
		`msi: { ref: app-macula, args: ["/qn", "/norestart"] }`,
		"Google.Chrome", // the winget half must still be there
	} {
		if !strings.Contains(string(got), want) {
			t.Errorf("generated recipe is missing %q:\n%s", want, got)
		}
	}

	// The manifest is what turns that ref into the library blob; without it
	// the build fails on an unresolved source.
	man, err := os.ReadFile(filepath.Join(dir, "manifests", "app-macula.yaml"))
	if err != nil {
		t.Fatalf("no manifest for the staged installer: %v", err)
	}
	for _, want := range []string{"id: app-macula", "kind: payload", "format: msi", agent.SHA256} {
		if !strings.Contains(string(man), want) {
			t.Errorf("manifest missing %q:\n%s", want, man)
		}
	}

	// And the whole thing still has to load and validate as a recipe.
	if r := assertLoads(t, dir, win.ID); r != nil {
		if len(r.Windows.Payload) != 1 || r.Windows.Payload[0].Ref != "app-macula" {
			t.Errorf("payload did not survive parsing: %+v", r.Windows.Payload)
		}
		ran := false
		for _, s := range r.Windows.Firstboot.Steps {
			if s.MSI != nil && s.MSI.Ref == "app-macula" {
				ran = true
			}
		}
		if !ran {
			t.Error("the installer is staged but never run — it would sit on the stick doing nothing")
		}
	}
}

// TestQuickInstallOfflineDomainJoin: Quick Install offers the offline path and
// only the offline path. A credentialed join needs a password, and a password
// on a command line lands in shell history — on top of the cleartext copy it
// already leaves on the stick — so that belongs in a workspace recipe where
// vars.local.yaml is gitignored.
func TestQuickInstallOfflineDomainJoin(t *testing.T) {
	lib, err := library.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e, ok := Get("windows-11")
	if !ok {
		t.Skip("no windows entry")
	}
	blob := filepath.Join(t.TempDir(), "pc01.odj")
	dir, err := scaffoldQuickWorkspace(lib, e, Options{Edition: "Pro", DomainBlob: blob}, nil)
	if err != nil {
		t.Fatal(err)
	}
	r := assertLoads(t, dir, e.ID)
	if r == nil || r.Windows == nil {
		t.Fatal("recipe did not load")
	}
	d := r.Windows.Domain
	if !d.Enabled() || !d.Offline() {
		t.Fatalf("offline domain join did not reach the recipe: %+v", d)
	}
	if d.Blob != filepath.ToSlash(blob) {
		t.Errorf("blob path = %q, want %q", d.Blob, filepath.ToSlash(blob))
	}
	// Nothing credentialed may appear, however the recipe was generated.
	if d.Join != "" || d.Username != "" || d.Password != "" {
		t.Errorf("Quick Install emitted join credentials: %+v", d)
	}
	for _, f := range r.Lint() {
		if f.Severity == "error" {
			t.Errorf("lint error: %s", f.Message)
		}
	}

	// And no domain block at all when none was asked for.
	bare, err := scaffoldQuickWorkspace(lib, e, Options{Edition: "Pro"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rb := assertLoads(t, bare, e.ID); rb != nil && rb.Windows.Domain.Enabled() {
		t.Error("a build with no --domain-blob produced a domain join")
	}
}

// TestCustomAppManifestSurvivesPruning: the OS manifest of a previous build is
// cleared between Quick Installs, and an installer swept up with it would
// break the build that was just written to refer to it.
func TestCustomAppManifestSurvivesPruning(t *testing.T) {
	dir := t.TempDir()
	man := filepath.Join(dir, "app-macula.yaml")
	if err := os.WriteFile(man, []byte("id: app-macula\nkind: payload\nformat: msi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "old-os.yaml"), []byte("id: old-os\nkind: os-image\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pruneOtherManifests(dir, "windows-11", nil)
	if _, err := os.Stat(man); err != nil {
		t.Error("the staged installer's manifest was pruned — the build would fail on an unresolved ref")
	}
	if _, err := os.Stat(filepath.Join(dir, "old-os.yaml")); err == nil {
		t.Error("a stale OS manifest was kept")
	}
}

// TestCatalogURLsLive is the link-rot alarm. Distros move and prune: Garuda
// and PikaOS publish no permanent alias and delete old builds outright, and a
// pinned URL that has become a 404 is invisible here — the tests pass, the
// index publishes, and the first person to learn is a user whose download
// fails.
//
// It checks reachability, not content. Verifying the bytes would mean
// downloading well over a hundred gigabytes; the pinned hash already catches
// a changed file at pull time, and this catches the case that check never
// reaches. Every entry is reported before failing, so one run says everything
// that rotted rather than only the first thing.
//
// Network, so it is skipped under -short. CI runs -short; the scheduled
// catalog-health workflow is what runs this.
func TestCatalogURLsLive(t *testing.T) {
	if testing.Short() {
		t.Skip("network test")
	}
	client := &http.Client{Timeout: 90 * time.Second}
	for _, e := range Catalog() {
		if e.ImportOnly() {
			continue // nothing to fetch by design
		}
		if e.URL == "" {
			continue // Windows resolves through Fido at pull time
		}
		size, err := reachable(client, e.URL)
		switch {
		case err != nil:
			t.Errorf("%s: %s\n    %v", e.ID, e.URL, err)
		// Every entry here is an OS image; anything this small is an error
		// page or a stub that happened to return 200.
		case size >= 0 && size < 100<<20:
			t.Errorf("%s: %s\n    returned only %d bytes — not an OS image", e.ID, e.URL, size)
		default:
			t.Logf("%s: ok (%d MiB)", e.ID, size>>20)
		}
	}
}

// reachable reports the size the server declares for a URL. HEAD first, since
// it costs nothing; some hosts refuse it, so a refusal falls back to asking
// for the first byte rather than being reported as rot.
func reachable(client *http.Client, url string) (int64, error) {
	req, err := http.NewRequest(http.MethodHead, url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", buildinfo.UserAgent())
	resp, err := client.Do(req)
	if err == nil {
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return resp.ContentLength, nil
		}
	}

	rangeReq, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	rangeReq.Header.Set("User-Agent", buildinfo.UserAgent())
	rangeReq.Header.Set("Range", "bytes=0-0")
	r2, err2 := client.Do(rangeReq)
	if err2 != nil {
		return 0, err2
	}
	defer r2.Body.Close()
	_, _ = io.Copy(io.Discard, r2.Body)
	if r2.StatusCode != http.StatusOK && r2.StatusCode != http.StatusPartialContent {
		return 0, fmt.Errorf("HEAD and ranged GET both refused (GET returned HTTP %d)", r2.StatusCode)
	}
	// A 206 states the full length after the slash in Content-Range.
	if cr := r2.Header.Get("Content-Range"); cr != "" {
		if i := strings.LastIndex(cr, "/"); i >= 0 {
			if n, err := strconv.ParseInt(cr[i+1:], 10, 64); err == nil {
				return n, nil
			}
		}
	}
	return -1, nil // reachable, size not stated
}

// TestRawImageEntriesCompose guards the single-board path: a Raspberry Pi
// image is a compressed raw disk image, not an installer ISO, so it has to
// reach compose as raw-img or it would be written as if it were bootable
// media for a PC.
func TestRawImageEntriesCompose(t *testing.T) {
	lib, err := library.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	raw := 0
	for _, e := range Catalog() {
		if e.Kind() != ImageRaw {
			continue
		}
		raw++
		dir, err := scaffoldQuickWorkspace(lib, e, Options{}, nil)
		if err != nil {
			t.Fatalf("%s: scaffold: %v", e.ID, err)
		}
		r := assertLoads(t, dir, e.ID)
		if r == nil {
			continue
		}
		if got := string(r.OS.Type); got != "raw-img" {
			t.Errorf("%s: os.type = %q, want raw-img", e.ID, got)
		}
		src, err := workspace.Load(dir)
		if err != nil {
			t.Fatal(err)
		}
		s, err := src.Source(e.ID)
		if err != nil {
			t.Fatalf("%s: manifest: %v", e.ID, err)
		}
		if s.Format != "img" {
			t.Errorf("%s: manifest format = %q, want img", e.ID, s.Format)
		}
		for _, f := range r.Lint() {
			if f.Severity == "error" {
				t.Errorf("%s: lint error: %s", e.ID, f.Message)
			}
		}
	}
	if raw == 0 {
		t.Skip("no raw-image entries in the catalog")
	}
}

// TestGenericKeysComplete makes sure every Windows edition option has a key
// and an ei.cfg name.
func TestGenericKeysComplete(t *testing.T) {
	for _, e := range Catalog() {
		if e.Family != Windows {
			continue
		}
		for _, ed := range e.Editions {
			// Server editions name images on the media rather than client
			// SKUs: no generic key exists for them, and evaluation media
			// takes no key at all.
			if e.IsWindowsServer() {
				if serverImages[ed] == 0 {
					t.Errorf("%s: no image index for edition %q", e.ID, ed)
				}
				continue
			}
			if genericKeys[ed] == "" {
				t.Errorf("%s: no generic key for edition %q", e.ID, ed)
			}
			if editionName(ed) == "" {
				t.Errorf("%s: no ei.cfg name for edition %q", e.ID, ed)
			}
		}
	}
}

// TestSaveRecipeIntoAScaffoldedWorkspace saves Quick Install options as named
// recipes into a real `dsky init` workspace and requires that they load, lint
// clean, keep the name somebody chose, and leave the workspace's own files
// alone — a saved recipe must never overwrite a hand-edited template.
func TestSaveRecipeIntoAScaffoldedWorkspace(t *testing.T) {
	lib, err := library.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	wsDir := filepath.Join(t.TempDir(), "acme-workspace")
	if err := workspace.Scaffold(wsDir, "Acme IT"); err != nil {
		t.Fatal(err)
	}
	ownTemplate := filepath.Join(wsDir, "templates", "autounattend.xml.tmpl")
	before, err := os.ReadFile(ownTemplate)
	if err != nil {
		t.Fatal(err)
	}

	win, ok := Get("windows-11")
	if !ok {
		t.Fatal("windows-11 missing from catalog")
	}
	winOpts := Options{Edition: "Pro", AccountMode: "local", Debloat: "standard", BypassRequirement: true}
	if _, err := SaveRecipe(context.Background(), lib, wsDir, "front-desk-pc", "Front desk PC", win, winOpts, nil); err != nil {
		t.Fatalf("save windows recipe: %v", err)
	}
	var linux Entry
	for _, e := range Catalog() {
		if e.Family == Linux && !e.ImportOnly() {
			linux = e
			break
		}
	}
	if _, err := SaveRecipe(context.Background(), lib, wsDir, "lab-linux", "Lab Linux", linux, Options{}, nil); err != nil {
		t.Fatalf("save %s recipe: %v", linux.ID, err)
	}

	ws, err := workspace.Load(wsDir)
	if err != nil {
		t.Fatal(err)
	}
	r, err := ws.Recipe("front-desk-pc")
	if err != nil {
		t.Fatalf("saved windows recipe does not load: %v", err)
	}
	if r.Name != "Front desk PC" || r.OS.Source != win.ID {
		t.Errorf("windows recipe: name %q source %q", r.Name, r.OS.Source)
	}
	if r.Windows == nil || r.Windows.Unattend.Template != savedTemplate {
		t.Errorf("windows recipe does not use its own template: %+v", r.Windows)
	}
	for _, f := range r.Lint() {
		if f.Severity == "error" {
			t.Errorf("windows recipe lint: %s", f.Message)
		}
	}
	if _, err := ws.Source(win.ID); err != nil {
		t.Errorf("no manifest for the OS image: %v", err)
	}
	if lr, err := ws.Recipe("lab-linux"); err != nil || lr.OS.Source != linux.ID {
		t.Errorf("linux recipe: %v %+v", err, lr)
	}
	// The example recipes `dsky init` wrote are still there and still load.
	if all, err := ws.Recipes(); err != nil || len(all) < 4 {
		t.Errorf("workspace recipes after saving: %d, %v", len(all), err)
	}
	after, _ := os.ReadFile(ownTemplate)
	if string(after) != string(before) {
		t.Error("saving a recipe changed the workspace's own template")
	}

	if _, err := SaveRecipe(context.Background(), lib, wsDir, "front-desk-pc", "Again", win, winOpts, nil); err == nil ||
		!strings.Contains(err.Error(), "already exists") {
		t.Errorf("saving over an existing recipe: %v", err)
	}
}

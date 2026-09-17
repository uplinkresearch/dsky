// Package oscatalog is the built-in list of installable operating systems and
// the Quick-Install flow: pick an OS, choose a few options, and build media
// with no workspace to author. It synthesizes an ephemeral workspace and
// recipe and runs them through the normal compose pipeline.
package oscatalog

import (
	"context"
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/uplinkresearch/dsky/internal/appcatalog"
	"github.com/uplinkresearch/dsky/internal/compose"
	"github.com/uplinkresearch/dsky/internal/driverresolve"
	"github.com/uplinkresearch/dsky/internal/helpers"
	"github.com/uplinkresearch/dsky/internal/library"
	"github.com/uplinkresearch/dsky/internal/manifest"
	"github.com/uplinkresearch/dsky/internal/recipe"
	"github.com/uplinkresearch/dsky/internal/workspace"
)

//go:embed templates/*.tmpl
var templatesFS embed.FS

// Family groups OSes.
type Family string

const (
	Windows Family = "windows"
	Linux   Family = "linux"
)

// Category groups the catalog for the pickers. A flat list stops being
// browsable somewhere around a dozen entries, and "I want a server" is a
// different errand from "I want to try a desktop".
type Category string

const (
	Desktop   Category = "desktop"
	Server    Category = "server"
	Appliance Category = "appliance" // single-board and purpose-built images
)

// ImageKind says how the downloaded file is written. Most installers are
// hybrid ISOs; single-board images are raw disk images, usually compressed,
// which take a different compose path.
type ImageKind string

const (
	ImageISO ImageKind = "iso"
	ImageRaw ImageKind = "raw"
)

// Entry is one installable OS. The json tags are load-bearing: the same
// struct is serialised into the signed catalog index, so the shipped list
// and the published one cannot drift apart.
type Entry struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Family        Family   `json:"family"`
	Category      Category `json:"category,omitempty"`
	Version       string   `json:"version,omitempty"`
	Notes         string   `json:"notes,omitempty"`
	FirmwareNotes string   `json:"firmware_notes,omitempty"`

	// Image defaults to ImageISO. Arch defaults to amd64 and exists so an
	// arm64 board image is never offered as if it would boot a PC.
	Image ImageKind `json:"image,omitempty"`
	Arch  string    `json:"arch,omitempty"`

	// Windows sources resolve their ISO at pull time via Fido; Linux
	// sources pin url + sha256 (or a ChecksumsURL to verify against).
	Provider     string             `json:"provider,omitempty"`
	Fido         *manifest.FidoSpec `json:"fido,omitempty"`
	URL          string             `json:"url,omitempty"`
	SHA256       string             `json:"sha256,omitempty"`
	ChecksumsURL string             `json:"checksums_url,omitempty"` // fetch + verify when SHA256 is empty
	Filename     string             `json:"filename,omitempty"`

	// ImportFrom is where a person goes to download this image by hand, for
	// the entries that have no URL anything can fetch — an account-walled
	// vendor portal, typically. Such entries also carry
	// Requires: ["import-only"], so a build that predates this feature skips
	// them instead of offering a download that cannot happen.
	ImportFrom string `json:"import_from,omitempty"`

	// Requires names capabilities an entry needs from the program reading
	// it. A build that does not know one of them skips the entry rather
	// than offering something it cannot build — this is what lets a newer
	// catalog stay safe to read on an older binary.
	Requires []string `json:"requires,omitempty"`

	// Windows-only options.
	Editions []string `json:"editions,omitempty"` // e.g. Pro, Home
}

// Options are the Quick-Install choices.
type Options struct {
	Edition           string // windows: Pro | Home
	AccountMode       string // windows: local | oobe
	Debloat           string // windows: off | standard | aggressive
	BypassRequirement bool   // windows: skip TPM/SecureBoot/RAM checks
	// Hardware asks for driver packs to be found and staged for these
	// machines (windows only) — what hwdetect found on this box, via
	// driverresolve.SpecsFor. Entries no catalog covers are dropped: a
	// detected GPU the vendors do not carry a pack for (common; Windows
	// Update covers most) must not fail a build.
	Hardware []recipe.HardwareSpec
	// Kept are the driver packs a recipe being edited already had. They were
	// resolved when it was saved and their manifests are in the workspace, so
	// they are written through as they are: re-resolving them fetched the
	// vendor pack again on a machine that no longer had it -- a gigabyte to
	// rename a recipe -- and dropped the machine from the recipe entirely if
	// the vendor's feed could not be reached at that moment.
	Kept []recipe.HardwareSpec
	// Models are machines somebody named themselves, for imaging computers
	// they are not sitting at. These are never dropped. Dropping one was
	// silent -- a recipe saved with "HP EliteBook x360 1040 G8" picked in the
	// dialog came back with no drivers in it at all and no error anywhere,
	// because a pack that could not be resolved right then was quietly left
	// out along with the machine that asked for it.
	Models []recipe.HardwareSpec
	// Apps are appcatalog picker ids to install with the operating system: at
	// first boot through winget on Windows, and through the installer's own
	// answers on Ubuntu (apt, snaps, Flathub, a vendor's repository).
	Apps []string
	// ThirdPartyDrivers is "drivers for this computer" on Ubuntu, where that
	// means the proprietary ones the kernel does not carry — NVIDIA above
	// all. Ubuntu's installer finds and installs them itself when the answers
	// ask it to, so there is nothing to detect and nothing to stage, which is
	// why this is a flag rather than Hardware above.
	ThirdPartyDrivers bool
	// DomainBlob is a file from `djoin /provision`: an offline domain join.
	//
	// Only the offline path is offered here, and that asymmetry is deliberate.
	// A credentialed join needs a password, and a password passed to a command
	// lands in shell history and in the terminal scrollback of whoever is
	// watching — on top of the cleartext copy it already leaves on the stick.
	// A workspace recipe keeps it in vars.local.yaml, which is gitignored, so
	// that is where credentialed joins belong.
	DomainBlob string
	// DomainBlobsDir is a folder of join files named by serial number, so one
	// stick joins a batch of computers (windows.domain.blobs_by_serial).
	DomainBlobsDir string
}

// sourceFormat is the manifest format for this entry's download. Raw images
// arrive compressed (.img.xz); the flash engine sniffs and decompresses on
// the way to the device, so the manifest just says "img".
func (e Entry) sourceFormat() manifest.Format {
	if e.Kind() == ImageRaw {
		return manifest.FormatImg
	}
	return manifest.FormatISO
}

// recipeOSType is the compose pipeline this entry runs through.
func (e Entry) recipeOSType() string {
	if e.Kind() == ImageRaw {
		return "raw-img"
	}
	return "linux-iso"
}

// FeatureImportOnly marks an entry with no URL this tool can fetch: the
// operator downloads the ISO themselves and hands it over. Red Hat
// Enterprise Linux is the case that forced it — its images sit behind an
// account and expiring signed URLs, so nothing stays pinnable — and Windows
// Server is the same shape. Naming it as a required feature means a binary
// built before this existed skips the entry rather than offering a download
// that cannot happen.
const FeatureImportOnly = "import-only"

// ImportOnly reports whether this entry can only be supplied by hand. It
// reads the feature list rather than inferring from an empty URL, because
// that list is also what older builds gate on: the two must agree.
func (e Entry) ImportOnly() bool {
	for _, f := range e.Requires {
		if f == FeatureImportOnly {
			return true
		}
	}
	return false
}

// ImportOnlyError explains how to supply an image this tool cannot fetch.
// One wording, used by the CLI, the portal and the wizard alike, because
// this is the error most likely to be someone's first surprise.
func (e Entry) ImportOnlyError() error {
	where := e.ImportFrom
	if where == "" {
		where = "the vendor"
	}
	return fmt.Errorf("%s cannot be downloaded automatically — get the ISO from %s, then: dsky install %s --iso <file>",
		e.Name, where, e.ID)
}

// ImageKind is the entry's image kind, defaulting to a hybrid ISO.
func (e Entry) Kind() ImageKind {
	if e.Image != "" {
		return e.Image
	}
	return ImageISO
}

// CPUArch is the entry's architecture, defaulting to amd64.
func (e Entry) CPUArch() string {
	if e.Arch != "" {
		return e.Arch
	}
	return "amd64"
}

// Group is the entry's category, defaulting by family: Windows and Linux
// entries that do not say otherwise are desktops.
func (e Entry) Group() Category {
	if e.Category != "" {
		return e.Category
	}
	return Desktop
}

// DriverOS is the driver-catalog OS token for this entry ("win11"/"win10").
func (e Entry) DriverOS() string {
	if e.Fido != nil && e.Fido.Win == "10" {
		return "win10"
	}
	return "win11"
}

// resolvedApps splits the picker ids into winget packages and the operator's
// own installers. The error is dropped on purpose: BuildQuick validates the
// same list up front and refuses the build, so by the time the recipe is
// rendered these all resolve.
func (o Options) resolvedApps() ([]string, []appcatalog.Custom) {
	pkgs, custom, _ := appcatalog.Resolve(o.Apps)
	return pkgs, custom
}

func (o *Options) defaults(e Entry) {
	if o.Edition == "" && len(e.Editions) > 0 {
		o.Edition = e.Editions[0]
	}
	if o.AccountMode == "" {
		o.AccountMode = "local"
	}
	if o.Debloat == "" {
		o.Debloat = "standard"
	}
}

// genericKeys are Microsoft's public edition-select keys (they choose the
// edition Setup installs; activation still needs a real license).
var genericKeys = map[string]string{
	"Pro":        "VK7JG-NPHTM-C97JM-9MPGT-3V66T",
	"Home":       "YTMG3-N6DKC-DKB77-7M9GH-8HVX7",
	"Pro N":      "2B87N-8KFHP-DKV6R-Y2C8J-PKCKT",
	"Education":  "YNMGQ-8RYV3-4PGQ3-C8XTP-7CFBY",
	"Enterprise": "XGVPP-NMH47-7TTHJ-W3FW7-8HV2C",
}

// Builtin is the list compiled into this program — the fallback when no
// published index has been verified. Catalog() is what callers want.
func Builtin() []Entry { return builtin }

// quickMu serialises everything that writes the Quick Install workspace. It is
// one directory, rewritten and pruned to a single recipe on every build, so
// two builds at once — setting up one image while writing another, which the
// portal now allows — would delete each other's recipe mid-build.
var quickMu sync.Mutex

// quickTemplate is where the Quick Install workspace keeps its answer file.
const quickTemplate = "templates/autounattend.xml.tmpl"

// savedTemplate is the answer file a saved recipe uses. A different name from
// the one `dsky init` scaffolds, because a workspace's own template is often
// edited by hand, and saving a recipe must never overwrite it.
const savedTemplate = "templates/dsky-windows-autounattend.xml.tmpl"

// Fetch downloads this entry's OS image into the library if it is not there
// already, with nothing built and no stick involved.
func Fetch(ctx context.Context, lib *library.Library, e Entry, progress func(stage string, done, total int64)) error {
	return ensureSource(ctx, lib, e, progress)
}

// SaveRecipe writes the Quick Install options for e as a named recipe in the
// workspace at wsDir, so the same media can be built again later — by anyone
// who has that workspace — without re-entering the options.
//
// Driver packs are resolved now and pinned into the workspace, because compose
// requires every windows.hardware entry to match a staged pack, and a saved
// recipe must build as saved rather than depending on whatever the catalogs
// say on the day it is built. That can download, so it takes progress.
//
// The OS image is not downloaded: a recipe says what to build, and building
// fetches what is missing.
func SaveRecipe(ctx context.Context, lib *library.Library, wsDir, id, name string, e Entry, opts Options, progress func(stage string, done, total int64)) (string, error) {
	opts.defaults(e)
	if e.Family == Windows && genericKeys[opts.Edition] == "" {
		return "", fmt.Errorf("unknown Windows edition %q (have: %s)", opts.Edition, strings.Join(e.Editions, ", "))
	}
	if err := checkPrograms(e, opts.Apps); err != nil {
		return "", err
	}
	path := filepath.Join(wsDir, "recipes", id+".yaml")
	if _, err := os.Stat(path); err == nil {
		return "", fmt.Errorf("a recipe called %s already exists in this workspace — choose another name", id)
	}
	for _, sub := range []string{"templates", "manifests", "recipes", "payload"} {
		if err := os.MkdirAll(filepath.Join(wsDir, sub), 0o755); err != nil {
			return "", err
		}
	}
	// Existing files are left alone: another recipe may depend on them, and
	// somebody may have edited them.
	writeIfMissing := func(rel string, data []byte) error {
		p := filepath.Join(wsDir, filepath.FromSlash(rel))
		if _, err := os.Stat(p); err == nil {
			return nil
		}
		return os.WriteFile(p, data, 0o644)
	}
	if e.Family == Windows {
		tmpl, err := templatesFS.ReadFile("templates/autounattend.xml.tmpl")
		if err != nil {
			return "", err
		}
		if err := writeIfMissing(savedTemplate, tmpl); err != nil {
			return "", err
		}
	}
	if err := writeIfMissing("manifests/"+e.ID+".yaml", []byte(manifestYAML(e))); err != nil {
		return "", err
	}
	if _, customApps := opts.resolvedApps(); len(customApps) > 0 {
		for _, c := range customApps {
			if err := writeIfMissing("manifests/"+c.SourceID()+".yaml", []byte(customManifestYAML(c))); err != nil {
				return "", err
			}
		}
	}
	var hw []recipe.HardwareSpec
	if e.Family == Windows {
		var err error
		if hw, err = resolveDrivers(ctx, lib, wsDir, opts, progress); err != nil {
			return "", err
		}
	}
	meta := recipeMeta{ID: id, Name: name, Template: savedTemplate}
	// ProgramsSupported is what makes these answers meaningful: they are
	// Ubuntu's autoinstall, and appending them to any other distro's ISO
	// gives it a CIDATA partition its installer will never read. Checked here
	// rather than trusted from the caller, because all three of them build
	// these options separately.
	if e.Family == Linux && e.ProgramsSupported() && (len(opts.Apps) > 0 || opts.ThirdPartyDrivers) {
		rel, err := writeLinuxAnswers(wsDir, id, e, opts)
		if err != nil {
			return "", err
		}
		meta.UserData = rel
	}
	if err := os.WriteFile(path, []byte(recipeYAML(meta, e, opts, hw)), 0o644); err != nil {
		return "", err
	}
	// Loaded back before reporting success, so a recipe that would not build
	// is not left behind looking saved.
	ws, err := workspace.Load(wsDir)
	if err == nil {
		_, err = ws.Recipe(id)
	}
	if err != nil {
		os.Remove(path)
		return "", fmt.Errorf("the saved recipe did not load back: %w", err)
	}
	return path, nil
}

// BuildQuick pulls the OS (if needed), synthesizes an ephemeral workspace and
// recipe from the entry + options, and composes flashable media.
func BuildQuick(ctx context.Context, lib *library.Library, e Entry, opts Options, progress func(stage string, done, total int64)) (*compose.Artifact, error) {
	quickMu.Lock()
	defer quickMu.Unlock()
	opts.defaults(e)
	if e.Family == Windows && genericKeys[opts.Edition] == "" {
		return nil, fmt.Errorf("unknown Windows edition %q (have: %s)", opts.Edition, strings.Join(e.Editions, ", "))
	}
	// Fail before downloading gigabytes, not after.
	if err := checkPrograms(e, opts.Apps); err != nil {
		return nil, err
	}
	if e.Family == Windows {
		if err := helpers.WindowsMediaToolsError(lib.HelpersDir()); err != nil {
			return nil, err
		}
	}

	if err := ensureSource(ctx, lib, e, progress); err != nil {
		return nil, err
	}
	wsDir, err := scaffoldQuickWorkspace(lib, e, opts, nil)
	if err != nil {
		return nil, err
	}
	ws, err := workspace.Load(wsDir)
	if err != nil {
		return nil, err
	}

	// Driver auto-resolve rewrites the recipe, because compose requires every
	// windows.hardware entry to match a staged pack: a detected GPU the
	// catalogs don't carry (common — Windows Update covers most) must not
	// fail the build, so only what resolved goes in. A machine named by hand
	// is not in that bargain: if its pack cannot be found, the build stops
	// and says so, rather than writing a stick with no drivers for exactly
	// the computer it was made for.
	if e.Family == Windows && (len(opts.Hardware) > 0 || len(opts.Models) > 0) {
		specs, err := resolveDrivers(ctx, lib, wsDir, opts, progress)
		if err != nil {
			return nil, err
		}
		if len(specs) > 0 {
			if err := writeQuickRecipe(wsDir, e, opts, specs); err != nil {
				return nil, err
			}
			if ws, err = workspace.Load(wsDir); err != nil {
				return nil, err
			}
		}
	}

	r, err := ws.Recipe(e.ID)
	if err != nil {
		return nil, err
	}
	return compose.Build(ctx, compose.Request{
		Workspace: ws, Library: lib, Recipe: r,
		Progress: progress,
	})
}

// BuildQuickPayload builds the chosen programs and drivers as a payload for a
// machine that already runs Windows -- the Install screen's options, without
// installing anything.
//
// It is BuildQuick without the operating system, and that is the whole point:
// no ISO is fetched, and the media tools are not needed, because nothing reads
// an ISO or splits a WIM. The quick workspace it builds through still names
// the catalog entry, which is how the recipe knows what edition's programs
// were chosen; the bytes of that entry are never touched.
func BuildQuickPayload(ctx context.Context, lib *library.Library, e Entry, opts Options, progress func(stage string, done, total int64)) (*compose.Artifact, error) {
	quickMu.Lock()
	defer quickMu.Unlock()
	if e.Family != Windows {
		return nil, fmt.Errorf("%s is not Windows; a payload sets up programs on a machine that already runs Windows", e.Name)
	}
	opts.defaults(e)
	if err := checkPrograms(e, opts.Apps); err != nil {
		return nil, err
	}
	wsDir, err := scaffoldQuickWorkspace(lib, e, opts, nil)
	if err != nil {
		return nil, err
	}
	if len(opts.Hardware) > 0 || len(opts.Models) > 0 {
		specs, err := resolveDrivers(ctx, lib, wsDir, opts, progress)
		if err != nil {
			return nil, err
		}
		if len(specs) > 0 {
			if err := writeQuickRecipe(wsDir, e, opts, specs); err != nil {
				return nil, err
			}
		}
	}
	ws, err := workspace.Load(wsDir)
	if err != nil {
		return nil, err
	}
	r, err := ws.Recipe(e.ID)
	if err != nil {
		return nil, err
	}
	return compose.BuildPayload(ctx, compose.Request{
		Workspace: ws, Library: lib, Recipe: r,
		Progress: progress,
	})
}

// InLibrary reports whether this entry's OS image is already local, so a
// caller can skip both the fetch and a re-import.
func InLibrary(lib *library.Library, e Entry) bool {
	_, err := lib.Resolve(e.ID)
	return err == nil
}

// CheckISO validates an ISO path before any expensive work starts, so a typo
// fails immediately rather than after "hashing several GB" has been announced.
func CheckISO(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	if st.IsDir() {
		return fmt.Errorf("%s is a directory; point --iso at the .iso file", path)
	}
	if !strings.EqualFold(filepath.Ext(path), ".iso") {
		return fmt.Errorf("%s is not a .iso", filepath.Base(path))
	}
	if st.Size() < 1<<30 {
		return fmt.Errorf("%s is only %d MiB — that is too small to be an OS installer ISO",
			filepath.Base(path), st.Size()>>20)
	}
	return nil
}

// ImportISO files an ISO the operator downloaded themselves under this
// entry's id, so Quick Install uses it instead of fetching. Microsoft
// rate-limits the on-demand Windows fetch hard enough (roughly one per day
// per address) that "download it yourself once" is a normal path, not a
// fallback — and there is otherwise no way to hand Quick Install an ISO,
// since `sources import` needs a workspace and a manifest.
func ImportISO(lib *library.Library, e Entry, path string, progress func(stage string, done, total int64)) (library.Entry, error) {
	if err := CheckISO(path); err != nil {
		return library.Entry{}, err
	}
	src := &manifest.Source{
		ID: e.ID, Kind: manifest.KindOSImage, Format: manifest.FormatISO,
		Filename: filepath.Base(path),
	}
	// With progress: a Windows ISO is read twice here, once to hash and once
	// to copy, and ten gigabytes of silence is indistinguishable from a hang.
	return lib.ImportWithProgress(src, path, progress)
}

// PlanDrivers finds and downloads the driver packs a machine needs without
// building any media, reporting what each piece of hardware resolved to. It
// stages into the same Quick Install workspace the real build uses, so
// nothing is fetched twice.
func PlanDrivers(ctx context.Context, lib *library.Library, e Entry, hw []recipe.HardwareSpec, progress func(stage string, done, total int64)) (*driverresolve.Resolved, error) {
	quickMu.Lock()
	defer quickMu.Unlock()
	opts := Options{}
	opts.defaults(e)
	wsDir, err := scaffoldQuickWorkspace(lib, e, opts, nil)
	if err != nil {
		return nil, err
	}
	ws, err := workspace.Load(wsDir)
	if err != nil {
		return nil, err
	}
	return driverresolve.Resolve(ctx, ws, lib, hw, false, progress)
}

// ensureSource makes sure the OS ISO is in the library.
func ensureSource(ctx context.Context, lib *library.Library, e Entry, progress func(string, int64, int64)) error {
	if _, err := lib.Resolve(e.ID); err == nil {
		return nil
	}
	// Nothing to fetch, and a confusing failure deep in the downloader is the
	// wrong way to learn that: say what to do instead.
	if e.ImportOnly() {
		return e.ImportOnlyError()
	}
	// A Windows ISO already downloaded in a browser is used rather than
	// asking Microsoft, which refuses an address for a day after a few
	// requests. This is the one place every path fetches through — the
	// Install dialog, a saved recipe, Download only, the command line — so
	// none of them can skip it; v0.7.10 only checked in the Install dialog,
	// and writing a recipe went to Microsoft anyway.
	if found := FindDownloadedISO(e); found != "" {
		if progress != nil {
			progress("using "+filepath.Base(found)+" from "+filepath.Base(filepath.Dir(found)), 0, -1)
		}
		_, err := ImportISO(lib, e, found, progress)
		return err
	}
	src := &manifest.Source{
		ID: e.ID, Kind: manifest.KindOSImage, Format: e.sourceFormat(),
		Provider: e.Provider, Fido: e.Fido, URL: e.URL, SHA256: e.SHA256, Filename: e.Filename,
	}
	// Distros that publish a SHA256SUMS file: resolve the pin now.
	if src.SHA256 == "" && e.ChecksumsURL != "" {
		sum, err := resolveChecksum(ctx, e)
		if err != nil {
			return err
		}
		src.SHA256 = sum
	}
	resolver := func(ctx context.Context, s *manifest.Source) (string, error) {
		if progress != nil {
			progress("resolving download URL", 0, -1)
		}
		return helpers.ResolveFidoURL(ctx, lib.HelpersDir(), s.Fido)
	}
	// Windows (Fido, unpinnable) uses trust-on-first-use; pinned distros verify.
	_, err := lib.Pull(ctx, src, true, resolver, func(done, total int64) {
		if progress != nil {
			progress("downloading "+e.Name, done, total)
		}
	})
	if err != nil && e.Family == Windows && e.Fido != nil {
		// Say where DSKY looked, so a download saved somewhere else, or under
		// another name, is recognisably the reason.
		return fmt.Errorf("%w. %s has not been downloaded on this computer yet, and DSKY found no Win%s_….iso in %s", err, e.Name, e.Fido.Win, strings.Join(downloadDirs(), " or "))
	}
	return err
}

// scaffoldQuickWorkspace writes an ephemeral workspace under the library with
// the embedded template, a manifest for the OS, and a synthesized recipe.
func scaffoldQuickWorkspace(lib *library.Library, e Entry, opts Options, hw []recipe.HardwareSpec) (string, error) {
	dir := filepath.Join(lib.Root, "quick")
	for _, sub := range []string{"templates", "manifests", "recipes", "payload"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return "", err
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "workspace.yaml"),
		[]byte("version: 1\norg:\n  name: \"Quick Install\"\n  id: quick\ndefaults:\n  locale: en-US\n"), 0o644); err != nil {
		return "", err
	}
	tmpl, err := templatesFS.ReadFile("templates/autounattend.xml.tmpl")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "templates", "autounattend.xml.tmpl"), tmpl, 0o644); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "manifests", e.ID+".yaml"), []byte(manifestYAML(e)), 0o644); err != nil {
		return "", err
	}
	// One manifest per operator-supplied installer, so the recipe's payload
	// refs resolve to the library blobs they were filed under.
	if _, customApps := opts.resolvedApps(); len(customApps) > 0 {
		for _, c := range customApps {
			if err := os.WriteFile(filepath.Join(dir, "manifests", c.SourceID()+".yaml"),
				[]byte(customManifestYAML(c)), 0o644); err != nil {
				return "", err
			}
		}
	}
	if err := writeQuickRecipe(dir, e, opts, hw); err != nil {
		return "", err
	}
	// Only keep this build's recipe so ws.Recipes() stays unambiguous.
	pruneOtherRecipes(filepath.Join(dir, "recipes"), e.ID)
	pruneOtherManifests(filepath.Join(dir, "manifests"), e.ID)
	return dir, nil
}

func writeQuickRecipe(dir string, e Entry, opts Options, hw []recipe.HardwareSpec) error {
	meta := recipeMeta{ID: e.ID, Name: e.Name, Template: quickTemplate}
	if e.Family == Linux && e.ProgramsSupported() && (len(opts.Apps) > 0 || opts.ThirdPartyDrivers) {
		rel, err := writeLinuxAnswers(dir, e.ID, e, opts)
		if err != nil {
			return err
		}
		meta.UserData = rel
	}
	return os.WriteFile(filepath.Join(dir, "recipes", e.ID+".yaml"), []byte(recipeYAML(meta, e, opts, hw)), 0o644)
}

func pruneOtherRecipes(dir, keep string) {
	entries, _ := os.ReadDir(dir)
	for _, en := range entries {
		if en.Name() != keep+".yaml" {
			os.Remove(filepath.Join(dir, en.Name()))
		}
	}
}

// pruneOtherManifests drops a previous Quick Install's OS manifest but keeps
// resolved driver packs: they are pinned, already downloaded, and only ever
// matched by hardware, so keeping them makes repeat installs on the same
// machine skip the catalog lookups entirely.
//
// Operator-supplied installers are kept for the same reason — and, unlike a
// driver pack, removing one would break the build outright, since the recipe
// written moments ago refers to it by ref.
func pruneOtherManifests(dir, keep string) {
	entries, _ := os.ReadDir(dir)
	for _, en := range entries {
		if en.Name() == keep+".yaml" {
			continue
		}
		p := filepath.Join(dir, en.Name())
		if b, err := os.ReadFile(p); err == nil {
			if strings.Contains(string(b), "kind: driver-pack") ||
				strings.Contains(string(b), "kind: payload") {
				continue
			}
		}
		os.Remove(p)
	}
}

// customManifestYAML pins an operator-supplied installer to the library blob
// it was imported as. No url: nothing fetches it, and the hash is the file
// that was handed over.
func customManifestYAML(c appcatalog.Custom) string {
	return fmt.Sprintf("id: %s\nkind: payload\nformat: %s\nsha256: %q\nfilename: %s\n",
		c.SourceID(), c.Format, c.SHA256, c.Filename)
}

func manifestYAML(e Entry) string {
	var b strings.Builder
	fmt.Fprintf(&b, "id: %s\nkind: os-image\nformat: %s\n", e.ID, e.sourceFormat())
	if e.Provider != "" {
		fmt.Fprintf(&b, "provider: %s\n", e.Provider)
		if e.Fido != nil {
			fmt.Fprintf(&b, "fido:\n  win: %q\n  release: %s\n  edition: %s\n  language: %s\n  arch: %s\n",
				e.Fido.Win, e.Fido.Release, e.Fido.Edition, e.Fido.Language, e.Fido.Arch)
		}
	} else {
		fmt.Fprintf(&b, "url: %s\n", e.URL)
	}
	fmt.Fprintf(&b, "sha256: %q\n", e.SHA256)
	if e.Filename != "" {
		fmt.Fprintf(&b, "filename: %s\n", e.Filename)
	}
	return b.String()
}

// hardwareYAML renders resolved machines as a windows.hardware block. Vendor
// packs and hardware IDs become separate entries, which is how compose
// matches them against staged manifests.
func hardwareYAML(hw []recipe.HardwareSpec) string {
	if len(hw) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("  hardware:\n")
	for _, h := range hw {
		osName := h.OS
		if osName == "" {
			osName = "win11"
		}
		if h.Vendor != "" {
			fmt.Fprintf(&b, "    - { vendor: %s, model: %q, os: %s }\n", h.Vendor, h.Model, osName)
		}
		if len(h.HWIDs) > 0 {
			quoted := make([]string, len(h.HWIDs))
			for i, id := range h.HWIDs {
				quoted[i] = fmt.Sprintf("%q", id)
			}
			fmt.Fprintf(&b, "    - { hwids: [%s], os: %s }\n", strings.Join(quoted, ", "), osName)
		}
	}
	return b.String()
}

// resolveDrivers finds the packs a build needs, treating the two kinds of
// request differently: hardware DSKY detected is best-effort, and a machine
// somebody named is not. A named model whose pack cannot be resolved stops
// the save with the vendor's own words, rather than writing a recipe that has
// silently forgotten the machine it was saved for.
//
// It plans rather than fetches: the manifests it writes carry each pack's URL
// and SHA-256, and the bytes are fetched when a stick is built. Saving a
// recipe used to download the vendor's whole pack first, so naming an HP
// EliteBook meant waiting on 1.2 GB before the recipe file existed -- and
// losing the recipe entirely if that download was interrupted.
func resolveDrivers(ctx context.Context, lib *library.Library, wsDir string, opts Options, progress func(stage string, done, total int64)) ([]recipe.HardwareSpec, error) {
	if len(opts.Hardware) == 0 && len(opts.Models) == 0 && len(opts.Kept) == 0 {
		return nil, nil
	}
	ws, err := workspace.Load(wsDir)
	if err != nil {
		return nil, err
	}
	hw := append([]recipe.HardwareSpec{}, opts.Kept...)
	if len(opts.Hardware) > 0 {
		res, err := driverresolve.Plan(ctx, ws, lib, opts.Hardware, false, progress)
		if err != nil {
			return nil, err
		}
		if progress != nil {
			for _, m := range res.Missing {
				progress("no driver pack found for "+m, 0, -1)
			}
		}
		hw = append(hw, res.Specs...)
	}
	if len(opts.Models) > 0 {
		res, err := driverresolve.Plan(ctx, ws, lib, opts.Models, true, progress)
		if err != nil {
			return nil, err
		}
		hw = append(hw, res.Specs...)
	}
	return dedupeSpecs(hw), nil
}

// dedupeSpecs drops repeats, so keeping a recipe's drivers and picking the
// same model again in the dialog does not stage it twice.
func dedupeSpecs(hw []recipe.HardwareSpec) []recipe.HardwareSpec {
	seen := map[string]bool{}
	out := hw[:0]
	for _, h := range hw {
		key := strings.ToLower(h.Vendor + "|" + h.Model + "|" + h.OS + "|" + strings.Join(h.HWIDs, ","))
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, h)
	}
	return out
}

// recipeMeta is what differs between a Quick Install recipe, which is named
// after its OS and rewritten on every build, and a saved one, which has a name
// somebody chose and may share a workspace with a hand-edited template.
type recipeMeta struct {
	ID, Name, Template string
	// UserData is the workspace-relative autoinstall answers for a Linux
	// entry with programs; empty writes the ISO as it is.
	UserData string
}

func recipeYAML(m recipeMeta, e Entry, opts Options, hw []recipe.HardwareSpec) string {
	if e.Family == Linux {
		// Raw images arrive compressed and expand several times over, so the
		// stick they need is much bigger than the download suggests.
		minStick := "4GiB"
		if e.Kind() == ImageRaw {
			minStick = "8GiB"
		}
		linux := ""
		switch {
		case m.UserData != "" && e.kickstartPrograms():
			linux = fmt.Sprintf("linux:\n  kickstart:\n    file: %s\n", m.UserData)
			minStick = "8GiB"
		case m.UserData != "":
			// Desktop keeps Ubuntu's confirmation screen; Server is hands-off.
			linux = fmt.Sprintf("linux:\n  autoinstall:\n    user_data: %s\n    kernel_patch: %v\n", m.UserData, !e.ubuntuDesktop())
			minStick = "8GiB"
		}
		return fmt.Sprintf(`version: 1
id: %s
name: %q
os:
  type: %s
  source: %s
target:
  min_stick: %s
  boot: uefi-only
%sflash:
  verify: readback-sha256
`, m.ID, m.Name, e.recipeOSType(), e.ID, minStick, linux)
	}
	bypass := "0"
	if opts.BypassRequirement {
		bypass = "1"
	}
	preset := debloatPreset(opts.Debloat)
	steps := "      - drivers\n"
	if preset != "off" {
		steps += "      - debloat\n"
	}
	pkgs, customApps := opts.resolvedApps()
	appsBlock := ""
	if len(pkgs) > 0 {
		var b strings.Builder
		b.WriteString("  apps:\n    winget:\n")
		for _, p := range pkgs {
			fmt.Fprintf(&b, "      - %s\n", p)
		}
		appsBlock = b.String()
		steps += "      - apps\n"
	}
	// The operator's own installers ride on the media, so each is staged as
	// payload and then run. They go after the winget step because winget needs
	// the network and these do not: if the machine is offline, the agent that
	// matters still lands.
	payloadBlock := ""
	if len(customApps) > 0 {
		var p, s strings.Builder
		p.WriteString("  payload:\n")
		for _, c := range customApps {
			fmt.Fprintf(&p, "    - { ref: %s }\n", c.SourceID())
			verb := "exe"
			if c.Format == "msi" {
				verb = "msi"
			}
			if args := c.RunArgs(); len(args) > 0 {
				quoted := make([]string, len(args))
				for i, a := range args {
					quoted[i] = fmt.Sprintf("%q", a)
				}
				fmt.Fprintf(&s, "      - %s: { ref: %s, args: [%s] }\n",
					verb, c.SourceID(), strings.Join(quoted, ", "))
			} else {
				fmt.Fprintf(&s, "      - %s: { ref: %s }\n", verb, c.SourceID())
			}
		}
		payloadBlock = p.String()
		steps += s.String()
	}
	// An offline-join blob is a path on this machine, so it goes in verbatim
	// rather than being copied into the ephemeral workspace: compose reads it
	// at build time and inlines the base64 into the answer file.
	domainBlock := ""
	switch {
	case opts.DomainBlob != "":
		domainBlock = fmt.Sprintf("  domain:\n    blob: %q\n", filepath.ToSlash(opts.DomainBlob))
	case opts.DomainBlobsDir != "":
		domainBlock = fmt.Sprintf("  domain:\n    blobs_by_serial: %q\n", filepath.ToSlash(opts.DomainBlobsDir))
	}
	// Staged GPU driver packages run past a gigabyte each, so driver media
	// outgrows the 8 GiB stick a bare Windows ISO fits on.
	minStick := "8GiB"
	if len(hw) > 0 {
		minStick = "16GiB"
	}
	return fmt.Sprintf(`version: 1
id: %s
name: %q
os:
  type: windows
  source: %s
  source_mode: iso
target:
  scheme: mbr
  filesystem: fat32
  volume_label: ESD-USB
  size: auto
  min_stick: %s
  boot: uefi-only
windows:
  ei_cfg: { edition: %s, channel: Retail, vl: false }
  unattend:
    template: %s
    vars:
      edition_key: %s
      locale: en-US
      admin_user: user
      admin_display_name: User
      admin_password: ""
      computer_name: "*"
      account_mode: %s
      bypass_requirements: "%s"
%s  debloat:
    preset: %s
%s%s%s  firstboot:
    mode: generate
    steps:
%s
flash:
  verify: readback-sha256
`, m.ID, m.Name, e.ID, minStick, editionName(opts.Edition), m.Template, genericKeys[opts.Edition],
		opts.AccountMode, bypass, hardwareYAML(hw), preset, appsBlock, payloadBlock, domainBlock, steps)
}

// editionName maps the option to the ei.cfg EditionID (drops the "N"/space).
func editionName(ed string) string {
	switch ed {
	case "Pro N":
		return "ProfessionalN"
	case "Pro":
		return "Professional"
	case "Home":
		return "Core"
	case "Education":
		return "Education"
	case "Enterprise":
		return "Enterprise"
	}
	return "Professional"
}

func debloatPreset(d string) string {
	if d == "" {
		return "off"
	}
	return d
}

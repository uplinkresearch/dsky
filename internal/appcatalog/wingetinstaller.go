package appcatalog

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Reading a package's real installer out of its winget manifest.
//
// Normally the agent installs a program by asking winget for it at first boot,
// which means the machine has to be online then. Plenty of them are not: a
// bench with no wifi password to hand, a site whose network is the thing being
// replaced, a van. For those, the same installer can be fetched here, at build
// time, on a machine that does have the internet, and put on the stick.
//
// What gets fetched is deliberately the exact file winget would have
// downloaded — the URL out of the package's own installer manifest, checked
// against the SHA-256 in that manifest. Not the vendor's "latest" link, and
// not something similar. That matters twice over: it is the only way to know
// the download was not tampered with in transit, and it is what keeps the
// machine upgradeable afterwards, because the Add/Remove Programs entry the
// real installer writes is what `winget upgrade` matches a package against
// later. Install something else and winget sees an unmanaged program.
//
// Nothing here downloads. This file reads manifests and decides whether a
// package can be pre-pulled at all; prepull.go does the fetching.

// WingetRawBase serves the manifest files themselves. The trees API used for
// the walk lists folders but not contents, and raw.githubusercontent has no
// 60-an-hour limit, so the two are separate hosts on purpose. A variable so
// tests can point it at a fake.
var WingetRawBase = "https://raw.githubusercontent.com/microsoft/winget-pkgs/master"

// ErrNoOfflineInstaller means the package exists but cannot be put on a stick:
// its installer is not a file that can be downloaded and run. Callers report
// this by name rather than quietly dropping the program, because "I ticked
// offline and three of my programs silently became online-only" is exactly the
// surprise that makes a bench trip wasted.
var ErrNoOfflineInstaller = errors.New("this package cannot be downloaded ahead of time")

// WingetInstaller is one package's installer as winget's manifest records it:
// where the file is, what it hashes to, and how to run it without a person.
type WingetInstaller struct {
	ID      string // the package id, spelled as the manifest spells it
	Version string // the manifest version this came from
	URL     string
	SHA256  string // lowercase hex, from the manifest, not from the download
	Type    string // msi | wix | burn | inno | nullsoft | exe
	Scope   string // machine | user | "" (unstated)
	Arch    string // x64 | x86 | arm64
	Args    []string
	// Needs are the packages this one's manifest says must be installed
	// first -- a Visual C++ runtime, a .NET desktop runtime. winget installs
	// them itself; an offline build has to carry them, so PrePull fetches
	// each of these too and stages it ahead of the package that named it.
	Needs []string
}

// MSI reports whether msiexec runs this file. A wix installer is an MSI built
// by the WiX toolset; a burn installer is not — it is an .exe bundle that
// happens to contain MSIs, and handing it to msiexec fails with 1620.
func (w WingetInstaller) MSI() bool { return w.Type == "msi" || w.Type == "wix" }

// Filename is what the file should be called on the stick. The URL's own last
// segment is used where it looks like a filename, because an operator reading
// a directory listing of the stick should recognise what is on it; where it
// does not (a CDN that serves an installer from a path of digits), the package
// id and version are used instead.
func (w WingetInstaller) Filename() string {
	ext := ".exe"
	if w.MSI() {
		ext = ".msi"
	}
	fallback := strings.ToLower(w.ID) + "-" + w.Version + ext
	u, err := url.Parse(w.URL)
	if err != nil {
		return fallback
	}
	base, err := url.PathUnescape(path.Base(u.Path))
	if err != nil || base == "" || base == "." || base == "/" {
		return fallback
	}
	if !strings.HasSuffix(strings.ToLower(base), ext) {
		return fallback
	}
	// Spaces and the like are legal in a URL and a nuisance on a stick.
	clean := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '.' || r == '-' || r == '_':
			return r
		default:
			return '-'
		}
	}, base)
	if len(clean) > 96 {
		return fallback
	}
	return clean
}

// installerManifest is the part of a winget installer manifest that matters
// here. Every field can appear either at the top level, applying to all of the
// package's installers, or on one installer, overriding it — Firefox states a
// type per installer because it publishes both an MSI and an EXE of the same
// build, while Chrome states one type for all three architectures. Both halves
// are decoded into the same shape and merged.
type installerManifest struct {
	PackageIdentifier string            `yaml:"PackageIdentifier"`
	PackageVersion    string            `yaml:"PackageVersion"`
	Installers        []wingetInstaller `yaml:"Installers"`
	wingetInstaller   `yaml:",inline"`
}

type wingetInstaller struct {
	Architecture      string            `yaml:"Architecture"`
	InstallerType     string            `yaml:"InstallerType"`
	NestedType        string            `yaml:"NestedInstallerType"`
	Scope             string            `yaml:"Scope"`
	InstallerLocale   string            `yaml:"InstallerLocale"`
	InstallerURL      string            `yaml:"InstallerUrl"`
	InstallerSha256   string            `yaml:"InstallerSha256"`
	InstallerSwitches installerSwitches `yaml:"InstallerSwitches"`
	Dependencies      dependencies      `yaml:"Dependencies"`
}

type installerSwitches struct {
	Silent             string `yaml:"Silent"`
	SilentWithProgress string `yaml:"SilentWithProgress"`
	Custom             string `yaml:"Custom"`
}

type dependencies struct {
	PackageDependencies []struct {
		PackageIdentifier string `yaml:"PackageIdentifier"`
	} `yaml:"PackageDependencies"`
	WindowsFeatures []string `yaml:"WindowsFeatures"`
}

// LookupWingetInstaller finds the installer a package would have downloaded,
// or says why it cannot be pre-pulled.
func LookupWingetInstaller(ctx context.Context, id string) (WingetInstaller, error) {
	if !ValidWingetID(id) {
		return WingetInstaller{}, fmt.Errorf("%q is not a winget package id (they look like Publisher.Package, such as Brave.Brave)", id)
	}
	name, dir, err := resolveWinget(ctx, id)
	if err != nil {
		return WingetInstaller{}, err
	}
	b, err := fetchManifest(ctx, dir+"/"+name+".installer.yaml")
	if err != nil {
		return WingetInstaller{}, err
	}
	var m installerManifest
	if err := yaml.Unmarshal(b, &m); err != nil {
		return WingetInstaller{}, fmt.Errorf("%w: %s's installer manifest could not be read: %v", ErrWingetUnchecked, name, err)
	}
	version := m.PackageVersion
	if version == "" {
		version = path.Base(dir)
	}
	return pickInstaller(name, version, m)
}

// fetchManifest reads one manifest file out of the repository.
func fetchManifest(ctx context.Context, p string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, WingetRawBase+"/"+p, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "dsky")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrWingetUnchecked, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, ErrWingetNotFound
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("%w: GitHub answered %s for %s", ErrWingetUnchecked, resp.Status, p)
	}
	// A manifest is a few kilobytes; anything near a megabyte is not one.
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

// pickInstaller chooses which of a package's installers to put on the stick,
// and refuses the ones that cannot go on a stick at all.
//
// The order of preference is x64 over x86 (arm64 is skipped: the stick is
// built for a fleet, and an x64 installer runs on Windows on ARM while the
// reverse is not true), machine scope over per-user, and an MSI over an EXE of
// the same build, because msiexec's /qn is the one silent switch that is the
// same everywhere.
func pickInstaller(id, version string, m installerManifest) (WingetInstaller, error) {
	if len(m.Installers) == 0 {
		return WingetInstaller{}, fmt.Errorf("%w: %s lists no installers at all", ErrNoOfflineInstaller, id)
	}
	var best WingetInstaller
	var bestScore int
	var refusals []string
	for _, in := range m.Installers {
		merged := mergeInstaller(m.wingetInstaller, in)
		cand, err := offlineInstaller(id, version, merged)
		if err != nil {
			refusals = append(refusals, strings.TrimSpace(err.Error()))
			continue
		}
		score := scoreInstaller(cand)
		if score < 0 {
			continue
		}
		if best.URL == "" || score > bestScore {
			best, bestScore = cand, score
		}
	}
	if best.URL == "" {
		why := "none of its installers can be downloaded and run without a person"
		if len(refusals) > 0 {
			why = dedupe(refusals)
		}
		return WingetInstaller{}, fmt.Errorf("%w: %s — %s", ErrNoOfflineInstaller, id, why)
	}
	return best, nil
}

// mergeInstaller lays one installer's fields over the manifest-wide ones.
func mergeInstaller(top, in wingetInstaller) wingetInstaller {
	out := in
	pick := func(dst *string, fallback string) {
		if *dst == "" {
			*dst = fallback
		}
	}
	pick(&out.InstallerType, top.InstallerType)
	pick(&out.NestedType, top.NestedType)
	pick(&out.Scope, top.Scope)
	pick(&out.InstallerLocale, top.InstallerLocale)
	pick(&out.InstallerSwitches.Silent, top.InstallerSwitches.Silent)
	pick(&out.InstallerSwitches.SilentWithProgress, top.InstallerSwitches.SilentWithProgress)
	pick(&out.InstallerSwitches.Custom, top.InstallerSwitches.Custom)
	if len(out.Dependencies.PackageDependencies) == 0 {
		out.Dependencies.PackageDependencies = top.Dependencies.PackageDependencies
	}
	return out
}

// offlineInstaller turns one manifest entry into something that can ride on a
// stick, or explains why it cannot.
func offlineInstaller(id, version string, in wingetInstaller) (WingetInstaller, error) {
	kind := strings.ToLower(in.InstallerType)
	switch kind {
	case "msstore":
		return WingetInstaller{}, errors.New("it comes from the Microsoft Store, which has no installer file to download")
	case "msix", "appx":
		return WingetInstaller{}, errors.New("it is a Store-style package (msix), which installs for one signed-in person and usually needs the Store's own framework packages")
	case "zip":
		return WingetInstaller{}, errors.New("it ships as a zip that winget unpacks itself, which is not an installer DSKY can run")
	case "portable":
		return WingetInstaller{}, errors.New("it is a portable program, not an installer")
	}
	if in.NestedType != "" {
		return WingetInstaller{}, errors.New("its installer is nested inside an archive, which is not an installer DSKY can run")
	}
	// A Windows feature is not a download: DISM fetches it from Windows
	// Update or from the installation media, and neither is available to a
	// machine with no network. Carrying the program without it would install
	// something that does not start.
	if len(in.Dependencies.WindowsFeatures) > 0 {
		return WingetInstaller{}, fmt.Errorf("it needs the Windows feature %s, which Windows fetches itself and cannot be put on a stick",
			strings.Join(in.Dependencies.WindowsFeatures, ", "))
	}
	var needs []string
	for _, d := range in.Dependencies.PackageDependencies {
		if d.PackageIdentifier != "" {
			needs = append(needs, d.PackageIdentifier)
		}
	}
	if in.InstallerURL == "" || in.InstallerSha256 == "" {
		return WingetInstaller{}, errors.New("its manifest gives no download link and hash")
	}
	if !strings.HasPrefix(strings.ToLower(in.InstallerURL), "https://") {
		return WingetInstaller{}, errors.New("its download link is not https")
	}
	args, err := silentArgs(kind, in.InstallerSwitches)
	if err != nil {
		return WingetInstaller{}, err
	}
	return WingetInstaller{
		ID:      id,
		Version: version,
		URL:     in.InstallerURL,
		SHA256:  strings.ToLower(in.InstallerSha256),
		Type:    kind,
		Scope:   strings.ToLower(in.Scope),
		Arch:    strings.ToLower(in.Architecture),
		Args:    args,
		Needs:   needs,
	}, nil
}

// silentArgs are the switches that make this installer run without a person.
//
// Where the manifest states them, they are used verbatim: the packager knew
// better than a table. Where it does not, the switches are the ones every
// installer of that kind takes, which is the whole reason winget records a
// type. A plain "exe" has no such switches — that is what the type means, an
// installer whose author invented their own — so one with nothing stated is
// refused rather than run and left sitting on a wizard nobody is watching.
func silentArgs(kind string, sw installerSwitches) ([]string, error) {
	stated := sw.Silent
	if stated == "" {
		stated = sw.SilentWithProgress
	}
	if stated != "" {
		return splitSwitches(stated), nil
	}
	switch kind {
	case "msi", "wix":
		// msiexec's own, applied by whatever runs it.
		return nil, nil
	case "burn":
		return []string{"/quiet", "/norestart"}, nil
	case "inno":
		return []string{"/VERYSILENT", "/SUPPRESSMSGBOXES", "/NORESTART", "/SP-"}, nil
	case "nullsoft":
		return []string{"/S"}, nil
	case "exe", "":
		return nil, errors.New("its manifest does not say how to install it without a person clicking through a wizard")
	}
	return nil, fmt.Errorf("DSKY does not know how to install a %q package without a person", kind)
}

// splitSwitches breaks a manifest's switch string into arguments. Manifests
// write them as one line the way a person would type it; quoted runs are kept
// whole because a path with a space in it is one argument.
func splitSwitches(s string) []string {
	var out []string
	var cur strings.Builder
	quote := rune(0)
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for _, r := range s {
		switch {
		case quote != 0 && r == quote:
			quote = 0
		case quote != 0:
			cur.WriteRune(r)
		case r == '"' || r == '\'':
			quote = r
		case r == ' ' || r == '\t':
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return out
}

// scoreInstaller ranks the installers a package offers; below zero means skip.
func scoreInstaller(w WingetInstaller) int {
	score := 0
	switch w.Arch {
	case "x64":
		score += 100
	case "x86":
		score += 50
	case "", "neutral":
		score += 40
	default: // arm64 and anything newer
		return -1
	}
	switch w.Scope {
	case "machine":
		score += 20
	case "user":
		score += 0
	default:
		score += 10
	}
	if w.MSI() {
		score += 5
	}
	// All else equal, the installer that needs less carried alongside it.
	score -= len(w.Needs)
	return score
}

// dedupe joins reasons without repeating one that applies to every installer
// of a package (Chrome would otherwise give the same sentence three times).
func dedupe(in []string) string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return strings.Join(out, "; ")
}

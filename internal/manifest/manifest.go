// Package manifest defines pinned-source files: small YAML documents, kept in
// an org workspace's git repo, that let any machine re-fetch multi-gigabyte
// binaries (ISOs, driver cabs, agent MSIs) by URL + SHA-256 — what a pinned
// fetch script does, declaratively.
package manifest

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Kind categorizes what a source is used for.
type Kind string

const (
	KindOSImage    Kind = "os-image"
	KindDriverPack Kind = "driver-pack"
	KindPayload    Kind = "payload"
	KindHelper     Kind = "helper"
)

// Format describes the container the bytes arrive in.
type Format string

const (
	FormatISO  Format = "iso"  // OS installer ISO (Windows: UDF; Linux: hybrid)
	FormatImg  Format = "img"  // raw disk image, optionally xz/zstd/gz compressed
	FormatZip  Format = "zip"  // archive holding an INF driver directory
	FormatCab  Format = "cab"  // Microsoft cabinet (expanded at first boot)
	FormatExe  Format = "exe"  // vendor installer run silently at first boot
	FormatMsi  Format = "msi"  // MSI installed at first boot
	FormatFile Format = "file" // anything staged verbatim
)

var validFormats = map[Format]bool{
	FormatISO: true, FormatImg: true, FormatZip: true, FormatCab: true,
	FormatExe: true, FormatMsi: true, FormatFile: true,
}

var (
	idRe     = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
	sha256Re = regexp.MustCompile(`^[0-9a-f]{64}$`)
	sha1Re   = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// FidoSpec selects an official Microsoft consumer ISO via the Fido helper
// (Microsoft's direct links are ephemeral, so the URL is resolved at pull
// time). Zero values default to Win 11 / Latest / Pro / English / x64.
// The json tags matter as well as the yaml ones: this struct is carried in
// the published catalog index, which is a format other builds have to read.
type FidoSpec struct {
	Win      string `yaml:"win,omitempty" json:"win,omitempty"`           // "11" | "10"
	Release  string `yaml:"release,omitempty" json:"release,omitempty"`   // "Latest" | "24H2" | ...
	Edition  string `yaml:"edition,omitempty" json:"edition,omitempty"`   // "Pro" | "Home" | ...
	Language string `yaml:"language,omitempty" json:"language,omitempty"` // "English" | ...
	Arch     string `yaml:"arch,omitempty" json:"arch,omitempty"`         // "x64" | "arm64"
}

// Source is one pinned download.
type Source struct {
	ID     string `yaml:"id"`
	Kind   Kind   `yaml:"kind"`
	Format Format `yaml:"format"`
	URL    string `yaml:"url,omitempty"`
	// Provider "fido" resolves the download URL at pull time via the Fido
	// helper instead of URL. sha256 pinning works the same way.
	Provider string    `yaml:"provider,omitempty"`
	Fido     *FidoSpec `yaml:"fido,omitempty"`
	// SHA256 pins the content. Empty means unpinned: `sources pull` then
	// requires --pin-tofu and prints the hash to commit into this file.
	SHA256 string `yaml:"sha256,omitempty"`
	// SHA1 is informational only (Microsoft Catalog URLs embed it).
	SHA1     string `yaml:"sha1,omitempty"`
	Size     int64  `yaml:"size,omitempty"`
	Filename string `yaml:"filename,omitempty"`
	Notes    string `yaml:"notes,omitempty"`

	// Driver packs found by `dsky drivers search/resolve` describe the
	// machine they serve and how to install them, so a recipe's
	// windows.hardware entries pick them up without naming them.
	Hardware *HardwareRef `yaml:"hardware,omitempty"`
	Install  string       `yaml:"install,omitempty"` // pnputil-sweep | expand-then-sweep | extract-then-sweep | exe
	Extract  []string     `yaml:"extract,omitempty"` // extract-then-sweep args; {dir} = destination
	Args     []string     `yaml:"args,omitempty"`    // exe: installer arguments
}

// HardwareRef ties a driver pack to a vendor model or a hardware ID.
type HardwareRef struct {
	Vendor string `yaml:"vendor,omitempty"`
	Model  string `yaml:"model,omitempty"`
	HWID   string `yaml:"hwid,omitempty"`
	OS     string `yaml:"os,omitempty"`
	// Gate is how the machine itself names the model, when that differs
	// from Model: an installer is run only where Windows reports this. ASUS
	// lists "Zenbook 14 OLED (UX3405MA)" and the laptop calls itself
	// "Zenbook 14 UX3405MA_UX3405MA"; the code is what both share.
	Gate string `yaml:"gate,omitempty"`
}

// Matches reports whether this pack serves the given machine.
func (h *HardwareRef) Matches(vendor, model, hwid string) bool {
	if h == nil {
		return false
	}
	if hwid != "" {
		return strings.EqualFold(strings.TrimSpace(h.HWID), strings.TrimSpace(hwid))
	}
	return strings.EqualFold(strings.TrimSpace(h.Vendor), strings.TrimSpace(vendor)) &&
		strings.EqualFold(strings.Join(strings.Fields(h.Model), " "), strings.Join(strings.Fields(model), " "))
}

// Validate checks internal consistency; path is used in error messages.
func (s *Source) Validate(path string) error {
	fail := func(format string, args ...any) error {
		return fmt.Errorf("%s: %s", path, fmt.Sprintf(format, args...))
	}
	switch {
	case s.ID == "":
		return fail("missing id")
	case !idRe.MatchString(s.ID):
		return fail("id %q must be lowercase letters, digits, dot, dash, underscore", s.ID)
	case s.Kind != KindOSImage && s.Kind != KindDriverPack && s.Kind != KindPayload && s.Kind != KindHelper:
		return fail("unknown kind %q", s.Kind)
	case !validFormats[s.Format]:
		return fail("unknown format %q", s.Format)
	case s.URL != "" && !strings.HasPrefix(s.URL, "https://") && !strings.HasPrefix(s.URL, "http://"):
		return fail("url must be http(s), got %q", s.URL)
	case s.Provider != "" && s.Provider != "fido":
		return fail("unknown provider %q (supported: fido)", s.Provider)
	case s.Provider == "fido" && s.URL != "":
		return fail("provider fido resolves the URL itself; remove url")
	case s.Provider == "fido" && s.Format != FormatISO:
		return fail("provider fido only produces ISOs")
	case s.SHA256 != "" && !sha256Re.MatchString(strings.ToLower(s.SHA256)):
		return fail("sha256 must be 64 hex characters")
	case s.SHA1 != "" && !sha1Re.MatchString(strings.ToLower(s.SHA1)):
		return fail("sha1 must be 40 hex characters")
	}
	return nil
}

// DownloadFilename is the name the fetched file is stored under.
func (s *Source) DownloadFilename() string {
	if s.Filename != "" {
		return s.Filename
	}
	if s.URL != "" {
		base := s.URL[strings.LastIndex(s.URL, "/")+1:]
		if i := strings.IndexAny(base, "?#"); i >= 0 {
			base = base[:i]
		}
		if base != "" {
			return base
		}
	}
	return s.ID
}

// Load reads and validates one manifest file.
func Load(path string) (*Source, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Source
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	s.SHA256 = strings.ToLower(s.SHA256)
	s.SHA1 = strings.ToLower(s.SHA1)
	if err := s.Validate(path); err != nil {
		return nil, err
	}
	return &s, nil
}

// LoadDir loads every *.yaml manifest under dir, sorted by ID. Duplicate IDs
// are an error. A missing dir yields an empty list.
func LoadDir(dir string) ([]*Source, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []*Source
	seen := map[string]string{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || (!strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml")) {
			continue
		}
		p := filepath.Join(dir, name)
		s, err := Load(p)
		if err != nil {
			return nil, err
		}
		if prev, dup := seen[s.ID]; dup {
			return nil, fmt.Errorf("%s: duplicate source id %q (also in %s)", p, s.ID, prev)
		}
		seen[s.ID] = p
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

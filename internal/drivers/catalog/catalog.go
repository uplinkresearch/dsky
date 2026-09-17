// Package catalog finds driver packs for specific machines from official
// feeds: the Dell, Lenovo, and HP enterprise driver-pack catalogs (by
// model) and the Microsoft Update Catalog (by hardware ID, covering
// vendors without a feed — Intel/ASUS NUCs included). Results carry the
// download URL, size, hashes, and the silent-extract switches needed to
// install the pack at first boot.
package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Vendor identifies a feed.
type Vendor string

const (
	Dell      Vendor = "dell"
	Lenovo    Vendor = "lenovo"
	HP        Vendor = "hp"
	MSCatalog Vendor = "mscatalog"
	Framework Vendor = "framework"
	Alienware Vendor = "alienware"
	Surface   Vendor = "surface"
	ASUS      Vendor = "asus"
	NUC       Vendor = "nuc"
	Samsung   Vendor = "samsung"
)

// VendorNames are the model feeds as a person would write them.
var VendorNames = map[Vendor]string{
	Dell: "Dell", HP: "HP", Lenovo: "Lenovo", Framework: "Framework", Alienware: "Alienware",
	Surface: "Surface", ASUS: "ASUS", NUC: "Intel NUC", Samsung: "Samsung",
}

// Pack is one downloadable driver package.
type Pack struct {
	Vendor Vendor
	Model  string // display model name (or MS Catalog title)
	// Component names one package of a model's drivers, for vendors that
	// publish a model's drivers as separate packages rather than one pack.
	Component string
	// Gate is the model as the machine reports it, where that differs from
	// Model, for installers that must run only on their own model.
	Gate      string
	SystemIDs []string // vendor machine/platform IDs
	OS        string   // "win11" | "win10"
	OSVersion string   // "24H2", "*" ...
	Version   string   // pack/driver version
	Released  string   // ISO-ish date as published
	URL       string
	Size      int64
	SHA256    string
	SHA1      string
	MD5       string
	Format    string // "cab" | "exe"
	// Install, when set, is how the pack is installed instead of the default
	// for its format: "exe" runs it with Args, which is how Framework's
	// bundles install.
	Install  string
	Args     []string
	Extract  []string // silent-extract args for exe packs; {dir} = destination
	Products string   // MS Catalog: products/OS column
	UpdateID string   // MS Catalog GUID
}

// ID derives a stable manifest id for the pack.
func (p Pack) ID() string {
	base := slug(p.Model)
	if p.Vendor == MSCatalog {
		base = "hwid-" + slug(p.Model)
	}
	parts := []string{string(p.Vendor), base}
	if p.OS != "" {
		parts = append(parts, p.OS)
	}
	if p.OSVersion != "" && p.OSVersion != "*" {
		parts = append(parts, strings.ToLower(p.OSVersion))
	}
	if p.Component != "" {
		// One model has dozens of these, and their names run long: the
		// component goes in, and a hash of the URL keeps two that truncate
		// to the same prefix from sharing an id.
		sum := sha256.Sum256([]byte(p.URL))
		base := strings.Join(append(parts[:2:2], slug(p.Component)), "-")
		if len(base) > 60 {
			base = strings.Trim(base[:60], "-.")
		}
		return base + "-" + hex.EncodeToString(sum[:])[:8]
	}
	id := strings.Join(parts, "-")
	if len(id) > 80 {
		id = id[:80]
	}
	return strings.Trim(id, "-.")
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

func slug(s string) string {
	return strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(s), "-"), "-")
}

// Query selects packs.
type Query struct {
	Model string // model substring or vendor machine ID (model feeds)
	HWID  string // hardware ID (MS Catalog)
	OS    string // "win11" (default) | "win10"
	Arch  string // "x64" (default) | "arm64"
}

func (q *Query) defaults() {
	if q.OS == "" {
		q.OS = "win11"
	}
	if q.Arch == "" {
		q.Arch = "x64"
	}
}

// Feed is one searchable catalog.
type Feed interface {
	Vendor() Vendor
	Search(ctx context.Context, q Query) ([]Pack, error)
}

// Lister is a feed that can name every model it has a pack for, so a person
// can pick one from a list instead of guessing how the vendor spells it.
type Lister interface {
	Models(ctx context.Context, os, arch string) ([]string, error)
}

// ModelFeeds are the vendors whose catalogs are organised by computer model.
var ModelFeeds = []Vendor{Dell, HP, Lenovo, Framework, Alienware, Surface, ASUS, NUC, Samsung}

// IsModelFeed reports whether a vendor name is one of ModelFeeds.
func IsModelFeed(vendor string) bool {
	for _, v := range ModelFeeds {
		if strings.EqualFold(string(v), strings.TrimSpace(vendor)) {
			return true
		}
	}
	return false
}

// ComponentFeed is a feed whose vendor publishes a model's drivers as many
// separate packages rather than one pack. Search still finds models; this
// lists every package to stage for exactly one.
type ComponentFeed interface {
	Components(ctx context.Context, q Query) ([]Pack, error)
}

// Exact picks the pack for exactly this model name when the search also
// matched longer names — "OptiPlex 7010" must not become "OptiPlex 7010 Plus"
// just because the Plus pack is newer. Without an exact match, the newest.
func Exact(packs []Pack, model string) Pack {
	want := strings.Join(strings.Fields(strings.ToLower(model)), " ")
	for _, p := range packs {
		if strings.Join(strings.Fields(strings.ToLower(p.Model)), " ") == want {
			return p
		}
	}
	return packs[0]
}

// PartialError is a model list that came back short: some of the vendor's
// pages could not be read, so the models they would have added are missing.
// A Lister returns it alongside the models it did find. It is not a failure
// to hide -- a list that quietly lost half of Alienware reads, to somebody
// looking for their machine, as DSKY not supporting it.
type PartialError struct {
	Failed, Of int
	Err        error // the first of the failures
}

func (e *PartialError) Error() string {
	return fmt.Sprintf("%d of %d could not be read (%v)", e.Failed, e.Of, e.Err)
}

func (e *PartialError) Unwrap() error { return e.Err }

// IsPartial reports whether err only says a list is incomplete.
func IsPartial(err error) bool {
	var p *PartialError
	return errors.As(err, &p)
}

// listResult finishes a list read from many pages: every failure when
// nothing was found, a PartialError when some were, nothing when all were.
func listResult(found int, errs []error, of int) error {
	switch {
	case len(errs) == 0:
		return nil
	case found == 0:
		return errs[0]
	default:
		return &PartialError{Failed: len(errs), Of: of, Err: errs[0]}
	}
}

// sortedUnique trims, de-duplicates case-insensitively and sorts model names.
func sortedUnique(names []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range names {
		n = strings.Join(strings.Fields(n), " ")
		k := strings.ToLower(n)
		if n == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i]) < strings.ToLower(out[j]) })
	return out
}

// Feeds returns the feed for a vendor name.
func FeedFor(vendor string, c *Cache) (Feed, error) {
	switch Vendor(strings.ToLower(vendor)) {
	case Dell:
		return &dellFeed{cache: c}, nil
	case Lenovo:
		return &lenovoFeed{cache: c}, nil
	case HP:
		return &hpFeed{cache: c}, nil
	case Framework:
		return &frameworkFeed{cache: c}, nil
	case Alienware:
		return &alienwareFeed{cache: c}, nil
	case Surface:
		return &surfaceFeed{cache: c}, nil
	case ASUS:
		return &asusFeed{cache: c}, nil
	case NUC:
		return &asusFeed{cache: c, nuc: true}, nil
	case Samsung:
		return &samsungFeed{cache: c}, nil
	case MSCatalog, "catalog", "ms", "microsoft":
		return &msCatalogFeed{}, nil
	default:
		return nil, fmt.Errorf("unknown driver feed %q (dell, lenovo, hp, framework, alienware, surface, asus, nuc, samsung, mscatalog)", vendor)
	}
}

// newestFirst orders packs by OS version then release date, descending.
func newestFirst(packs []Pack) {
	sort.SliceStable(packs, func(i, j int) bool {
		if packs[i].OSVersion != packs[j].OSVersion {
			return packs[i].OSVersion > packs[j].OSVersion
		}
		return packs[i].Released > packs[j].Released
	})
}

// modelMatches is a forgiving model comparison: case-insensitive, every
// query token must appear in the candidate.
func modelMatches(query, candidate string) bool {
	q := strings.ToLower(strings.TrimSpace(query))
	c := strings.ToLower(candidate)
	if q == "" {
		return false
	}
	for _, tok := range strings.Fields(q) {
		if !strings.Contains(c, tok) {
			return false
		}
	}
	return true
}

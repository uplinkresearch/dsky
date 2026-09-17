package catalog

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Identifier is a feed whose catalog names models differently from the way
// the machines report themselves to Windows. Identify takes either -- the
// model a computer reports, or a name out of the catalog -- and returns the
// catalog's name for it, which is what Search and Components are asked for.
//
// Dell, HP, Lenovo and Framework need none: their catalogs are searched by the
// name the machine reports. The newer vendors' are not:
//
//	what Windows reports             the catalog's name
//	Zenbook UX3405MA_UX3405MA        UX3405MA                            ASUS
//	NUC13ANKi7                       NUC 13 Pro Kit                      Intel NUC
//	960XGK                           Galaxy Book4 Pro (NP960XGK)         Samsung
//	Surface Laptop 4 (with an AMD)   Surface Laptop 4 (AMD)              Surface
//
// Alienware reports the catalog's own name, and is here so that a close
// spelling still finds it.
type Identifier interface {
	Identify(ctx context.Context, reported, osName string) (string, error)
}

// Identify returns the catalog name for a model on a vendor's feed, or the
// model unchanged for a feed that searches by the reported name.
func Identify(ctx context.Context, feed Feed, reported, osName string) (string, error) {
	id, ok := feed.(Identifier)
	if !ok {
		return reported, nil
	}
	return id.Identify(ctx, reported, osName)
}

// flatName is a name reduced for comparison: lower case, words only.
func flatName(s string) string {
	return strings.Join(strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	}), " ")
}

// sameName finds a name that is the one given, spelled loosely.
func sameName(reported string, names []string) (string, bool) {
	want := flatName(reported)
	for _, n := range names {
		if flatName(n) == want {
			return n, true
		}
	}
	return "", false
}

func notIdentified(vendor Vendor, reported string) error {
	return fmt.Errorf("%s lists no model that %q is — pick it from the model list instead", VendorNames[vendor], reported)
}

// Identify for Alienware: the catalog's names are what the machines report.
func (f *alienwareFeed) Identify(ctx context.Context, reported, osName string) (string, error) {
	systems, err := f.systems(ctx)
	if err != nil {
		return "", err
	}
	var names []string
	for _, s := range systems {
		if strings.EqualFold(s.SystemID, strings.TrimSpace(reported)) {
			return s.Model, nil
		}
		names = append(names, s.Model)
	}
	if n, ok := sameName(reported, names); ok {
		return n, nil
	}
	if !strings.Contains(strings.ToLower(reported), "alienware") {
		if n, ok := sameName("Alienware "+reported, names); ok {
			return n, nil
		}
	}
	return "", notIdentified(Alienware, reported)
}

// asusToken splits a name into the pieces one of which is a model code:
// "ASUS Zenbook 14 UX3405MA_UX3405MA", as a machine reports itself, holds
// UX3405MA, and so does "ASUS Zenbook 14 (UX3405MA)" in the catalog. A code
// has letters and digits both, which leaves out "Zenbook" and "14".
var asusToken = regexp.MustCompile(`[A-Za-z0-9-]+`)

func asusCodes(name string) []string {
	var out []string
	for _, t := range asusToken.FindAllString(name, -1) {
		t = strings.ToUpper(strings.Trim(t, "-"))
		if len(t) >= 4 && strings.ContainsAny(t, "0123456789") && strings.ContainsAny(t, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") {
			out = append(out, t)
		}
	}
	return out
}

// nucFamily is the part of a NUC's code shared by its kits, boards and mini
// PCs -- NUC13AN of NUC13ANKi7, NUC13ANBi5 and NUC13ANHi7 -- whose drivers are
// one INF pack that installs only what matches. Codes before the NUC 10 put
// the processor in the middle, NUC8i5BEH, and are the NUC8BE family.
var (
	nucFamilyNew = regexp.MustCompile(`(?i)^NUC(\d+)([A-Z]{2})`)
	nucFamilyOld = regexp.MustCompile(`(?i)^NUC(\d+)i\d([A-Z]{2})`)
)

func nucFamily(code string) string {
	code = strings.TrimSpace(code)
	for _, re := range []*regexp.Regexp{nucFamilyOld, nucFamilyNew} {
		if m := re.FindStringSubmatch(code); m != nil {
			return "NUC" + m[1] + strings.ToUpper(m[2])
		}
	}
	return ""
}

// asusIdentify is the product with the longest code the report holds; of
// products sharing that code, the shortest name, which is the plain model
// rather than a special edition of it.
func asusIdentify(reported string, names []string) string {
	reportedCodes := map[string]bool{}
	for _, c := range asusCodes(reported) {
		reportedCodes[c] = true
	}
	best, bestCode := "", ""
	for _, n := range names {
		for _, c := range asusCodes(n) {
			if !reportedCodes[c] {
				continue
			}
			if len(c) > len(bestCode) || len(c) == len(bestCode) && len(n) < len(best) {
				best, bestCode = n, c
			}
		}
	}
	return best
}

// Identify for ASUS finds the product whose code the machine reports; for a
// NUC, the product whose driver page names the same family of NUC.
func (f *asusFeed) Identify(ctx context.Context, reported, osName string) (string, error) {
	products, err := f.products(ctx)
	if err != nil && !IsPartial(err) {
		return "", err
	}
	names := make([]string, len(products))
	for i, p := range products {
		names[i] = p.Name
	}
	if n, ok := sameName(reported, names); ok {
		return n, nil
	}
	if !f.nuc {
		if best := asusIdentify(reported, names); best != "" {
			return best, nil
		}
		return "", notIdentified(ASUS, reported)
	}

	family := nucFamily(reported)
	if family == "" {
		return "", notIdentified(NUC, reported)
	}
	// The same family is a kit, a board and a mini PC, all with one driver
	// pack; the letter after the family says which this is, so the name DSKY
	// shows is the machine's own.
	kind := ""
	if rest := strings.ToUpper(strings.TrimSpace(reported)); len(rest) > len(family) {
		kind = map[byte]string{'K': "kit", 'H': "mini pc", 'B': "board"}[rest[len(family)]]
	}
	type hit struct {
		name  string
		exact bool
		kind  bool
	}
	var hits []hit
	for _, p := range products {
		list, err := f.driverList(ctx, p.ID)
		if err != nil || list.Result == nil {
			continue
		}
		code := strings.TrimSpace(list.Result.Model)
		if nucFamily(code) != family {
			continue
		}
		if len(asusComponents(p.Name, list, osName, true)) == 0 {
			continue
		}
		hits = append(hits, hit{p.Name, strings.EqualFold(code, strings.TrimSpace(reported)),
			kind != "" && strings.Contains(strings.ToLower(p.Name), kind)})
	}
	if len(hits) == 0 {
		return "", notIdentified(NUC, reported)
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].kind != hits[j].kind {
			return hits[i].kind
		}
		if hits[i].exact != hits[j].exact {
			return hits[i].exact
		}
		return hits[i].name < hits[j].name
	})
	return hits[0].name, nil
}

// samsungCode is a Samsung model code as a machine may report it, with or
// without the NP and the regional suffix: 960XGK, NP960XGK, NP960XGK-KG1US.
var samsungCode = regexp.MustCompile(`(?i)\b(?:NP)?(\d{3}[A-Z]{2,4}\d?)(?:-[A-Z0-9]+)?\b`)

func samsungIdentify(reported string, models []samsungModel) string {
	for _, found := range samsungCode.FindAllStringSubmatch(reported, -1) {
		code := "NP" + strings.ToUpper(found[1])
		for _, m := range models {
			for _, c := range m.Codes {
				if base, _, _ := strings.Cut(strings.ToUpper(c), "-"); base == code {
					return m.Name
				}
			}
		}
	}
	return ""
}

// Identify for Samsung matches the model code the machine reports against
// the codes each Galaxy Book is sold under.
func (f *samsungFeed) Identify(ctx context.Context, reported, osName string) (string, error) {
	models, err := f.models(ctx)
	if err != nil && !IsPartial(err) {
		return "", err
	}
	names := make([]string, len(models))
	for i, m := range models {
		names[i] = m.Name
	}
	if n, ok := sameName(reported, names); ok {
		return n, nil
	}
	if name := samsungIdentify(reported, models); name != "" {
		return name, nil
	}
	return "", notIdentified(Samsung, reported)
}

// surfaceWords is a Surface name as the words that tell models apart. "for
// Business", "with", "and" and "Microsoft" tell nothing; ordinals become their
// number, so the 7th Edition is 7; "+" is a word, since the Pro 7+ is not the
// Pro 7; and Intel and AMD are kept apart, as the variant.
func surfaceWords(name string) (words map[string]bool, variant string) {
	name = strings.ReplaceAll(strings.ToLower(name), "+", " plus ")
	words = map[string]bool{}
	for _, w := range strings.Fields(flatName(name)) {
		switch w {
		case "microsoft", "for", "business", "with", "and", "edition":
			continue
		case "intel", "amd":
			variant = w
			continue
		}
		if m := surfaceOrdinal.FindStringSubmatch(w); m != nil {
			w = m[1]
		}
		words[w] = true
	}
	return words, variant
}

var (
	surfaceOrdinal = regexp.MustCompile(`^(\d+)(?:st|nd|rd|th)$`)
	surfaceNumber  = regexp.MustCompile(`^\d+$`)
)

// Identify for Surface: the model whose name has every word the machine
// reports and the same numbers, fewest other words first -- "Surface Pro 10
// for Business" is the Pro 10, not the Pro 10 with 5G. When Windows reports
// the processor's maker too, as DSKY adds for Surface, a model for the other
// maker's chip is not it.
func (f *surfaceFeed) Identify(ctx context.Context, reported, osName string) (string, error) {
	q := Query{OS: osName}
	q.defaults()
	packs, err := f.packs(ctx, q.OS, q.Arch)
	if err != nil && !IsPartial(err) {
		return "", err
	}
	names := make([]string, len(packs))
	for i, p := range packs {
		names[i] = p.Model
	}
	if n, ok := sameName(reported, names); ok {
		return n, nil
	}
	best := surfaceIdentify(reported, names)
	if best == "" {
		return "", notIdentified(Surface, reported)
	}
	return best, nil
}

func surfaceIdentify(reported string, names []string) string {
	want, chip := surfaceWords(reported)
	numbers := func(ws map[string]bool) string {
		var n []string
		for w := range ws {
			if surfaceNumber.MatchString(w) {
				n = append(n, w)
			}
		}
		sort.Strings(n)
		return strings.Join(n, " ")
	}
	best, bestExtra := "", -1
	for _, n := range names {
		have, variant := surfaceWords(n)
		if chip != "" && variant != "" && chip != variant {
			continue
		}
		if numbers(have) != numbers(want) {
			continue
		}
		missing := false
		for w := range want {
			if !have[w] {
				missing = true
				break
			}
		}
		if missing {
			continue
		}
		if extra := len(have) - len(want); bestExtra < 0 || extra < bestExtra {
			best, bestExtra = n, extra
		}
	}
	return best
}

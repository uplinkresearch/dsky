package migrate

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Resolving is deciding where each application comes from on the new machine.
// It is the step between a scan, which only records what it saw, and a review,
// where a person settles what is left -- and the whole design goal is to leave
// that person as little as possible without ever deciding something they would
// have decided differently.
//
// Three sources, in this order, because that is the order of how much each one
// knows about this customer:
//
//  1. The site's own table. Somebody sat at this practice and worked out that
//     their dental software installs from a share with /S /v/qn. Nothing
//     guesses better than that, and it is why the table exists.
//  2. The table DSKY ships: the ninety-odd packages the app picker already
//     knows, each with a winget id that is checked against winget-pkgs every
//     week. Common software, spelled the way its own manifest spells it.
//  3. What the names say. "Notepad++ (64-bit x64)" in the registry is the
//     "Notepad++" DSKY knows; matching the two is arithmetic on words, and it
//     carries a score so that a good match installs and a fair one is only
//     ever a suggestion for the review.
//
// Two rules hold throughout. An application the resolver has not looked at has
// an empty status, so re-running it can never overwrite what a person decided
// -- it only fills blanks. And nothing is invented: a match below the
// confident threshold is written as a suggestion with its score, not as an
// answer.

// Confidence thresholds, from the implementation spec. Above the first, the
// name match is taken as the answer; between them it is offered to the review
// as a suggestion; below, nothing is said at all.
const (
	ConfidentMatch = 0.85
	ProposeMatch   = 0.60
)

// Resolver is somewhere a resolution can come from. (Not "source": in this
// package that word is already the machine being replaced.)
type Resolver interface {
	// Where is how it appears in the report ("the site's table").
	Where() string
	// Find returns what this source knows about the application.
	Find(a App) (Resolution, *ConfigCapture, bool)
}

// Result is what one pass of the resolver did, for the command that ran it
// and for the review that follows.
type Result struct {
	Resolved  int // now has somewhere to install from
	Suggested int // a name match good enough to offer, not to act on
	Dropped   int // the site said never migrate this
	Manual    int // there is no installer; a person does it
	Left      int // still nothing: the review's work
	BySource  map[string]int
}

// Resolve fills in every application nobody has decided about yet.
func Resolve(m *Manifest, from ...Resolver) Result {
	res := Result{BySource: map[string]int{}}
	for i := range m.Apps {
		a := &m.Apps[i]
		if !needsResolving(*a) {
			continue
		}
		found := false
		for _, s := range from {
			r, cc, ok := s.Find(*a)
			if !ok {
				continue
			}
			if r.Order == 0 && r.Status == StatusResolved {
				r.Order = prerequisiteOrder(a.DisplayName)
			}
			a.Resolution = r
			if cc != nil {
				a.ConfigCapture = cc
			}
			res.BySource[s.Where()]++
			found = true
			break
		}
		switch {
		case !found:
			// Looked and found nothing, which is not the same as not having
			// looked: the review asks about these, and the difference is what
			// tells an operator whether the tool tried.
			a.Resolution = Resolution{Status: StatusUnmapped, ResolvedBy: ByAuto}
			res.Left++
		case a.Resolution.Status == StatusDropped:
			res.Dropped++
		case a.Resolution.Status != StatusResolved:
			// A suggestion: the status stays unmapped so the review still
			// asks, but the reference is filled in so the answer is one key.
			res.Suggested++
			res.Left++
		case a.Resolution.Method == MethodManual:
			res.Manual++
		default:
			res.Resolved++
		}
	}
	return res
}

// needsResolving is the rule that lets this be run again. Anything a person
// settled is left alone for ever; anything the tool itself failed to place is
// tried again, because the reason to re-run is usually that somebody has just
// added the entry that places it.
func needsResolving(a App) bool {
	switch a.Resolution.Status {
	case StatusUnset:
		return true
	case StatusUnmapped:
		return a.Resolution.ResolvedBy != ByOperator
	}
	return false
}

// prerequisite names the runtimes and engines that other software needs
// present before it will install. They go first; the rest follow in the order
// the manifest lists them.
var prerequisite = regexp.MustCompile(`(?i)visual c\+\+|vcredist|\.net (desktop )?runtime|dotnet|sql server|sqlexpress|java (runtime|se)|jre\b|directx|edge ?webview`)

func prerequisiteOrder(name string) int {
	if prerequisite.MatchString(name) {
		return OrderPrereq
	}
	return OrderDefault
}

// ── the site's table, and DSKY's own ─────────────────────────────────────────

// TableResolver is a mapping table as a resolver.
type TableResolver struct {
	Table *Table
	What  string
	By    string // what to record as resolved_by
}

func (t TableResolver) Where() string { return t.What }

func (t TableResolver) Find(a App) (Resolution, *ConfigCapture, bool) {
	if t.Table == nil {
		return Resolution{}, nil, false
	}
	r, cc, ok := t.Table.Lookup(a)
	if ok && t.By != "" {
		r.ResolvedBy = t.By
	}
	return r, cc, ok
}

// SiteTable is the operator's own decisions for this customer.
func SiteTable(t *Table) Resolver {
	return TableResolver{Table: t, What: "the site's table", By: ByMapping}
}

// GlobalTable is what DSKY ships: the packages its app picker knows.
func GlobalTable() Resolver {
	return TableResolver{Table: BuiltinTable(), What: "DSKY's own list", By: ByMapping}
}

// ── matching by name ─────────────────────────────────────────────────────────

// NameMatch resolves by how much an application's name looks like one DSKY
// knows. It is last, and it is the only source that can be wrong, so it says
// how sure it is and stays quiet below the threshold where a person should
// look instead.
type NameMatch struct {
	Known []KnownApp
}

// KnownApp is one package the matcher can match against.
type KnownApp struct {
	Name      string // "Notepad++"
	Publisher string // as winget spells it, where it is known
	Ref       string // winget id
	Method    string
}

func (NameMatch) Where() string { return "the name" }

func (n NameMatch) Find(a App) (Resolution, *ConfigCapture, bool) {
	best, score := KnownApp{}, 0.0
	for _, k := range n.Known {
		s := similarity(a.DisplayName, k.Name)
		// The publisher breaks ties and rescues a short name: "Reader" from
		// Adobe is not "Reader" from anybody else.
		if a.Publisher != "" && k.Publisher != "" {
			if p := similarity(a.Publisher, k.Publisher); p > 0.6 {
				s += 0.05
			}
		}
		if s > score {
			best, score = k, s
		}
	}
	if score > 1 {
		score = 1
	}
	method := best.Method
	if method == "" {
		method = MethodWinget
	}
	switch {
	case score >= ConfidentMatch:
		return Resolution{Status: StatusResolved, Method: method, Ref: best.Ref,
			Confidence: round2(score), ResolvedBy: ByAuto, Order: prerequisiteOrder(a.DisplayName)}, nil, true
	case score >= ProposeMatch:
		// Offered, not taken: the status stays unmapped, so the review still
		// asks, with the answer already typed in.
		return Resolution{Status: StatusUnmapped, Ref: best.Ref, Method: method,
			Confidence: round2(score), ResolvedBy: ByAuto}, nil, true
	}
	return Resolution{}, nil, false
}

func round2(f float64) float64 { return float64(int(f*100+0.5)) / 100 }

// noiseWord is what a registry display name carries that a package name does
// not: the version, the architecture, the edition, the language, and the
// vendor's habit of repeating itself.
var noiseWord = map[string]bool{
	"x64": true, "x86": true, "amd64": true, "arm64": true, "32": true, "64": true,
	"bit": true, "32bit": true, "64bit": true, "edition": true, "version": true,
	"en": true, "us": true, "enus": true, "inc": true, "llc": true, "ltd": true,
	"corporation": true, "corp": true, "gmbh": true, "software": true, "the": true,
}

var notWord = regexp.MustCompile(`[^a-z0-9+#]+`)

// words reduces a name to what identifies it: lower case, no punctuation
// except the ones that are part of names people know (C++, C#, Notepad++),
// no version numbers, no architectures.
func words(s string) []string {
	var out []string
	for _, w := range notWord.Split(strings.ToLower(s), -1) {
		switch {
		case w == "", noiseWord[w]:
			continue
		case versionWord.MatchString(w):
			continue
		}
		out = append(out, w)
	}
	return out
}

// similarity is how much of the shorter name the two share: 1 when every word
// of the package name is in the application's name, 0 when none is. The
// shorter side is the denominator because a registry name is padded with
// things a package name does not have -- "7-Zip 24.08 (x64 edition)" is
// entirely "7-Zip" plus noise, and should score as a certainty, not a half.
func similarity(a, b string) float64 {
	aw, bw := words(a), words(b)
	if len(aw) == 0 || len(bw) == 0 {
		return 0
	}
	in := map[string]bool{}
	for _, w := range aw {
		in[w] = true
	}
	shared := 0
	for _, w := range bw {
		if in[w] {
			shared++
		}
	}
	short := len(bw)
	if len(aw) < short {
		short = len(aw)
	}
	score := float64(shared) / float64(short)
	// A single shared word is weak evidence when the names are otherwise
	// nothing alike: "Microsoft Teams" and "Microsoft Edge" share "microsoft".
	if shared == 1 && (len(aw) > 1 && len(bw) > 1) {
		score *= 0.6
	}
	return score
}

// ── describing what happened ─────────────────────────────────────────────────

// Summary is the result in a sentence, for the command line and the log.
func (r Result) Summary() string {
	parts := []string{fmt.Sprintf("%d resolved", r.Resolved)}
	if r.Manual > 0 {
		parts = append(parts, fmt.Sprintf("%d to do by hand", r.Manual))
	}
	if r.Dropped > 0 {
		parts = append(parts, fmt.Sprintf("%d left behind", r.Dropped))
	}
	if r.Suggested > 0 {
		parts = append(parts, fmt.Sprintf("%d with a suggestion to confirm", r.Suggested))
	}
	if r.Left > 0 {
		parts = append(parts, fmt.Sprintf("%d still with nowhere to install from", r.Left))
	}
	sources := make([]string, 0, len(r.BySource))
	for s, n := range r.BySource {
		sources = append(sources, fmt.Sprintf("%d from %s", n, s))
	}
	sort.Strings(sources)
	out := strings.Join(parts, ", ")
	if len(sources) > 0 {
		out += " (" + strings.Join(sources, ", ") + ")"
	}
	return out
}

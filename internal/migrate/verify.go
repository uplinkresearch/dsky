package migrate

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// Verifying is reading the new machine back and asking whether it is the
// machine the plan described.
//
// It exists because every other part of this feature reports on its own work.
// The scanner says what it found, the review says what was decided, the agent
// says what it did -- and all three can be right while the machine is still
// wrong, because a program can install and not run, a setting can be applied
// and overwritten by the program installed after it, and a queue can be added
// to an account nobody uses. The only reading that settles it is one taken
// from the finished machine, by something that was not involved in building
// it.
//
// So this compares two manifests: the plan somebody approved, and a fresh scan
// of the machine that came out. Nothing here trusts the build's own account of
// itself.

// Verification is what the new machine has, against what was asked for.
type Verification struct {
	Plan     *Manifest
	After    *Manifest
	Missing  []MissingApp // asked for, not found
	Extra    []App        // on the new machine, not in the plan
	Present  int          // asked for and found
	Deferred int          // the plan already said these would not be installed
	Settings []SettingDiff
	// Waiting are settings that belong to a person rather than the machine.
	// They are not differences and not faults: they are applied at somebody's
	// first sign-in, so a machine read before that has not got them yet, and
	// the person reading this may be the one who has not signed in.
	Waiting  []SettingDiff
	Identity []string // differences in name and domain, in plain words
}

// MissingApp is an application the plan asked for that is not on the machine,
// and the nearest thing to it that is.
//
// The near miss is the useful half. An application installed under a name
// nobody expected -- "Adobe Acrobat (64-bit)" where the plan said "Adobe
// Acrobat Reader DC" -- looks exactly like an application that failed to
// install, and the difference matters: one needs somebody's afternoon and the
// other needs one line in the site's mapping table so that the next machine
// gets it right without anybody noticing.
type MissingApp struct {
	Wanted     App
	Nearest    *App
	Confidence float64
}

// SettingDiff is one setting that is not what the plan asked for.
type SettingDiff struct {
	Key    string
	Wanted string
	Found  string
}

// AliasThreshold is how alike two names have to be before a missing
// application is offered as the same thing under another name. Deliberately
// higher than the resolver's proposal threshold: this writes into the site's
// table, where a wrong entry is wrong for every machine afterwards.
const AliasThreshold = 0.72

// Verify compares a fresh scan of the new machine against the approved plan.
func Verify(plan, after *Manifest) *Verification {
	v := &Verification{Plan: plan, After: after}

	found := map[string]bool{}
	for _, want := range plan.Apps {
		switch want.Resolution.Status {
		case StatusDropped:
			// The plan said this would not come across, so its absence is the
			// plan working, not the machine failing.
			v.Deferred++
			continue
		case StatusUnmapped, StatusUnset, StatusBlocked:
			v.Deferred++
			continue
		}
		if want.Resolution.Method == MethodManual {
			v.Deferred++
			continue
		}
		if i := indexOfApp(after.Apps, want); i >= 0 {
			found[after.Apps[i].ID] = true
			v.Present++
			continue
		}
		m := MissingApp{Wanted: want}
		if near, score := nearestApp(after.Apps, want); score >= AliasThreshold {
			m.Nearest, m.Confidence = near, score
			// Accounted for: something on this machine is probably this
			// program under another name. Listing it again below as
			// something the plan never mentioned would put the same
			// application on the page twice, in two framings that
			// contradict each other.
			found[near.ID] = true
		}
		v.Missing = append(v.Missing, m)
	}
	for _, got := range after.Apps {
		if !found[got.ID] && !inPlan(plan, got) {
			v.Extra = append(v.Extra, got)
		}
	}

	for _, want := range plan.Settings {
		how, ok := want.Apply["win11"]
		if !ok || how.Method == "" {
			continue // the plan never claimed this one would be set
		}
		got, ok := settingValueOf(after, want.Key)
		wanted := strings.TrimSpace(string(want.Value))
		if !ok {
			v.Settings = append(v.Settings, SettingDiff{Key: want.Key, Wanted: wanted, Found: "not read"})
			continue
		}
		if sameSettingValue(wanted, got) {
			continue
		}
		// A setting that belongs to a person is applied at their first
		// sign-in, not when the machine is built, so a machine read before
		// that has not got it yet -- and reporting it as wrong is how a
		// report earns a reputation for crying wolf. Seen doing exactly that
		// on the first machine this was run against: two per-user settings
		// listed as faults on a PC where nothing was wrong.
		if how.Method == ApplyRegistry && strings.HasPrefix(how.Ref, `HKCU\`) {
			v.Waiting = append(v.Waiting, SettingDiff{Key: want.Key, Wanted: wanted, Found: got})
			continue
		}
		v.Settings = append(v.Settings, SettingDiff{Key: want.Key, Wanted: wanted, Found: got})
	}

	if want, got := plan.Target.Hostname, after.Source.Hostname; want != "" && !strings.EqualFold(want, got) {
		v.Identity = append(v.Identity, fmt.Sprintf("the plan named this machine %s and it is called %s", want, got))
	}
	if plan.Identity.Domained() && !after.Identity.Domained() {
		v.Identity = append(v.Identity, fmt.Sprintf("the plan joins %s and this machine is in a workgroup", plan.Identity.DomainFQDN))
	}
	sort.SliceStable(v.Missing, func(i, j int) bool {
		return v.Missing[i].Wanted.DisplayName < v.Missing[j].Wanted.DisplayName
	})
	return v
}

// OK reports whether the machine matches the plan in every way that was
// promised.
func (v *Verification) OK() bool {
	return len(v.Missing) == 0 && len(v.Settings) == 0 && len(v.Identity) == 0
}

// Aliases are the missing applications near enough to something on the machine
// to be worth remembering as the same thing under another name.
func (v *Verification) Aliases() []MissingApp {
	var out []MissingApp
	for _, m := range v.Missing {
		if m.Nearest != nil {
			out = append(out, m)
		}
	}
	return out
}

// Summary is the one line a person reads first.
func (v *Verification) Summary() string {
	parts := []string{fmt.Sprintf("%d of %d program(s) the plan installs are here",
		v.Present, v.Present+len(v.Missing))}
	if n := len(v.Settings); n > 0 {
		parts = append(parts, fmt.Sprintf("%d setting(s) are not what the plan asked for", n))
	}
	if n := len(v.Waiting); n > 0 {
		parts = append(parts, fmt.Sprintf("%d setting(s) are waiting for the person who will use it to sign in", n))
	}
	if n := len(v.Identity); n > 0 {
		parts = append(parts, fmt.Sprintf("%d thing(s) about who this machine is", n))
	}
	if v.OK() {
		return parts[0] + "; nothing else to report"
	}
	return strings.Join(parts, "; ")
}

// indexOfApp finds the same application on the new machine: the same id, or
// the same name, whichever the scan produced.
func indexOfApp(in []App, want App) int {
	for i, got := range in {
		if got.ID == want.ID || sameAppName(got.DisplayName, want.DisplayName) {
			return i
		}
	}
	return -1
}

func inPlan(m *Manifest, got App) bool {
	for _, a := range m.Apps {
		if a.ID == got.ID || sameAppName(a.DisplayName, got.DisplayName) {
			return true
		}
	}
	return false
}

func sameAppName(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

// nearestApp is the closest thing on the machine to what was wanted.
func nearestApp(in []App, want App) (*App, float64) {
	best, bestScore := -1, 0.0
	for i, got := range in {
		if s := similarity(want.DisplayName, got.DisplayName); s > bestScore {
			best, bestScore = i, s
		}
	}
	if best < 0 {
		return nil, 0
	}
	return &in[best], bestScore
}

func settingValueOf(m *Manifest, key string) (string, bool) {
	for _, s := range m.Settings {
		if s.Key == key {
			return strings.TrimSpace(string(s.Value)), true
		}
	}
	return "", false
}

// sameSettingValue compares two captured values, which are JSON fragments.
// Quoted and unquoted forms of the same string are the same setting.
func sameSettingValue(a, b string) bool {
	return strings.EqualFold(strings.Trim(a, `"`), strings.Trim(b, `"`))
}

// VerifyReport renders the comparison as a page, in the same shape as the
// machine's own record: what needs a hand first, then what is only worth
// knowing, then what matched.
func VerifyReport(w io.Writer, v *Verification, version string) error {
	d := RecordData{
		RenderedAt: time.Now().UTC().Truncate(time.Second),
		Version:    version,
		R: &Record{
			Machine: v.After.Source.Hostname,
			Recipe:  v.Plan.Source.Hostname + " → " + v.Plan.Target.Hostname,
		},
	}
	for _, m := range v.Missing {
		note := "the plan installs this and it is not on the machine"
		if m.Nearest != nil {
			note = fmt.Sprintf("not found under this name — %s is here, which is %.0f%% alike and may be the same program",
				m.Nearest.DisplayName, m.Confidence*100)
		}
		d.Failed = append(d.Failed, Shown{Kind: KindApp, Name: m.Wanted.DisplayName, Note: note})
	}
	for _, s := range v.Settings {
		d.Failed = append(d.Failed, Shown{Kind: KindSetting, Name: s.Key,
			Note: fmt.Sprintf("the plan asked for %s and this machine has %s", s.Wanted, s.Found)})
	}
	for _, line := range v.Identity {
		d.Failed = append(d.Failed, Shown{Kind: KindDomain, Name: "this machine's identity", Note: line})
	}
	for _, s := range v.Waiting {
		d.Waiting = append(d.Waiting, Shown{Kind: KindSetting, Name: s.Key,
			Note: "this belongs to a person, so it is applied at their first sign-in — this machine has " + s.Found + " until then"})
	}
	for _, a := range v.Extra {
		d.Waiting = append(d.Waiting, Shown{Kind: KindApp, Name: a.DisplayName,
			Note: "on the machine and not in the plan — Windows ships some of these, and somebody may have installed the rest"})
	}
	d.Done = append(d.Done, Shown{Kind: KindApp,
		Name: fmt.Sprintf("%d program(s) the plan installs", v.Present), Note: ""})
	return renderRecord(w, d)
}

package migrate

import (
	"strings"
	"testing"
)

// The resolver's job is to leave a person as little to do as possible without
// once deciding something they would have decided differently. These tests are
// about both halves of that.

func defaultResolvers(site *Table) []Resolver {
	return []Resolver{SiteTable(site), GlobalTable(), NameMatch{Known: KnownApps()}}
}

// An ordinary office PC should come out of the resolver with almost nothing
// left to do: the spec asks for four in five, and the software on such a
// machine is exactly what DSKY's own list already covers.
func TestAnOrdinaryOfficePCResolvesItself(t *testing.T) {
	m := load(t, "office")
	// Start from what a scan produces: nobody has looked at any of them.
	for i := range m.Apps {
		m.Apps[i].Resolution = Resolution{}
	}
	res := Resolve(m, defaultResolvers(nil)...)
	settled := res.Resolved + res.Dropped + res.Manual
	if share := float64(settled) / float64(len(m.Apps)); share < 0.8 {
		t.Errorf("%d of %d settled (%.0f%%), want at least 80%%: %s",
			settled, len(m.Apps), share*100, res.Summary())
	}
	by := map[string]App{}
	for _, a := range m.Apps {
		by[a.ID] = a
	}
	// Each of these is a different path through the resolver.
	for id, want := range map[string]string{
		"chrome":        "Google.Chrome",                // DSKY's own list, exactly named
		"acrobatreader": "Adobe.Acrobat.Reader.64-bit",  // named "Adobe Acrobat Reader (64-bit)" here
		"7zip":          "7zip.7zip",                    // named "7-Zip 24.08 (x64)" here
		"vcredist2015":  "Microsoft.VCRedist.2015+.x64", // a runtime nobody picks
	} {
		got := by[id]
		if got.Resolution.Ref != want {
			t.Errorf("%s (%q): %q, want %q (%s, %.2f)", id, got.DisplayName,
				got.Resolution.Ref, want, got.Resolution.ResolvedBy, got.Resolution.Confidence)
		}
		if got.Resolution.Status != StatusResolved {
			t.Errorf("%s: status %q", id, got.Resolution.Status)
		}
	}
	// The runtime installs before the software that needs it.
	if o := by["vcredist2015"].Resolution.Order; o != OrderPrereq {
		t.Errorf("the C++ runtime is order %d, want %d", o, OrderPrereq)
	}
	if o := by["chrome"].Resolution.Order; o != OrderDefault {
		t.Errorf("Chrome is order %d, want %d", o, OrderDefault)
	}
	// Windows' own software is not installed again.
	if s := by["solitairecollection"].Resolution.Status; s != StatusDropped && s != StatusUnmapped {
		t.Errorf("Solitaire: %q", s)
	}
}

// The site's table is consulted first, because it holds decisions a person
// made about this customer -- and it must win even when DSKY's own list has
// an answer that looks good.
func TestTheSiteKnowsBest(t *testing.T) {
	site, err := ParseTable([]byte(`{
	  "schema_version": "1.0",
	  "entries": [
	    { "match": { "display_name": "^Google Chrome" },
	      "resolution": { "method": "msi", "ref": "\\\\fs\\installers\\chrome-enterprise.msi", "args": ["/qn"] },
	      "note": "the enterprise build, with the policies already set" },
	    { "match": { "display_name": "^Adobe" }, "resolution": { "status": "dropped" }, "note": "not licensed here" }
	  ]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	m := load(t, "office")
	for i := range m.Apps {
		m.Apps[i].Resolution = Resolution{}
	}
	Resolve(m, defaultResolvers(site)...)
	by := map[string]App{}
	for _, a := range m.Apps {
		by[a.ID] = a
	}
	if r := by["chrome"].Resolution; r.Method != MethodMSI || !strings.Contains(r.Ref, "chrome-enterprise.msi") || r.ResolvedBy != ByMapping {
		t.Errorf("the site's Chrome lost to DSKY's: %+v", r)
	}
	if r := by["acrobatreader"].Resolution; r.Status != StatusDropped {
		t.Errorf("the site said drop Adobe: %+v", r)
	}
}

// Re-running the resolver must never overwrite a decision: it fills blanks.
func TestResolvingTwiceKeepsWhatAPersonChose(t *testing.T) {
	m := load(t, "office")
	for i := range m.Apps {
		m.Apps[i].Resolution = Resolution{}
	}
	Resolve(m, defaultResolvers(nil)...)
	// The operator overrules one and drops another.
	for i, a := range m.Apps {
		switch a.ID {
		case "chrome":
			m.Apps[i].Resolution = Resolution{Status: StatusResolved, Method: MethodEXE,
				Ref: `\\fs\installers\chrome.exe`, ResolvedBy: ByOperator, Confidence: 1, Order: OrderDefault}
		case "7zip":
			m.Apps[i].Resolution = Resolution{Status: StatusDropped, ResolvedBy: ByOperator}
		}
	}
	Resolve(m, defaultResolvers(nil)...)
	for _, a := range m.Apps {
		switch a.ID {
		case "chrome":
			if a.Resolution.Method != MethodEXE || a.Resolution.ResolvedBy != ByOperator {
				t.Errorf("a second pass overwrote the operator's Chrome: %+v", a.Resolution)
			}
		case "7zip":
			if a.Resolution.Status != StatusDropped {
				t.Errorf("a second pass revived a dropped application: %+v", a.Resolution)
			}
		}
	}
}

// A fair match is a suggestion, not an answer: the review still asks, with
// the answer already filled in.
func TestAFairMatchIsOnlyOffered(t *testing.T) {
	known := []KnownApp{{Name: "Mozilla Firefox", Ref: "Mozilla.Firefox", Method: MethodWinget}}
	n := NameMatch{Known: known}

	// The same product, named as a registry names it.
	r, _, ok := n.Find(App{DisplayName: "Mozilla Firefox (x64 en-US)", Publisher: "Mozilla"})
	if !ok || r.Status != StatusResolved || r.Ref != "Mozilla.Firefox" {
		t.Errorf("Firefox: %+v (%v)", r, ok)
	}
	// Something that shares a word and nothing else stays unresolved.
	if r, _, ok := n.Find(App{DisplayName: "Mozilla Maintenance Service"}); ok && r.Status == StatusResolved {
		t.Errorf("the maintenance service was installed as Firefox: %+v", r)
	}
	// And something with no relation at all is not offered either.
	if _, _, ok := n.Find(App{DisplayName: "Dentrix G7.6", Publisher: "Henry Schein One"}); ok {
		t.Error("the practice software matched a package")
	}
}

// Software nobody can place is what the review is for, and it must arrive
// there marked as such rather than quietly dropped.
func TestWhatCannotBePlacedIsLeftForThePerson(t *testing.T) {
	m := &Manifest{Apps: []App{
		{ID: "dentrix", DisplayName: "Dentrix G7.6", Publisher: "Henry Schein One", SourceKind: SourceWin32},
		{ID: "labdental", DisplayName: "Lab Dental Suite", Publisher: "Lab Dental Systems", SourceKind: SourceWin32},
	}}
	res := Resolve(m, defaultResolvers(nil)...)
	if res.Left != 2 || res.Resolved != 0 {
		t.Errorf("%s", res.Summary())
	}
	for _, a := range m.Apps {
		if a.Resolution.Status == StatusResolved {
			t.Errorf("%s was resolved to %q on no evidence", a.DisplayName, a.Resolution.Ref)
		}
	}
}

// DSKY's own table is built from the picker's list, so a package added there
// helps a migration the same day -- and the runtimes and the
// nothing-to-install entries are the migration's own additions.
func TestTheBundledTableIsTheAppList(t *testing.T) {
	tbl := BuiltinTable()
	if err := tbl.Validate(); err != nil {
		t.Fatalf("the bundled table does not validate: %v", err)
	}
	if len(tbl.Entries) < 90 {
		t.Errorf("%d entries, expected the app picker's list and more", len(tbl.Entries))
	}
	find := func(name, publisher string) Resolution {
		r, _, _ := tbl.Lookup(App{DisplayName: name, Publisher: publisher})
		return r
	}
	if r := find("Microsoft Edge", "Microsoft Corporation"); r.Status != StatusDropped {
		t.Errorf("Edge comes with Windows: %+v", r)
	}
	if r := find("Microsoft Edge WebView2 Runtime", "Microsoft Corporation"); r.Ref != "Microsoft.EdgeWebView2Runtime" || r.Order != OrderPrereq {
		t.Errorf("WebView2: %+v", r)
	}
	if r := find("RustDesk", ""); r.Method != MethodManual || !strings.Contains(r.Ref, "rustdesk.com") {
		t.Errorf("RustDesk is not in winget and the note says where it is: %+v", r)
	}
	// A runtime's specific pattern must win over any general one that follows.
	if r := find("Microsoft Visual C++ 2015-2022 Redistributable (x64) - 14.40.33810", "Microsoft Corporation"); r.Ref != "Microsoft.VCRedist.2015+.x64" {
		t.Errorf("the C++ runtime: %+v", r)
	}
}

func TestSimilarityKnowsNamesFromNoise(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool // at least a confident match
	}{
		{"7-Zip 24.08 (x64 edition)", "7-Zip", true},
		{"Notepad++ (64-bit x64)", "Notepad++", true},
		{"Google Chrome", "Google Chrome", true},
		{"VLC media player", "VLC media player", true},
		{"Microsoft Teams", "Microsoft Edge", false},
		{"Adobe Acrobat Reader (64-bit)", "Adobe Acrobat Reader", true},
		{"Lab Dental Suite", "LibreOffice", false},
		{"Zoom Workplace (64-bit)", "Zoom", true},
	} {
		got := similarity(c.a, c.b)
		if (got >= ConfidentMatch) != c.want {
			t.Errorf("%q vs %q: %.2f (want confident=%v)", c.a, c.b, got, c.want)
		}
	}
}

// "Nobody has looked" and "looked and found nothing" are different states,
// and the difference is what lets the resolver be run again after somebody
// adds the entry that places the missing application -- without ever
// reopening something a person decided.
func TestResolvingAgainRetriesOnlyItsOwnFailures(t *testing.T) {
	m := &Manifest{Apps: []App{
		{ID: "labdental", DisplayName: "Lab Dental Suite", SourceKind: SourceWin32},
		{ID: "leftalone", DisplayName: "Some Old Tool", SourceKind: SourceWin32,
			Resolution: Resolution{Status: StatusUnmapped, ResolvedBy: ByOperator}},
	}}
	// One is tried and not placed; the other is the operator's and is not
	// touched at all, so it is not counted as work the resolver did.
	if res := Resolve(m, GlobalTable()); res.Left != 1 {
		t.Fatalf("%s", res.Summary())
	}
	if s := m.Apps[0].Resolution.Status; s != StatusUnmapped {
		t.Errorf("after looking and finding nothing: %q", s)
	}

	// The operator adds the site's own entry and runs it again.
	site, err := ParseTable([]byte(`{"schema_version":"1.0","entries":[
	  {"match":{"display_name":"^Lab Dental Suite"},"resolution":{"method":"exe","ref":"\\\\fs\\install\\labdental.exe","args":["/S"]}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	res := Resolve(m, SiteTable(site), GlobalTable())
	if res.Resolved != 1 {
		t.Errorf("the new entry did not take: %s", res.Summary())
	}
	if r := m.Apps[0].Resolution; r.Method != MethodEXE || r.Order != OrderDefault {
		t.Errorf("Lab Dental: %+v", r)
	}
	// The one an operator had left unmapped is still theirs.
	if r := m.Apps[1].Resolution; r.ResolvedBy != ByOperator || r.Status != StatusUnmapped {
		t.Errorf("an operator's decision was reopened: %+v", r)
	}
}

// One package installs once, however many times the machine registered it.
func TestOneProductInTwoHivesInstallsOnce(t *testing.T) {
	m := &Manifest{Apps: []App{
		{ID: "sevenzip86", DisplayName: "7-Zip 24.08", SourceKind: SourceWin32, Arch: "x86"},
		{ID: "sevenzip64", DisplayName: "7-Zip 24.08 (x64 edition)", SourceKind: SourceWin32, Arch: "x64"},
		{ID: "npp", DisplayName: "Notepad++ (64-bit x64)", SourceKind: SourceWin32, Arch: "x64"},
	}}
	Resolve(m, GlobalTable(), NameMatch{Known: KnownApps()})
	order := m.InstallOrder()
	if len(order) != 2 {
		names := []string{}
		for _, a := range order {
			names = append(names, a.DisplayName+" -> "+a.Resolution.Ref)
		}
		t.Fatalf("install order: %v", names)
	}
	// Both are still in the manifest and in the report: what the machine had
	// is a fact, and only the installing is deduplicated.
	if len(m.Apps) != 3 {
		t.Errorf("the manifest lost an application: %d", len(m.Apps))
	}
}

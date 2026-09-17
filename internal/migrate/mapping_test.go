package migrate

import (
	"path/filepath"
	"strings"
	"testing"
)

const sampleTable = `{
  "schema_version": "1.0",
  "site": "mead",
  "entries": [
    { "match": { "display_name": "^Dentrix" },
      "resolution": { "method": "exe", "ref": "\\\\fileserver\\installers\\dentrix\\setup.exe", "args": ["/S", "/v/qn"], "order": 60 },
      "config_capture": { "paths": ["C:\\ProgramData\\Dentrix\\*.ini"], "registry_keys": [] },
      "added_by": "dusty", "added_at": "2026-09-17", "note": "needs SQL Express first" },
    { "match": { "display_name": "^Dell SupportAssist" }, "resolution": { "status": "dropped" }, "note": "OEM bloat" },
    { "match": { "display_name": "^Acrobat", "publisher": "^Adobe" },
      "resolution": { "method": "winget", "ref": "Adobe.Acrobat.Reader.64-bit" } }
  ]
}`

func TestTableAppliesOperatorDecisions(t *testing.T) {
	tbl, err := ParseTable([]byte(sampleTable))
	if err != nil {
		t.Fatal(err)
	}
	// A line-of-business application nobody's index has ever heard of.
	r, cc, ok := tbl.Lookup(App{DisplayName: "Dentrix G7.6", SourceKind: SourceWin32})
	if !ok || r.Status != StatusResolved || r.Method != MethodEXE || r.ResolvedBy != ByMapping || r.Order != 60 {
		t.Fatalf("Dentrix: %+v (found %v)", r, ok)
	}
	if cc == nil || len(cc.Paths) != 1 {
		t.Errorf("Dentrix config capture: %+v", cc)
	}

	// "Never migrate this, don't ask again." A dropped entry carries no
	// install source, whatever the table said.
	r, _, ok = tbl.Lookup(App{DisplayName: "Dell SupportAssist", SourceKind: SourceWin32})
	if !ok || r.Status != StatusDropped || r.Method != "" || r.Ref != "" {
		t.Errorf("SupportAssist: %+v (found %v)", r, ok)
	}

	// A publisher in the match has to match too, so one vendor's "Acrobat"
	// does not answer for another's.
	if _, _, ok := tbl.Lookup(App{DisplayName: "Acrobat Helper", Publisher: "Nitro Software"}); ok {
		t.Error("an entry matched the wrong publisher")
	}
	if _, _, ok := tbl.Lookup(App{DisplayName: "Acrobat Reader", Publisher: "Adobe Inc."}); !ok {
		t.Error("the Adobe entry did not match Adobe")
	}

	// Nothing in the table means nothing claimed — that application is the
	// operator's to settle in review.
	if _, _, ok := tbl.Lookup(App{DisplayName: "Some Practice Tool 3"}); ok {
		t.Error("an application matched an entry that should not cover it")
	}
}

// What the review saves is what the next machine reads: the same decision,
// matched on the name as registered, not on it as a pattern.
func TestRememberRoundTripsAndDoesNotGrow(t *testing.T) {
	tbl := &Table{}
	app := App{DisplayName: "Smith & Sons (Practice) 4.2", Publisher: "Smith + Sons, Inc.", SourceKind: SourceWin32}
	dec := Resolution{Status: StatusResolved, Method: MethodMSI, Ref: `\\fs\installers\smith.msi`, Args: []string{"/qn"}, Order: OrderDefault}
	tbl.Remember(app, dec, nil, "dusty")

	path := filepath.Join(t.TempDir(), MappingName)
	if err := tbl.Save(path); err != nil {
		t.Fatal(err)
	}
	again, err := LoadTable(path)
	if err != nil {
		t.Fatal(err)
	}
	r, _, ok := again.Lookup(app)
	if !ok {
		t.Fatalf("a remembered decision did not match the application it came from: %+v", again.Entries)
	}
	if r.Method != MethodMSI || r.Ref != dec.Ref || strings.Join(r.Args, " ") != "/qn" {
		t.Errorf("remembered: %+v", r)
	}
	// A name is not a pattern: the brackets and the plus must have been
	// escaped, or this entry would match nothing (or everything).
	if !strings.Contains(again.Entries[0].Match.DisplayName, `\(Practice\)`) {
		t.Errorf("name was stored as a live pattern: %q", again.Entries[0].Match.DisplayName)
	}
	// Reviewing the same application again replaces the entry, keeping the
	// note somebody wrote on it.
	again.Entries[0].Note = "licence key in the password manager"
	again.Remember(app, Resolution{Status: StatusDropped}, nil, "dusty")
	if len(again.Entries) != 1 {
		t.Errorf("the table grew a duplicate: %d entries", len(again.Entries))
	}
	if again.Entries[0].Note != "licence key in the password manager" {
		t.Errorf("the note was lost: %+v", again.Entries[0])
	}
}

func TestTableRefusesNonsense(t *testing.T) {
	for _, c := range []struct{ what, body, says string }{
		{"no version", `{"entries":[]}`, "schema_version"},
		{"later version", `{"schema_version":"2.0","entries":[]}`, "later DSKY"},
		{"no match", `{"schema_version":"1.0","entries":[{"match":{},"resolution":{"method":"winget","ref":"A.B"}}]}`, "display_name"},
		{"bad pattern", `{"schema_version":"1.0","entries":[{"match":{"display_name":"^Dentrix("},"resolution":{"method":"winget","ref":"A.B"}}]}`, "not a valid pattern"},
		{"no decision", `{"schema_version":"1.0","entries":[{"match":{"display_name":"^X"},"resolution":{}}]}`, "say how it installs"},
		{"method with no ref", `{"schema_version":"1.0","entries":[{"match":{"display_name":"^X"},"resolution":{"method":"msi"}}]}`, "needs a ref"},
		{"unknown field", `{"schema_version":"1.0","entries":[],"auto_approve":true}`, "auto_approve"},
	} {
		if _, err := ParseTable([]byte(c.body)); err == nil {
			t.Errorf("%s: accepted", c.what)
		} else if !strings.Contains(err.Error(), c.says) {
			t.Errorf("%s: error %q does not mention %q", c.what, err, c.says)
		}
	}
	// A site with no decisions yet is the normal state of a new site, not an
	// error to report at the operator.
	tbl, err := LoadTable(filepath.Join(t.TempDir(), "nothing-here.json"))
	if err != nil || tbl == nil || len(tbl.Entries) != 0 {
		t.Errorf("missing table: %v %+v", err, tbl)
	}
}

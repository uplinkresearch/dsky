package migrate

import (
	"strings"
	"testing"
)

// A review is a conversation, so it is tested as one: a script of what a
// person types, and then what the plan says afterwards. The transcript is
// worth reading in these tests -- if it does not make sense here it will not
// make sense at a bench.

func runReview(t *testing.T, m *Manifest, site *Table, typed string) (string, *Review) {
	t.Helper()
	var out strings.Builder
	r := &Review{
		In: strings.NewReader(typed), Out: &out, By: "dusty",
		Site: site, SitePath: "mappings.json",
		Save: func(*Table) error { return nil },
	}
	if _, err := r.Run(m); err != nil {
		t.Fatalf("review: %v\n%s", err, out.String())
	}
	return out.String(), r
}

// The whole of a review of the machine this feature exists for: a blocker, an
// application nobody could place, a store to stage files through, and an
// approval at the end.
func TestAReviewSettlesEverythingAndApproves(t *testing.T) {
	m := load(t, "customized")
	site := &Table{SchemaVersion: MappingVersion}
	// blocker: drop the incompatible viewer, remember it
	// unplaced (WWFO Field Sync): an installer on a share, with switches
	// the store: a share, encrypted
	// identity: leave alone; a note; approve
	// The fixture already names a USMT store and asks for it to be encrypted,
	// so the review has nothing to ask there.
	typed := strings.Join([]string{
		"d", // drop AutoVue, which the blocker is about
		"",  // remember that for the site (default yes)
		"f", // Field Sync installs from a file
		`\\wwfo-fs01\installers\fieldsync\setup.exe`,
		"/S",
		"y", // remember it for the site
		"n", // identity: no change
		"replacing the 2018 box; GIS lead signed off", // a note
		"y", // approve
	}, "\n") + "\n"

	out, _ := runReview(t, m, site, typed)

	if !m.Approval.Approved || m.Approval.ApprovedBy != "dusty" {
		t.Fatalf("not approved:\n%s", out)
	}
	if err := m.CheckApproved(); err != nil {
		t.Errorf("the approval it just wrote does not hold: %v", err)
	}
	// The blocker was answered by dropping the application it was about.
	for _, c := range m.Compat {
		if c.Severity == Blocker && !c.Acknowledged {
			t.Errorf("a blocker is still unanswered: %+v", c)
		}
	}
	by := map[string]App{}
	for _, a := range m.Apps {
		by[a.ID] = a
	}
	if r := by["oldcadviewer"].Resolution; r.Status != StatusDropped || r.ResolvedBy != ByOperator {
		t.Errorf("AutoVue: %+v", r)
	}
	if r := by["fieldmapssync"].Resolution; r.Method != MethodEXE ||
		!strings.Contains(r.Ref, "fieldsync") || strings.Join(r.Args, " ") != "/S" {
		t.Errorf("Field Sync: %+v", r)
	}
	if m.Data.StorePath == "" || !m.Data.Encrypted {
		t.Errorf("the store: %+v", m.Data)
	}
	if len(m.Notes) == 0 || !strings.Contains(m.Notes[len(m.Notes)-1], "GIS lead") {
		t.Errorf("notes: %v", m.Notes)
	}
	// Both decisions were offered to the site's table and taken.
	if len(site.Entries) != 2 {
		t.Errorf("the site learned %d decision(s): %+v", len(site.Entries), site.Entries)
	}
	// And the transcript reads like something a person can follow.
	for _, want := range []string{"BLOCKER", "AutoVue 2D Professional", "WWFO Field Sync",
		"Remember this for the rest of this site?", "Approve this plan as dusty?", "Approved."} {
		if !strings.Contains(out, want) {
			t.Errorf("the review never said %q:\n%s", want, out)
		}
	}
}

// Leaving things for later is allowed, and then the plan cannot be approved:
// the review says so rather than approving something with holes in it.
func TestAReviewThatSettlesNothingApprovesNothing(t *testing.T) {
	m := load(t, "customized")
	// s = leave the blocker, s = leave the application, then enter through
	// the rest.
	out, _ := runReview(t, m, nil, "s\ns\n\n\n\n\n")
	if m.Approval.Approved {
		t.Error("a plan with an unanswered blocker was approved")
	}
	if !strings.Contains(out, "Not ready to approve") {
		t.Errorf("the review did not say why it stopped:\n%s", out)
	}
	if !strings.Contains(out, "blocker") {
		t.Errorf("it did not say which thing is outstanding:\n%s", out)
	}
}

// Enter on an application's question means "decide later", never a guess --
// somebody holding down Enter through a long list must not be agreeing to
// installs they never read.
func TestEnterNeverDecides(t *testing.T) {
	m := &Manifest{
		SchemaVersion: SchemaVersion, GeneratedBy: "test",
		Apps: []App{{ID: "mystery", DisplayName: "Some Practice Tool", SourceKind: SourceWin32,
			Resolution: Resolution{Status: StatusUnmapped, Ref: "Some.Guess", Confidence: 0.7, ResolvedBy: ByAuto}}},
		Data: Data{Strategy: DataNoneStrat},
	}
	runReview(t, m, nil, strings.Repeat("\n", 8))
	if r := m.Apps[0].Resolution; r.Status != StatusUnmapped || r.ResolvedBy == ByOperator {
		t.Errorf("Enter decided something: %+v", r)
	}
	if m.Approval.Approved {
		t.Error("Enter approved a plan")
	}
}

// Accepting the resolver's suggestion is one key, because that is the common
// case and a review that takes ten keystrokes per application does not get
// done.
func TestAcceptingASuggestionIsOneKey(t *testing.T) {
	m := &Manifest{
		SchemaVersion: SchemaVersion, GeneratedBy: "test",
		Apps: []App{{ID: "npp", DisplayName: "Notepad++ (64-bit x64)", SourceKind: SourceWin32,
			Resolution: Resolution{Status: StatusUnmapped, Method: MethodWinget,
				Ref: "Notepad++.Notepad++", Confidence: 0.78, ResolvedBy: ByAuto}}},
		Data: Data{Strategy: DataNoneStrat},
	}
	site := &Table{SchemaVersion: MappingVersion}
	out, _ := runReview(t, m, site, "y\ny\n\ny\n")
	r := m.Apps[0].Resolution
	if r.Status != StatusResolved || r.Ref != "Notepad++.Notepad++" || r.ResolvedBy != ByOperator {
		t.Errorf("accepted suggestion: %+v\n%s", r, out)
	}
	if !strings.Contains(out, "78% sure") {
		t.Errorf("the review did not say how sure the suggestion was:\n%s", out)
	}
	if len(site.Entries) != 1 {
		t.Errorf("the site did not learn it: %+v", site.Entries)
	}
	if !m.Approval.Approved {
		t.Errorf("nothing was left outstanding, so it should have approved:\n%s", out)
	}
}

// The path a repeat machine at a known site takes: no questions, and only
// when there is nothing to ask about.
func TestAutoApproveRefusesWork(t *testing.T) {
	m := load(t, "customized")
	if err := AutoApprove(m, "dusty"); err == nil {
		t.Fatal("auto-approved a plan with a blocker and an unplaced application")
	} else if !strings.Contains(err.Error(), "blocker") {
		t.Errorf("%v", err)
	}
	// Settle it the way a review would, and it goes through.
	for i := range m.Compat {
		m.Compat[i].Acknowledged = true
	}
	for i, a := range m.Apps {
		if a.Resolution.Status == StatusUnmapped || a.Resolution.Status == StatusUnset || a.Resolution.Status == StatusBlocked {
			m.Apps[i].Resolution = Resolution{Status: StatusDropped, ResolvedBy: ByOperator}
		}
	}
	if err := AutoApprove(m, "dusty"); err != nil {
		t.Fatalf("a settled plan: %v", err)
	}
	if err := m.CheckApproved(); err != nil {
		t.Errorf("auto-approval does not hold: %v", err)
	}
}

// "Accept the risk" on the missing migration tool cannot leave a plan that
// still says it will copy somebody's files: there would be nothing to copy
// them with, and the first anybody would hear of it is an empty Documents
// folder on a new machine.
func TestAcceptingTheUSMTBlockerSaysTheFilesStayBehind(t *testing.T) {
	m := &Manifest{
		SchemaVersion: SchemaVersion, GeneratedBy: "test",
		Source: Source{Hostname: "PC01"},
		Data:   Data{Strategy: DataUSMT, Users: []string{`LAB\reception`}},
		Compat: []Compat{{Severity: Blocker, Subject: "USMT",
			Reason: "This machine's files are local, so they need USMT, and scanstate.exe was not found."}},
	}
	out, _ := runReview(t, m, nil, "a\nn\n\ny\n")
	if m.Data.Strategy != DataNoneStrat {
		t.Errorf("the plan still claims it will copy files: %+v", m.Data)
	}
	if !strings.Contains(out, "the files will not be copied") {
		t.Errorf("the review did not say what accepting meant:\n%s", out)
	}
	var said bool
	for _, n := range m.Notes {
		if strings.Contains(n, "will NOT be copied") {
			said = true
		}
	}
	if !said {
		t.Errorf("the report would not say it either: %v", m.Notes)
	}
	if !m.Approval.Approved {
		t.Errorf("nothing else was outstanding:\n%s", out)
	}
}

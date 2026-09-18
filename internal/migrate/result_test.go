package migrate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The page answers one question — what do I have to do? — so what needs a hand
// leads, what is waiting for a person is explained as not a fault, and what is
// finished comes last.
func TestTheResultPageLeadsWithWhatNeedsAHand(t *testing.T) {
	r := &Record{
		Machine: "NEWDESK01", Recipe: "windows-11",
		Started:  time.Now().Add(-9 * time.Minute),
		Finished: time.Now(),
		Outcomes: []Outcome{
			{Kind: KindDomain, Name: "domain join", State: StateDone},
			{Kind: KindApp, Name: "Mozilla.Firefox", State: StateDone},
			{Kind: KindPrinter, Name: "Back Office", State: StateFailed,
				Note: "its driver HP UPD PCL6 is not on this machine"},
			{Kind: KindSetting, Name: "explorer.show_extensions", State: StateAtSignIn,
				Note: "this belongs to a person, so it is applied when they first sign in"},
		},
	}
	var b strings.Builder
	if err := RecordReport(&b, r, nil, "v0.7.45"); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	// The template wraps its prose, so match on the words rather than on
	// where the lines happen to break.
	flat := strings.Join(strings.Fields(out), " ")

	// The thing somebody has to act on is above the things they do not.
	needs := strings.Index(out, "Needs a hand")
	waiting := strings.Index(out, "Waiting for the person")
	done := strings.Index(out, ">Done<")
	if !(needs > 0 && needs < waiting && waiting < done) {
		t.Errorf("the page is not in the order somebody reads it: %d %d %d", needs, waiting, done)
	}
	// A failure without its reason is no use to the person holding the page.
	if !strings.Contains(out, "HP UPD PCL6 is not on this machine") {
		t.Error("the page does not say why the printer failed")
	}
	// And the waiting section has to say it is not a fault, or somebody spends
	// an afternoon looking for one.
	for _, want := range []string{"not faults", "first time that person signs in", "administrator account will not show them"} {
		if !strings.Contains(flat, want) {
			t.Errorf("the page does not explain the waiting state: %q missing", want)
		}
	}
	if !strings.Contains(out, "Set up in") || !strings.Contains(out, "NEWDESK01") {
		t.Error("the page does not say which machine, or how long it took")
	}
}

// With the plan alongside, a program is called what people call it. The
// machine only ever knew which file it ran.
func TestTheResultPageNamesProgramsTheWayThePlanDoes(t *testing.T) {
	m := load(t, "customized")
	var ref, want string
	for _, a := range m.Apps {
		if a.Resolution.Method == MethodWinget && a.Resolution.Ref != "" {
			ref, want = a.Resolution.Ref, a.DisplayName
			break
		}
	}
	if ref == "" {
		t.Skip("no winget app in the fixture")
	}
	r := &Record{Machine: "PC", Outcomes: []Outcome{{Kind: KindApp, Name: ref, State: StateDone}}}
	var b strings.Builder
	if err := RecordReport(&b, r, m, "v1"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), want) {
		t.Errorf("the page still calls it %q rather than %q", ref, want)
	}
}

// A record that is not one is refused by name, rather than rendering an empty
// page that looks like a machine with nothing on it.
func TestSomethingThatIsNotARecordIsRefused(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "notes.json")
	if err := os.WriteFile(p, []byte(`{"hello":"world"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRecord(p); err == nil {
		t.Fatal("a file with no machine and no outcomes was accepted as a record")
	}
}

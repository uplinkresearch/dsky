package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The record has to survive a restart, because a migration guarantees one: the
// domain join asks for it before the programs are installed. A record only
// written at the end is a record that is never written on exactly the machines
// this feature exists for.
func TestTheRecordSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	j, err := OpenJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	start := time.Now()

	// First boot: the join happens, then the machine restarts.
	a := &Agent{Dir: dir, Manifest: &Manifest{Recipe: "front-desk"}, J: j}
	a.done(KindDomain, "domain join")
	a.writeResult("NEWDESK01", start, false)

	// Second boot: a fresh agent, which knows nothing of the first.
	b := &Agent{Dir: dir, Manifest: &Manifest{Recipe: "front-desk"}, J: j}
	b.done(KindApp, "Mozilla.Firefox")
	b.failed(KindPrinter, "Back Office", "its driver is not on this machine")
	b.atSignIn(KindSetting, "explorer.show_extensions", "belongs to a person")
	b.writeResult("NEWDESK01", start, true)

	var r Result
	blob, err := os.ReadFile(filepath.Join(dir, ResultName))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(blob, &r); err != nil {
		t.Fatalf("the record is not readable: %v\n%s", err, blob)
	}
	if r.Machine != "NEWDESK01" || r.Recipe != "front-desk" {
		t.Errorf("header: %+v", r)
	}
	if r.Finished.IsZero() {
		t.Error("the record does not say the run finished")
	}
	got := map[string]Outcome{}
	for _, o := range r.Outcomes {
		got[o.Kind+"/"+o.Name] = o
	}
	if len(r.Outcomes) != 4 {
		t.Fatalf("outcomes: %+v", r.Outcomes)
	}
	// The join happened before the restart and is still there afterwards.
	if got["domain/domain join"].State != StateDone {
		t.Errorf("the join was lost across the restart: %+v", r.Outcomes)
	}
	if got["printer/Back Office"].State != StateFailed ||
		got["printer/Back Office"].Note == "" {
		t.Errorf("a failure with no reason is no use: %+v", got["printer/Back Office"])
	}
	// Waiting for somebody to sign in is neither done nor failed. Calling it
	// done would be a lie; calling it failed sends a technician looking for a
	// fault that is not there.
	if got["setting/explorer.show_extensions"].State != StateAtSignIn {
		t.Errorf("per-user work was misreported: %+v", got["setting/explorer.show_extensions"])
	}
}

// A step that was interrupted part-way runs again, and its earlier word must
// not appear twice.
func TestARerunStepReplacesItsOwnOutcome(t *testing.T) {
	dir := t.TempDir()
	j, _ := OpenJournal(dir)
	defer j.Close()

	a := &Agent{Dir: dir, Manifest: &Manifest{}, J: j}
	a.failed(KindApp, "Mozilla.Firefox", "no network yet")
	a.writeResult("PC", time.Now(), false)

	b := &Agent{Dir: dir, Manifest: &Manifest{}, J: j}
	b.done(KindApp, "Mozilla.Firefox")
	b.writeResult("PC", time.Now(), true)

	var r Result
	blob, _ := os.ReadFile(filepath.Join(dir, ResultName))
	if err := json.Unmarshal(blob, &r); err != nil {
		t.Fatal(err)
	}
	if len(r.Outcomes) != 1 {
		t.Fatalf("the same program is in the record twice: %+v", r.Outcomes)
	}
	if r.Outcomes[0].State != StateDone {
		t.Errorf("the later run should have the last word: %+v", r.Outcomes[0])
	}
}

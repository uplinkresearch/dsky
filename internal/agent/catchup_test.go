package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sampleRecord() OfflineRecord {
	return OfflineRecord{
		BuiltAt: "2026-09-19T10:00:00Z",
		Programs: []OfflineEntry{
			{ID: "Google.Chrome", Version: "153.0.8010.53", File: "chrome64.msi"},
			{ID: "7zip.7zip", Version: "26.03", File: "7z2603-x64.msi"},
		},
	}
}

// The script is a .cmd, run by a scheduled task and by whoever finds it. Two
// things about cmd.exe make it easy to write one that lies: it reads a file
// with Unix line endings as a single line, and it expands a variable inside a
// parenthesised block when the block is parsed rather than when it runs --
// which is exactly how the batch first-boot script this agent replaced came to
// log a failed driver extract as "exited with 0".
func TestTheCatchUpScriptReadsItsExitCodesWhenTheyHappen(t *testing.T) {
	s := catchUpScriptText(sampleRecord())
	if !strings.Contains(s, "\r\n") {
		t.Fatal("no Windows line endings, which cmd.exe reads as one long line")
	}
	for _, line := range strings.Split(s, "\r\n") {
		if !strings.Contains(line, "%ERRORLEVEL%") {
			continue
		}
		// Every ERRORLEVEL read must be its own statement. Inside "if (" ...
		// ")" the value would be the one from before the block started.
		if strings.HasPrefix(strings.TrimSpace(line), "  ") || strings.Contains(line, "if (") {
			t.Errorf("an exit code is read inside a block, where cmd expands it too early: %q", line)
		}
	}
	// Every package must be named, or a machine quietly never updates one.
	for _, p := range sampleRecord().Programs {
		if !strings.Contains(s, "call :update "+p.ID) {
			t.Errorf("the script never updates %s", p.ID)
		}
	}
	// The three codes that mean "nothing to do here" must all be accepted, or
	// a machine that is entirely current keeps its sign-in task forever.
	for _, code := range []string{"0", "-1978335189", "-1978335212"} {
		if !strings.Contains(s, `if "%ERRORLEVEL%"=="`+code+`" goto :eof`) {
			t.Errorf("exit code %s is not treated as nothing-to-do", code)
		}
	}
	// And it must stop running once it has nothing to do.
	if !strings.Contains(s, `schtasks /delete /tn "`+catchUpTask+`" /f`) {
		t.Error("the script never takes its own task away, so it runs at every sign-in forever")
	}
	// It must not delete itself: the next technician wants to re-run it.
	if strings.Contains(s, "%~f0") {
		t.Error("the script deletes itself")
	}
}

// The script names the same packages however the recipe was ordered, so two
// builds of the same set of programs do not produce two different scripts.
func TestTheCatchUpScriptIsTheSameWhateverOrderThePackagesArrivedIn(t *testing.T) {
	rec := sampleRecord()
	reversed := OfflineRecord{BuiltAt: rec.BuiltAt, Programs: []OfflineEntry{rec.Programs[1], rec.Programs[0]}}
	if catchUpScriptText(rec) != catchUpScriptText(reversed) {
		t.Error("the same programs in a different order produced a different script")
	}
}

// The record answers "what was this machine built with?" on a machine nobody
// watched being built. It is written before anything is attempted, because a
// catch-up that fails half way through must still leave that answer behind.
func TestTheRecordIsWrittenBeforeAnythingIsTried(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ProgramData", dir)
	a := &Agent{}
	if err := a.writeCatchUpFiles(sampleRecord()); err != nil {
		t.Fatalf("writing: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "DSKY", offlineRecordName))
	if err != nil {
		t.Fatalf("reading it back: %v", err)
	}
	var got OfflineRecord
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("it is not readable JSON: %v", err)
	}
	if got.BuiltAt != "2026-09-19T10:00:00Z" || len(got.Programs) != 2 {
		t.Errorf("record = %+v", got)
	}
	if got.Programs[0].ID != "Google.Chrome" || got.Programs[0].Version != "153.0.8010.53" {
		t.Errorf("the version the media carried was not recorded: %+v", got.Programs[0])
	}
	if _, err := os.Stat(filepath.Join(dir, "DSKY", catchUpScript)); err != nil {
		t.Errorf("no script beside the record: %v", err)
	}
}

// A manifest with no offline programs must not leave a record, a script or a
// task on an ordinary machine.
func TestNothingIsLeftBehindWhenTheProgramsCameFromTheVendor(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ProgramData", dir)
	a := &Agent{Manifest: &Manifest{Apps: &Apps{Winget: []string{"Google.Chrome"}}}}
	a.catchUpStep()
	if _, err := os.Stat(filepath.Join(dir, "DSKY")); err == nil {
		t.Error("an online build left the offline catch-up's files on the machine")
	}
}

// The task must run as a person rather than SYSTEM: winget arrives as a
// per-user package, and there is no winget for SYSTEM to run.
func TestTheCatchUpTaskRunsAsTheSignedInPerson(t *testing.T) {
	x := catchUpTaskXML(`CORP\jo`, `C:\ProgramData\DSKY\update-programs.cmd`)
	for _, want := range []string{
		"<UserId>CORP\\jo</UserId>",
		"<LogonType>InteractiveToken</LogonType>",
		"<RunLevel>HighestAvailable</RunLevel>",
		"<LogonTrigger>",
		"<CalendarTrigger>", // a machine left signed in for a month still catches up
		"update-programs.cmd",
	} {
		if !strings.Contains(x, want) {
			t.Errorf("the task XML has no %q", want)
		}
	}
	// Letting Windows decide whether the network is good enough would leave a
	// machine stale with nothing saying why; the script decides.
	if !strings.Contains(x, "<RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable>") {
		t.Error("the task lets Windows decide whether to run it on this network")
	}
}

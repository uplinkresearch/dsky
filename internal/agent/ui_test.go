package agent

import (
	"bytes"
	"image/png"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Everything the screen does must be safe on a machine that has no window --
// another operating system, a session with no desktop, a window that would not
// create. The agent calls these from the middle of provisioning, and a machine
// must never be left half built because a window failed to open.
func TestNoWindowIsNotAFailure(t *testing.T) {
	var s *screen // what openScreen returns when there can be no window
	s.Doing("drivers", "unpacking")
	s.Detail("264 driver files")
	s.Finished("drivers", 0)
	s.Restarting("finish installing the drivers")
	s.Summary("This machine is ready", []string{"all done"})
	s.Close()
	s.WaitDismiss() // must not block

	if got := s.AskRestart("finish installing the drivers", time.Minute); got != choiceNone {
		t.Errorf("with no window the restart must go ahead by itself, got %q", got)
	}
}

// With nobody standing there the countdown runs out and the machine restarts
// itself, which is what an unattended bench needs.
func TestTheCountdownRunsOutWhenNobodyIsThere(t *testing.T) {
	s := testScreen()
	start := time.Now()
	if got := s.AskRestart("finish installing the drivers", 200*time.Millisecond); got != choiceNone {
		t.Fatalf("got %q, want the countdown to run out", got)
	}
	if time.Since(start) < 150*time.Millisecond {
		t.Error("it did not wait for the countdown")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.buttons) != 0 {
		t.Errorf("the buttons are still up: %v", s.buttons)
	}
}

// Somebody standing there can take the restart immediately.
func TestRestartNowIsTakenAtOnce(t *testing.T) {
	s := testScreen()
	go func() {
		waitForButtons(s)
		s.clicked <- 0 // "Restart now"
	}()
	start := time.Now()
	if got := s.AskRestart("finish installing the drivers", 10*time.Second); got != choiceNow {
		t.Fatalf("got %q, want an immediate restart", got)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("clicking Restart now waited for the countdown anyway")
	}
}

// While the countdown runs, the screen says what is about to happen and why.
func TestTheCountdownSaysWhatIsHappening(t *testing.T) {
	s := testScreen()
	go func() {
		waitForButtons(s)
		s.mu.Lock()
		note := s.note
		s.mu.Unlock()
		if !strings.Contains(note, "Restarting in") || !strings.Contains(note, "finish installing the drivers") {
			t.Errorf("the screen says %q", note)
		}
		if !strings.Contains(note, "carries on by itself") {
			t.Errorf("it does not say the machine carries on afterwards: %q", note)
		}
		s.clicked <- 0
	}()
	s.AskRestart("finish installing the drivers", 5*time.Second)
}

// The finish screen waits for somebody to say they have seen it -- the work is
// over by then, so nothing is held up.
func TestTheFinishScreenWaitsToBeDismissed(t *testing.T) {
	s := testScreen()
	s.Summary("This machine is ready", []string{"Everything the build asked for is installed."})
	done := make(chan struct{})
	go func() { s.WaitDismiss(); close(done) }()
	select {
	case <-done:
		t.Fatal("it did not wait")
	case <-time.After(100 * time.Millisecond):
	}
	s.clicked <- 0
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("clicking Finish did not dismiss it")
	}
}

// A window closed from the keyboard must also release a finished machine,
// rather than leaving the agent waiting on a button nobody can press.
func TestAClosedWindowReleasesIt(t *testing.T) {
	s := testScreen()
	done := make(chan struct{})
	go func() { s.WaitDismiss(); close(done) }()
	s.dismissed()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("closing the window left the agent waiting")
	}
	s.dismissed() // twice must not panic
}

// The checklist follows the work: one step in progress at a time, and what
// happened to the others still readable.
func TestTheChecklistFollowsTheWork(t *testing.T) {
	s := testScreen()
	s.Doing("drivers", "unpacking")
	s.Detail("installing 264 driver files (this is the long part)")
	s.Finished("drivers", 0)
	s.Doing("apps", "")
	s.Finished("apps", 2)

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.steps) != 2 {
		t.Fatalf("%d steps on the screen, want 2", len(s.steps))
	}
	if s.steps[0].State != stepDone {
		t.Errorf("drivers is in state %v, want done", s.steps[0].State)
	}
	if s.steps[1].State != stepProblem {
		t.Errorf("a step with two failures is in state %v, want the one that says so", s.steps[1].State)
	}
	// A finished step must not still say what it was in the middle of.
	if strings.Contains(s.steps[0].Detail, "installing") {
		t.Errorf("a finished step still reads as in progress: %q", s.steps[0].Detail)
	}
}

// What it says at the end.
func TestTheSummaryIsReadableFromAcrossTheRoom(t *testing.T) {
	if h := summaryHeading(nil); h != "This machine is ready" {
		t.Errorf("a clean run says %q", h)
	}
	if h := summaryHeading([]string{"a", "b"}); !strings.Contains(h, "2 problem") {
		t.Errorf("a run with problems says %q", h)
	}
	// Every failure named, up to a point, then a pointer to the log -- a
	// screen with forty lines on it is no better than a blank one.
	many := make([]string, 20)
	for i := range many {
		many[i] = "apps could not install something"
	}
	lines := summaryLines(many, time.Minute)
	if len(lines) > 11 {
		t.Errorf("%d lines on the finish screen", len(lines))
	}
	if !strings.Contains(strings.Join(lines, "\n"), "firstboot.log") {
		t.Error("it does not say where the full record is")
	}
}

func TestDurationsReadLikeSpeech(t *testing.T) {
	for _, c := range []struct {
		d    time.Duration
		want string
	}{
		{45 * time.Second, "45 seconds"},
		{60 * time.Second, "1 minute"},
		{90 * time.Second, "1 minute"},
		{5 * time.Minute, "5 minutes"},
		{18 * time.Minute, "18 minutes"},
	} {
		if got := shortDur(c.d); got != c.want {
			t.Errorf("shortDur(%s) = %q, want %q", c.d, got, c.want)
		}
	}
}

// testScreen is a screen with no window behind it: the state and the decisions
// are what these tests are about, and the drawing is only checkable on a real
// machine.
func testScreen() *screen {
	return &screen{
		machine: "HP EliteBook x360 1040 G8",
		heading: "Setting up this machine",
		clicked: make(chan int, 4),
		dismiss: make(chan struct{}),
		w:       nowindow{},
	}
}

// nowindow accepts everything the screen tells it and draws nothing.
type nowindow struct{}

func (nowindow) refresh() {}
func (nowindow) close()   {}

// waitForButtons waits until the screen is offering a decision.
func waitForButtons(s *screen) {
	for i := 0; i < 200; i++ {
		s.mu.Lock()
		n := len(s.buttons)
		s.mu.Unlock()
		if n > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// After a restart the checklist still shows what was done before it, marked
// done, rather than starting from the step it resumed on.
func TestAResumedRunStillListsWhatWasDoneBefore(t *testing.T) {
	s := testScreen()
	prev := openScreenFn
	openScreenFn = func(string) *screen { return s }
	t.Cleanup(func() { openScreenFn = prev })

	dir := t.TempDir()
	m := &Manifest{Version: ManifestVersion, Recipe: "windows-11", Steps: []string{"drivers", "debloat"},
		Debloat: &Debloat{Preset: "standard"}}
	if err := m.Save(filepath.Join(dir, ManifestName)); err != nil {
		t.Fatal(err)
	}
	st := LoadState(dir)
	st.Finish("drivers") // done before the machine went down

	f := &fakeRun{}
	f.install(t)
	go func() { waitForButtons(s); s.clicked <- 0 }() // press Finish when it appears
	if err := Apply(dir); err != nil {
		t.Fatal(err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	var names []string
	for _, st := range s.steps {
		names = append(names, st.Name)
		if st.Name == "drivers" && st.State != stepDone {
			t.Errorf("drivers, finished before the restart, is shown in state %v", st.State)
		}
	}
	if len(names) < 2 || names[0] != "drivers" {
		t.Errorf("the checklist after a restart is %v; the drivers done before it are missing", names)
	}
}

// A build that restarted is timed from when it began, not from the last boot.
func TestTheFinishScreenTimesTheWholeBuild(t *testing.T) {
	st := &State{Started: time.Now().Add(-50 * time.Minute).Format(time.RFC3339)}
	thisBoot := time.Now().Add(-10 * time.Minute)
	// The start is recorded to the second, so allow that much either way.
	if got := buildTook(st, thisBoot); got < 49*time.Minute+59*time.Second || got > 50*time.Minute+2*time.Second {
		t.Errorf("a build begun fifty minutes ago and restarted ten minutes ago took %s", got)
	}
	// A state with no start recorded falls back to this run.
	if got := buildTook(&State{}, thisBoot); got < 10*time.Minute || got > 10*time.Minute+2*time.Second {
		t.Errorf("with no recorded start: %s", got)
	}
}

// The machine says its name once. Seen on the real HP: "HP HP EliteBook x360
// 1040 G8 Notebook PC", because HP's firmware reports the maker as "HP" and
// the model already begins with it.
func TestTheMachineNameIsNotDoubled(t *testing.T) {
	for _, c := range []struct{ vendor, model, want string }{
		{"HP", "HP EliteBook x360 1040 G8 Notebook PC", "HP EliteBook x360 1040 G8 Notebook PC"},
		{"Dell Inc.", "OptiPlex 3070", "Dell Inc. OptiPlex 3070"},
		{"LENOVO", "20XW troubleshooting", "LENOVO 20XW troubleshooting"},
		{"Framework", "Framework Laptop 13", "Framework Laptop 13"},
		{"QEMU", "Standard PC (Q35 + ICH9, 2009)", "QEMU Standard PC (Q35 + ICH9, 2009)"},
		{"HP", "", "HP"},
		{"", "EliteBook", "EliteBook"},
		{"", "", "unknown machine"},
	} {
		if got := machineName(c.vendor, c.model); got != c.want {
			t.Errorf("machineName(%q, %q) = %q, want %q", c.vendor, c.model, got, c.want)
		}
	}
}

// The wordmark the status window draws has to be a real image the agent can
// decode, at a size that fits the screen it is drawn on.
func TestTheWordmarkDecodes(t *testing.T) {
	img, err := png.Decode(bytes.NewReader(logoPNG))
	if err != nil {
		t.Fatalf("the embedded wordmark does not decode: %v", err)
	}
	b := img.Bounds()
	if b.Dx() < 120 || b.Dx() > 600 || b.Dy() < 40 || b.Dy() > 300 {
		t.Errorf("the wordmark is %dx%d; too big or small for the window", b.Dx(), b.Dy())
	}
	if len(logoPNG) > 100*1024 {
		t.Errorf("the wordmark is %d KB; it travels in every agent on every stick", len(logoPNG)/1024)
	}
	// Something has to be drawn: an image of one flat colour would mean the
	// mark was lost in scaling.
	seen := map[uint32]bool{}
	for y := b.Min.Y; y < b.Max.Y; y += 3 {
		for x := b.Min.X; x < b.Max.X; x += 3 {
			r, g, bb, a := img.At(x, y).RGBA()
			seen[r>>8<<24|g>>8<<16|bb>>8<<8|a>>8] = true
		}
	}
	if len(seen) < 4 {
		t.Errorf("the wordmark has %d distinct pixels; it is not a picture of anything", len(seen))
	}
}

// Escape goes only to the Start menu and its search box. Sent anywhere else it
// could close somebody's dialog, and sent to the status window it hides it.
func TestOnlyTheStartMenuGetsAnEscape(t *testing.T) {
	for _, p := range []string{
		`C:\Windows\SystemApps\Microsoft.Windows.StartMenuExperienceHost_cw5n1h2txyewy\StartMenuExperienceHost.exe`,
		`C:\Windows\SystemApps\MicrosoftWindows.Client.CBS_cw5n1h2txyewy\SearchHost.exe`,
		`C:\WINDOWS\SYSTEMAPPS\X\STARTMENUEXPERIENCEHOST.EXE`,
	} {
		if !isShellFlyoutImage(p) {
			t.Errorf("%s is the Start menu and would not be closed", p)
		}
	}
	for _, p := range []string{
		`C:\Windows\Setup\Scripts\dsky-agent.exe`,
		`C:\Windows\explorer.exe`,
		`C:\Program Files\Google\Chrome\Application\chrome.exe`,
		`C:\Windows\System32\msiexec.exe`,
		`C:\Users\user\AppData\Local\StartMenuExperienceHost.exe.bak`,
		``,
	} {
		if isShellFlyoutImage(p) {
			t.Errorf("%q would be sent an Escape", p)
		}
	}
}

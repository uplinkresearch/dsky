package agent

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func standaloneManifest() *Manifest {
	return &Manifest{Version: ManifestVersion, Recipe: "office-apps", Mode: ModeStandalone, Build: "b1",
		Steps: []string{"drivers"}, Drivers: Drivers{Sweep: true}}
}

// Every manifest written before payloads existed has no mode, and must keep
// meaning a first boot. An unknown mode is refused rather than guessed at,
// because guessing "first boot" on a machine in use would take over its screen.
func TestTheModeIsReadStrictly(t *testing.T) {
	dir := t.TempDir()
	save := func(m *Manifest) string {
		p := filepath.Join(dir, ManifestName)
		if err := m.Save(p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	old := &Manifest{Version: ManifestVersion, Recipe: "r"}
	got, err := LoadManifest(save(old))
	if err != nil || got.Standalone() {
		t.Errorf("a manifest with no mode: standalone=%v err=%v; it must mean first boot", got != nil && got.Standalone(), err)
	}

	bad := &Manifest{Version: ManifestVersion, Recipe: "r", Mode: "kiosk"}
	if _, err := LoadManifest(save(bad)); err == nil {
		t.Error("an unknown mode was accepted")
	}

	noBuild := &Manifest{Version: ManifestVersion, Recipe: "r", Mode: ModeStandalone}
	if _, err := LoadManifest(save(noBuild)); err == nil {
		t.Error("a standalone payload with no build was accepted; re-runs could not tell it from a newer one")
	}
}

// A first boot takes the whole screen; a payload on somebody's machine does not.
func TestOnlyAFirstBootTakesOverTheScreen(t *testing.T) {
	for _, c := range []struct {
		mode     string
		takeover bool
	}{{"", true}, {ModeStandalone, false}} {
		dir := t.TempDir()
		m := &Manifest{Version: ManifestVersion, Recipe: "r", Mode: c.mode, Build: "b1"}
		if err := m.Save(filepath.Join(dir, ManifestName)); err != nil {
			t.Fatal(err)
		}
		var asked []bool
		prev := openScreenFn
		openScreenFn = func(_ string, takeover bool) *screen { asked = append(asked, takeover); return nil }
		if _, err := Apply(dir, RunOptions{}); err != nil {
			t.Fatal(err)
		}
		openScreenFn = prev
		if len(asked) != 1 || asked[0] != c.takeover {
			t.Errorf("mode %q: window takeover=%v, want %v", c.mode, asked, c.takeover)
		}
	}
}

// Quiet means no window at all.
func TestQuietOpensNoWindow(t *testing.T) {
	dir := t.TempDir()
	if err := standaloneManifest().Save(filepath.Join(dir, ManifestName)); err != nil {
		t.Fatal(err)
	}
	prev := openScreenFn
	opened := false
	openScreenFn = func(string, bool) *screen { opened = true; return nil }
	t.Cleanup(func() { openScreenFn = prev })
	if _, err := Apply(dir, RunOptions{Quiet: true}); err != nil {
		t.Fatal(err)
	}
	if opened {
		t.Error("a quiet run opened a window")
	}
}

// The shortcuts already on a machine belong to its owner. A payload removes
// only the ones that appear while it runs -- and remembers which were there
// across a restart, so the installers' icons are never mistaken for the
// owner's.
func TestAPayloadLeavesTheOwnersShortcutsAlone(t *testing.T) {
	desk := t.TempDir()
	prevDirs := desktopDirsFn
	desktopDirsFn = func() []string { return []string{desk} }
	t.Cleanup(func() { desktopDirsFn = prevDirs })

	theirs := filepath.Join(desk, "Payroll.lnk")
	if err := os.WriteFile(theirs, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	m := standaloneManifest()
	m.Steps = []string{"apps"}
	m.Apps = &Apps{}
	if err := m.Save(filepath.Join(dir, ManifestName)); err != nil {
		t.Fatal(err)
	}
	// An installer drops an icon while the payload runs.
	f := &fakeRun{}
	f.do = func(name string, args []string) (result, error) {
		os.WriteFile(filepath.Join(desk, "Zoom.lnk"), []byte("x"), 0o644)
		return result{}, nil
	}
	f.install(t)
	m.Apps.Installers = []Installer{{File: "zoom.msi", MSI: true}}
	if err := m.Save(filepath.Join(dir, ManifestName)); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "zoom.msi"), []byte("x"), 0o644)

	if _, err := Apply(dir, RunOptions{Quiet: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(theirs); err != nil {
		t.Error("the owner's own shortcut was deleted")
	}
	if _, err := os.Stat(filepath.Join(desk, "Zoom.lnk")); !os.IsNotExist(err) {
		t.Error("the shortcut the payload's installer added was left behind")
	}

	// A first boot, by contrast, has no owner yet and clears everything.
	dir2 := t.TempDir()
	fb := &Manifest{Version: ManifestVersion, Recipe: "r", Steps: []string{}}
	if err := fb.Save(filepath.Join(dir2, ManifestName)); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(dir2, RunOptions{Quiet: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(theirs); !os.IsNotExist(err) {
		t.Error("a first boot left a shortcut behind")
	}
}

// Only a first boot arranged an automatic sign-in, so only a first boot puts
// one away. A kiosk that signs itself in on purpose must keep doing so.
func TestAPayloadNeverTouchesTheAutomaticSignIn(t *testing.T) {
	for _, c := range []struct {
		mode   string
		disarm bool
	}{{"", true}, {ModeStandalone, false}} {
		dir := t.TempDir()
		m := &Manifest{Version: ManifestVersion, Recipe: "r", Mode: c.mode, Build: "b1"}
		if err := m.Save(filepath.Join(dir, ManifestName)); err != nil {
			t.Fatal(err)
		}
		disarmed := false
		prev := disarmAutoLogonFn
		disarmAutoLogonFn = func(*Agent) { disarmed = true }
		if _, err := Apply(dir, RunOptions{Quiet: true}); err != nil {
			t.Fatal(err)
		}
		disarmAutoLogonFn = prev
		if disarmed != c.disarm {
			t.Errorf("mode %q: automatic sign-in disarmed=%v, want %v", c.mode, disarmed, c.disarm)
		}
	}
}

// A machine somebody may be using is not restarted by itself: the run stops
// at the restart Windows asked for and carries on after the next one. Only an
// unattended run restarts it.
func TestAPayloadAsksBeforeRestarting(t *testing.T) {
	for _, c := range []struct {
		name       string
		unattended bool
		restarts   int
	}{{"attended, no window", false, 0}, {"unattended", true, 1}} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			stageINF(t, dir)
			if err := standaloneManifest().Save(filepath.Join(dir, ManifestName)); err != nil {
				t.Fatal(err)
			}
			res := &fakeResume{}
			res.install(t)
			f := &fakeRun{}
			f.do = func(name string, args []string) (result, error) {
				if name == "pnputil" {
					return result{Code: pnputilRebootNeeded, Out: "Driver package added successfully."}, nil
				}
				return result{}, nil
			}
			f.install(t)

			if _, err := Apply(dir, RunOptions{Quiet: true, Unattended: c.unattended}); err != nil {
				t.Fatal(err)
			}
			if len(res.restarts) != c.restarts {
				t.Errorf("restarted %d time(s), want %d", len(res.restarts), c.restarts)
			}
			if !LoadState(dir).Finished("drivers") {
				t.Error("the drivers were not recorded as done, so the next run would repeat them")
			}
			if res.ensured == 0 {
				t.Error("nothing arranged for the payload to carry on after the next restart")
			}
		})
	}
}

// The options survive a restart: the resume task starts the agent the same
// way it was started.
func TestTheResumeTaskKeepsTheRunsOptions(t *testing.T) {
	got := taskArgs(`C:\ProgramData\DSKY\payloads\office-b1`, RunOptions{Quiet: true, Unattended: true}.Args())
	want := `apply "C:\ProgramData\DSKY\payloads\office-b1" --quiet --unattended`
	if got != want {
		t.Errorf("task arguments %q, want %q", got, want)
	}
}

func TestApplyArguments(t *testing.T) {
	dir, opts, err := parseApplyArgs([]string{"--quiet", `D:\payload`, "--unattended"})
	if err != nil || dir != `D:\payload` || !opts.Quiet || !opts.Unattended {
		t.Errorf("got %q %+v %v", dir, opts, err)
	}
	if _, _, err := parseApplyArgs([]string{"--silent"}); err == nil {
		t.Error("an unknown option was accepted")
	}
	if _, _, err := parseApplyArgs([]string{"a", "b"}); err == nil {
		t.Error("two directories were accepted")
	}
}

// A payload refuses to run without an administrator's token, saying how to
// get one, rather than failing half way through at pnputil.
func TestAPayloadNeedsAnAdministrator(t *testing.T) {
	prev := isElevatedFn
	isElevatedFn = func() bool { return false }
	t.Cleanup(func() { isElevatedFn = prev })
	_, _, err := startStandalone(t.TempDir(), standaloneManifest(), RunOptions{})
	if err == nil || !strings.Contains(err.Error(), "Run DSKY.cmd") {
		t.Errorf("not elevated: %v", err)
	}
}

// Off the stick and onto the machine, then run from there -- with the options
// it was given, and without carrying the stick's old logs along.
func TestAPayloadCopiesItselfOntoTheMachine(t *testing.T) {
	src, root := t.TempDir(), t.TempDir()
	for name, body := range map[string]string{
		"dsky-agent.exe": "agent", ManifestName: "{}", "zoom.msi": "msi",
		LogName: "an old log", StateName: "{}",
	} {
		os.WriteFile(filepath.Join(src, name), []byte(body), 0o644)
	}
	os.MkdirAll(filepath.Join(src, "Drivers", "hp"), 0o755)
	os.WriteFile(filepath.Join(src, "Drivers", "hp", "x.inf"), []byte("inf"), 0o644)

	prevE, prevR, prevRun := isElevatedFn, payloadRootFn, runPayloadCopyFn
	isElevatedFn = func() bool { return true }
	payloadRootFn = func() string { return root }
	var ranExe string
	var ranArgs []string
	runPayloadCopyFn = func(exe string, args []string) (int, error) { ranExe, ranArgs = exe, args; return 3, nil }
	t.Cleanup(func() { isElevatedFn, payloadRootFn, runPayloadCopyFn = prevE, prevR, prevRun })

	m := standaloneManifest()
	elsewhere, problems, err := startStandalone(src, m, RunOptions{Quiet: true})
	if err != nil || !elsewhere || problems != 3 {
		t.Fatalf("elsewhere=%v problems=%d err=%v", elsewhere, problems, err)
	}
	home := filepath.Join(root, "office-apps-b1")
	for _, want := range []string{"dsky-agent.exe", ManifestName, "zoom.msi", filepath.Join("Drivers", "hp", "x.inf")} {
		if _, err := os.Stat(filepath.Join(home, want)); err != nil {
			t.Errorf("%s was not copied: %v", want, err)
		}
	}
	for _, left := range []string{LogName, StateName} {
		if _, err := os.Stat(filepath.Join(home, left)); err == nil {
			t.Errorf("%s from the stick was carried onto the machine", left)
		}
	}
	if ranExe != filepath.Join(home, "dsky-agent.exe") {
		t.Errorf("ran %q, want the copy on the machine", ranExe)
	}
	if strings.Join(ranArgs, " ") != "apply "+home+" --quiet" {
		t.Errorf("ran with %q", ranArgs)
	}

	// Already running from its home: carries on in place.
	elsewhere, _, err = startStandalone(home, m, RunOptions{})
	if err != nil || elsewhere {
		t.Errorf("from its own home: elsewhere=%v err=%v", elsewhere, err)
	}
}

// The exit code is the problem count, capped, and nothing at all when clean.
func TestTheExitCodeCountsProblems(t *testing.T) {
	var codes []int
	prev := exitFn
	exitFn = func(c int) { codes = append(codes, c) }
	t.Cleanup(func() { exitFn = prev })
	exitWith(0)
	exitWith(2)
	exitWith(500)
	if len(codes) != 2 || codes[0] != 2 || codes[1] != 100 {
		t.Errorf("exit codes %v, want [2 100]", codes)
	}
}

var _ = errors.New

// A payload run a second time does the work a second time.
//
// This is what somebody reported: a payload ran and installed none of its
// programs, and the payload they built and ran next went straight to the
// finish screen without attempting anything. A payload lives in a folder
// named for its build, the same build is the same folder, and the state file
// the first run left there said every step was done -- so the second run
// skipped all of them and told them the machine was ready. Somebody who runs
// a payload again is asking for the work, not for a report on the last time.
func TestRunningAPayloadAgainDoesTheWorkAgain(t *testing.T) {
	dir := t.TempDir()
	stageINF(t, dir)
	if err := standaloneManifest().Save(filepath.Join(dir, ManifestName)); err != nil {
		t.Fatal(err)
	}
	f := &fakeRun{}
	f.do = func(name string, args []string) (result, error) {
		if name == "pnputil" {
			return result{Code: 0, Out: "Driver package added successfully."}, nil
		}
		return result{}, nil
	}
	f.install(t)

	if _, err := Apply(dir, RunOptions{Quiet: true}); err != nil {
		t.Fatal(err)
	}
	first := len(f.calls)
	if first == 0 {
		t.Fatal("the first run did nothing, so the second proves nothing")
	}

	if _, err := Apply(dir, RunOptions{Quiet: true}); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) == first {
		t.Error("the second run did nothing: the finished run's state skipped every step")
	}
	if strings.Contains(logText(t, dir), "already done on an earlier boot") {
		t.Error("the second run treated a finished run's steps as its own")
	}
}

// The other half of the same rule: a run that was interrupted is still
// carried on rather than started over, because that is a machine part way
// through a build rather than somebody asking for another one.
func TestAnInterruptedPayloadIsCarriedOnRatherThanRestarted(t *testing.T) {
	dir := t.TempDir()
	st := LoadState(dir)
	st.Finish("drivers")

	got := StateForRun(dir)
	if !got.Finished("drivers") {
		t.Error("an interrupted run's finished step was thrown away, so the drivers would be installed twice")
	}

	// And once that run reaches its end, the next one starts from nothing.
	st.Complete()
	if StateForRun(dir).Finished("drivers") {
		t.Error("a finished run's state was carried into the next run")
	}
}

// What a new run does not start over on: the shortcuts that were on the
// desktop before any payload touched this machine. Reading them again now
// would take the icons the last run installed for the owner's own, and a
// payload that adopts them never removes them again.
func TestTheOwnersShortcutsOutliveTheRunThatRecordedThem(t *testing.T) {
	dir := t.TempDir()
	st := LoadState(dir)
	own := st.OwnShortcuts(func() []string {
		return []string{`C:\Users\dgb\Desktop\Quotes.lnk`}
	})
	if len(own) != 1 {
		t.Fatalf("the owner's shortcuts were recorded as %v", own)
	}
	st.Finish("apps")
	st.Complete()

	next := StateForRun(dir)
	if next.Finished("apps") {
		t.Error("a finished run's steps were carried into the next run")
	}
	again := next.OwnShortcuts(func() []string {
		t.Error("the owner's desktop was read again, after a payload had installed onto it")
		return nil
	})
	if !again[strings.ToLower(`C:\Users\dgb\Desktop\Quotes.lnk`)] {
		t.Errorf("the owner's shortcut was forgotten: %v", again)
	}
}

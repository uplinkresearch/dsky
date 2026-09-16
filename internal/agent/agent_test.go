package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeRun records what the agent would run and lets a test decide what
// happens, so the logic that judges a driver pack can be tested without a
// Windows machine and without a gigabyte of vendor installer.
type fakeRun struct {
	calls []string
	do    func(name string, args []string) (result, error)
}

func (f *fakeRun) install(t *testing.T) {
	t.Helper()
	prev := runner
	runner = func(_ context.Context, name string, args ...string) result {
		f.calls = append(f.calls, name+" "+strings.Join(args, " "))
		if f.do == nil {
			return result{}
		}
		r, _ := f.do(name, args)
		return r
	}
	t.Cleanup(func() { runner = prev })
}

func newAgent(t *testing.T, m *Manifest) (*Agent, string) {
	t.Helper()
	dir := t.TempDir()
	j, err := OpenJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })
	return &Agent{Dir: dir, Manifest: m, J: j, State: LoadState(dir)}, dir
}

func logText(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, LogName))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A pack that unpacks nothing with the switches its catalog lists is retried
// with the other style, and the log says what really happened. This is the
// HP EliteBook failure: the pack printed its usage text, wrote no files and
// exited 0, and the script it replaced recorded that as success.
func TestExtractRetriesWhenNoDriverFilesAppear(t *testing.T) {
	m := &Manifest{Version: ManifestVersion, Recipe: "hp", Steps: []string{"drivers"},
		Drivers: Drivers{Sweep: true, Extracts: []Extract{{
			File: "sp142792.exe", Dir: "hp-pack",
			Args:    []string{"-pdf", "-e", "-s", `-f"{dir}"`},
			AltArgs: []string{"/s", "/e", "/f", `"{dir}"`},
		}}}}
	a, dir := newAgent(t, m)
	os.WriteFile(filepath.Join(dir, "sp142792.exe"), []byte("pack"), 0o755)

	f := &fakeRun{}
	f.do = func(name string, args []string) (result, error) {
		// The old switches "succeed" and write nothing, as the real pack did.
		if strings.Contains(strings.Join(args, " "), "-pdf") {
			return result{Code: 0, Out: "Usage: /s /e /f <target-path>"}, nil
		}
		if strings.HasSuffix(name, "sp142792.exe") {
			dest := filepath.Join(dir, "Drivers", "hp-pack", "net")
			os.MkdirAll(dest, 0o755)
			os.WriteFile(filepath.Join(dest, "e1d.inf"), []byte("inf"), 0o644)
			return result{Code: 0}, nil
		}
		if name == "pnputil" {
			return result{Code: 0, Out: "Driver package added successfully.\n"}, nil
		}
		return result{}, nil
	}
	f.install(t)
	a.driversStep()

	log := logText(t, dir)
	if !strings.Contains(log, "no driver files appeared, retrying") {
		t.Errorf("no retry recorded:\n%s", log)
	}
	if strings.Contains(log, "FAILED") {
		t.Errorf("the retry worked but something was logged as failed:\n%s", log)
	}
	if !strings.Contains(log, "1 driver file(s) present") {
		t.Errorf("the log does not say what was unpacked:\n%s", log)
	}
	var swept bool
	for _, c := range f.calls {
		if strings.HasPrefix(c, "pnputil") {
			swept = true
		}
	}
	if !swept {
		t.Error("pnputil was never run after a successful unpack")
	}
}

// A pack that unpacks nothing either way is reported as a failure, in plain
// words, rather than as a success with an exit code attached.
func TestExtractFailureIsReportedHonestly(t *testing.T) {
	m := &Manifest{Version: ManifestVersion, Recipe: "hp", Steps: []string{"drivers"},
		Drivers: Drivers{Sweep: true, Extracts: []Extract{{
			File: "pack.exe", Dir: "d", Args: []string{"-x"}, AltArgs: []string{"/x"},
		}}}}
	a, dir := newAgent(t, m)
	os.WriteFile(filepath.Join(dir, "pack.exe"), []byte("pack"), 0o755)
	f := &fakeRun{do: func(string, []string) (result, error) { return result{Code: 0}, nil }}
	f.install(t)
	a.driversStep()

	log := logText(t, dir)
	if !strings.Contains(log, "FAILED") || !strings.Contains(log, "unpacked no driver files") {
		t.Errorf("a pack that did nothing was not reported as failed:\n%s", log)
	}
	// pnputil on an empty folder exits 87 and reads like a real error.
	if !strings.Contains(log, "no driver files to install") {
		t.Errorf("pnputil should have been skipped:\n%s", log)
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "pnputil") {
			t.Error("pnputil was run with nothing to install")
		}
	}
	if fails := a.J.Failures(); len(fails) != 1 {
		t.Errorf("failures recorded: %v", fails)
	}
}

// A driver pack for another model is skipped, so one stick can serve a bench
// of different machines.
func TestVendorInstallerRunsOnlyOnItsModel(t *testing.T) {
	if !modelMatches("HP", "EliteBook x360 1040 G8", "HP Inc.", "HP EliteBook x360 1040 G8 Notebook PC") {
		t.Error("the model this pack is for was not recognised")
	}
	if modelMatches("Dell", "OptiPlex 3070", "HP Inc.", "HP EliteBook x360 1040 G8") {
		t.Error("a pack for another vendor matched")
	}
	if modelMatches("HP", "EliteBook 840 G9", "HP Inc.", "HP EliteBook x360 1040 G8") {
		t.Error("a pack for another model matched")
	}
}

// Steps already finished are not repeated when the agent runs again, so a
// machine that reboots part way through carries on.
func TestFinishedStepsAreSkippedOnASecondRun(t *testing.T) {
	dir := t.TempDir()
	s := LoadState(dir)
	s.Finish("drivers")
	again := LoadState(dir)
	if !again.Finished("drivers") {
		t.Error("a finished step was forgotten")
	}
	if again.Finished("apps") {
		t.Error("a step that never ran was recorded as finished")
	}
}

// The manifest the build writes is the manifest the agent reads.
func TestManifestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ManifestName)
	in := &Manifest{Recipe: "front-desk", Steps: []string{"drivers", "apps"},
		Apps:    &Apps{Winget: []string{"Google.Chrome"}, Scope: "machine"},
		Debloat: &Debloat{Preset: "standard", Apps: []string{"Microsoft.BingNews"}}}
	if err := in.Save(p); err != nil {
		t.Fatal(err)
	}
	out, err := LoadManifest(p)
	if err != nil {
		t.Fatal(err)
	}
	if out.Recipe != in.Recipe || len(out.Steps) != 2 || out.Apps.Winget[0] != "Google.Chrome" ||
		out.Debloat.Preset != "standard" {
		t.Errorf("came back as %+v", out)
	}
	// A manifest from a newer DSKY must be refused rather than half-read.
	os.WriteFile(p, []byte(`{"version":999,"recipe":"x","steps":[]}`), 0o644)
	if _, err := LoadManifest(p); err == nil {
		t.Error("a manifest from a newer DSKY was accepted")
	}
}

// Every line of both logs is written by the agent from what it observed.
func TestJournalRecordsFailuresForTheSummary(t *testing.T) {
	dir := t.TempDir()
	j, err := OpenJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	j.Info("apps", "installed %s", "Chrome")
	j.Fail("apps", "could not install %s", "Spotify")
	j.Close()

	text := logText(t, dir)
	if !strings.Contains(text, "apps: installed Chrome") || !strings.Contains(text, "apps: FAILED could not install Spotify") {
		t.Errorf("human log reads:\n%s", text)
	}
	events, _ := os.ReadFile(filepath.Join(dir, EventsName))
	if !strings.Contains(string(events), `"level":"FAILED"`) {
		t.Errorf("machine log reads:\n%s", events)
	}
}

// The checker reads the machine against the manifest that built it. Every
// failure in the first watched install would have shown here in seconds.
func TestVerifyReportsWhatTheMachineActuallyHas(t *testing.T) {
	dir := t.TempDir()
	m := &Manifest{Recipe: "hp", Steps: []string{"drivers", "apps"},
		Drivers: Drivers{Sweep: true, Extracts: []Extract{{File: "sp1.exe", Dir: "hp"}}},
		Apps:    &Apps{Winget: []string{"Google.Chrome", "Spotify.Spotify"}}}
	if err := m.Save(filepath.Join(dir, ManifestName)); err != nil {
		t.Fatal(err)
	}
	// A machine where the pack unpacked nothing, Chrome landed and Spotify
	// did not, and first boot stopped early: exactly the HP.
	os.WriteFile(filepath.Join(dir, LogName), []byte(
		"[x] drivers: sp1.exe unpacked no driver files\n"+
			"[x] apps: installed Google.Chrome\n"), 0o644)
	LoadState(dir).Finish("drivers")

	checks, err := Verify(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, c := range checks {
		got[c.What] = c.OK
	}
	for what, want := range map[string]bool{
		"first boot ran":          false,
		"step drivers":            true,
		"step apps":               false,
		"driver pack sp1.exe":     false,
		"drivers installed":       false,
		"program Google.Chrome":   true,
		"program Spotify.Spotify": false,
	} {
		if v, ok := got[what]; !ok {
			t.Errorf("the check %q was not reported at all", what)
		} else if v != want {
			t.Errorf("%q reported %v, want %v", what, v, want)
		}
	}
}

// The whole run: every step the manifest asks for happens, in order, is
// recorded as finished, and the closing line says what went wrong. A second
// run does the work again only where it did not finish.
func TestApplyRunsEveryStepInOrder(t *testing.T) {
	dir := t.TempDir()
	m := &Manifest{Recipe: "flow", Steps: []string{"drivers", "debloat", "apps"},
		Drivers: Drivers{Extracts: []Extract{{File: "pack.exe", Dir: "d", Args: []string{"-x"}}}},
		Debloat: &Debloat{Preset: "standard", Apps: []string{"Microsoft.BingNews"}},
		Apps:    &Apps{Installers: []Installer{{File: "agent.msi", MSI: true}}}}
	if err := m.Save(filepath.Join(dir, ManifestName)); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "pack.exe"), []byte("x"), 0o755)
	os.WriteFile(filepath.Join(dir, "agent.msi"), []byte("x"), 0o644)

	f := &fakeRun{do: func(name string, args []string) (result, error) {
		if strings.HasSuffix(name, "pack.exe") {
			dest := filepath.Join(dir, "Drivers", "d")
			os.MkdirAll(dest, 0o755)
			os.WriteFile(filepath.Join(dest, "x.inf"), []byte("inf"), 0o644)
		}
		return result{Code: 0}, nil
	}}
	f.install(t)

	if _, err := Apply(dir, RunOptions{}); err != nil {
		t.Fatal(err)
	}
	log := logText(t, dir)
	for _, want := range []string{
		"first-boot agent starting for recipe flow",
		"1 driver file(s) present",
		"installed agent.msi",
		"agent done",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("the log is missing %q:\n%s", want, log)
		}
	}
	// msiexec, not the file itself: an .msi is not an executable.
	var sawMSI bool
	for _, c := range f.calls {
		if strings.HasPrefix(c, "msiexec ") && strings.Contains(c, "/qn") {
			sawMSI = true
		}
	}
	if !sawMSI {
		t.Errorf("the installer was not run through msiexec: %v", f.calls)
	}

	st := LoadState(dir)
	for _, step := range m.Steps {
		if !st.Finished(step) {
			t.Errorf("step %q was not recorded as finished", step)
		}
	}

	// A second run repeats nothing.
	before := len(f.calls)
	if _, err := Apply(dir, RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != before {
		t.Errorf("a second run did %d more things; it should have skipped every finished step", len(f.calls)-before)
	}
	if !strings.Contains(logText(t, dir), "already done on an earlier boot") {
		t.Error("the log does not say the steps were skipped")
	}
}

// Vendor switches are written for a command line, where the shell removes
// the quotes around a path. The agent starts the program itself, so it must
// remove them: a path with quotes still in it is a different path, and the
// extractor says so by printing its usage.
func TestExtractArgumentsCarryNoShellQuotes(t *testing.T) {
	got := expandArgs([]string{"/s", "/e", "/f", `"{dir}"`}, `C:\Windows\Setup\Scripts\Drivers\hp`)
	want := `C:\Windows\Setup\Scripts\Drivers\hp`
	if got[3] != want {
		t.Errorf("the destination came out as %q, want %q", got[3], want)
	}
	old := expandArgs([]string{"-pdf", "-e", "-s", `-f"{dir}"`}, `C:\d`)
	if old[3] != `-fC:\d` {
		t.Errorf("the older style came out as %q", old[3])
	}
	for _, a := range append(got, old...) {
		if strings.Contains(a, `"`) {
			t.Errorf("%q still carries a quote", a)
		}
	}
}

// Windows hands back exit codes unsigned; every document that describes them
// writes them signed. Reading them raw is why a package that refuses an
// administrator was never recognised as one.
func TestWindowsExitCodesAreReadAsSigned(t *testing.T) {
	for raw, want := range map[int]int{
		2316632150: wingetProhibitsElev,    // 0x8A150056
		2316632080: wingetNoInstaller,      // 0x8A150010
		2316632107: wingetAlreadyInstalled, // 0x8A15002B
		0:          0,
		1:          1,
		3010:       3010,
	} {
		if got := signedExit(raw); got != want {
			t.Errorf("exit code %d read as %d, want %d", raw, got, want)
		}
	}
}

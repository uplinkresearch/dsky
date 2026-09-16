package agent

import (
	"encoding/xml"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeResume stands in for the two things that only happen on a real machine:
// registering the task that starts the agent again, and taking the machine
// down.
type fakeResume struct {
	ensured  int
	ensueErr error
	restarts []string
	failRest error
}

func (f *fakeResume) install(t *testing.T) {
	t.Helper()
	pe, pr := ensureResumeFn, restartFn
	ensureResumeFn = func(a *Agent) error { f.ensured++; return f.ensueErr }
	restartFn = func(a *Agent, reason string) error {
		f.restarts = append(f.restarts, reason)
		return f.failRest
	}
	t.Cleanup(func() { ensureResumeFn, restartFn = pe, pr })
}

// driversThenDebloat is the shape of the recipe that failed on the HP. The
// programs step is left out on purpose: it waits five minutes for winget and
// five for the network, which is right on a machine at first boot and wrong
// in a test.
func driversThenDebloat() *Manifest {
	return &Manifest{Version: ManifestVersion, Recipe: "windows-11",
		Steps:   []string{"drivers", "debloat"},
		Drivers: Drivers{Sweep: true},
		Debloat: &Debloat{Preset: "standard", Apps: []string{"Microsoft.XboxApp"}}}
}

// stageINF puts one driver file where the sweep will find it, so pnputil is
// reached at all.
func stageINF(t *testing.T, dir string) {
	t.Helper()
	d := filepath.Join(dir, "Drivers", "pack")
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "x.inf"), []byte("[Version]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// This is the HP EliteBook x360 failure, in full: 282 driver packages go in,
// pnputil says the machine must restart, and the agent used to walk straight
// into removing consumer apps on a servicing stack mid-change. The machine
// bugchecked four minutes later and the programs never ran.
func TestAStepThatWindowsSaysNeedsARestartGetsOne(t *testing.T) {
	dir := t.TempDir()
	stageINF(t, dir)
	res := &fakeResume{}
	res.install(t)
	f := &fakeRun{}
	f.do = func(name string, args []string) (result, error) {
		if name == "pnputil" {
			return result{Code: pnputilRebootNeeded,
				Out: "Driver package added successfully.\nSystem reboot is needed to complete install operations!"}, nil
		}
		return result{}, nil
	}
	f.install(t)

	m := driversThenDebloat()
	if err := m.Save(filepath.Join(dir, ManifestName)); err != nil {
		t.Fatal(err)
	}
	if err := Apply(dir); err != nil {
		t.Fatal(err)
	}

	if len(res.restarts) != 1 {
		t.Fatalf("expected one restart, got %d: %v", len(res.restarts), res.restarts)
	}
	if !strings.Contains(res.restarts[0], "drivers") {
		t.Errorf("the restart reason does not mention the drivers: %q", res.restarts[0])
	}
	if res.ensured == 0 {
		t.Error("the machine was restarted without arranging to carry on afterwards")
	}
	// The run stops at the restart: everything after the drivers waits for
	// the machine to come back.
	for _, forbidden := range []string{"powershell", "winget"} {
		for _, c := range f.calls {
			if strings.HasPrefix(c, forbidden) {
				t.Errorf("%q ran on a machine that Windows said must restart first", c)
			}
		}
	}
	// And the drivers are recorded as done, so the next run carries on
	// rather than installing them all again.
	st := LoadState(dir)
	if !st.Finished("drivers") {
		t.Error("the drivers were not recorded as done, so the next run would repeat them")
	}
	if st.Finished("debloat") {
		t.Error("debloat is recorded as done and it never ran")
	}
	if st.Reboots != 1 {
		t.Errorf("reboots recorded = %d, want 1", st.Reboots)
	}
}

// The run after the restart picks up where it left off, and this time there
// is nothing left to carry on with, so the task goes away.
func TestTheRunAfterARestartCarriesOn(t *testing.T) {
	dir := t.TempDir()
	stageINF(t, dir)
	res := &fakeResume{}
	res.install(t)
	f := &fakeRun{}
	f.install(t)

	m := driversThenDebloat()
	if err := m.Save(filepath.Join(dir, ManifestName)); err != nil {
		t.Fatal(err)
	}
	// As the interrupted run left it.
	st := LoadState(dir)
	st.Finish("drivers")
	st.CountReboot()

	if err := Apply(dir); err != nil {
		t.Fatal(err)
	}
	log := logText(t, dir)
	if !strings.Contains(log, "already done on an earlier boot") {
		t.Error("the drivers were not skipped on the second run")
	}
	if len(res.restarts) != 0 {
		t.Errorf("restarted again with nothing asking for it: %v", res.restarts)
	}
	got := LoadState(dir)
	for _, step := range []string{"drivers", "debloat"} {
		if !got.Finished(step) {
			t.Errorf("%s did not finish", step)
		}
	}
}

// A machine is never restarted unless the agent knows it will be started
// again, because a machine that reboots into nothing is worse off than one
// that carries on now.
func TestNoRestartWhenItCouldNotArrangeToCarryOn(t *testing.T) {
	dir := t.TempDir()
	stageINF(t, dir)
	res := &fakeResume{ensueErr: errors.New("schtasks: access is denied")}
	res.install(t)
	f := &fakeRun{}
	f.do = func(name string, args []string) (result, error) {
		if name == "pnputil" {
			return result{Code: pnputilRebootNeeded, Out: "Driver package added successfully."}, nil
		}
		return result{}, nil
	}
	f.install(t)

	m := driversThenDebloat()
	if err := m.Save(filepath.Join(dir, ManifestName)); err != nil {
		t.Fatal(err)
	}
	if err := Apply(dir); err != nil {
		t.Fatal(err)
	}
	if len(res.restarts) != 0 {
		t.Fatalf("restarted a machine that would not have carried on: %v", res.restarts)
	}
	log := logText(t, dir)
	if !strings.Contains(log, "not restarting") {
		t.Errorf("the log does not say why it did not restart:\n%s", log)
	}
	// It carries on instead of stopping, so the machine still gets its
	// programs.
	if !LoadState(dir).Finished("debloat") {
		t.Error("the run stopped rather than carrying on without the restart")
	}
}

// A step that asks for a restart every time must not loop the machine.
func TestRestartsAreCapped(t *testing.T) {
	dir := t.TempDir()
	stageINF(t, dir)
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
	m := driversThenDebloat()
	if err := m.Save(filepath.Join(dir, ManifestName)); err != nil {
		t.Fatal(err)
	}
	st := LoadState(dir)
	for i := 0; i < maxReboots; i++ {
		st.CountReboot()
	}

	if err := Apply(dir); err != nil {
		t.Fatal(err)
	}
	if len(res.restarts) != 0 {
		t.Fatalf("restarted past the cap: %v", res.restarts)
	}
	if !strings.Contains(logText(t, dir), "carrying on without another") {
		t.Error("the log does not say it stopped restarting")
	}
	if !LoadState(dir).Finished("debloat") {
		t.Error("the run did not finish after the cap was reached")
	}
}

// Whatever happens, the agent arranges to be started again before it does any
// work -- the HP crashed, and nothing on the machine ever ran it a second
// time.
func TestItArrangesToCarryOnBeforeDoingAnything(t *testing.T) {
	dir := t.TempDir()
	res := &fakeResume{}
	res.install(t)
	f := &fakeRun{}
	f.do = func(name string, args []string) (result, error) {
		if res.ensured == 0 {
			t.Errorf("%s ran before anything arranged for the machine to carry on after a restart", name)
		}
		return result{}, nil
	}
	f.install(t)
	m := driversThenDebloat()
	if err := m.Save(filepath.Join(dir, ManifestName)); err != nil {
		t.Fatal(err)
	}
	if err := Apply(dir); err != nil {
		t.Fatal(err)
	}
	if res.ensured == 0 {
		t.Fatal("nothing arranged for the machine to carry on after a restart")
	}
}

// The task description is XML that only Task Scheduler ever reads, on a
// machine nobody is watching. A typo in it does not fail a build or a test --
// it means a machine that quietly stops provisioning, which is the failure
// this whole file exists to end. So it is parsed and read here.
func TestTheResumeTaskSaysWhatItMustSay(t *testing.T) {
	x := resumeTaskXML(`WIN-I79\user`, `C:\Windows\Setup\Scripts\dsky-agent.exe`, `C:\Windows\Setup\Scripts`)

	var task struct {
		Triggers struct {
			Logon struct {
				UserId string `xml:"UserId"`
				Delay  string `xml:"Delay"`
			} `xml:"LogonTrigger"`
		} `xml:"Triggers"`
		Principals struct {
			Principal struct {
				UserId    string `xml:"UserId"`
				LogonType string `xml:"LogonType"`
				RunLevel  string `xml:"RunLevel"`
			} `xml:"Principal"`
		} `xml:"Principals"`
		Settings struct {
			OnBatteries string `xml:"DisallowStartIfOnBatteries"`
			TimeLimit   string `xml:"ExecutionTimeLimit"`
			Multiple    string `xml:"MultipleInstancesPolicy"`
		} `xml:"Settings"`
		Actions struct {
			Exec struct {
				Command string `xml:"Command"`
				Args    string `xml:"Arguments"`
			} `xml:"Exec"`
		} `xml:"Actions"`
	}
	if err := parseTask(x, &task); err != nil {
		t.Fatalf("Task Scheduler would reject this: %v", err)
	}

	if task.Triggers.Logon.UserId != `WIN-I79\user` {
		t.Errorf("sign-in trigger is for %q", task.Triggers.Logon.UserId)
	}
	// Elevated, in the signed-in user's session: pnputil needs the first,
	// winget and the installers that refuse an administrator need the second.
	if task.Principals.Principal.RunLevel != "HighestAvailable" {
		t.Errorf("RunLevel is %q; the drivers step needs an administrator", task.Principals.Principal.RunLevel)
	}
	if task.Principals.Principal.LogonType != "InteractiveToken" {
		t.Errorf("LogonType is %q; the programs step needs the signed-in session", task.Principals.Principal.LogonType)
	}
	// A laptop part way through provisioning is often on its own battery,
	// and Task Scheduler's default is to refuse to start there.
	if task.Settings.OnBatteries != "false" {
		t.Errorf("DisallowStartIfOnBatteries is %q, so a laptop on battery would never carry on", task.Settings.OnBatteries)
	}
	// PT0S is Task Scheduler's "no limit". The default is three days, but a
	// driver sweep plus a dozen installs has no business being cut off.
	if task.Settings.TimeLimit != "PT0S" {
		t.Errorf("ExecutionTimeLimit is %q, not unlimited", task.Settings.TimeLimit)
	}
	if task.Settings.Multiple != "IgnoreNew" {
		t.Errorf("MultipleInstancesPolicy is %q; two agents must not provision one machine at once", task.Settings.Multiple)
	}
	if task.Actions.Exec.Command != `C:\Windows\Setup\Scripts\dsky-agent.exe` {
		t.Errorf("runs %q", task.Actions.Exec.Command)
	}
	if task.Actions.Exec.Args != `apply "C:\Windows\Setup\Scripts"` {
		t.Errorf("arguments are %q", task.Actions.Exec.Args)
	}
}

// A machine name or account with an ampersand in it must not break the XML.
func TestTheResumeTaskEscapesTheUserName(t *testing.T) {
	x := resumeTaskXML(`SALES&CO\a "user"`, `C:\x\dsky-agent.exe`, `C:\x`)
	var task struct {
		Principals struct {
			Principal struct {
				UserId string `xml:"UserId"`
			} `xml:"Principal"`
		} `xml:"Principals"`
	}
	if err := parseTask(x, &task); err != nil {
		t.Fatalf("a name with an ampersand in it breaks the task: %v", err)
	}
	if task.Principals.Principal.UserId != `SALES&CO\a "user"` {
		t.Errorf("the name came back as %q", task.Principals.Principal.UserId)
	}
}

// parseTask reads the task description. The declaration says UTF-16 because
// that is what schtasks requires of the file on disk, which ensureResume
// writes through utf16LE; the text itself is the same characters either way.
func parseTask(x string, into any) error {
	d := xml.NewDecoder(strings.NewReader(x))
	d.CharsetReader = func(_ string, r io.Reader) (io.Reader, error) { return r, nil }
	return d.Decode(into)
}

// The automatic sign-in is arranged for provisioning and put away by it. A
// machine that finishes must not keep signing itself in, with the password
// the answer file stored still in the registry, on its owner's next boots.
func TestAFinishedMachineStopsSigningItselfIn(t *testing.T) {
	dir := t.TempDir()
	stageINF(t, dir)
	res := &fakeResume{}
	res.install(t)
	var cleared, disarmed int
	pc, pd := clearResumeFn, disarmAutoLogonFn
	clearResumeFn = func(a *Agent) { cleared++ }
	disarmAutoLogonFn = func(a *Agent) { disarmed++ }
	t.Cleanup(func() { clearResumeFn, disarmAutoLogonFn = pc, pd })

	f := &fakeRun{}
	f.install(t)
	m := driversThenDebloat()
	if err := m.Save(filepath.Join(dir, ManifestName)); err != nil {
		t.Fatal(err)
	}
	if err := Apply(dir); err != nil {
		t.Fatal(err)
	}
	if cleared != 1 || disarmed != 1 {
		t.Fatalf("at the end of the work: resume task cleared %d time(s), automatic sign-in put away %d time(s); want 1 and 1",
			cleared, disarmed)
	}
}

// On the way into a restart it must do neither: the machine needs the
// automatic sign-in to come back, and the task to carry on when it does.
func TestARestartKeepsWhatItNeedsToComeBack(t *testing.T) {
	dir := t.TempDir()
	stageINF(t, dir)
	res := &fakeResume{}
	res.install(t)
	var cleared, disarmed int
	pc, pd := clearResumeFn, disarmAutoLogonFn
	clearResumeFn = func(a *Agent) { cleared++ }
	disarmAutoLogonFn = func(a *Agent) { disarmed++ }
	t.Cleanup(func() { clearResumeFn, disarmAutoLogonFn = pc, pd })

	f := &fakeRun{}
	f.do = func(name string, args []string) (result, error) {
		if name == "pnputil" {
			return result{Code: pnputilRebootNeeded, Out: "Driver package added successfully."}, nil
		}
		return result{}, nil
	}
	f.install(t)
	m := driversThenDebloat()
	if err := m.Save(filepath.Join(dir, ManifestName)); err != nil {
		t.Fatal(err)
	}
	if err := Apply(dir); err != nil {
		t.Fatal(err)
	}
	if len(res.restarts) != 1 {
		t.Fatalf("expected a restart, got %d", len(res.restarts))
	}
	if cleared != 0 || disarmed != 0 {
		t.Errorf("on the way into a restart the machine lost what it needs to come back: cleared %d, disarmed %d",
			cleared, disarmed)
	}
}

// The resume task starts the agent as soon as there is a session to draw on.
// A minute's delay meant a minute of somebody watching an ordinary desktop
// after a restart, wondering whether provisioning was still happening.
func TestTheResumeTaskDoesNotDawdle(t *testing.T) {
	x := resumeTaskXML(`WIN\user`, `C:\x\dsky-agent.exe`, `C:\x`)
	var task struct {
		Triggers struct {
			Logon struct {
				Delay string `xml:"Delay"`
			} `xml:"LogonTrigger"`
		} `xml:"Triggers"`
	}
	if err := parseTask(x, &task); err != nil {
		t.Fatal(err)
	}
	switch task.Triggers.Logon.Delay {
	case "PT1M", "PT2M", "PT5M":
		t.Errorf("the agent waits %s after signing in before it says anything", task.Triggers.Logon.Delay)
	case "":
		t.Error("no delay at all: the session may not be ready to draw on")
	}
}

// A machine that restarts part way through the drivers -- Windows does this
// for its own display driver -- must not unpack the vendor's pack again on the
// way back. On an HP EliteBook that is a 1.2 GB file and several minutes,
// spent to arrive at files that were already on the disk.
func TestAnAlreadyUnpackedPackIsNotUnpackedAgain(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sp142792.exe"), []byte("pack"), 0o755); err != nil {
		t.Fatal(err)
	}
	// As the interrupted boot left it: unpacked, into the pack's own folder.
	packDir := filepath.Join(dir, "Drivers", "hp-pack")
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packDir, "net.inf"), []byte("[Version]\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	f := &fakeRun{}
	f.install(t)
	a, _ := newAgent(t, &Manifest{Version: ManifestVersion})
	a.Dir = dir
	a.extractPack(Extract{File: "sp142792.exe", Dir: "hp-pack", Args: []string{"/s", "/e", "/f", "{dir}"}}, filepath.Join(dir, "Drivers"))

	for _, c := range f.calls {
		if strings.Contains(c, "sp142792.exe") {
			t.Errorf("unpacked the pack again: %q", c)
		}
	}
}

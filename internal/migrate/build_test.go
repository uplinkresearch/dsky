package migrate

import (
	"strings"
	"testing"
)

// Building is the step that costs a stick and twenty minutes, so everything
// here is about refusing early rather than producing media that is subtly not
// what somebody approved.

func approved(t *testing.T, name string) *Manifest {
	t.Helper()
	m := load(t, name)
	for i := range m.Compat {
		m.Compat[i].Acknowledged = true
	}
	for i, a := range m.Apps {
		if s := a.Resolution.Status; s == StatusUnmapped || s == StatusUnset || s == StatusBlocked {
			m.Apps[i].Resolution = Resolution{Status: StatusDropped, ResolvedBy: ByOperator}
		}
	}
	if m.Data.Strategy == DataUSMT && m.Data.StorePath == "" {
		m.Data.StorePath = `\\fs\migration$\PC01`
	}
	if err := m.Approve("dusty"); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestABuildRefusesWhatWasNotApproved(t *testing.T) {
	m := load(t, "office") // not approved
	if err := Preflight(m); err == nil || !strings.Contains(err.Error(), "not approved") {
		t.Errorf("unapproved: %v", err)
	}

	m = approved(t, "office")
	if err := Preflight(m); err != nil {
		t.Fatalf("an approved plan was refused: %v", err)
	}
	// Edited after approval: the hash is recomputed here, never trusted.
	m.Target.Hostname = "SOMETHING-ELSE"
	if err := Preflight(m); err == nil || !strings.Contains(err.Error(), "edited after") {
		t.Errorf("edited: %v", err)
	}

	// An application nobody placed stops the build: "it will sort itself out
	// at first boot" is how a machine arrives without the software it needs.
	// The review will not approve such a plan, so this is the second line of
	// the same defence -- what stops a manifest whose hash was made to match
	// by something other than a review.
	m = approved(t, "office")
	m.Apps[0].Resolution = Resolution{Status: StatusUnmapped, ResolvedBy: ByAuto}
	if err := m.Approve("dusty"); err == nil {
		t.Fatal("approving with an unplaced application should not be possible")
	}
	rehash(t, m)
	if err := Preflight(m); err == nil || !strings.Contains(err.Error(), "nowhere to install from") {
		t.Errorf("unplaced: %v", err)
	}

	// The same for a blocker nobody answered.
	m = approved(t, "office")
	m.Compat = append(m.Compat, Compat{Severity: Blocker, Subject: "x", Reason: "something nobody answered"})
	rehash(t, m)
	if err := Preflight(m); err == nil || !strings.Contains(err.Error(), "unanswered") {
		t.Errorf("blocker: %v", err)
	}

	// And for a domain machine with no OU to put the computer account in.
	m = approved(t, "office")
	m.Identity.ComputerOUDN = ""
	rehash(t, m)
	if err := Preflight(m); err == nil || !strings.Contains(err.Error(), "which OU") {
		t.Errorf("no OU: %v", err)
	}
}

// rehash makes the approval match the manifest as it now stands, which is
// what a forged or hand-edited approval would look like. Preflight is not
// allowed to rely on the hash alone.
func rehash(t *testing.T, m *Manifest) {
	t.Helper()
	h, err := m.Hash()
	if err != nil {
		t.Fatal(err)
	}
	m.Approval.ContentHash = h
}

// A domain machine needs its own join file, and the plan has to say which OU
// the computer account belongs in — that is what djoin is told.
func TestABuildNeedsTheJoinFileAndTheOU(t *testing.T) {
	m := approved(t, "office")
	if _, err := PlanBuild(m, ""); err == nil || !strings.Contains(err.Error(), "offline join file") {
		t.Errorf("no blob: %v", err)
	}
	p, err := PlanBuild(m, "/tmp/pc-odj.txt")
	if err != nil {
		t.Fatal(err)
	}
	if p.DomainBlob != "/tmp/pc-odj.txt" {
		t.Errorf("the join file did not reach the build: %+v", p)
	}
	// The blob names the computer, so the build does not -- and that still
	// holds now the agent applies it after OOBE rather than Setup applying it
	// in specialize. Proven on a lab machine: built with no name of its own,
	// it came back from the agent's join as NEWDESK01 in lab.dsky.local, the
	// name its computer account was provisioned for.
	if p.Hostname != "" {
		t.Errorf("a hostname was sent alongside an offline join: %q", p.Hostname)
	}
	if cmd := DjoinCommand(m, "pc-odj.txt"); !strings.Contains(cmd, `/machineou "OU=Front Desk,OU=Workstations,DC=mead,DC=local"`) ||
		!strings.Contains(cmd, "/reuse") {
		t.Errorf("djoin command: %s", cmd)
	}
	// Single-quoted for PowerShell: a computer account ends in $, which a
	// double-quoted string would read as a variable.
	if g := GroupCommands(m); len(g) != 1 || !strings.Contains(g[0], `-Identity 'MEAD\Practice Workstations'`) ||
		!strings.Contains(g[0], `-Members 'MEAD-FRONT-02$'`) {
		t.Errorf("group commands: %v", g)
	}

	// Without a domain, the machine's name is the build's to set.
	k := approved(t, "kiosk")
	p, err = PlanBuild(k, "")
	if err != nil {
		t.Fatal(err)
	}
	if p.Hostname != "MEAD-LOBBY-KIOSK" {
		t.Errorf("hostname: %q", p.Hostname)
	}
}

// The plan's applications become the choices the rest of DSKY already takes.
func TestTheBuildInstallsWhatThePlanSays(t *testing.T) {
	m := approved(t, "customized")
	p, err := PlanBuild(m, "/tmp/pc-odj.txt")
	if err != nil {
		t.Fatal(err)
	}
	apps := strings.Join(p.Apps, " ")
	for _, want := range []string{"winget:Microsoft.DotNet.DesktopRuntime.6", "migrate-arcgispro", "migrate-sqlexpress2019"} {
		if !strings.Contains(apps, want) {
			t.Errorf("%q is not in what the build installs: %v", want, p.Apps)
		}
	}
	// The machine's own settings come across where DSKY can carry them.
	if p.Locale != "en-US" || p.Timezone != "Mountain Standard Time" {
		t.Errorf("locale/timezone: %+v", p)
	}
	if p.AdminUser != "uplink" || p.Edition != "Pro" {
		t.Errorf("account/edition: %+v", p)
	}
	// Manual work is not installed; it is a checklist.
	for _, a := range p.Manual {
		if a.Resolution.Method != MethodManual {
			t.Errorf("%s is on the checklist but installs by %s", a.DisplayName, a.Resolution.Method)
		}
	}
	if len(p.Manual) != 1 || p.Manual[0].ID != "adobeacrobatpro" {
		t.Errorf("checklist: %+v", p.Manual)
	}
	// The files an operator's installer comes from are named for the caller
	// to stage into the library.
	ins := Installers(m)
	if len(ins) != 3 {
		t.Fatalf("installers: %+v", ins)
	}
	for _, i := range ins {
		if i.Path == "" || i.ID == "" {
			t.Errorf("installer: %+v", i)
		}
	}
	// And the parts of the plan this build does not carry yet are said out
	// loud rather than quietly dropped.
	w := strings.Join(p.Warnings, " | ")
	for _, want := range []string{"USMT", "printer", "setting"} {
		if !strings.Contains(w, want) {
			t.Errorf("the build did not warn about %s: %s", want, w)
		}
	}
}

func TestEditionFollowsTheOldMachine(t *testing.T) {
	for was, want := range map[string]string{
		"Professional": "Pro", "Core": "Home", "CoreSingleLanguage": "Home",
		"Enterprise": "Pro", "Education": "Pro", "": "Pro",
	} {
		if got := edition(was); got != want {
			t.Errorf("%q -> %q, want %q", was, got, want)
		}
	}
}

// Which settings the machine gets and which wait for a person is the decision
// that makes a migration feel like the old PC or not. Half of what somebody
// notices lives in HKCU, and the agent runs as an administrator nobody uses.
func TestSettingsThatBelongToAPersonWaitForThatPerson(t *testing.T) {
	m := approved(t, "customized")
	machine, perUser := SettingActions(m, "win11")
	if len(machine) == 0 || len(perUser) == 0 {
		t.Fatalf("machine=%+v perUser=%+v", machine, perUser)
	}
	for _, a := range perUser {
		if a.Method != ApplyRegistry || !strings.HasPrefix(a.Ref, `HKCU\`) {
			t.Errorf("%s waits for a person but is not theirs to set: %+v", a.Key, a)
		}
	}
	for _, a := range machine {
		if a.Method == ApplyRegistry && strings.HasPrefix(a.Ref, `HKCU\`) {
			t.Errorf("%s would be set for an administrator nobody signs in as: %+v", a.Key, a)
		}
	}
	// A setting nobody can apply is in neither list — it stays in the plan and
	// in the report, which is how "do this by hand" reaches a person.
	for _, a := range append(machine, perUser...) {
		if a.Ref == "" || a.Method == "" {
			t.Errorf("a setting with no way to apply it was handed to the agent: %+v", a)
		}
	}
	// And the operator is told, because a setting that appears only when
	// somebody signs in is not one they can check at the bench.
	p, err := PlanBuild(m, "/tmp/pc-odj.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(p.Warnings, " | "), "first sign-in") {
		t.Errorf("the build did not say the per-user settings come later: %v", p.Warnings)
	}
}

// The two kinds of printer are told apart before a stick is written, because
// only one of them may need somebody to install a driver by hand.
func TestTheBuildSaysWhichPrintersNeedAHand(t *testing.T) {
	m := approved(t, "customized")
	m.Peripherals.Printers = []Printer{
		{Name: "Front Desk", SharedPath: `\\fs01\FrontDesk`},
		{Name: "Back Office", IP: "10.0.0.50", DriverName: "HP UPD PCL6"},
	}
	rehash(t, m)
	p, err := PlanBuild(m, "/tmp/pc-odj.txt")
	if err != nil {
		t.Fatal(err)
	}
	w := strings.Join(p.Warnings, " | ")
	if !strings.Contains(w, "own address") {
		t.Errorf("nothing warned that a direct printer needs its driver: %s", w)
	}
	if !strings.Contains(w, "print server") || !strings.Contains(w, "first sign-in") {
		t.Errorf("nothing said the shared queue arrives at sign-in: %s", w)
	}
	printers, _ := PrintersAndDrives(m)
	if len(printers) != 2 {
		t.Errorf("printers did not reach the build: %+v", printers)
	}
}

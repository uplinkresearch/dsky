package compose

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/uplinkresearch/dsky/internal/agentbin"
	"github.com/uplinkresearch/dsky/internal/recipe"
)

// The agent takes over first boot only for recipes it can carry out
// completely. Everything else keeps the generated scripts, so a recipe that
// worked last week still works.
func TestAgentCoversOnlyWhatItCanDo(t *testing.T) {
	if !agentbin.Available(agentbin.AMD64) {
		t.Skip("this build has no agent embedded (`./build-agent.sh`)")
	}
	base := recipe.WindowsSpec{
		Firstboot: recipe.FirstbootSpec{Mode: "generate", Steps: []recipe.Step{{Drivers: true}, {Debloat: true}, {Apps: true}}},
	}
	covered, why := agentCovers(&recipe.Recipe{ID: "a", Windows: &base})
	if !covered {
		t.Errorf("the ordinary recipe is not covered: %s", why)
	}

	withCmd := base
	withCmd.Firstboot.Steps = append([]recipe.Step{{Cmd: "shutdown /r"}}, base.Firstboot.Steps...)
	if covered, _ := agentCovers(&recipe.Recipe{ID: "b", Windows: &withCmd}); covered {
		t.Error("a recipe running its own command was handed to the agent")
	}

	withTemplate := base
	withTemplate.Firstboot = recipe.FirstbootSpec{Mode: "template", Template: "t.cmd"}
	if covered, _ := agentCovers(&recipe.Recipe{ID: "c", Windows: &withTemplate}); covered {
		t.Error("a hand-written first-boot script was replaced by the agent")
	}

	// A single offline join file is the agent's to apply, after OOBE, on a
	// machine that is up. A migration joins a domain by definition.
	withDomain := base
	withDomain.Domain = &recipe.DomainSpec{Blob: "pc.txt"}
	if covered, why := agentCovers(&recipe.Recipe{ID: "d", Windows: &withDomain}); !covered {
		t.Errorf("an offline join kept the agent away: %s", why)
	}
	// A credentialed join stays with the generated scripts: it is proven as
	// it stands, and its password handling is the part least worth
	// disturbing.
	withCreds := base
	withCreds.Domain = &recipe.DomainSpec{Join: "corp.example.com", Username: "svc", Password: "x"}
	if covered, _ := agentCovers(&recipe.Recipe{ID: "e", Windows: &withCreds}); covered {
		t.Error("a credentialed join was handed to the agent")
	}
}

// What the operator asked for reaches the manifest unchanged, including the
// second set of extract switches that saved the HP pack.
func TestManifestCarriesTheRecipe(t *testing.T) {
	r := &recipe.Recipe{ID: "front-desk", Windows: &recipe.WindowsSpec{
		Debloat:   &recipe.DebloatSpec{Preset: "standard"},
		Apps:      &recipe.AppsSpec{Winget: []string{"Google.Chrome", "Spotify.Spotify"}},
		Firstboot: recipe.FirstbootSpec{Mode: "generate", Steps: []recipe.Step{{Drivers: true}, {Debloat: true}, {Apps: true}}},
	}}
	var drivers recipe.ResolvedDrivers
	drivers.HasSweepable = true
	drivers.Extracts = append(drivers.Extracts, struct {
		File string
		Dir  string
		Args []string
	}{File: "sp142792.exe", Dir: "hp", Args: []string{"-pdf", "-e", "-s", `-f"{dir}"`}})

	m := buildManifest(r, drivers, nil, nil, "verify.ps1", migrateParts{})
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(b) {
		t.Fatal("the manifest is not valid JSON")
	}
	if got := strings.Join(m.Steps, ","); got != "drivers,debloat,apps" {
		t.Errorf("steps are %q", got)
	}
	if m.Apps == nil || len(m.Apps.Winget) != 2 || m.Apps.Scope != "machine" {
		t.Errorf("apps came out as %+v", m.Apps)
	}
	if m.Debloat == nil || m.Debloat.Preset != "standard" || len(m.Debloat.Apps) == 0 {
		t.Errorf("debloat came out as %+v", m.Debloat)
	}
	if len(m.Drivers.Extracts) != 1 {
		t.Fatalf("extracts: %+v", m.Drivers.Extracts)
	}
	ex := m.Drivers.Extracts[0]
	if strings.Join(ex.Args, " ") != `-pdf -e -s -f"{dir}"` {
		t.Errorf("the recipe's own switches were changed: %v", ex.Args)
	}
	// The pack that printed its usage and exited 0 is retried with the other
	// generation's switches; without this the machine gets no drivers.
	if strings.Join(ex.AltArgs, " ") != `/s /e /f "{dir}"` {
		t.Errorf("no second set of switches to try: %v", ex.AltArgs)
	}
	if m.VerifyScript != "verify.ps1" {
		t.Errorf("verify script: %q", m.VerifyScript)
	}
}

// The first-boot file the answer file runs must hand over to the agent, and
// must not be a script of its own: that is the whole point.
func TestAgentFirstbootJustRunsTheAgent(t *testing.T) {
	for _, want := range []string{`"%~dp0dsky-agent.exe" apply`, "@echo off"} {
		if !strings.Contains(agentFirstboot, want) {
			t.Errorf("the launcher is missing %q", want)
		}
	}
	for _, unwanted := range []string{"powershell", "pnputil", "winget"} {
		if strings.Contains(agentFirstboot, unwanted) {
			t.Errorf("the launcher still does work itself (%s)", unwanted)
		}
	}
	for i := 0; i < len(agentFirstboot); i++ {
		if agentFirstboot[i] > 127 {
			t.Fatalf("non-ASCII byte in the launcher at %d", i)
		}
	}
}

// The join a migration needs does not go in the answer file. A machine that is
// a domain member while OOBE runs never reaches a desktop: OOBE refuses to
// sign a local account in on one ("Not setting autologon for new local user.
// e.g. upgrade, domain-joined, or system-managed user"), hands the sign-in to
// defaultuser0 for user OOBE, and user OOBE cannot finish with nobody at the
// keyboard -- so the OOBE monitor resets the image state and reboots into OOBE,
// for as long as anybody lets it. The agent applies the blob afterwards.
func TestAnOfflineJoinIsTheAgentsToDoAfterOOBE(t *testing.T) {
	r := &recipe.Recipe{ID: "front-desk", Windows: &recipe.WindowsSpec{
		// Blob and nothing else, which is the only shape an offline join has:
		// a domain name alongside it would make the spec credentialed.
		Domain:    &recipe.DomainSpec{Blob: "newdesk01.txt"},
		Firstboot: recipe.FirstbootSpec{Mode: "generate", Steps: []recipe.Step{{Drivers: true}, {Debloat: true}, {Apps: true}}},
	}}
	m := buildManifest(r, recipe.ResolvedDrivers{}, nil, nil, "", migrateParts{})
	if m.Domain == nil {
		t.Fatal("the manifest does not carry the join, so the machine would install into a workgroup and look fine")
	}
	if m.Domain.File != agentODJName {
		t.Errorf("join: %+v", m.Domain)
	}
	// First, not last: a machine that has to restart for the join should do
	// it before an hour of installing programs, not after.
	if got := strings.Join(m.Steps, ","); got != "domain,drivers,debloat,apps" {
		t.Errorf("steps are %q", got)
	}
	// And with no domain there is no such step, so every recipe that worked
	// before this still produces the manifest it did.
	plain := &recipe.Recipe{ID: "p", Windows: &recipe.WindowsSpec{
		Firstboot: recipe.FirstbootSpec{Mode: "generate", Steps: []recipe.Step{{Drivers: true}}},
	}}
	if m := buildManifest(plain, recipe.ResolvedDrivers{}, nil, nil, "", migrateParts{}); m.Domain != nil ||
		strings.Join(m.Steps, ",") != "drivers" {
		t.Errorf("a build with no domain grew a join: %+v %v", m.Domain, m.Steps)
	}
}

// A hand-written first-boot script and a domain join cannot be combined, and
// the build says so instead of writing a stick that strands the machine.
//
// The join has to be applied by first boot rather than by Windows Setup, and
// DSKY cannot add that to a script it did not write. The refusal names the
// command, so somebody who wants both can put it in their own script.
func TestAHandWrittenFirstBootCannotAlsoJoinADomain(t *testing.T) {
	r := &recipe.Recipe{Version: 1, ID: "bench", Name: "Bench",
		OS: recipe.OSSpec{Type: "windows", Source: "win11", SourceMode: recipe.SourceISO},
		Windows: &recipe.WindowsSpec{
			Domain:    &recipe.DomainSpec{Blob: "PC-042.txt"},
			Firstboot: recipe.FirstbootSpec{Mode: "template", Template: "mine.cmd"},
		}}
	err := checkJoinCanBeDeferred(r)
	if err == nil {
		t.Fatal("a hand-written first boot was combined with a domain join")
	}
	for _, want := range []string{"hand-written", "djoin /requestodj", "firstboot.mode: generate"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal never mentions %q:\n%v", want, err)
		}
	}
}

// And the shapes that can be deferred are not refused.
func TestAJoinThatCanBeDeferredIsAllowed(t *testing.T) {
	for _, tc := range []struct {
		name string
		w    recipe.WindowsSpec
	}{
		{"generated first boot", recipe.WindowsSpec{
			Domain:    &recipe.DomainSpec{Blob: "PC-042.txt"},
			Firstboot: recipe.FirstbootSpec{Mode: "generate", Steps: []recipe.Step{{Drivers: true}}},
		}},
		{"a hand-written script with no domain", recipe.WindowsSpec{
			Firstboot: recipe.FirstbootSpec{Mode: "template", Template: "mine.cmd"},
		}},
	} {
		if err := checkJoinCanBeDeferred(&recipe.Recipe{ID: "x", Windows: &tc.w}); err != nil {
			t.Errorf("%s was refused: %v", tc.name, err)
		}
	}
}

// An offline build hands the agent installers and no package ids. Two things
// have to survive that: the apps step must still be in the manifest (nothing
// else runs the installers), and each file must still be tied to the winget
// package it is, or the machine has a pile of programs nothing can account
// for and no way to bring them up to date.
func TestAnOfflineBuildKeepsTheAppsStepAndTheRecordOfWhatItInstalled(t *testing.T) {
	r := &recipe.Recipe{ID: "offline", Windows: &recipe.WindowsSpec{
		Apps: &recipe.AppsSpec{
			BuiltAt: "2026-09-19T10:00:00Z",
			Offline: []recipe.OfflineApp{
				{ID: "Google.Chrome", Version: "153.0.8010.53", Ref: "winget-google.chrome"},
				{ID: "Gone.Missing", Version: "1.0", Ref: "winget-gone.missing"},
			},
		},
		Firstboot: recipe.FirstbootSpec{Mode: "generate", Steps: []recipe.Step{
			{Drivers: true},
			{MSI: &recipe.RunItem{Ref: "winget-google.chrome"}},
		}},
	}}
	refFiles := map[string]string{"winget-google.chrome": "chrome64.msi"}
	m := buildManifest(r, recipe.ResolvedDrivers{}, agentInstallers(r.Windows, refFiles),
		agentOfflineApps(r.Windows, refFiles), "", migrateParts{})

	if got := strings.Join(m.Steps, ","); got != "drivers,apps" {
		t.Fatalf("steps = %q; without an apps step nothing runs the installers", got)
	}
	if len(m.Apps.Winget) != 0 {
		t.Errorf("an offline build still asks winget for %v, over a network it has not got", m.Apps.Winget)
	}
	if len(m.Apps.Installers) != 1 || m.Apps.Installers[0].File != "chrome64.msi" {
		t.Errorf("installers = %+v", m.Apps.Installers)
	}
	// One record, not two: a ref that never got staged must not be recorded,
	// or the machine tries to update a program it never received.
	if len(m.Apps.Offline) != 1 {
		t.Fatalf("offline record = %+v", m.Apps.Offline)
	}
	got := m.Apps.Offline[0]
	if got.ID != "Google.Chrome" || got.Version != "153.0.8010.53" || got.File != "chrome64.msi" {
		t.Errorf("record = %+v", got)
	}
	if m.Apps.BuiltAt != "2026-09-19T10:00:00Z" {
		t.Errorf("the media's age did not reach the machine: %q", m.Apps.BuiltAt)
	}
}

// The defect this work found: a recipe with only the operator's own installer
// and no winget packages produced a manifest whose steps were "drivers", with
// installers no step ever reached. The agent took somebody's RMM installer to
// a machine and quietly did not install it.
func TestInstallersRunWhenNoWingetPackageWasChosen(t *testing.T) {
	r := &recipe.Recipe{ID: "own", Windows: &recipe.WindowsSpec{
		Firstboot: recipe.FirstbootSpec{Mode: "generate", Steps: []recipe.Step{
			{Drivers: true},
			{MSI: &recipe.RunItem{Ref: "app-agent"}},
		}},
	}}
	m := buildManifest(r, recipe.ResolvedDrivers{}, agentInstallers(r.Windows, map[string]string{"app-agent": "agent.msi"}),
		nil, "", migrateParts{})
	if got := strings.Join(m.Steps, ","); got != "drivers,apps" {
		t.Fatalf("steps = %q, so agent.msi is staged and never run", got)
	}
}

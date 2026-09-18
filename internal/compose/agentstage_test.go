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

	// A single offline join file is performed by Setup in specialize, long
	// before the agent runs at first logon: the two never meet, and a
	// migration joins a domain by definition.
	withDomain := base
	withDomain.Domain = &recipe.DomainSpec{Blob: "pc.txt"}
	if covered, why := agentCovers(&recipe.Recipe{ID: "d", Windows: &withDomain}); !covered {
		t.Errorf("an offline join kept the agent away: %s", why)
	}
	// A credentialed join stays with the generated scripts, and so does a
	// by-serial batch: the generated first boot is what reports a join that
	// failed, and losing that would make it silent.
	withCreds := base
	withCreds.Domain = &recipe.DomainSpec{Join: "corp.example.com", Username: "svc", Password: "x"}
	if covered, _ := agentCovers(&recipe.Recipe{ID: "e", Windows: &withCreds}); covered {
		t.Error("a credentialed join was handed to the agent")
	}
	withSerials := base
	withSerials.Domain = &recipe.DomainSpec{BlobsBySerial: "blobs"}
	if covered, _ := agentCovers(&recipe.Recipe{ID: "f", Windows: &withSerials}); covered {
		t.Error("a by-serial batch was handed to the agent, which does not report a failed join")
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

	m := buildManifest(r, drivers, nil, "verify.ps1")
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

package compose

import (
	"archive/zip"
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/uplinkresearch/dsky/internal/agent"
	"github.com/uplinkresearch/dsky/internal/agentbin"
	"github.com/uplinkresearch/dsky/internal/library"
	"github.com/uplinkresearch/dsky/internal/manifest"
	"github.com/uplinkresearch/dsky/internal/recipe"
	"github.com/uplinkresearch/dsky/internal/workspace"
)

// payloadFixture is a workspace, library and recipe with everything a payload
// carries: an INF pack from the workspace, a cab from the library, an MSI
// named by a first-boot step, debloat and winget programs.
func payloadFixture(t *testing.T) Request {
	t.Helper()
	root := t.TempDir()
	wsDir := filepath.Join(root, "ws")
	if err := workspace.Scaffold(wsDir, "Test Org"); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(wsDir, "drivers", "intel-lan", "e1d.inf"), []byte("[Version]\r\n"))

	lib, err := library.Open(filepath.Join(root, "lib"))
	if err != nil {
		t.Fatal(err)
	}
	cabHost := filepath.Join(root, "intel-sst.cab")
	writeFile(t, cabHost, []byte("MSCFfake-cab-bytes"))
	if _, err := lib.Import(&manifest.Source{ID: "intel-sst-cab", Kind: manifest.KindDriverPack, Format: manifest.FormatCab, Filename: "intel-sst.cab"}, cabHost); err != nil {
		t.Fatal(err)
	}
	msiHost := filepath.Join(root, "ScreenConnect.ClientSetup.msi")
	writeFile(t, msiHost, []byte("fake-msi-bytes"))
	if _, err := lib.Import(&manifest.Source{ID: "sc-msi", Kind: manifest.KindPayload, Format: manifest.FormatMsi}, msiHost); err != nil {
		t.Fatal(err)
	}

	recipeYAML := `version: 1
id: office-apps
name: "Office apps"
os:
  type: windows
  source_mode: tree
  tree_path: ` + filepath.ToSlash(filepath.Join(root, "never-read")) + `
target:
  volume_label: ESD-USB
  min_stick: 1GiB
windows:
  driver_packs:
    - { path: drivers/intel-lan, install: pnputil-sweep }
    - { ref: intel-sst-cab, install: expand-then-sweep }
  debloat:
    preset: standard
  apps:
    winget: [Google.Chrome, 7zip.7zip]
  firstboot:
    mode: generate
    steps:
      - drivers
      - debloat
      - apps
      - msi: { ref: sc-msi, args: ["/qn"] }
`
	writeFile(t, filepath.Join(wsDir, "recipes", "office-apps.yaml"), []byte(recipeYAML))
	ws, err := workspace.Load(wsDir)
	if err != nil {
		t.Fatal(err)
	}
	r, err := ws.Recipe("office-apps")
	if err != nil {
		t.Fatal(err)
	}
	return Request{Workspace: ws, Library: lib, Recipe: r}
}

// readZip returns every file in the zip by name.
func readZip(t *testing.T, path string) map[string][]byte {
	t.Helper()
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	out := map[string][]byte{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		out[f.Name] = b
	}
	return out
}

// The payload is the recipe without the operating system: the agent, a
// standalone manifest naming this build, the driver packs, the installer the
// first-boot step asked for, a way in for a person and a note saying what it
// all is -- and nothing that belongs to installing an OS.
func TestAPayloadCarriesTheRecipeWithoutTheOS(t *testing.T) {
	if !agentbin.Available(agentbin.AMD64) {
		t.Skip("this build has no agent embedded (`./build-agent.sh`)")
	}
	req := payloadFixture(t)
	art, err := BuildPayload(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if art.Kind != "payload" || !strings.HasSuffix(art.Path, ".zip") {
		t.Fatalf("artifact: %+v", art)
	}

	files := readZip(t, art.Path)
	for _, want := range []string{
		"dsky-agent.exe", "dsky-agent.json", "Run DSKY.cmd", "README.txt",
		"intel-sst.cab", "ScreenConnect.ClientSetup.msi",
		"Drivers/intel-lan/e1d.inf",
	} {
		if _, ok := files[want]; !ok {
			names := make([]string, 0, len(files))
			for n := range files {
				names = append(names, n)
			}
			t.Fatalf("the payload is missing %s; it holds %v", want, names)
		}
	}
	// Nothing that is about installing an operating system.
	for name := range files {
		for _, banned := range []string{"autounattend", "ei.cfg", "boot.wim", "verify.ps1", "firstboot.cmd"} {
			if strings.Contains(strings.ToLower(name), banned) {
				t.Errorf("%s belongs to an OS install, not a payload", name)
			}
		}
	}

	var m agent.Manifest
	if err := json.Unmarshal(files["dsky-agent.json"], &m); err != nil {
		t.Fatalf("the staged manifest does not parse: %v", err)
	}
	if !m.Standalone() || m.Build != art.InputsKey {
		t.Errorf("manifest mode %q build %q; want standalone, build %q", m.Mode, m.Build, art.InputsKey)
	}
	if len(m.Apps.Installers) != 1 || m.Apps.Installers[0].File != "ScreenConnect.ClientSetup.msi" || !m.Apps.Installers[0].MSI {
		t.Errorf("the first-boot step's installer did not reach the manifest: %+v", m.Apps.Installers)
	}
	if len(m.Drivers.Cabs) != 1 || m.Drivers.Cabs[0].File != "intel-sst.cab" {
		t.Errorf("the cab did not reach the manifest: %+v", m.Drivers.Cabs)
	}
	if m.VerifyScript != "" {
		t.Errorf("the manifest asks to run %s, whose paths assume a first boot", m.VerifyScript)
	}

	launcher := string(files["Run DSKY.cmd"])
	for _, want := range []string{"Start-Process -Verb RunAs", "'apply'", "Extract the whole folder first"} {
		if !strings.Contains(launcher, want) {
			t.Errorf("the launcher is missing %q:\n%s", want, launcher)
		}
	}
	if strings.Contains(launcher, "\n") && !strings.Contains(launcher, "\r\n") {
		t.Error("the launcher is not CRLF; cmd reads it wrongly")
	}
	readme := string(files["README.txt"])
	for _, want := range []string{"--quiet --unattended", "dsky-agent.exe verify", "does not install an operating system", "consumer apps"} {
		if !strings.Contains(readme, want) {
			t.Errorf("README.txt is missing %q", want)
		}
	}
}

// The same inputs are the same zip, byte for byte, so payloads cache by their
// inputs the way images do.
func TestAPayloadIsReproducible(t *testing.T) {
	if !agentbin.Available(agentbin.AMD64) {
		t.Skip("this build has no agent embedded (`./build-agent.sh`)")
	}
	req := payloadFixture(t)
	first, err := BuildPayload(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	req.Rebuild = true
	second, err := BuildPayload(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if first.SHA256 != second.SHA256 {
		t.Errorf("two builds of the same inputs differ: %s then %s", first.SHA256, second.SHA256)
	}
}

// A recipe the agent cannot carry out completely is refused with the reason,
// not built as a payload that quietly does less than the recipe says.
func TestAPayloadRefusesWhatTheAgentCannotDo(t *testing.T) {
	req := payloadFixture(t)
	req.Recipe.Windows.Firstboot.Steps = append(req.Recipe.Windows.Firstboot.Steps,
		recipe.Step{Cmd: "shutdown /r"})
	_, err := BuildPayload(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "cannot be built as a payload") {
		t.Fatalf("a recipe with a bare command was built as a payload: %v", err)
	}

	linux := &recipe.Recipe{ID: "ubuntu"}
	if _, err := BuildPayload(context.Background(), Request{Recipe: linux}); err == nil {
		t.Error("a Linux recipe was built as a Windows payload")
	}
}

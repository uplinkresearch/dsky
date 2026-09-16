package webui

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/uplinkresearch/dsky/internal/agentbin"
)

// payloadRecipe writes a Windows recipe the agent can carry out, naming an
// OS source that is deliberately absent from the library: a payload must
// never fetch one.
func payloadRecipe(t *testing.T, s *Server) {
	t.Helper()
	ws := s.workspace()
	if ws == nil {
		t.Fatal("no workspace")
	}
	yaml := `version: 1
id: office-apps
name: "Office apps"
os:
  type: windows
  source: windows-11
  source_mode: iso
target:
  volume_label: ESD-USB
  min_stick: 8GiB
windows:
  unattend:
    template: templates/autounattend.xml.tmpl
    vars:
      edition_key: VK7JG-NPHTM-C97JM-9MPGT-3V66T
      locale: en-US
  debloat:
    preset: standard
  apps:
    winget: [Google.Chrome]
  firstboot:
    mode: generate
    steps:
      - debloat
      - apps
`
	if err := os.WriteFile(filepath.Join(ws.Dir, "recipes", "office-apps.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.SetWorkspaceDir(ws.Dir); err != nil {
		t.Fatal(err)
	}
}

// Building a payload from the page must not fetch an operating system. The
// recipe names a Windows ISO that is not in the library; an image build would
// go and download it, and a payload that did the same would spend five
// gigabytes to install Chrome.
func TestBuildingAPayloadFetchesNoOperatingSystem(t *testing.T) {
	if !agentbin.Available(agentbin.AMD64) {
		t.Skip("this build has no agent embedded (`./build-agent.sh`)")
	}
	s := testServer(t)
	payloadRecipe(t, s)
	h := s.handler()

	body := `{"recipe":"office-apps","payload":true}`
	w := post(t, h, "/api/build", body)
	if w.Code != 202 {
		t.Fatalf("build refused: %d %s", w.Code, w.Body)
	}
	ev := waitJob(t, s.Reg, w.Body.Bytes())
	if ev.Err != "" {
		t.Fatalf("the payload build failed: %s", ev.Err)
	}
	if !strings.HasSuffix(ev.Result, ".zip") {
		t.Errorf("built %q, want a payload zip", ev.Result)
	}
	if _, err := os.Stat(ev.Result); err != nil {
		t.Fatalf("the payload is not on disk: %v", err)
	}
	// Nothing that looks like an operating system was fetched.
	if entries, err := os.ReadDir(s.Lib.ArtifactsDir()); err == nil {
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".img") {
				t.Errorf("a payload build produced %s", e.Name())
			}
		}
	}
}

// The page is told which recipes can be payloads, by the same rule the
// builder uses -- not by guessing from the OS type.
func TestTheStateSaysWhichRecipesCanBePayloads(t *testing.T) {
	s := testServer(t)
	payloadRecipe(t, s)
	req := httptest.NewRequest("GET", "/api/state", nil)
	req.Host = "127.0.0.1:8931"
	req.Header.Set("X-DSKY-Token", "sekrit")
	w := httptest.NewRecorder()
	s.handler().ServeHTTP(w, req)

	var state struct {
		Recipes []struct {
			ID      string `json:"id"`
			Payload bool   `json:"payload"`
		} `json:"recipes"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, rc := range state.Recipes {
		if rc.ID == "office-apps" {
			found = true
			if !rc.Payload {
				t.Error("a recipe the agent can carry out is not offered as a payload")
			}
		}
	}
	if !found {
		t.Fatalf("the recipe is not in the state: %s", w.Body)
	}
}

// A payload can be copied where the operator wants it, and cannot be copied
// over itself. Only things this library built can be copied at all.
func TestAPayloadIsCopiedNotFlashed(t *testing.T) {
	s := testServer(t)
	h := s.handler()
	src := filepath.Join(s.Lib.ArtifactsDir(), "office-apps-payload-abc.zip")
	if err := os.WriteFile(src, []byte("payload bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()

	w := post(t, h, "/api/artifacts/copy", `{"path":"`+src+`","to":"`+dest+`"}`)
	if w.Code != 202 {
		t.Fatalf("copy refused: %d %s", w.Code, w.Body)
	}
	if ev := waitJob(t, s.Reg, w.Body.Bytes()); ev.Err != "" {
		t.Fatalf("the copy failed: %s", ev.Err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "office-apps-payload-abc.zip"))
	if err != nil || string(got) != "payload bytes" {
		t.Fatalf("the copy is %q: %v", got, err)
	}

	// Onto itself: refused, rather than truncating the only copy.
	if w := post(t, h, "/api/artifacts/copy", `{"path":"`+src+`","to":"`+s.Lib.ArtifactsDir()+`"}`); w.Code != 400 {
		t.Errorf("copying onto itself returned %d %s", w.Code, w.Body)
	}
	// Something outside the library: refused.
	outside := filepath.Join(t.TempDir(), "elsewhere.zip")
	os.WriteFile(outside, []byte("x"), 0o644)
	if w := post(t, h, "/api/artifacts/copy", `{"path":"`+outside+`","to":"`+dest+`"}`); w.Code != 400 {
		t.Errorf("copying a file outside the library returned %d", w.Code)
	}
	// A folder that is not one: refused.
	if w := post(t, h, "/api/artifacts/copy", `{"path":"`+src+`","to":"`+src+`"}`); w.Code != 400 {
		t.Errorf("copying into a file returned %d", w.Code)
	}

	// And a payload can be thrown away like any other build product.
	if w := post(t, h, "/api/artifacts/delete", `{"path":"`+src+`"}`); w.Code != 200 {
		t.Errorf("deleting a payload returned %d %s", w.Code, w.Body)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Error("the payload is still there")
	}
}

// The Install screen builds a payload straight from the options on it, with no
// recipe and no workspace -- and still without fetching an operating system.
// This is the path for a machine that already has Windows and only needs the
// programs.
func TestTheInstallScreenBuildsAPayloadWithoutAnOS(t *testing.T) {
	if !agentbin.Available(agentbin.AMD64) {
		t.Skip("this build has no agent embedded (`./build-agent.sh`)")
	}
	s := testServer(t)
	h := s.handler()

	body := `{"os_id":"windows-11","mode":"payload","edition":"Pro","account_mode":"local","debloat":"standard","apps":["7zip"]}`
	w := post(t, h, "/api/install", body)
	if w.Code != 202 {
		t.Fatalf("refused: %d %s", w.Code, w.Body)
	}
	ev := waitJob(t, s.Reg, w.Body.Bytes())
	if ev.Err != "" {
		t.Fatalf("the payload build failed: %s", ev.Err)
	}
	zips, _ := filepath.Glob(filepath.Join(s.Lib.ArtifactsDir(), "*payload*.zip"))
	if len(zips) != 1 {
		t.Fatalf("payload zips built: %v", zips)
	}
	imgs, _ := filepath.Glob(filepath.Join(s.Lib.ArtifactsDir(), "*.img"))
	if len(imgs) != 0 {
		t.Errorf("an operating system image was built too: %v", imgs)
	}
	if _, err := s.Lib.Resolve("windows-11"); err == nil {
		t.Error("the Windows ISO was fetched for a payload that never reads one")
	}
}

// Only Windows. A payload sets up programs on a machine that already runs it.
func TestTheInstallScreenRefusesANonWindowsPayload(t *testing.T) {
	s := testServer(t)
	w := post(t, s.handler(), "/api/install", `{"os_id":"ubuntu-26.04-desktop","mode":"payload"}`)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "already runs Windows") {
		t.Errorf("got %d %s", w.Code, w.Body)
	}
}

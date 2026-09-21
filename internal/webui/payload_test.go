package webui

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/uplinkresearch/dsky/internal/agent"
	"github.com/uplinkresearch/dsky/internal/agentbin"
	"github.com/uplinkresearch/dsky/internal/compose"
	"github.com/uplinkresearch/dsky/internal/fsimg"
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
	if !strings.HasSuffix(ev.Result, ".exe") {
		t.Errorf("built %q, want a payload that can be double-clicked", ev.Result)
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
	// Same rule, same dependency: agentCovers reports false when no agent was
	// embedded, so without one this asserts the fallback, not the rule.
	if !agentbin.Available(agentbin.AMD64) {
		t.Skip("this build has no agent embedded (`./build-agent.sh`)")
	}
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

	w := postObj(t, h, "/api/artifacts/copy", map[string]string{"path": src, "to": dest})
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
	if w := postObj(t, h, "/api/artifacts/copy", map[string]string{"path": src, "to": s.Lib.ArtifactsDir()}); w.Code != 400 {
		t.Errorf("copying onto itself returned %d %s", w.Code, w.Body)
	}
	// Something outside the library: refused.
	outside := filepath.Join(t.TempDir(), "elsewhere.zip")
	os.WriteFile(outside, []byte("x"), 0o644)
	if w := postObj(t, h, "/api/artifacts/copy", map[string]string{"path": outside, "to": dest}); w.Code != 400 {
		t.Errorf("copying a file outside the library returned %d", w.Code)
	}
	// A folder that is not one: refused.
	if w := postObj(t, h, "/api/artifacts/copy", map[string]string{"path": src, "to": src}); w.Code != 400 {
		t.Errorf("copying into a file returned %d", w.Code)
	}

	// And a payload can be thrown away like any other build product.
	if w := postObj(t, h, "/api/artifacts/delete", map[string]string{"path": src}); w.Code != 200 {
		t.Errorf("deleting a payload returned %d %s", w.Code, w.Body)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Error("the payload is still there")
	}
}

// The Payload screen builds a payload straight from the options on it, with no
// recipe and no workspace -- and still without fetching an operating system.
// This is the path for a machine that already has Windows and only needs the
// programs.
func TestThePayloadScreenBuildsAPayloadWithoutAnOS(t *testing.T) {
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
	payloads, _ := filepath.Glob(filepath.Join(s.Lib.ArtifactsDir(), "*payload*.exe"))
	if len(payloads) != 1 {
		t.Fatalf("payloads built: %v", payloads)
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

// Payloads built from the Payload screen have no recipe name, so the list says
// what each one carries instead -- read from the manifest inside it, with
// program names where DSKY knows them.
func TestAPayloadSaysWhatItCarries(t *testing.T) {
	path := fakePayload(t, t.TempDir(), agent.Manifest{
		Version: agent.ManifestVersion, Recipe: "windows-11", Mode: agent.ModeStandalone, Build: "abc",
		Apps: &agent.Apps{
			Winget:     []string{"VideoLAN.VLC", "Some.Unknown"},
			Installers: []agent.Installer{{File: "ScreenConnect.ClientSetup.msi", MSI: true}},
		},
		Drivers: agent.Drivers{Cabs: []agent.Cab{{File: "a.cab"}, {File: "b.cab"}}},
		Debloat: &agent.Debloat{Preset: "standard"},
	})

	p := readPayload(&compose.Artifact{Path: path})
	if p == nil {
		t.Fatal("the payload could not be read")
	}
	want := "VLC media player, Some.Unknown, ScreenConnect.ClientSetup.msi, 2 driver packs, removes bloatware (standard)"
	if got := p.Contents(); got != want {
		t.Errorf("contents read as\n  %q\nwant\n  %q", got, want)
	}
	// With no choices kept, editing starts from what the manifest can say:
	// programs by their picker ids, unknown ones by winget id, and bloatware.
	if p.ChoicesKnown || strings.Join(p.Choices.Apps, ",") != "vlc,winget:Some.Unknown" || p.Choices.Debloat != "standard" {
		t.Errorf("choices derived as %+v (known %v)", p.Choices, p.ChoicesKnown)
	}
	if readPayload(&compose.Artifact{Path: filepath.Join(t.TempDir(), "missing.exe")}) != nil {
		t.Error("an unreadable payload claimed contents")
	}

	// Choices that were kept win over what can be derived.
	kept, _ := json.Marshal(payloadChoices{Apps: []string{"vlc"}, DriversFor: "dell:OptiPlex 3070", Debloat: "standard"})
	p = readPayload(&compose.Artifact{Path: path, Choices: kept})
	if !p.ChoicesKnown || p.Choices.DriversFor != "dell:OptiPlex 3070" {
		t.Errorf("kept choices were not used: %+v", p.Choices)
	}
}

// fakePayload writes a file shaped like a payload -- bytes standing in for the
// agent, then a zip holding the manifest -- with the sidecar that says it is
// one, into dir.
func fakePayload(t *testing.T, dir string, m agent.Manifest) string {
	t.Helper()
	path := filepath.Join(dir, "windows-11-payload-"+m.Build+".exe")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	prefix := []byte("MZ stands in for the agent")
	f.Write(prefix)
	zw := zip.NewWriter(f)
	zw.SetOffset(int64(len(prefix)))
	w, err := zw.Create(agent.ManifestName)
	if err != nil {
		t.Fatal(err)
	}
	json.NewEncoder(w).Encode(m)
	zw.Close()
	f.Close()
	meta, _ := json.Marshal(compose.Artifact{RecipeID: m.Recipe, Kind: "payload", Path: path})
	if err := os.WriteFile(compose.MetaPath(path), meta, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// The file on the stick is named for the payload, so at the customer's
// machine it says which one it is -- and nothing FAT refuses gets through.
func TestAPayloadIsNamedForItsStick(t *testing.T) {
	for in, want := range map[string]string{
		"Acme Dental front desk":  "Acme Dental front desk.exe",
		"":                        "DSKY payload.exe",
		"   ":                     "DSKY payload.exe",
		`Smith & Co: "reception"`: "Smith & Co- -reception.exe",
		"Café":                    "Caf.exe",
		strings.Repeat("a", 80):   strings.Repeat("a", 60) + ".exe",
		"trailing dots...":        "trailing dots.exe",
	} {
		if got := stickFileName(in); got != want {
			t.Errorf("stickFileName(%q) = %q, want %q", in, got, want)
		}
	}
}

// The stick is laid out as one FAT32 volume holding the payload, byte for
// byte, under its name -- read back out of the image, not assumed.
func TestAPayloadStickHoldsThePayload(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "windows-11-payload-abc.exe")
	body := bytes.Repeat([]byte("payload bytes "), 100000)
	if err := os.WriteFile(src, body, 0o644); err != nil {
		t.Fatal(err)
	}
	img, err := buildPayloadStick(dir, src, "Front desk.exe", func(string, int64, int64) {})
	if err != nil {
		t.Fatal(err)
	}
	rc, closeImg, err := fsimg.OpenImageFile(img, "/Front desk.exe")
	if err != nil {
		t.Fatalf("the payload is not on the stick under its name: %v", err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	closeImg()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("the stick holds %d bytes, the payload is %d", len(got), len(body))
	}
	st, _ := os.Stat(img)
	if st.Size() > 1<<30 {
		t.Errorf("a 1.4 MiB payload made a %d MiB image; it should be sized for the payload, not the stick", st.Size()>>20)
	}
}

// Writing to a stick is refused for anything that is not a payload this
// library built, before any device is looked at.
func TestOnlyALibraryPayloadIsWrittenToAStick(t *testing.T) {
	s := testServer(t)
	h := s.handler()
	image := filepath.Join(s.Lib.ArtifactsDir(), "office-apps-abc.img")
	os.WriteFile(image, []byte("an image"), 0o644)
	meta, _ := json.Marshal(compose.Artifact{RecipeID: "office-apps", Kind: "image", Path: image})
	os.WriteFile(compose.MetaPath(image), meta, 0o644)
	outside := fakePayload(t, t.TempDir(), agent.Manifest{Version: agent.ManifestVersion, Recipe: "x", Mode: agent.ModeStandalone, Build: "out"})

	for name, path := range map[string]string{"an image": image, "a payload outside the library": outside} {
		w := postObj(t, h, "/api/payloads/write", map[string]string{"path": path, "device_id": "/dev/sdz", "confirm": "1"})
		if w.Code != 400 {
			t.Errorf("writing %s returned %d %s", name, w.Code, w.Body)
		}
	}
}

// Editing a payload builds the new one with the changes and removes the old
// one, keeping the name and the options it was built with -- and a rename that
// builds the same payload keeps the one file rather than deleting it.
func TestEditingAPayloadReplacesIt(t *testing.T) {
	if !agentbin.Available(agentbin.AMD64) {
		t.Skip("this build has no agent embedded (`./build-agent.sh`)")
	}
	s := testServer(t)
	h := s.handler()
	build := func(body map[string]any) string {
		t.Helper()
		w := postObj(t, h, "/api/install", body)
		if w.Code != 202 {
			t.Fatalf("refused: %d %s", w.Code, w.Body)
		}
		if ev := waitJob(t, s.Reg, w.Body.Bytes()); ev.Err != "" {
			t.Fatalf("the build failed: %s", ev.Err)
		}
		payloads, _ := filepath.Glob(filepath.Join(s.Lib.ArtifactsDir(), "*payload*.exe"))
		if len(payloads) != 1 {
			t.Fatalf("payloads in the library: %v", payloads)
		}
		return payloads[0]
	}

	first := build(map[string]any{"os_id": "windows-11", "mode": "payload", "name": "Front desk", "debloat": "off", "apps": []string{"7zip"}})
	a, err := compose.LoadArtifact(compose.MetaPath(first))
	if err != nil || a.Name != "Front desk" {
		t.Fatalf("the name was not kept: %+v %v", a, err)
	}

	second := build(map[string]any{"os_id": "windows-11", "mode": "payload", "name": "Front desk", "debloat": "standard",
		"apps": []string{"7zip", "vlc"}, "replaces": first})
	if second == first {
		t.Fatal("a changed payload was built to the same file")
	}
	b, _ := compose.LoadArtifact(compose.MetaPath(second))
	p := readPayload(b)
	if p == nil || !p.ChoicesKnown || strings.Join(p.Choices.Apps, ",") != "7zip,vlc" || p.Choices.Debloat != "standard" {
		t.Errorf("the edited payload's options were not kept: %+v", p)
	}

	// Renamed only: the same inputs, the same file, now under the new name.
	third := build(map[string]any{"os_id": "windows-11", "mode": "payload", "name": "Reception", "debloat": "standard",
		"apps": []string{"7zip", "vlc"}, "replaces": second})
	if third != second {
		t.Errorf("a rename rebuilt to %s", third)
	}
	c, _ := compose.LoadArtifact(compose.MetaPath(third))
	if c.Name != "Reception" {
		t.Errorf("renamed to %q", c.Name)
	}

	// And a replaces that is not a library payload is refused up front.
	if w := postObj(t, h, "/api/install", map[string]any{"os_id": "windows-11", "mode": "payload", "apps": []string{"7zip"},
		"replaces": "/etc/passwd"}); w.Code != 400 {
		t.Errorf("replacing a file outside the library returned %d", w.Code)
	}
}

// A backup of a named payload is saved under its name, so a folder of them
// says which is which; an unnamed one keeps its library filename.
func TestAPayloadBackupIsSavedUnderItsName(t *testing.T) {
	s := testServer(t)
	h := s.handler()
	named := fakePayload(t, s.Lib.ArtifactsDir(), agent.Manifest{Version: agent.ManifestVersion, Recipe: "windows-11", Mode: agent.ModeStandalone, Build: "named"})
	a, _ := compose.LoadArtifact(compose.MetaPath(named))
	a.Name = "Acme Dental front desk"
	if err := a.Save(); err != nil {
		t.Fatal(err)
	}
	unnamed := fakePayload(t, s.Lib.ArtifactsDir(), agent.Manifest{Version: agent.ManifestVersion, Recipe: "windows-11", Mode: agent.ModeStandalone, Build: "plain"})

	dest := t.TempDir()
	for _, src := range []string{named, unnamed} {
		w := postObj(t, h, "/api/artifacts/copy", map[string]string{"path": src, "to": dest})
		if w.Code != 202 {
			t.Fatalf("copy refused: %d %s", w.Code, w.Body)
		}
		if ev := waitJob(t, s.Reg, w.Body.Bytes()); ev.Err != "" {
			t.Fatal(ev.Err)
		}
	}
	for _, want := range []string{"Acme Dental front desk.exe", filepath.Base(unnamed)} {
		if _, err := os.Stat(filepath.Join(dest, want)); err != nil {
			t.Errorf("the backup folder has no %s", want)
		}
	}
}

// Writing a payload's file raw to a stick is refused: it is a program, not a
// disk image, and the stick would come out holding nothing Windows can read.
func TestAPayloadIsNeverWrittenRawToAStick(t *testing.T) {
	s := testServer(t)
	path := fakePayload(t, s.Lib.ArtifactsDir(), agent.Manifest{Version: agent.ManifestVersion, Recipe: "windows-11", Mode: agent.ModeStandalone, Build: "raw"})
	w := postObj(t, s.handler(), "/api/flash", map[string]string{"artifact": path, "device_id": "/dev/sdz", "confirm": "1"})
	if w.Code != 400 || !strings.Contains(w.Body.String(), "is a payload, not a disk image") {
		t.Errorf("raw-writing a payload returned %d %s", w.Code, w.Body)
	}
}

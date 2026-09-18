package webui

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/uplinkresearch/dsky/internal/manifest"
)

func getJSON(t *testing.T, s *Server, path string, v any) int {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	req.Host = "127.0.0.1:8931"
	req.Header.Set("X-DSKY-Token", "sekrit")
	w := httptest.NewRecorder()
	s.handler().ServeHTTP(w, req)
	if v != nil {
		json.Unmarshal(w.Body.Bytes(), v)
	}
	return w.Code
}

// A saved recipe opens with its settings in the dialog's words, is changed
// from the dialog under the same id, and is deleted.
func TestRecipeOpenEditDelete(t *testing.T) {
	s := testServer(t)
	h := s.handler()
	w := post(t, h, "/api/recipes/save", `{"os_id":"windows-11","name":"Front desk","edition":"Pro","account_mode":"local","debloat":"standard","apps":["brave"]}`)
	if w.Code != 202 {
		t.Fatalf("save: %d %s", w.Code, w.Body)
	}
	if ev := waitJob(t, s.Reg, w.Body.Bytes()); ev.Err != "" {
		t.Fatal(ev.Err)
	}

	var d recipeDetail
	if code := getJSON(t, s, "/api/recipes/get?id=front-desk", &d); code != 200 || d.Form == nil {
		t.Fatalf("get: %d %+v", code, d)
	}
	lines := map[string]string{}
	for _, l := range d.Lines {
		lines[l[0]] = l[1]
	}
	if lines["Edition"] != "Pro" || lines["Install programs"] != "Brave" || lines["Remove bloatware"] != "Standard" ||
		lines["Account setup"] != "Local account, no setup screens" {
		t.Errorf("lines: %v", lines)
	}

	w = post(t, h, "/api/recipes/update", `{"id":"front-desk","os_id":"windows-11","name":"Front desk PCs","edition":"Home","account_mode":"oobe","debloat":"off","apps":["vlc","brave"]}`)
	if w.Code != 202 {
		t.Fatalf("update: %d %s", w.Code, w.Body)
	}
	if ev := waitJob(t, s.Reg, w.Body.Bytes()); ev.Err != "" {
		t.Fatal(ev.Err)
	}
	d = recipeDetail{}
	getJSON(t, s, "/api/recipes/get?id=front-desk", &d)
	if d.Name != "Front desk PCs" || d.Form == nil || d.Form.Edition != "Home" || d.Form.AccountMode != "oobe" ||
		strings.Join(d.Form.Apps, ",") != "vlc,brave" {
		t.Errorf("after update: %+v form %+v", d, d.Form)
	}
	if w := post(t, h, "/api/recipes/update", `{"id":"front-desk","os_id":"ubuntu-26.04-desktop"}`); w.Code != 400 {
		t.Errorf("changing the OS: %d", w.Code)
	}

	// File edits: a broken or renamed recipe is refused and the file kept.
	before, _ := os.ReadFile(d.Path)
	for _, bad := range []string{"version: 1\nid: front-desk\nos: [", strings.Replace(string(before), "id: front-desk", "id: other", 1)} {
		body, _ := json.Marshal(map[string]string{"id": "front-desk", "yaml": bad})
		if w := post(t, h, "/api/recipes/file", string(body)); w.Code != 400 {
			t.Errorf("bad file saved: %d %s", w.Code, w.Body)
		}
	}
	if after, _ := os.ReadFile(d.Path); string(after) != string(before) {
		t.Error("a refused edit changed the file")
	}
	edited := strings.Replace(string(before), `name: "Front desk PCs"`, `name: "Reception"`, 1)
	body, _ := json.Marshal(map[string]string{"id": "front-desk", "yaml": edited})
	if w := post(t, h, "/api/recipes/file", string(body)); w.Code != 200 {
		t.Fatalf("file edit: %d %s", w.Code, w.Body)
	}
	if r, _ := s.workspace().Recipe("front-desk"); r == nil || r.Name != "Reception" {
		t.Errorf("file edit not loaded: %+v", r)
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(d.Path), ".edit-*")); len(left) != 0 {
		t.Errorf("temporary files left: %v", left)
	}

	if w := post(t, h, "/api/recipes/delete", `{"id":"front-desk"}`); w.Code != 200 {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	if code := getJSON(t, s, "/api/recipes/get?id=front-desk", nil); code != 404 {
		t.Errorf("deleted recipe still opens: %d", code)
	}
	// The scaffold's example builds from a source not in the OS list: it opens
	// with its settings, but only as a file.
	d = recipeDetail{}
	getJSON(t, s, "/api/recipes/get?id=example-win11", &d)
	if d.Form != nil || d.NotEditable == "" || d.YAML == "" {
		t.Errorf("example recipe: form %+v, not editable %q", d.Form, d.NotEditable)
	}
}

func TestDownloadsListAndDelete(t *testing.T) {
	s := testServer(t)
	e := linuxEntry(t)
	iso := filepath.Join(t.TempDir(), "x.iso")
	os.WriteFile(iso, []byte("not really an iso"), 0o644)
	if _, err := s.Lib.Import(&manifest.Source{ID: e.ID, Kind: manifest.KindOSImage, Format: manifest.FormatISO}, iso); err != nil {
		t.Fatal(err)
	}
	var st stateResp
	getJSON(t, s, "/api/state", &st)
	if len(st.Downloads) != 1 || st.Downloads[0].OSID != e.ID {
		t.Fatalf("downloads: %+v", st.Downloads)
	}
	h := s.handler()
	if w := post(t, h, "/api/downloads/delete", `{"os_id":"`+e.ID+`"}`); w.Code != 200 {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	st = stateResp{}
	getJSON(t, s, "/api/state", &st)
	if len(st.Downloads) != 0 {
		t.Errorf("still listed: %+v", st.Downloads)
	}
	if w := post(t, h, "/api/downloads/delete", `{"os_id":"`+e.ID+`"}`); w.Code != 404 {
		t.Errorf("deleting again: %d", w.Code)
	}
}

// A kickstart recipe is as automatic as an autoinstall one. The detail view
// knew only the autoinstall shape, so a saved Fedora recipe said "Boots the
// installer — nothing set in advance" — of the one recipe shape that clears
// the disk without asking — and left its program list off the page entirely.
func TestFedoraRecipeDetailSaysWhatItDoes(t *testing.T) {
	s := testServer(t)
	h := s.handler()
	w := post(t, h, "/api/recipes/save", `{"os_id":"fedora-44-server","name":"Lab","apps":["git","nmap"]}`)
	if w.Code != 202 {
		t.Fatalf("save: %d %s", w.Code, w.Body)
	}
	if ev := waitJob(t, s.Reg, w.Body.Bytes()); ev.Err != "" {
		t.Fatal(ev.Err)
	}

	var d recipeDetail
	if code := getJSON(t, s, "/api/recipes/get?id=lab", &d); code != 200 {
		t.Fatalf("get: %d %+v", code, d)
	}
	lines := map[string]string{}
	for _, l := range d.Lines {
		lines[l[0]] = l[1]
	}
	if got := lines["Installer"]; got != "Installs by itself and erases the computer's disk" {
		t.Errorf("Installer line: %q", got)
	}
	if got := lines["Install programs"]; !strings.Contains(got, "Git") || !strings.Contains(got, "Nmap") {
		t.Errorf("Install programs line: %q", got)
	}
	// And it can be reopened in the dialog that made it.
	if d.Form == nil {
		t.Fatalf("not editable: %+v", d)
	}
	if strings.Join(d.Form.Apps, ",") != "git,nmap" {
		t.Errorf("form apps %v", d.Form.Apps)
	}
}

package webui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uplinkresearch/dsky/internal/drivers/catalog"
	"github.com/uplinkresearch/dsky/internal/jobs"
	"github.com/uplinkresearch/dsky/internal/library"
	"github.com/uplinkresearch/dsky/internal/manifest"
	"github.com/uplinkresearch/dsky/internal/oscatalog"
)

// The Install screen without a stick: download the OS, set up the image,
// save the settings as a recipe, and throw away a built image. None of these
// may need a device, and none may reach past what it is for.

func post(t *testing.T, h http.Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Host = "127.0.0.1:8931"
	req.Header.Set("X-DSKY-Token", "sekrit")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// postObj is post for a body built from Go values. Anything carrying a
// filesystem path must go through here: a Windows path pasted into a JSON
// string literal turns C:\Users into an escape sequence and the body never
// parses, which reads at the far end as the handler rejecting a good request.
func postObj(t *testing.T, h http.Handler, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return post(t, h, path, string(b))
}

// waitJob follows the registry until the job reports its end.
func waitJob(t *testing.T, reg *jobs.Registry, body []byte) jobs.Event {
	t.Helper()
	var accepted struct {
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal(body, &accepted); err != nil || accepted.JobID == "" {
		t.Fatalf("no job in %s", body)
	}
	events, stop := reg.Subscribe()
	defer stop()
	timeout := time.After(20 * time.Second)
	for {
		select {
		case ev := <-events:
			if ev.JobID == accepted.JobID && ev.Final {
				return ev
			}
		case <-timeout:
			t.Fatalf("job %s did not finish", accepted.JobID)
		}
	}
}

func linuxEntry(t *testing.T) oscatalog.Entry {
	t.Helper()
	for _, e := range oscatalog.Catalog() {
		if e.Family == oscatalog.Linux && !e.ImportOnly() {
			return e
		}
	}
	t.Fatal("no downloadable Linux entry in the catalog")
	return oscatalog.Entry{}
}

func TestInstallWithoutAStick(t *testing.T) {
	s := testServer(t)
	h := s.handler()

	w := post(t, h, "/api/install", `{"os_id":"windows-11"}`)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "set up the image") {
		t.Errorf("install with no device: %d %s", w.Code, w.Body)
	}
	if w := post(t, h, "/api/install", `{"os_id":"windows-11","mode":"format-c"}`); w.Code != 400 {
		t.Errorf("unknown mode accepted: %d", w.Code)
	}

	// Download of an OS already in the library finishes without touching the
	// network, which is what lets this run offline.
	e := linuxEntry(t)
	fake := filepath.Join(t.TempDir(), "fake.iso")
	if err := os.WriteFile(fake, []byte("not really an iso"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Lib.Import(&manifest.Source{ID: e.ID, Kind: manifest.KindOSImage, Format: manifest.FormatISO, Filename: "fake.iso"}, fake); err != nil {
		t.Fatal(err)
	}
	w = post(t, h, "/api/install", `{"os_id":"`+e.ID+`","mode":"download"}`)
	if w.Code != 202 {
		t.Fatalf("download: %d %s", w.Code, w.Body)
	}
	if ev := waitJob(t, s.Reg, w.Body.Bytes()); ev.Err != "" || !strings.Contains(ev.Result, "downloaded") {
		t.Errorf("download job: %+v", ev)
	}
}

func TestSaveRecipeWithNoWorkspaceOpen(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	lib, err := library.Open(filepath.Join(t.TempDir(), "lib"))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Lib: lib, Token: "sekrit", Reg: jobs.NewRegistry()}
	h := s.handler()
	e := linuxEntry(t)

	w := post(t, h, "/api/recipes/save", `{"os_id":"`+e.ID+`","name":"Lab machines"}`)
	if w.Code != 202 {
		t.Fatalf("save: %d %s", w.Code, w.Body)
	}
	var resp struct {
		Created string `json:"workspace_created"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Created == "" || !strings.HasPrefix(resp.Created, home) {
		t.Errorf("workspace created at %q, want one under the home directory", resp.Created)
	}
	if ev := waitJob(t, s.Reg, w.Body.Bytes()); ev.Err != "" {
		t.Fatalf("save job failed: %s", ev.Err)
	}
	if _, err := os.Stat(filepath.Join(resp.Created, "recipes", "lab-machines.yaml")); err != nil {
		t.Errorf("recipe file: %v", err)
	}
	ws := s.workspace()
	if ws == nil || ws.Dir != resp.Created {
		t.Fatalf("the new workspace was not opened")
	}
	if r, err := ws.Recipe("lab-machines"); err != nil || r.Name != "Lab machines" {
		t.Errorf("saved recipe: %v %+v", err, r)
	}
	// A workspace made on somebody's behalf holds what they saved, not the
	// examples `dsky init` teaches with.
	if all, err := ws.Recipes(); err != nil || len(all) != 1 {
		t.Errorf("recipes in the new workspace: %d, %v", len(all), err)
	}

	// Saving again under the same name is refused before any work starts.
	if w := post(t, h, "/api/recipes/save", `{"os_id":"`+e.ID+`","name":"Lab Machines"}`); w.Code != 400 {
		t.Errorf("duplicate name: %d %s", w.Code, w.Body)
	}
	if w := post(t, h, "/api/recipes/save", `{"os_id":"`+e.ID+`","name":"  "}`); w.Code != 400 {
		t.Errorf("blank name: %d", w.Code)
	}
}

func TestDeleteArtifactStaysInTheLibrary(t *testing.T) {
	s := testServer(t)
	h := s.handler()

	outside := filepath.Join(t.TempDir(), "precious.img")
	os.WriteFile(outside, []byte("x"), 0o644)
	for _, p := range []string{outside, filepath.Join(s.Lib.ArtifactsDir(), "..", "catalog.json"), filepath.Join(s.Lib.ArtifactsDir(), "x.json")} {
		body, _ := json.Marshal(map[string]string{"path": p})
		if w := post(t, h, "/api/artifacts/delete", string(body)); w.Code != 400 {
			t.Errorf("delete %s: %d, want 400", p, w.Code)
		}
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatal("a file outside the library was deleted")
	}

	os.MkdirAll(s.Lib.ArtifactsDir(), 0o755)
	img := filepath.Join(s.Lib.ArtifactsDir(), "win-abc.img")
	os.WriteFile(img, []byte("image"), 0o644)
	os.WriteFile(img+".json", []byte("{}"), 0o644)
	body, _ := json.Marshal(map[string]string{"path": img})
	if w := post(t, h, "/api/artifacts/delete", string(body)); w.Code != 200 {
		t.Fatalf("delete built image: %d %s", w.Code, w.Body)
	}
	for _, p := range []string{img, img + ".json"} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s still there", p)
		}
	}
}

// The model picker's list: every vendor asked, for the OS being installed, and
// one vendor failing does not take the others down with it.
func TestDriverModelsPerVendor(t *testing.T) {
	s := testServer(t)
	asked := map[string]string{}
	var mu sync.Mutex
	s.ModelLister = func(ctx context.Context, vendor, osName string) ([]string, error) {
		mu.Lock()
		asked[vendor] = osName
		mu.Unlock()
		if vendor == "hp" {
			return nil, errors.New("catalog unreachable")
		}
		return []string{vendor + " Model A", vendor + " Model B"}, nil
	}
	// windows-11 rather than windows-10: Windows 10 left the catalog, and an
	// os_id that resolves to no entry would exercise the fallback instead of
	// the lookup this asserts.
	req := httptest.NewRequest("GET", "/api/drivers/models?os_id=windows-11", nil)
	req.Host = "127.0.0.1:8931"
	req.Header.Set("X-DSKY-Token", "sekrit")
	w := httptest.NewRecorder()
	s.handler().ServeHTTP(w, req)
	var resp struct {
		OS      string `json:"os"`
		Vendors []struct {
			Vendor string   `json:"vendor"`
			Name   string   `json:"name"`
			Models []string `json:"models"`
			Error  string   `json:"error"`
		} `json:"vendors"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if resp.OS != "win11" || len(asked) != len(catalog.ModelFeeds) || asked["framework"] != "win11" || asked["alienware"] != "win11" {
		t.Errorf("os %q, asked %v", resp.OS, asked)
	}
	byVendor := map[string]int{}
	for i, v := range resp.Vendors {
		byVendor[v.Vendor] = i
	}
	if d := resp.Vendors[byVendor["dell"]]; d.Name != "Dell" || len(d.Models) != 2 || d.Error != "" {
		t.Errorf("dell: %+v", d)
	}
	if h := resp.Vendors[byVendor["hp"]]; h.Error == "" || h.Models == nil || len(h.Models) != 0 {
		t.Errorf("hp failure not reported alongside the others: %+v", h)
	}
}

// The picker asks one vendor at a time, from a list written into the page. A
// vendor added to the catalog and not to the page would never be asked --
// which is how a new maker goes missing from the picker with nothing failing.
func TestPickerAsksEveryModelFeed(t *testing.T) {
	page := string(indexHTML)
	a := strings.Index(page, "const MODEL_VENDORS = [")
	if a < 0 {
		t.Fatal("the page has no MODEL_VENDORS list")
	}
	block := page[a : a+strings.Index(page[a:], "];")]
	for _, v := range catalog.ModelFeeds {
		want := `{ id: "` + string(v) + `", name: "` + catalog.VendorNames[v] + `" }`
		if !strings.Contains(block, want) {
			t.Errorf("the page's vendor list lacks %s", want)
		}
	}
	if n := strings.Count(block, "{ id:"); n != len(catalog.ModelFeeds) {
		t.Errorf("the page lists %d vendors, the catalog %d", n, len(catalog.ModelFeeds))
	}
}

// One vendor's list, and an incomplete list shown with its reason rather than
// thrown away.
func TestDriverModelsOneVendorPartial(t *testing.T) {
	s := testServer(t)
	s.ModelLister = func(ctx context.Context, vendor, osName string) ([]string, error) {
		return []string{"Alienware m16 R2"}, &catalog.PartialError{Failed: 3, Of: 66, Err: errors.New("403 Forbidden")}
	}
	get := func(q string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/api/drivers/models?"+q, nil)
		req.Host = "127.0.0.1:8931"
		req.Header.Set("X-DSKY-Token", "sekrit")
		w := httptest.NewRecorder()
		s.handler().ServeHTTP(w, req)
		return w
	}
	if w := get("os_id=windows-11&vendor=acer"); w.Code != 400 {
		t.Errorf("unknown vendor: %d", w.Code)
	}
	w := get("os_id=windows-11&vendor=Alienware")
	var resp struct {
		Vendors []vendorModels `json:"vendors"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if len(resp.Vendors) != 1 {
		t.Fatalf("asked for one vendor, got %d", len(resp.Vendors))
	}
	v := resp.Vendors[0]
	if v.Vendor != "alienware" || len(v.Models) != 1 || !v.Partial || !strings.Contains(v.Error, "3 of 66") {
		t.Errorf("partial list: %+v", v)
	}
}

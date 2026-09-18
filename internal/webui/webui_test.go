package webui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/uplinkresearch/dsky/internal/appcatalog"
	"github.com/uplinkresearch/dsky/internal/jobs"
	"github.com/uplinkresearch/dsky/internal/library"
	"github.com/uplinkresearch/dsky/internal/workspace"
)

func testServer(t *testing.T) *Server {
	t.Helper()
	root := t.TempDir()
	wsDir := filepath.Join(root, "ws")
	if err := workspace.Scaffold(wsDir, "Test Org"); err != nil {
		t.Fatal(err)
	}
	lib, err := library.Open(filepath.Join(root, "lib"))
	if err != nil {
		t.Fatal(err)
	}
	// Cfg left nil so the test never writes to the real user config dir.
	s := &Server{Lib: lib, Token: "sekrit", Reg: jobs.NewRegistry()}
	if err := s.SetWorkspaceDir(wsDir); err != nil {
		t.Fatal(err)
	}
	return s
}

func do(t *testing.T, h http.Handler, method, path, host, origin, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.Host = host
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if token != "" {
		req.Header.Set("X-DSKY-Token", token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestAuthAndGuards(t *testing.T) {
	h := testServer(t).handler()

	// The page itself needs no token but a loopback Host.
	if w := do(t, h, "GET", "/", "127.0.0.1:8931", "", ""); w.Code != 200 {
		t.Errorf("index: %d", w.Code)
	}
	// DNS rebinding: non-loopback Host is refused everywhere.
	if w := do(t, h, "GET", "/", "evil.example:8931", "", ""); w.Code != http.StatusForbidden {
		t.Errorf("rebound host: got %d, want 403", w.Code)
	}
	// API without token: 401.
	if w := do(t, h, "GET", "/api/state", "localhost:8931", "", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("no token: got %d, want 401", w.Code)
	}
	// API with wrong token: 401.
	if w := do(t, h, "GET", "/api/state", "localhost:8931", "", "wrong"); w.Code != http.StatusUnauthorized {
		t.Errorf("bad token: got %d, want 401", w.Code)
	}
	// API with token: 200.
	if w := do(t, h, "GET", "/api/state", "localhost:8931", "", "sekrit"); w.Code != 200 {
		t.Errorf("state: got %d body %s", w.Code, w.Body.String())
	}
	// Cross-origin POST is refused even with the token.
	if w := do(t, h, "POST", "/api/build", "127.0.0.1:8931", "https://evil.example", "sekrit"); w.Code != http.StatusForbidden {
		t.Errorf("cross-origin post: got %d, want 403", w.Code)
	}
	// Same-origin POST passes the guard (fails later on the empty body).
	if w := do(t, h, "POST", "/api/build", "127.0.0.1:8931", "http://127.0.0.1:8931", "sekrit"); w.Code != http.StatusBadRequest {
		t.Errorf("same-origin post: got %d, want 400", w.Code)
	}
}

func TestFlashRefusesBadConfirm(t *testing.T) {
	s := testServer(t)
	h := s.handler()
	req := httptest.NewRequest("POST", "/api/flash", nil)
	req.Host = "127.0.0.1:8931"
	req.Header.Set("X-DSKY-Token", "sekrit")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("empty flash: got %d, want 400", w.Code)
	}
}

// The program picker needs the labels, starter sets and not-in-winget hints
// from state, and a typed winget id checked by the server.
func TestProgramPickerData(t *testing.T) {
	h := testServer(t).handler()
	w := do(t, h, "GET", "/api/state", "127.0.0.1:8931", "", "sekrit")
	var st stateResp
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	byID := map[string]appEntry{}
	for _, a := range st.Apps {
		byID[a.ID] = a
	}
	// Two labels now: Spotify installs per-user and wants a desktop, and the
	// picker says both rather than picking one to mention.
	if a := byID["spotify"]; a.Winget != "Spotify.Spotify" || len(a.Labels) != 2 {
		t.Errorf("spotify = %+v, want its winget id, the per-user label and the desktop one", a)
	}
	if len(st.AppSets) != 3 || len(st.NotInWinget) == 0 {
		t.Errorf("sets %d, hints %d", len(st.AppSets), len(st.NotInWinget))
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		trees := map[string]string{
			"manifests/b":                 `{"tree":[{"path":"Brave","type":"tree"}]}`,
			"manifests/b/Brave":           `{"tree":[{"path":"Brave","type":"tree"}]}`,
			"manifests/b/Brave/Brave":     `{"tree":[{"path":"1.0","type":"tree"}]}`,
			"manifests/b/Brave/Brave/1.0": `{"tree":[{"path":"Brave.Brave.yaml","type":"blob"}]}`,
		}
		body, ok := trees[strings.TrimPrefix(r.URL.Path, "/git/trees/master:")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(body))
	}))
	defer srv.Close()
	old := appcatalog.WingetRepoAPI
	appcatalog.WingetRepoAPI = srv.URL
	defer func() { appcatalog.WingetRepoAPI = old }()

	w = do(t, h, "GET", "/api/apps/winget?id=brave.brave", "127.0.0.1:8931", "", "sekrit")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"id":"Brave.Brave"`) {
		t.Errorf("brave.brave: %d %s", w.Code, w.Body.String())
	}
	if w = do(t, h, "GET", "/api/apps/winget?id=Nobody.Nothing", "127.0.0.1:8931", "", "sekrit"); w.Code != 404 {
		t.Errorf("missing package: %d %s", w.Code, w.Body.String())
	}
	if w = do(t, h, "GET", "/api/apps/winget?id=no%20dots", "127.0.0.1:8931", "", "sekrit"); w.Code != 400 {
		t.Errorf("malformed id: %d %s", w.Code, w.Body.String())
	}
}

// A domain join file is checked when the install is asked for, and never
// saved into a recipe: it is one computer's account.
func TestDomainJoinOption(t *testing.T) {
	s := testServer(t)
	good := filepath.Join(t.TempDir(), "PC-042.txt")
	if err := os.WriteFile(good, []byte("QUJDREVGR0g="), 0o600); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(t.TempDir(), "notes.txt")
	os.WriteFile(bad, []byte("this is not a join file"), 0o600)

	_, opts, err := s.installOptions(context.Background(), installRequest{OSID: "windows-11", DomainBlob: good})
	if err != nil || opts.DomainBlob != good {
		t.Fatalf("good file: %v, %+v", err, opts.DomainBlob)
	}
	if _, _, err := s.installOptions(context.Background(), installRequest{OSID: "windows-11", DomainBlob: bad}); err == nil {
		t.Fatal("a file that is not base64 was accepted")
	}
	if _, _, err := s.installOptions(context.Background(), installRequest{OSID: "ubuntu-26.04-server", DomainBlob: good}); err == nil {
		t.Fatal("domain join accepted for Linux")
	}

	h := s.handler()
	body := strings.NewReader(`{"os_id":"windows-11","name":"Front desk","domain_blob":"` + strings.ReplaceAll(good, `\`, `\\`) + `"}`)
	req := httptest.NewRequest("POST", "/api/recipes/save", body)
	req.Host = "127.0.0.1:8931"
	req.Header.Set("X-DSKY-Token", "sekrit")
	req.Header.Set("Origin", "http://127.0.0.1:8931")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "one computer") {
		t.Fatalf("saving a recipe with a join file: %d %s", w.Code, w.Body.String())
	}
}

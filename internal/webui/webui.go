// Package webui serves the local control page: pick a recipe, build, pick a
// device, arm, flash — with live progress. Binds loopback only, never runs
// elevated (flashes go through the same elevated worker as the CLI).
package webui

import (
	"archive/zip"
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/uplinkresearch/dsky/internal/agent"
	"github.com/uplinkresearch/dsky/internal/appcatalog"
	"github.com/uplinkresearch/dsky/internal/appconfig"
	"github.com/uplinkresearch/dsky/internal/buildinfo"
	"github.com/uplinkresearch/dsky/internal/compose"
	"github.com/uplinkresearch/dsky/internal/device"
	"github.com/uplinkresearch/dsky/internal/diskutil"
	"github.com/uplinkresearch/dsky/internal/driverresolve"
	"github.com/uplinkresearch/dsky/internal/drivers/catalog"
	"github.com/uplinkresearch/dsky/internal/filepicker"
	"github.com/uplinkresearch/dsky/internal/flashrun"
	"github.com/uplinkresearch/dsky/internal/fsimg"
	"github.com/uplinkresearch/dsky/internal/helpers"
	"github.com/uplinkresearch/dsky/internal/hwdetect"
	"github.com/uplinkresearch/dsky/internal/jobs"
	"github.com/uplinkresearch/dsky/internal/library"
	"github.com/uplinkresearch/dsky/internal/manifest"
	"github.com/uplinkresearch/dsky/internal/oscatalog"
	"github.com/uplinkresearch/dsky/internal/recipe"
	"github.com/uplinkresearch/dsky/internal/selfupdate"
	"github.com/uplinkresearch/dsky/internal/workspace"
)

//go:embed index.html
var indexHTML []byte

// The wordmark in the header. Embedded like the page, because the page makes
// no network requests, and served unauthenticated like the page, because it is
// the same picture that is on the README.
//
//go:embed logo.webp
var logoWebP []byte

// The app icon at page scale: the browser tab when the portal is opened in a
// browser, and the window icon Chromium's app mode takes from the page.
//
//go:embed favicon.png
var faviconPNG []byte

//go:embed appicon.png
var appIconPNG []byte

// AppIcon is the DSKY icon at 256px, for a native window that has to supply
// its own: the macOS Dock takes a running program's icon from its app bundle,
// and dsky runs from ~/.local/bin, outside one.
func AppIcon() []byte { return appIconPNG }

// Server holds the wiring for one serve session. The workspace is optional
// and switchable at runtime, so the app can launch to a home screen and let
// the operator open a workspace from the page.
type Server struct {
	Lib     *library.Library
	CLIVars map[string]string
	Token   string
	Reg     *jobs.Registry
	Cfg     *appconfig.Config

	wsMu sync.RWMutex
	ws   *workspace.Workspace

	// IdleTimeout stops the server this long after the last page closes.
	// Zero leaves it running until something else stops it.
	//
	// Closing a browser tab is the gesture everyone actually uses, and
	// without this it leaks a server every time: the port auto-increments, so
	// they accumulate rather than collide, and the next one you open is a
	// different instance than the one you were looking at.
	IdleTimeout time.Duration

	quit func() // cancels Serve; set in Serve
	// OnUpdated, when set, is how the host restarts itself after installing a
	// new build. It is a hook rather than something this package does, because
	// only the host knows whether it has a window to close, and whether being
	// replaced mid-sentence is survivable for it.
	OnUpdated func()
	deviceMu  sync.Mutex // one raw-device operation at a time

	idleMu    sync.Mutex
	clients   int         // pages with the event stream open
	sawClient bool        // a page has connected at least once
	idleTimer *time.Timer // running only while clients == 0

	guidesRun map[string]bool // guides closed, when there is no Cfg to keep it in

	// ModelLister names every model a vendor has driver packs for. Nil means
	// the real vendor catalogs; tests substitute their own.
	ModelLister func(ctx context.Context, vendor, osName string) ([]string, error)

	updMu  sync.Mutex // guards the cached update check
	updRel *selfupdate.Release
	updAt  time.Time
}

// SetWorkspaceDir loads the workspace rooted at dir and makes it current,
// recording it in the recents. Used at startup and by the page.
func (s *Server) SetWorkspaceDir(dir string) error {
	root, err := workspace.Find(dir)
	if err != nil {
		return err
	}
	ws, err := workspace.Load(root)
	if err != nil {
		return err
	}
	s.wsMu.Lock()
	s.ws = ws
	s.wsMu.Unlock()
	if s.Cfg != nil {
		s.Cfg.AddRecent(root)
	}
	return nil
}

func (s *Server) workspace() *workspace.Workspace {
	s.wsMu.RLock()
	defer s.wsMu.RUnlock()
	return s.ws
}

// WorkspaceName returns the current workspace's org name, or "" if none.
func (s *Server) WorkspaceName() string {
	if ws := s.workspace(); ws != nil {
		return ws.Config.Org.Name
	}
	return ""
}

// Serve runs on ln until ctx is canceled or the page requests quit.
func Serve(ctx context.Context, ln net.Listener, s *Server) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.quit = cancel
	srv := &http.Server{
		Handler:           s.handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()
	select {
	case <-ctx.Done():
		shutdownCtx, c := context.WithTimeout(context.Background(), 3*time.Second)
		defer c()
		_ = srv.Shutdown(shutdownCtx)
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})
	mux.HandleFunc("GET /logo.webp", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/webp")
		w.Header().Set("Cache-Control", "max-age=86400")
		w.Write(logoWebP)
	})
	mux.HandleFunc("GET /favicon.png", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "max-age=86400")
		w.Write(faviconPNG)
	})
	mux.HandleFunc("GET /api/state", s.auth(s.handleState))
	mux.HandleFunc("POST /api/workspace", s.auth(s.handleSetWorkspace))
	mux.HandleFunc("POST /api/browse", s.auth(s.handleBrowse))
	mux.HandleFunc("POST /api/browse/image", s.auth(s.handleBrowseImage))
	mux.HandleFunc("POST /api/browse/installer", s.auth(s.handleBrowseInstaller))
	mux.HandleFunc("POST /api/browse/djoin", s.auth(s.handleBrowseDjoin))
	mux.HandleFunc("GET /api/apps", s.auth(s.handleApps))
	mux.HandleFunc("GET /api/apps/winget", s.auth(s.handleWingetLookup))
	mux.HandleFunc("POST /api/apps/add", s.auth(s.handleAppAdd))
	mux.HandleFunc("POST /api/apps/set", s.auth(s.handleAppSet))
	mux.HandleFunc("POST /api/apps/remove", s.auth(s.handleAppRemove))
	mux.HandleFunc("GET /api/workspace/defaults", s.auth(s.handleWorkspaceDefaults))
	mux.HandleFunc("POST /api/workspace/new", s.auth(s.handleNewWorkspace))
	mux.HandleFunc("GET /api/disks", s.auth(s.handleDisks))
	mux.HandleFunc("POST /api/disks/prepare", s.auth(s.handleDiskPrepare))
	mux.HandleFunc("POST /api/build", s.auth(s.handleBuild))
	mux.HandleFunc("POST /api/flash", s.auth(s.handleFlash))
	mux.HandleFunc("POST /api/install", s.auth(s.handleInstall))
	mux.HandleFunc("POST /api/recipes/save", s.auth(s.handleSaveRecipe))
	mux.HandleFunc("POST /api/artifacts/delete", s.auth(s.handleDeleteArtifact))
	mux.HandleFunc("POST /api/artifacts/copy", s.auth(s.handleCopyArtifact))
	mux.HandleFunc("POST /api/payloads/write", s.auth(s.handleWritePayload))
	mux.HandleFunc("GET /api/recipes/get", s.auth(s.handleRecipeGet))
	mux.HandleFunc("POST /api/recipes/update", s.auth(s.handleRecipeUpdate))
	mux.HandleFunc("POST /api/recipes/file", s.auth(s.handleRecipeFile))
	mux.HandleFunc("POST /api/recipes/delete", s.auth(s.handleRecipeDelete))
	mux.HandleFunc("POST /api/downloads/delete", s.auth(s.handleDownloadDelete))
	mux.HandleFunc("GET /api/detect", s.auth(s.handleDetect))
	mux.HandleFunc("GET /api/drivers/models", s.auth(s.handleDriverModels))
	mux.HandleFunc("GET /api/update", s.auth(s.handleUpdateCheck))
	mux.HandleFunc("POST /api/update", s.auth(s.handleUpdateApply))
	mux.HandleFunc("POST /api/capture", s.auth(s.handleCapture))
	mux.HandleFunc("GET /api/identify", s.auth(s.handleIdentify))
	mux.HandleFunc("POST /api/duplicate", s.auth(s.handleDuplicate))
	mux.HandleFunc("POST /api/quit", s.auth(s.handleQuit))
	mux.HandleFunc("POST /api/guide", s.auth(s.handleGuide))
	mux.HandleFunc("GET /api/events", s.auth(s.handleEvents))
	return s.hostGuard(mux)
}

// hostGuard kills DNS-rebinding: only loopback Host headers are served, and
// mutating requests must carry a same-origin (or no) Origin.
func (s *Server) hostGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(r.Host); err == nil {
			host = h
		}
		if host != "127.0.0.1" && host != "localhost" && host != "::1" {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet {
			if origin := r.Header.Get("Origin"); origin != "" {
				u, err := url.Parse(origin)
				oh := ""
				if err == nil {
					oh = u.Hostname()
				}
				if oh != "127.0.0.1" && oh != "localhost" && oh != "::1" {
					http.Error(w, "forbidden origin", http.StatusForbidden)
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := r.Header.Get("X-DSKY-Token")
		if subtle.ConstantTimeCompare([]byte(tok), []byte(s.Token)) != 1 {
			http.Error(w, "missing or wrong token — open the exact URL dsky serve printed", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func httpErr(w http.ResponseWriter, status int, format string, args ...any) {
	writeJSON(w, status, map[string]string{"error": fmt.Sprintf(format, args...)})
}

// ── state ────────────────────────────────────────────────────────────────────

type stateResp struct {
	Org          string         `json:"org"`
	Workspace    string         `json:"workspace"`
	HasWorkspace bool           `json:"has_workspace"`
	Recent       []recentWS     `json:"recent"`
	Recipes      []recipeInfo   `json:"recipes"`
	Sources      []sourceInfo   `json:"sources"`
	Devices      []deviceInfo   `json:"devices"`
	Artifacts    []artifactInfo `json:"artifacts"`
	// Downloads are the OS images already in the library.
	Downloads   []downloadInfo `json:"downloads"`
	Catalog     []catalogEntry `json:"catalog"`
	Apps        []appEntry     `json:"apps"`
	AppSets     []appSet       `json:"app_sets"`
	NotInWinget []elsewhere    `json:"not_in_winget"`
	LibraryRoot string         `json:"library_root"`
	// HostOS is runtime.GOOS. WindowsFetch says whether Windows can be fetched
	// from Microsoft here (off Windows it needs PowerShell 7); when it can't,
	// the page asks for the ISO file instead.
	HostOS       string `json:"host_os"`
	WindowsFetch bool   `json:"windows_fetch"`
	// WindowsTools names what this computer lacks to build Windows media, with
	// the command that installs it; empty when nothing is missing.
	WindowsToolsMissing []string `json:"windows_tools_missing,omitempty"`
	WindowsToolsInstall string   `json:"windows_tools_install,omitempty"`
	// GuidesSeen: the guides closed on this machine — "home" for the start
	// screen's, and one per screen.
	GuidesSeen []string `json:"guides_seen"`
}

// appEntry is one program the install picker can offer.
type appEntry struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Category string `json:"category"`
	// Mine marks an installer the operator added, which behaves differently
	// enough to say so: it rides on the stick and needs no network.
	Mine bool `json:"mine,omitempty"`
	// Windows, Ubuntu and Fedora say where the program can be installed; the
	// picker shows only what the chosen OS can take.
	Windows    bool   `json:"windows,omitempty"`
	Ubuntu     bool   `json:"ubuntu,omitempty"`
	UbuntuNote string `json:"ubuntu_note,omitempty"`
	Fedora     bool   `json:"fedora,omitempty"`
	FedoraNote string `json:"fedora_note,omitempty"`
	// Winget is the package id, searchable for someone who knows it.
	Winget string `json:"winget,omitempty"`
	// Labels say how the program differs from installed-for-everyone and
	// ready to use: per-user only, needs a licence, a large download.
	Labels []string `json:"labels,omitempty"`
}

// appSet is a starter group the picker adds in one click.
type appSet struct {
	ID   string   `json:"id"`
	Name string   `json:"name"`
	Apps []string `json:"apps"`
}

// elsewhere is a program people search for that winget does not have.
type elsewhere struct {
	Name  string   `json:"name"`
	Words []string `json:"words"`
	Note  string   `json:"note"`
}

type catalogEntry struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Family        string   `json:"family"`
	Category      string   `json:"category"`
	Arch          string   `json:"arch"`
	Version       string   `json:"version"`
	Notes         string   `json:"notes"`
	FirmwareNotes string   `json:"firmware_notes,omitempty"`
	Editions      []string `json:"editions,omitempty"`
	// ImportOnly entries have no download; the page must ask for a path
	// instead of offering a button that cannot work.
	ImportOnly bool   `json:"import_only,omitempty"`
	ImportFrom string `json:"import_from,omitempty"`
	// Downloaded: the OS image is already in the library.
	Downloaded bool `json:"downloaded,omitempty"`
	// Programs: the program picker is offered — Windows, Ubuntu's installer,
	// which takes autoinstall answers, and Anaconda, which takes a kickstart.
	Programs bool `json:"programs,omitempty"`
	// ThirdPartyDrivers: "drivers for this computer" means something here.
	// Ubuntu's installer can fetch the proprietary ones; Anaconda has no
	// equivalent, so the dialog does not offer a control that does nothing.
	ThirdPartyDrivers bool `json:"third_party_drivers,omitempty"`
	// AppTarget is which program list this entry takes — windows, ubuntu or
	// fedora. Sent rather than inferred: the dialog used to work it out from
	// whether third-party drivers were offered, which is true of Ubuntu today
	// and would quietly show the wrong list for the next entry that is neither.
	AppTarget string `json:"app_target,omitempty"`
	// FoundISO: not in the library, but its ISO is sitting in Downloads, so
	// the dialog can use it instead of asking Microsoft.
	FoundISO string `json:"found_iso,omitempty"`
}

type recentWS struct {
	Dir  string `json:"dir"`
	Name string `json:"name"`
}

type recipeInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
	// Source is the OS image the recipe builds from.
	Source string   `json:"source"`
	Lint   []string `json:"lint,omitempty"`
	// Payload says the agent can carry this recipe out on a machine that
	// already runs Windows, so the page can offer that as well as media.
	// Decided here, by the same rule the builder uses, rather than by the
	// page guessing from the OS type.
	Payload bool `json:"payload,omitempty"`
}

type sourceInfo struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Pinned    bool   `json:"pinned"`
	InLibrary bool   `json:"in_library"`
	SizeMB    int64  `json:"size_mb,omitempty"`
}

type deviceInfo struct {
	ID        string   `json:"id"`
	Model     string   `json:"model"`
	Bus       string   `json:"bus"`
	SizeGiB   string   `json:"size_gib"`
	Flashable bool     `json:"flashable"`
	System    bool     `json:"system"`
	Mounts    []string `json:"mounts,omitempty"`
	Confirm   string   `json:"confirm"`
}

type artifactInfo struct {
	Path     string `json:"path"`
	RecipeID string `json:"recipe_id"`
	SizeMB   int64  `json:"size_mb"`
	Created  string `json:"created"`
	// Kind is "image" for bootable media, "payload" for a zip that installs
	// programs onto a machine that already runs Windows. The page offers a
	// different thing to do with each, and writing a payload to a stick is
	// refused outright.
	Kind string `json:"kind"`
	// Contents says what a payload puts on a machine, read from the manifest
	// inside it: two payloads built from the Payload screen have no recipe
	// name to tell them apart, and "10 MiB, built at 19:58" is not a way to
	// tell which one has the programs a customer asked for.
	Contents string `json:"contents,omitempty"`
	// Name is what somebody called the payload, if they did.
	Name string `json:"name,omitempty"`
	// Payload is the rest of what the Payload screen shows when one is
	// opened, and what editing it starts from.
	Payload *payloadSummary `json:"payload,omitempty"`
}

// payloadChoices are the Payload screen's options, kept with each payload
// built from it so that editing one starts from what it was built with.
type payloadChoices struct {
	Apps       []string `json:"apps"`
	DriversFor string   `json:"drivers_for"`
	Debloat    string   `json:"debloat"`
}

// payloadSummary is what a payload carries, read from the manifest inside it.
type payloadSummary struct {
	Programs    []string `json:"programs"`
	Installers  []string `json:"installers"`
	DriverPacks []string `json:"driver_packs"`
	Debloat     string   `json:"debloat,omitempty"`
	Build       string   `json:"build,omitempty"`
	// Choices are the options to edit it from: the ones it was built with
	// when they were kept, otherwise as much as the manifest can say --
	// programs and bloatware, but not which computer models its driver packs
	// were for, which the manifest does not record.
	Choices      payloadChoices `json:"choices"`
	ChoicesKnown bool           `json:"choices_known"`
}

// Contents is the summary in one line, for a list row.
func (p *payloadSummary) Contents() string {
	var parts []string
	parts = append(parts, p.Programs...)
	parts = append(parts, p.Installers...)
	switch n := len(p.DriverPacks); {
	case n == 1:
		parts = append(parts, "1 driver pack")
	case n > 1:
		parts = append(parts, fmt.Sprintf("%d driver packs", n))
	}
	if p.Debloat != "" {
		parts = append(parts, "removes bloatware ("+p.Debloat+")")
	}
	return strings.Join(parts, ", ")
}

// readPayload summarises a payload from the manifest it carries, in the words
// the Payload screen uses: program names where DSKY knows them, the operator's
// own installers by file, then drivers and bloatware. Nil when the file cannot
// be read -- the row still shows, it just says less.
func readPayload(a *compose.Artifact) *payloadSummary {
	zr, err := zip.OpenReader(a.Path)
	if err != nil {
		return nil
	}
	defer zr.Close()
	f, err := zr.Open(agent.ManifestName)
	if err != nil {
		return nil
	}
	defer f.Close()
	var m agent.Manifest
	if err := json.NewDecoder(f).Decode(&m); err != nil {
		return nil
	}
	byWinget := map[string]appcatalog.App{}
	for _, app := range appcatalog.Catalog() {
		if app.Winget != "" {
			byWinget[strings.ToLower(app.Winget)] = app
		}
	}
	byFile := map[string]appcatalog.Custom{}
	for _, c := range appcatalog.CustomApps() {
		byFile[strings.ToLower(c.Filename)] = c
	}
	p := &payloadSummary{Programs: []string{}, Installers: []string{}, DriverPacks: []string{}, Build: m.Build}
	derived := payloadChoices{Apps: []string{}, Debloat: "off"}
	if m.Apps != nil {
		for _, id := range m.Apps.Winget {
			if app, ok := byWinget[strings.ToLower(id)]; ok {
				p.Programs = append(p.Programs, app.Name)
				derived.Apps = append(derived.Apps, app.ID)
			} else {
				p.Programs = append(p.Programs, id)
				derived.Apps = append(derived.Apps, "winget:"+id)
			}
		}
		for _, in := range m.Apps.Installers {
			if c, ok := byFile[strings.ToLower(in.File)]; ok {
				p.Installers = append(p.Installers, c.Name)
				derived.Apps = append(derived.Apps, c.ID)
			} else {
				p.Installers = append(p.Installers, in.File)
			}
		}
	}
	for _, c := range m.Drivers.Cabs {
		p.DriverPacks = append(p.DriverPacks, c.File)
	}
	for _, x := range m.Drivers.Extracts {
		p.DriverPacks = append(p.DriverPacks, x.File)
	}
	for _, x := range m.Drivers.Exes {
		p.DriverPacks = append(p.DriverPacks, x.File)
	}
	if m.Debloat != nil {
		p.Debloat = m.Debloat.Preset
		derived.Debloat = m.Debloat.Preset
	}
	var kept payloadChoices
	if len(a.Choices) > 0 && json.Unmarshal(a.Choices, &kept) == nil {
		p.Choices, p.ChoicesKnown = kept, true
	} else {
		p.Choices = derived
	}
	return p
}

// deviceInfoOf is the one place a device becomes a page row, so the devices
// list and the disks view cannot drift apart.
func deviceInfoOf(d device.Device) deviceInfo {
	return deviceInfo{
		ID: d.ID, Model: d.Model, Bus: d.Bus,
		SizeGiB:   d.SizeConfirmation(),
		Flashable: d.Flashable(), System: d.System,
		Mounts: d.Mounts, Confirm: d.SizeConfirmation(),
	}
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	resp := stateResp{
		Recent: []recentWS{}, Recipes: []recipeInfo{}, Sources: []sourceInfo{},
		Devices: []deviceInfo{}, Artifacts: []artifactInfo{}, Catalog: []catalogEntry{},
		Apps: []appEntry{}, AppSets: []appSet{}, NotInWinget: []elsewhere{},
		Downloads:   s.downloads(),
		LibraryRoot: s.Lib.Root, GuidesSeen: s.seenGuides(), HostOS: runtime.GOOS,
		WindowsFetch:        helpers.CanFetchWindows(r.Context()),
		WindowsToolsMissing: helpers.MissingForWindowsMedia(s.Lib.HelpersDir()),
	}
	// Only programs that install somewhere: an option that cannot run is
	// worse than no option, and the picker filters again by the OS chosen.
	for _, a := range appcatalog.Catalog() {
		if !a.InstallsOnWindows() && a.Ubuntu == nil && a.Fedora == nil {
			continue
		}
		ent := appEntry{
			ID: a.ID, Name: a.Name, Category: a.Category,
			Mine: a.Custom != nil, Winget: a.Winget, Labels: a.Labels(),
			Windows: a.InstallsOnWindows(),
			Ubuntu:  a.Ubuntu != nil, Fedora: a.Fedora != nil,
		}
		if a.Ubuntu != nil {
			ent.UbuntuNote = a.Ubuntu.Note
		}
		if a.Fedora != nil {
			ent.FedoraNote = a.Fedora.Note
		}
		resp.Apps = append(resp.Apps, ent)
	}
	for _, set := range appcatalog.Sets {
		resp.AppSets = append(resp.AppSets, appSet{ID: set.ID, Name: set.Name, Apps: set.Apps})
	}
	for _, e := range appcatalog.NotInWinget {
		resp.NotInWinget = append(resp.NotInWinget, elsewhere{Name: e.Name, Words: e.Words, Note: e.Note})
	}
	resp.WindowsToolsInstall = helpers.InstallCommand(resp.WindowsToolsMissing)
	for _, e := range oscatalog.Catalog() {
		downloaded := oscatalog.InLibrary(s.Lib, e)
		found := ""
		if !downloaded {
			found = oscatalog.FindDownloadedISO(e)
		}
		resp.Catalog = append(resp.Catalog, catalogEntry{
			ID: e.ID, Name: e.Name, Family: string(e.Family),
			Category: string(e.Group()), Arch: e.CPUArch(), Version: e.Version,
			Notes: e.Notes, FirmwareNotes: e.FirmwareNotes, Editions: e.Editions,
			ImportOnly: e.ImportOnly(), ImportFrom: e.ImportFrom,
			Downloaded: downloaded, FoundISO: found, Programs: e.ProgramsSupported(),
			ThirdPartyDrivers: e.ThirdPartyDriversSupported(),
			AppTarget:         string(e.AppTarget()),
		})
	}
	if s.Cfg != nil {
		for _, dir := range s.Cfg.Recent {
			resp.Recent = append(resp.Recent, recentWS{Dir: dir, Name: workspaceName(dir)})
		}
	}

	ws := s.workspace()
	if ws != nil {
		resp.HasWorkspace = true
		resp.Org = ws.Config.Org.Name
		resp.Workspace = ws.Dir
		if recipes, err := ws.Recipes(); err == nil {
			for _, rc := range recipes {
				info := recipeInfo{ID: rc.ID, Name: rc.Name, Type: string(rc.OS.Type), Source: rc.OS.Source}
				if rc.Windows != nil {
					info.Payload, _ = compose.AgentMedia(rc)
				}
				for _, f := range rc.Lint() {
					info.Lint = append(info.Lint, f.String())
				}
				resp.Recipes = append(resp.Recipes, info)
			}
		}
		if sources, err := ws.Sources(); err == nil {
			for _, src := range sources {
				info := sourceInfo{ID: src.ID, Kind: string(src.Kind), Pinned: src.SHA256 != ""}
				if e, err := s.Lib.Resolve(src.ID); err == nil {
					info.InLibrary = true
					info.SizeMB = e.Size >> 20
				}
				resp.Sources = append(resp.Sources, info)
			}
		}
	}

	if devs, err := device.List(r.Context()); err == nil {
		for _, d := range devs {
			resp.Devices = append(resp.Devices, deviceInfoOf(d))
		}
	}

	metas, _ := filepath.Glob(filepath.Join(s.Lib.ArtifactsDir(), "*.img.json"))
	// Payloads are programs rather than disk images, and .zip is what they
	// were before they became one file that runs itself. Both are listed:
	// the library may still hold one built by an earlier release.
	for _, pat := range []string{"*.exe.json", "*.zip.json"} {
		if more, err := filepath.Glob(filepath.Join(s.Lib.ArtifactsDir(), pat)); err == nil {
			metas = append(metas, more...)
		}
	}
	{
		for _, m := range metas {
			a, err := compose.LoadArtifact(m)
			if err != nil {
				continue
			}
			if _, err := os.Stat(a.Path); err != nil {
				continue
			}
			info := artifactInfo{
				Path: a.Path, RecipeID: a.RecipeID, SizeMB: a.Size >> 20,
				Created: a.CreatedAt.Format("2006-01-02 15:04"), Kind: a.Kind,
			}
			if a.Kind == "payload" {
				info.Name = a.Name
				if p := readPayload(a); p != nil {
					info.Payload = p
					info.Contents = p.Contents()
				}
			}
			resp.Artifacts = append(resp.Artifacts, info)
		}
		sort.Slice(resp.Artifacts, func(i, j int) bool { return resp.Artifacts[i].Created > resp.Artifacts[j].Created })
	}
	writeJSON(w, 200, resp)
}

// ── build ────────────────────────────────────────────────────────────────────

func (s *Server) handleBuild(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Recipe string `json:"recipe"`
		// Payload builds the recipe's programs and drivers for a machine
		// that already runs Windows, instead of media that installs one.
		Payload bool `json:"payload"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Recipe == "" {
		httpErr(w, 400, "body must be {\"recipe\": \"<id>\"}")
		return
	}
	title := req.Recipe
	if req.Payload {
		title = req.Recipe + " (payload)"
	}
	job := s.Reg.New("build", title)
	go func() {
		art, err := s.build(context.Background(), req.Recipe, req.Payload, progressFor(job))
		if err != nil {
			job.Fail(err)
			return
		}
		job.Finish(art.Path)
	}()
	writeJSON(w, 202, map[string]string{"job_id": job.ID})
}

func (s *Server) build(ctx context.Context, recipeID string, payload bool, progress func(string, int64, int64)) (*compose.Artifact, error) {
	ws := s.workspace()
	if ws == nil {
		return nil, fmt.Errorf("no workspace is open")
	}
	rc, err := ws.Recipe(recipeID)
	if err != nil {
		return nil, err
	}
	for _, f := range rc.Lint() {
		if f.Severity == "error" {
			return nil, fmt.Errorf("lint: %s", f.Message)
		}
	}
	req := compose.Request{
		Workspace: ws, Library: s.Lib, Recipe: rc, CLIVars: s.CLIVars,
		Progress: progress,
	}
	if payload {
		// No operating system is involved, so none is fetched: a payload
		// that quietly downloaded five gigabytes of Windows to install
		// Chrome would be absurd. The driver packs and installers it does
		// need are fetched by compose as it stages them. Nor are the media
		// tools needed, since no ISO is read and no WIM is split.
		return compose.BuildPayload(ctx, req)
	}
	// A Windows recipe that will have to read its ISO needs the tools before
	// the download, not after it.
	if rc.Windows != nil && rc.OS.SourceMode != recipe.SourceTree {
		if err := helpers.WindowsMediaToolsError(s.Lib.HelpersDir()); err != nil {
			return nil, err
		}
	}
	if err := s.ensureSources(ctx, ws, rc, progress); err != nil {
		return nil, err
	}
	return compose.Build(ctx, req)
}

// ensureSources downloads whatever a recipe needs that is not in the library
// yet. Building a recipe used to fail on a missing ISO with nothing to do about
// it in the page; the CLI's `go` has always fetched first.
//
// A source named after a catalog OS is fetched the way Quick Install fetches
// it — Microsoft's rotating links, distros that publish checksums beside the
// image — because that is where recipes saved from the Install screen point.
// Anything else comes from the workspace's manifest, trusted on first use only
// when its provider cannot be pinned (Fido), as the CLI does for Quick Install.
func (s *Server) ensureSources(ctx context.Context, ws *workspace.Workspace, rc *recipe.Recipe, progress func(string, int64, int64)) error {
	for _, ref := range rc.SourceRefs() {
		if _, err := s.Lib.Resolve(ref); err == nil {
			continue
		}
		if e, ok := oscatalog.Get(ref); ok {
			if err := oscatalog.Fetch(ctx, s.Lib, e, progress); err != nil {
				return err
			}
			continue
		}
		src, err := ws.Source(ref)
		if err != nil {
			return fmt.Errorf("%s is not in the library and has no manifest", ref)
		}
		if src.URL == "" && src.Provider == "" {
			return fmt.Errorf("%s is not in the library and its manifest has nowhere to download it from — import it with `dsky sources import %s <file>`", ref, ref)
		}
		resolver := func(ctx context.Context, m *manifest.Source) (string, error) {
			progress("resolving download URL", 0, -1)
			return helpers.ResolveFidoURL(ctx, s.Lib.HelpersDir(), m.Fido)
		}
		if _, err := s.Lib.Pull(ctx, src, src.Provider != "", resolver, func(done, total int64) {
			progress("downloading "+ref, done, total)
		}, func(s string) { progress(s, 0, -1) }); err != nil {
			return err
		}
	}
	return nil
}

// workspaceName reads a workspace directory's org name for display, falling
// back to the folder name.
func workspaceName(dir string) string {
	if ws, err := workspace.Load(dir); err == nil && ws.Config.Org.Name != "" {
		return ws.Config.Org.Name
	}
	return filepath.Base(dir)
}

// handleSetWorkspace opens the workspace at the posted dir.
func (s *Server) handleSetWorkspace(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Dir string `json:"dir"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Dir) == "" {
		httpErr(w, 400, "body must be {\"dir\": \"<path>\"}")
		return
	}
	if err := s.SetWorkspaceDir(strings.TrimSpace(req.Dir)); err != nil {
		httpErr(w, 400, "%v", err)
		return
	}
	writeJSON(w, 200, map[string]string{"ok": "1"})
}

// seenGuides lists the guides that have been closed. Without a config (tests,
// or a portal started with no user config) they are remembered for this run.
func (s *Server) seenGuides() []string {
	if s.Cfg != nil {
		return s.Cfg.SeenGuides()
	}
	s.idleMu.Lock()
	defer s.idleMu.Unlock()
	out := []string{}
	for g := range s.guidesRun {
		out = append(out, g)
	}
	sort.Strings(out)
	return out
}

var guideName = regexp.MustCompile(`^[a-z]{0,20}$`)

// handleGuide records that a guide was closed — {"guide": "install", "seen":
// true} — or forgets it so it is offered again. {"seen": false} with no guide
// forgets them all.
func (s *Server) handleGuide(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Guide string `json:"guide"`
		Seen  bool   `json:"seen"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || !guideName.MatchString(req.Guide) || (req.Seen && req.Guide == "") {
		httpErr(w, 400, "body must be {\"guide\": \"<name>\", \"seen\": true|false}")
		return
	}
	if s.Cfg != nil {
		if err := s.Cfg.MarkGuide(req.Guide, req.Seen); err != nil {
			httpErr(w, 500, "saving settings: %v", err)
			return
		}
	} else {
		s.idleMu.Lock()
		if s.guidesRun == nil {
			s.guidesRun = map[string]bool{}
		}
		switch {
		case req.Seen:
			s.guidesRun[req.Guide] = true
		case req.Guide == "":
			s.guidesRun = map[string]bool{}
		default:
			delete(s.guidesRun, req.Guide)
		}
		s.idleMu.Unlock()
	}
	writeJSON(w, 200, map[string]any{"guides_seen": s.seenGuides()})
}

// vendorModels is one vendor's list for the Install dialog's model picker.
type vendorModels struct {
	Vendor string   `json:"vendor"`
	Name   string   `json:"name"`
	Models []string `json:"models"`
	Error  string   `json:"error,omitempty"`
	// Partial is an Error that left the list incomplete rather than empty.
	Partial bool `json:"partial,omitempty"`
}

// handleDriverModels lists every model the model feeds publish driver
// packs for, for the OS being installed, so a model is picked from the
// vendor's own list instead of typed and guessed at. Each vendor is fetched
// on its own: one catalog being unreachable leaves the other two usable, and
// says why the missing one is missing.
func (s *Server) handleDriverModels(w http.ResponseWriter, r *http.Request) {
	osName := "win11"
	if e, ok := oscatalog.Get(r.URL.Query().Get("os_id")); ok && e.Family == oscatalog.Windows {
		osName = e.DriverOS()
	}
	lister := s.ModelLister
	if lister == nil {
		cache := catalog.NewCache(s.Lib.HelpersDir())
		lister = func(ctx context.Context, vendor, osName string) ([]string, error) {
			feed, err := catalog.FeedFor(vendor, cache)
			if err != nil {
				return nil, err
			}
			l, ok := feed.(catalog.Lister)
			if !ok {
				return nil, fmt.Errorf("%s has no model list", vendor)
			}
			return l.Models(ctx, osName, "x64")
		}
	}
	names := catalog.VendorNames
	// One vendor at a time is what the Install screen asks for, so the fast
	// lists show while a slow one is still coming in; with no vendor, all.
	feeds := catalog.ModelFeeds
	if want := r.URL.Query().Get("vendor"); want != "" {
		if !catalog.IsModelFeed(want) {
			httpErr(w, 400, "%q has no model list", want)
			return
		}
		feeds = []catalog.Vendor{catalog.Vendor(strings.ToLower(want))}
	}
	out := make([]vendorModels, len(feeds))
	var wg sync.WaitGroup
	for i, v := range feeds {
		wg.Add(1)
		go func(i int, v catalog.Vendor) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
			defer cancel()
			vm := vendorModels{Vendor: string(v), Name: names[v], Models: []string{}}
			// A list that came back incomplete is still shown, with the
			// reason next to it: dropping it made a vendor look unsupported.
			models, err := lister(ctx, string(v), osName)
			if err != nil {
				vm.Error = err.Error()
				vm.Partial = catalog.IsPartial(err)
			}
			if models != nil {
				vm.Models = models
			}
			out[i] = vm
		}(i, v)
	}
	wg.Wait()
	writeJSON(w, 200, map[string]any{"os": osName, "vendors": out})
}

// handleQuit stops the server (the app's clean exit).
func (s *Server) handleQuit(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]string{"ok": "1"})
	go func() {
		time.Sleep(150 * time.Millisecond)
		if s.quit != nil {
			s.quit()
		}
	}()
}

// buildProgressJob wraps compose progress into job events.
func progressFor(job *jobs.Job) func(stage string, done, total int64) {
	return func(stage string, done, total int64) { job.Progress(stage, done, total) }
}

// ── flash ────────────────────────────────────────────────────────────────────

func (s *Server) handleFlash(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Recipe   string `json:"recipe,omitempty"`
		Artifact string `json:"artifact,omitempty"`
		DeviceID string `json:"device_id"`
		Confirm  string `json:"confirm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.DeviceID == "" {
		httpErr(w, 400, "body must include device_id, confirm, and recipe or artifact")
		return
	}
	if (req.Recipe == "") == (req.Artifact == "") {
		httpErr(w, 400, "exactly one of recipe or artifact is required")
		return
	}
	// A payload is a program, not a disk image. Written raw it would wipe the
	// stick and leave nothing Windows can read. The CLI has always refused it;
	// the page could still reach it by choosing a payload's file as "your own
	// ISO". Refused before any device is looked at.
	if req.Artifact != "" {
		if a, err := compose.LoadArtifact(compose.MetaPath(req.Artifact)); err == nil && a.Kind == "payload" {
			httpErr(w, 400, "%s is a payload, not a disk image — use Write to USB… on the Payload screen, which puts it on the drive as a file", filepath.Base(req.Artifact))
			return
		}
	}

	// Re-enumerate NOW; never trust a stale listing for a destructive op.
	dev, err := s.findDevice(r.Context(), req.DeviceID)
	if err != nil {
		httpErr(w, 400, "%v", err)
		return
	}
	if !dev.Flashable() {
		httpErr(w, 400, "%s is not flashable (bus=%s, system=%v)", dev.ID, dev.Bus, dev.System)
		return
	}
	// Server-side arm check: the typed size must match.
	if strings.TrimSpace(req.Confirm) != dev.SizeConfirmation() {
		httpErr(w, 400, "confirmation mismatch: device %s is %s GiB — type exactly %q to arm",
			dev.ID, dev.SizeConfirmation(), dev.SizeConfirmation())
		return
	}

	title := req.Recipe
	if title == "" {
		title = filepath.Base(req.Artifact)
	}
	job := s.Reg.New("flash", fmt.Sprintf("%s → %s", title, dev.ID))
	go func() {
		s.deviceMu.Lock()
		defer s.deviceMu.Unlock()
		var art *compose.Artifact
		var err error
		if req.Artifact != "" {
			// Any image the operator points at, not only one this tool built:
			// same resolution the CLI uses, so the two cannot disagree about
			// compression or the minimum stick size.
			art, _, err = compose.ResolveImage(req.Artifact)
		} else {
			art, err = s.build(context.Background(), req.Recipe, false, progressFor(job))
		}
		if err != nil {
			job.Fail(err)
			return
		}
		if err := flashrun.RunFlash(context.Background(), art, dev, progressFor(job)); err != nil {
			job.Fail(err)
			return
		}
		job.Finish("flashed and verified — safe to remove")
	}()
	writeJSON(w, 202, map[string]string{"job_id": job.ID})
}

// installRequest is the Quick Install options as the page sends them, shared
// by installing, setting up an image, downloading, and saving a recipe.
type installRequest struct {
	OSID              string   `json:"os_id"`
	Edition           string   `json:"edition"`
	AccountMode       string   `json:"account_mode"`
	Debloat           string   `json:"debloat"`
	BypassRequirement bool     `json:"bypass_requirement"`
	Drivers           bool     `json:"drivers"`
	DriversFor        string   `json:"drivers_for"`
	Apps              []string `json:"apps"`
	ISO               string   `json:"iso"`
	// DomainBlob is a file from `djoin /provision`: an offline domain join
	// for one computer.
	DomainBlob string `json:"domain_blob"`
	// Mode is "install" (build and write to device_id, the default), "setup"
	// (build the image and keep it, no stick), or "download" (fetch the OS
	// image only).
	Mode     string `json:"mode"`
	DeviceID string `json:"device_id"`
	Confirm  string `json:"confirm"`
	// Name is the recipe name, for /api/recipes/save, or the payload's name.
	Name string `json:"name"`
	// Replaces is a payload being edited: once the new one is built, the old
	// one is removed, so editing a payload does not leave two behind.
	Replaces string `json:"replaces"`
}

// installOptions validates a request's options and turns them into what the
// catalog builds from. It detects this machine's hardware when drivers are
// asked for, so it can take a moment.
func (s *Server) installOptions(ctx context.Context, req installRequest) (oscatalog.Entry, oscatalog.Options, error) {
	e, ok := oscatalog.Get(req.OSID)
	if !ok {
		return e, oscatalog.Options{}, fmt.Errorf("unknown OS %q", req.OSID)
	}
	var hw []recipe.HardwareSpec
	// "Drivers for this computer" is two jobs under one name. On Windows it
	// detects this machine and stages its packs. On Ubuntu there is nothing to
	// stage — the kernel carries all of it but the proprietary drivers — so
	// the answers ask Ubuntu's own installer to put those on.
	thirdParty := req.Drivers && e.ThirdPartyDriversSupported()
	if req.Drivers && e.Family == oscatalog.Windows {
		h, err := hwdetect.Detect(ctx)
		if err != nil {
			return e, oscatalog.Options{}, fmt.Errorf("hardware detection failed: %v", err)
		}
		if hw = driverresolve.SpecsFor(h, e.DriverOS()); len(hw) == 0 {
			return e, oscatalog.Options{}, fmt.Errorf("nothing to resolve drivers for on this machine")
		}
	}
	// Named models are additive with detection: pnputil installs only what
	// matches, so one stick can carry packs for several machines. They are
	// kept apart from what was detected because they are treated differently
	// when a pack cannot be found -- see oscatalog.Options.
	var models []recipe.HardwareSpec
	for _, spec := range strings.Split(req.DriversFor, ",") {
		if strings.TrimSpace(spec) == "" {
			continue
		}
		if e.Family != oscatalog.Windows {
			return e, oscatalog.Options{}, fmt.Errorf("drivers for a named model is a Windows option")
		}
		h, err := driverresolve.SpecForModel(spec, e.DriverOS())
		if err != nil {
			return e, oscatalog.Options{}, err
		}
		models = append(models, h)
	}
	if err := oscatalog.CheckPrograms(e, req.Apps); err != nil {
		return e, oscatalog.Options{}, err
	}
	blob := strings.TrimSpace(req.DomainBlob)
	if blob != "" {
		if e.Family != oscatalog.Windows {
			return e, oscatalog.Options{}, fmt.Errorf("joining a domain is a Windows option")
		}
		if err := compose.CheckODJBlob(blob); err != nil {
			return e, oscatalog.Options{}, fmt.Errorf("domain join file: %w", err)
		}
	}
	return e, oscatalog.Options{
		Edition: req.Edition, AccountMode: req.AccountMode,
		Debloat: req.Debloat, BypassRequirement: req.BypassRequirement,
		Hardware: hw, Models: models, Apps: req.Apps, ThirdPartyDrivers: thirdParty,
		DomainBlob: blob,
	}, nil
}

// handleInstall is Quick Install: build media for a catalog OS + options and
// flash it to the chosen stick — no workspace required. With mode "setup" it
// builds the image and keeps it to write later, and with mode "download" it
// only fetches the OS; neither needs a stick plugged in.
func (s *Server) handleInstall(w http.ResponseWriter, r *http.Request) {
	var req installRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.OSID == "" {
		httpErr(w, 400, "body must include os_id")
		return
	}
	mode := req.Mode
	if mode == "" {
		mode = "install"
	}
	if mode != "install" && mode != "setup" && mode != "download" && mode != "payload" {
		httpErr(w, 400, "mode must be install, setup, download or payload")
		return
	}
	e, ok := oscatalog.Get(req.OSID)
	if !ok {
		httpErr(w, 400, "unknown OS %q", req.OSID)
		return
	}
	var dev device.Device
	if mode == "install" {
		if req.DeviceID == "" {
			httpErr(w, 400, "installing needs device_id and confirm — or set up the image now and write it later")
			return
		}
		var err error
		if dev, err = s.findDevice(r.Context(), req.DeviceID); err != nil {
			httpErr(w, 400, "%v", err)
			return
		}
		if !dev.Flashable() {
			httpErr(w, 400, "%s is not flashable (bus=%s, system=%v)", dev.ID, dev.Bus, dev.System)
			return
		}
		if strings.TrimSpace(req.Confirm) != dev.SizeConfirmation() {
			httpErr(w, 400, "confirmation mismatch: device %s is %s GiB — type exactly %q to arm",
				dev.ID, dev.SizeConfirmation(), dev.SizeConfirmation())
			return
		}
	}
	// Refused before anything starts: an import-only entry has nothing to
	// download, so no later step can make up for a missing path.
	iso := strings.TrimSpace(req.ISO)
	if e.ImportOnly() && iso == "" && !oscatalog.InLibrary(s.Lib, e) {
		httpErr(w, 400, "%v", e.ImportOnlyError())
		return
	}
	if iso != "" && !oscatalog.InLibrary(s.Lib, e) {
		if err := oscatalog.CheckISO(iso); err != nil {
			httpErr(w, 400, "%v", err)
			return
		}
	}
	if mode == "payload" && e.Family != oscatalog.Windows {
		httpErr(w, 400, "%s is not Windows; a payload sets up programs on a machine that already runs Windows", e.Name)
		return
	}
	var replaces string
	if mode == "payload" && strings.TrimSpace(req.Replaces) != "" {
		old, err := s.libraryArtifact(req.Replaces)
		if err != nil {
			httpErr(w, 400, "%v", err)
			return
		}
		replaces = old
	}
	var opts oscatalog.Options
	if mode != "download" {
		var err error
		if _, opts, err = s.installOptions(r.Context(), req); err != nil {
			httpErr(w, 400, "%v", err)
			return
		}
	}

	var job *jobs.Job
	switch mode {
	case "install":
		job = s.Reg.New("install", fmt.Sprintf("%s → %s", e.Name, dev.ID))
	case "setup":
		job = s.Reg.New("setup", "set up "+e.Name)
	case "payload":
		job = s.Reg.New("build", "payload: programs and drivers")
	default:
		job = s.Reg.New("download", "download "+e.Name)
	}
	go func() {
		progress := progressFor(job)
		// An ISO the operator downloaded themselves, filed first so the
		// Microsoft fetch (rate-limited to about one a day per address) is
		// skipped. In the job rather than the request: a Windows ISO is read
		// twice to import, and that is minutes of a request hanging.
		if iso != "" && !oscatalog.InLibrary(s.Lib, e) {
			if _, err := oscatalog.ImportISO(s.Lib, e, iso, progress); err != nil {
				job.Fail(fmt.Errorf("importing %s: %w", filepath.Base(iso), err))
				return
			}
		}
		switch mode {
		case "download":
			if err := oscatalog.Fetch(context.Background(), s.Lib, e, progress); err != nil {
				job.Fail(err)
				return
			}
			job.Finish(e.Name + " is downloaded — setting up or installing it will not download it again")
		case "payload":
			art, err := oscatalog.BuildQuickPayload(context.Background(), s.Lib, e, opts, progress)
			if err != nil {
				job.Fail(err)
				return
			}
			// Kept beside the file: the name it is listed by, and the
			// options it was built from, which is what editing starts with.
			art.Name = strings.TrimSpace(req.Name)
			art.Choices, _ = json.Marshal(payloadChoices{Apps: req.Apps, DriversFor: req.DriversFor, Debloat: req.Debloat})
			if err := art.Save(); err != nil {
				job.Fail(err)
				return
			}
			// The old one goes only once the new one exists, and never when
			// the edit changed nothing that is built -- a rename, say -- in
			// which case they are the same file.
			if replaces != "" && !sameFile(replaces, art.Path) {
				_ = os.Remove(replaces)
				_ = os.Remove(compose.MetaPath(replaces))
			}
			label := art.Name
			if label == "" {
				label = filepath.Base(art.Path)
			}
			job.Finish("the payload is in Built payloads — write it to a USB drive, or save a copy: " + label)
		case "setup":
			art, err := oscatalog.BuildQuick(context.Background(), s.Lib, e, opts, progress)
			if err != nil {
				job.Fail(err)
				return
			}
			if strings.HasPrefix(art.Path, s.Lib.ArtifactsDir()) {
				job.Finish("ready to write — it is in Built images")
			} else {
				// Plain ISOs and disk images are written as they are: the
				// download is the image, and there is nothing else to build.
				job.Finish(e.Name + " is ready to write — nothing more to build for it")
			}
		default:
			s.deviceMu.Lock()
			defer s.deviceMu.Unlock()
			art, err := oscatalog.BuildQuick(context.Background(), s.Lib, e, opts, progress)
			if err != nil {
				job.Fail(err)
				return
			}
			if err := flashrun.RunFlash(context.Background(), art, dev, progress); err != nil {
				job.Fail(err)
				return
			}
			job.Finish("installed — safe to remove and boot the target machine")
		}
	}()
	writeJSON(w, 202, map[string]string{"job_id": job.ID})
}

// handleSaveRecipe saves Quick Install options as a named recipe in the open
// workspace, creating one first if none is open — a recipe has to live
// somewhere that can be backed up, shared and opened again, and asking
// somebody to understand workspaces before they can save their settings is
// backwards.
func (s *Server) handleSaveRecipe(w http.ResponseWriter, r *http.Request) {
	var req installRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.OSID == "" {
		httpErr(w, 400, "body must include os_id and name")
		return
	}
	name := strings.TrimSpace(req.Name)
	id := slugify(name)
	if name == "" || id == "" {
		httpErr(w, 400, "the recipe needs a name")
		return
	}
	// A join file is one computer's account, usable once, so a recipe that
	// carried it would join every later machine as that one computer.
	if strings.TrimSpace(req.DomainBlob) != "" {
		httpErr(w, 400, "a domain join file is for one computer, so it is not saved into a recipe — set up or install that computer's stick directly, or leave the domain join off to save the recipe")
		return
	}
	e, opts, err := s.installOptions(r.Context(), req)
	if err != nil {
		httpErr(w, 400, "%v", err)
		return
	}
	created := ""
	ws := s.workspace()
	if ws == nil {
		dir, isNew, err := s.defaultWorkspace()
		if err != nil {
			httpErr(w, 500, "%v", err)
			return
		}
		if isNew {
			created = dir
		}
		ws = s.workspace()
	}
	if _, err := os.Stat(filepath.Join(ws.Dir, "recipes", id+".yaml")); err == nil {
		httpErr(w, 400, "a recipe called %s already exists in this workspace — choose another name", id)
		return
	}
	iso := strings.TrimSpace(req.ISO)
	if iso != "" && !oscatalog.InLibrary(s.Lib, e) {
		if err := oscatalog.CheckISO(iso); err != nil {
			httpErr(w, 400, "%v", err)
			return
		}
	}
	job := s.Reg.New("recipe", "save recipe "+name)
	go func() {
		// A recipe for an OS that only arrives as your own ISO is useless
		// without that ISO, so it is filed now rather than asked for again.
		if iso != "" && !oscatalog.InLibrary(s.Lib, e) {
			if _, err := oscatalog.ImportISO(s.Lib, e, iso, progressFor(job)); err != nil {
				job.Fail(fmt.Errorf("importing %s: %w", filepath.Base(iso), err))
				return
			}
		}
		path, err := oscatalog.SaveRecipe(context.Background(), s.Lib, ws.Dir, id, name, e, opts, progressFor(job))
		if err != nil {
			job.Fail(err)
			return
		}
		msg := "saved to " + path
		if created != "" {
			msg += " — in a new workspace, " + created
		}
		job.Finish(msg)
	}()
	writeJSON(w, 202, map[string]string{"job_id": job.ID, "workspace_created": created})
}

// defaultWorkspace opens the workspace a recipe is saved into when none is
// open: the one New workspace would suggest, created if it does not exist yet.
func (s *Server) defaultWorkspace() (dir string, created bool, err error) {
	parent := suggestWorkspaceParent()
	org := suggestOrgName()
	dir = filepath.Join(parent, slugify(org)+"-workspace")
	if _, statErr := os.Stat(filepath.Join(dir, "workspace.yaml")); statErr != nil {
		if _, statErr := os.Stat(dir); statErr == nil {
			return "", false, fmt.Errorf("%s exists but is not a workspace — open or create a workspace first", dir)
		}
		if err := os.MkdirAll(parent, 0o755); err != nil {
			return "", false, fmt.Errorf("cannot use %s: %v", parent, err)
		}
		if err := workspace.ScaffoldEmpty(dir, org); err != nil {
			return "", false, err
		}
		created = true
	}
	if err := s.SetWorkspaceDir(dir); err != nil {
		return "", false, err
	}
	return dir, created, nil
}

// handleDeleteArtifact removes a built image and its description. Only files
// in the library's artifacts directory: the path comes from the page, and the
// page is not trusted to name anything else on the disk.
// isArtifactFile reports whether a path is one of the things a build
// produces: a disk image, or a payload -- which is one file that both runs
// and unzips, and was a plain .zip before that.
func isArtifactFile(p string) bool {
	return strings.HasSuffix(p, ".img") || strings.HasSuffix(p, ".exe") || strings.HasSuffix(p, ".zip")
}

// libraryArtifact checks that a path names a payload this library built, and
// returns it cleaned. The path comes from the page, and the page is not
// trusted to name anything else on the disk.
func (s *Server) libraryArtifact(path string) (string, error) {
	dir, _ := filepath.Abs(s.Lib.ArtifactsDir())
	p, err := filepath.Abs(path)
	if err != nil || filepath.Dir(p) != dir || !isArtifactFile(p) {
		return "", fmt.Errorf("%s is not something this library built", path)
	}
	a, err := compose.LoadArtifact(compose.MetaPath(p))
	if err != nil || a.Kind != "payload" {
		return "", fmt.Errorf("%s is not a payload", filepath.Base(path))
	}
	return p, nil
}

// stickLabel is the volume name a payload stick gets: FAT allows eleven
// characters, and this is what somebody sees in Explorer at the customer's
// machine.
const stickLabel = "DSKYPAYLOAD"

// stickFileName is what the payload is called on the stick: its name, if it
// has one, so the file at the customer's machine says which payload it is,
// rather than a recipe id and a hash.
func stickFileName(name string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == ' ', r == '-', r == '_', r == '(', r == ')', r == '&', r == ',':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	base := strings.TrimRight(strings.TrimSpace(b.String()), ". -")
	if len(base) > 60 {
		base = strings.TrimRight(base[:60], ". -")
	}
	if base == "" {
		return "DSKY payload.exe"
	}
	return base + ".exe"
}

// handleWritePayload writes a payload to a USB drive: the stick is erased and
// becomes one FAT32 volume holding the payload and nothing else.
//
// It goes through the same write as every stick DSKY makes -- a disk image,
// written, then read back and compared -- rather than formatting the stick and
// copying a file onto it. That would need the stick mounted afterwards, which
// is a different job on each of three operating systems and one this tool
// does not otherwise do; the image works the same everywhere, and the readback
// says the file on the stick is the file that was built. Anyone who wants the
// payload on a stick without erasing what is on it saves a copy to it.
func (s *Server) handleWritePayload(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path     string `json:"path"`
		DeviceID string `json:"device_id"`
		Confirm  string `json:"confirm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Path == "" || req.DeviceID == "" {
		httpErr(w, 400, "body must include path, device_id and confirm")
		return
	}
	src, err := s.libraryArtifact(req.Path)
	if err != nil {
		httpErr(w, 400, "%v", err)
		return
	}
	art, err := compose.LoadArtifact(compose.MetaPath(src))
	if err != nil {
		httpErr(w, 400, "%v", err)
		return
	}
	// Re-enumerate now; never trust a stale listing for a destructive op.
	dev, err := s.findDevice(r.Context(), req.DeviceID)
	if err != nil {
		httpErr(w, 400, "%v", err)
		return
	}
	if !dev.Flashable() {
		httpErr(w, 400, "%s is not flashable (bus=%s, system=%v)", dev.ID, dev.Bus, dev.System)
		return
	}
	if strings.TrimSpace(req.Confirm) != dev.SizeConfirmation() {
		httpErr(w, 400, "confirmation mismatch: device %s is %s GiB — type exactly %q to arm",
			dev.ID, dev.SizeConfirmation(), dev.SizeConfirmation())
		return
	}
	name := stickFileName(art.Name)
	job := s.Reg.New("flash", fmt.Sprintf("payload %s → %s", name, dev.ID))
	go func() {
		s.deviceMu.Lock()
		defer s.deviceMu.Unlock()
		progress := progressFor(job)
		img, err := buildPayloadStick(s.Lib.TmpDir(), src, name, progress)
		if img != "" {
			defer os.Remove(img)
		}
		if err != nil {
			job.Fail(err)
			return
		}
		stick, _, err := compose.ResolveImage(img)
		if err != nil {
			job.Fail(err)
			return
		}
		if err := flashrun.RunFlash(context.Background(), stick, dev, progress); err != nil {
			job.Fail(err)
			return
		}
		job.Finish("written and verified — " + name + " is on the stick; safe to remove")
	}()
	writeJSON(w, 202, map[string]string{"job_id": job.ID})
}

// buildPayloadStick lays out the stick as an image: one FAT32 volume, sized
// for the payload with room to spare rather than for the whole stick, so a
// small payload writes in seconds instead of writing gigabytes of nothing.
// Disk utility's Erase and prepare gives the whole stick back.
func buildPayloadStick(tmpDir, src, name string, progress func(string, int64, int64)) (string, error) {
	st, err := os.Stat(src)
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp(tmpDir, "payload-stick-*.img")
	if err != nil {
		return "", err
	}
	img := f.Name()
	f.Close()
	stage := fsimg.StageMap{}
	stage.AddFile(src, "/"+name)
	opts := fsimg.Options{
		Scheme: fsimg.SchemeMBR, Label: stickLabel,
		SizeBytes: fsimg.SizeForContent(st.Size(), 1),
	}
	if err := fsimg.BuildStaged(img, opts, stage, func(done, total int64) {
		progress("laying out the stick", done, total)
	}); err != nil {
		return img, err
	}
	return img, nil
}

// handleCopyArtifact copies a built payload somewhere the operator chose: a
// stick, a share, a folder on this machine. A payload is carried to a machine
// rather than written to one, so copying it is the useful act -- and doing it
// here means the page can offer it without asking anybody to find the library
// on disk.
func (s *Server) handleCopyArtifact(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
		To   string `json:"to"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Path == "" || req.To == "" {
		httpErr(w, 400, "body must be {\"path\": \"<artifact>\", \"to\": \"<folder>\"}")
		return
	}
	dir, _ := filepath.Abs(s.Lib.ArtifactsDir())
	src, err := filepath.Abs(req.Path)
	if err != nil || filepath.Dir(src) != dir || !isArtifactFile(src) {
		httpErr(w, 400, "%s is not something this library built", req.Path)
		return
	}
	st, err := os.Stat(req.To)
	if err != nil || !st.IsDir() {
		httpErr(w, 400, "%s is not a folder on this machine", req.To)
		return
	}
	// A named payload is saved under its name: a backup folder of
	// "windows-11-payload-249016a39fc58f54.exe" files says nothing about which
	// customer each one is for. Saving the same payload again replaces the
	// older copy, which is what a backup wants.
	base := filepath.Base(src)
	if a, err := compose.LoadArtifact(compose.MetaPath(src)); err == nil && a.Kind == "payload" && strings.TrimSpace(a.Name) != "" {
		base = stickFileName(a.Name)
	}
	dst := filepath.Join(req.To, base)
	if sameFile(src, dst) {
		httpErr(w, 400, "that is where it already is")
		return
	}
	job := s.Reg.New("copy", "copy "+filepath.Base(src))
	go func() {
		if err := copyArtifactFile(src, dst, progressFor(job)); err != nil {
			job.Fail(err)
			return
		}
		job.Finish("copied to " + dst)
	}()
	writeJSON(w, 202, map[string]string{"job_id": job.ID})
}

// sameFile reports whether two paths are the same file, so a copy onto itself
// cannot truncate the only copy.
func sameFile(a, b string) bool {
	sa, err1 := os.Stat(a)
	sb, err2 := os.Stat(b)
	return err1 == nil && err2 == nil && os.SameFile(sa, sb)
}

// copyArtifactFile copies with progress and lands the whole file or none of
// it: a half-copied payload on a stick is one somebody would carry to a
// machine and run.
func copyArtifactFile(src, dst string, progress func(string, int64, int64)) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	tmp := dst + ".partial"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	stage := "copying " + filepath.Base(src)
	progress(stage, 0, st.Size())
	buf := make([]byte, 4<<20)
	var done int64
	for {
		n, rerr := in.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				out.Close()
				os.Remove(tmp)
				return werr
			}
			done += int64(n)
			progress(stage, done, st.Size())
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			out.Close()
			os.Remove(tmp)
			return rerr
		}
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

func (s *Server) handleDeleteArtifact(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Path == "" {
		httpErr(w, 400, "body must be {\"path\": \"<artifact>\"}")
		return
	}
	dir, _ := filepath.Abs(s.Lib.ArtifactsDir())
	p, err := filepath.Abs(req.Path)
	if err != nil || filepath.Dir(p) != dir || !isArtifactFile(p) {
		httpErr(w, 400, "%s is not something this library built", req.Path)
		return
	}
	if s.Reg.BusyExcept("") {
		httpErr(w, 409, "something is running — delete it when the activity has finished")
		return
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		httpErr(w, 500, "%v", err)
		return
	}
	_ = os.Remove(compose.MetaPath(p))
	writeJSON(w, 200, map[string]string{"ok": "1"})
}

// handleUpdateCheck reports whether a newer release exists. The result is
// cached: the page asks on load, and hitting the release feed every time
// would be rude to it and slow for no gain.
func (s *Server) handleUpdateCheck(w http.ResponseWriter, r *http.Request) {
	// An explicit "check now" must actually check. The hour-long cache is
	// right for the automatic check on page load, but returning a stale answer
	// to someone who just pressed the button would look broken — especially
	// right after a release, which is exactly when they press it.
	force := r.URL.Query().Get("force") != ""

	s.updMu.Lock()
	fresh := !force && s.updAt.After(time.Now().Add(-time.Hour))
	rel := s.updRel
	s.updMu.Unlock()

	if !fresh {
		got, err := selfupdate.Check(r.Context())
		if err != nil {
			// Offline is the normal case for a tool used on a bench; say so
			// quietly rather than making the page look broken.
			writeJSON(w, 200, map[string]any{"current": buildinfo.Version, "unavailable": err.Error()})
			return
		}
		s.updMu.Lock()
		s.updRel, s.updAt = got, time.Now()
		s.updMu.Unlock()
		rel = got
	}
	writeJSON(w, 200, map[string]any{
		"current": buildinfo.Version,
		"latest":  rel.Version,
		"newer":   rel.Newer,
		"asset":   rel.Asset,
		"size_mb": rel.Size >> 20,
	})
}

// handleUpdateApply installs the newest release over this binary. The server
// keeps running the old image until it is restarted — a process cannot swap
// itself out mid-flight — so the page says so rather than implying otherwise.
func (s *Server) handleUpdateApply(w http.ResponseWriter, r *http.Request) {
	rel, err := selfupdate.Check(r.Context())
	if err != nil {
		httpErr(w, 502, "%v", err)
		return
	}
	if !rel.Newer {
		httpErr(w, 400, "%s is already the newest release", rel.Version)
		return
	}
	job := s.Reg.New("update", "update to "+rel.Version)
	go func() {
		path, err := selfupdate.Apply(context.Background(), rel, func(done, total int64) {
			progressFor(job)("downloading "+rel.Asset, done, total)
		})
		if err != nil {
			job.Fail(err)
			return
		}
		// Nothing else may be running. A flash writes through a separate
		// elevated worker and would survive this process going away, but a
		// build would not — and restarting out from under somebody mid-job to
		// save them a double-click is not a trade worth making.
		if s.OnUpdated == nil || s.Reg.BusyExcept(job.ID) {
			job.Finish("updated " + path + " to " + rel.Version + " — restart to run it")
			return
		}
		job.Finish("updated to " + rel.Version + " — restarting")
		// The page hears about this over the event stream, so the restart
		// waits long enough for that to arrive. Otherwise the window vanishes
		// with no explanation and comes back looking like nothing happened.
		time.AfterFunc(1200*time.Millisecond, s.OnUpdated)
	}()
	writeJSON(w, 202, map[string]string{"job_id": job.ID})
}

// handleBrowse opens the host's own folder chooser and returns what was
// picked. The browser cannot produce an absolute path itself, and the server
// is on the same machine as the person clicking, so it asks the desktop.
// handleBrowseImage asks the desktop for an image file. A browser cannot hand
// a server an absolute path, and this tool needs one — it streams the file
// from disk rather than through an upload, because these are several
// gigabytes and copying them through the browser would be pure waste.
func (s *Server) handleBrowseImage(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	path, err := filepicker.PickImage(ctx, "Select an ISO or disk image")
	switch {
	case errors.Is(err, filepicker.ErrCancelled):
		writeJSON(w, 200, map[string]string{"path": ""})
		return
	case errors.Is(err, filepicker.ErrUnavailable):
		httpErr(w, 501, "no file chooser on this host — type the path instead")
		return
	case err != nil:
		httpErr(w, 500, "%v", err)
		return
	}
	// Described here rather than at flash time so the page can show the size
	// and whether it is compressed before anyone arms a device.
	art, composed, err := compose.ResolveImage(path)
	if err != nil {
		httpErr(w, 400, "%v", err)
		return
	}
	writeJSON(w, 200, map[string]any{
		"path": path, "size_mb": art.Size >> 20,
		"compress": art.Compress, "composed": composed,
	})
}

// customApp is one operator-supplied installer as the page shows it.
type customApp struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Category string `json:"category,omitempty"`
	Filename string `json:"filename"`
	Format   string `json:"format"`
	Args     string `json:"args"`
	SizeMB   int64  `json:"size_mb"`
	SHA256   string `json:"sha256"`
	Added    string `json:"added"`
	// RunLine is the command the first-boot script will run. Shown because a
	// silent switch that is wrong leaves a machine sitting on an installer
	// dialog, and nobody finds out until they walk up to it.
	RunLine string `json:"run_line"`
}

func customAppOf(c appcatalog.Custom) customApp {
	return customApp{
		ID: c.ID, Name: c.Name, Category: c.Category,
		Filename: c.Filename, Format: c.Format,
		Args: strings.Join(c.Args, " "), SizeMB: c.Size >> 20,
		SHA256: c.SHA256, Added: c.AddedAt.Format("2006-01-02"),
		RunLine: c.RunLine(),
	}
}

// handleWingetLookup confirms a typed winget package id exists before the
// picker accepts it, and returns the spelling winget's own list uses. The
// server asks GitHub, not the page: the page talks only to this server.
func (s *Server) handleWingetLookup(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	canon, err := appcatalog.LookupWinget(r.Context(), id)
	switch {
	case err == nil:
		writeJSON(w, 200, map[string]any{"id": canon, "checked": true})
	case errors.Is(err, appcatalog.ErrWingetNotFound):
		httpErr(w, 404, "winget has no package %s. Package ids look like Publisher.Package; winget search on a Windows PC shows them.", id)
	case errors.Is(err, appcatalog.ErrWingetUnchecked):
		// Offline or rate-limited: the id is well formed, so let the person
		// decide, and say it was not confirmed.
		writeJSON(w, 200, map[string]any{"id": id, "checked": false, "note": err.Error()})
	default:
		httpErr(w, 400, "%s", err.Error())
	}
}

func (s *Server) handleApps(w http.ResponseWriter, r *http.Request) {
	out := []customApp{}
	for _, c := range appcatalog.CustomApps() {
		out = append(out, customAppOf(c))
	}
	writeJSON(w, 200, map[string]any{"apps": out})
}

// handleAppAdd takes an installer from a path on this machine. The path comes
// from the desktop's own chooser (or is typed), never an upload: the server is
// on the same machine, and pushing an installer through the browser to reach
// it would be pure waste.
func (s *Server) handleAppAdd(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path     string `json:"path"`
		ID       string `json:"id"`
		Name     string `json:"name"`
		Args     string `json:"args"`
		Category string `json:"category"`
		Replace  bool   `json:"replace"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Path) == "" {
		httpErr(w, 400, "body must include the path to a .msi or .exe")
		return
	}
	c, err := appcatalog.AddInstaller(s.Lib.Root, s.Lib, appcatalog.Installer{
		Path:    strings.Trim(strings.TrimSpace(req.Path), `"'`),
		ID:      strings.TrimSpace(req.ID),
		Name:    strings.TrimSpace(req.Name),
		Args:    req.Args,
		Replace: req.Replace,
	}, nil)
	if err != nil {
		httpErr(w, 400, "%v", err)
		return
	}
	writeJSON(w, 200, customAppOf(c))
}

func (s *Server) handleAppSet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID   string  `json:"id"`
		Name *string `json:"name,omitempty"`
		Args *string `json:"args,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID == "" {
		httpErr(w, 400, "body must include id")
		return
	}
	// Pointers, so a field the page did not send is left alone — and "" stays
	// a legitimate value for args, meaning "no switches".
	c, err := appcatalog.UpdateCustom(s.Lib.Root, req.ID, func(c *appcatalog.Custom) {
		if req.Name != nil {
			c.Name = strings.TrimSpace(*req.Name)
		}
		if req.Args != nil {
			c.Args = appcatalog.SplitArgs(*req.Args)
		}
	})
	if err != nil {
		httpErr(w, 400, "%v", err)
		return
	}
	writeJSON(w, 200, customAppOf(c))
}

func (s *Server) handleAppRemove(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID == "" {
		httpErr(w, 400, "body must include id")
		return
	}
	c, err := appcatalog.RemoveCustom(s.Lib.Root, req.ID)
	if err != nil {
		httpErr(w, 400, "%v", err)
		return
	}
	writeJSON(w, 200, map[string]string{"id": c.ID, "name": c.Name, "filename": c.Filename})
}

// handleBrowseDjoin asks the desktop for a `djoin /provision` file and checks
// it is one before the dialog accepts it.
func (s *Server) handleBrowseDjoin(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	path, err := filepicker.PickFile(ctx, "Select the file from djoin /provision", "Domain join files", []string{"txt", "djoin", "blob"})
	switch {
	case errors.Is(err, filepicker.ErrCancelled):
		writeJSON(w, 200, map[string]string{"path": ""})
		return
	case errors.Is(err, filepicker.ErrUnavailable):
		httpErr(w, 501, "no file chooser on this host — type the path instead")
		return
	case err != nil:
		httpErr(w, 500, "%v", err)
		return
	}
	if err := compose.CheckODJBlob(path); err != nil {
		httpErr(w, 400, "%v", err)
		return
	}
	writeJSON(w, 200, map[string]string{"path": path})
}

// handleBrowseInstaller asks the desktop for a .msi or .exe and offers back
// the defaults the add form should start from, so the common case is one
// click and a confirm.
func (s *Server) handleBrowseInstaller(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	path, err := filepicker.PickInstaller(ctx, "Select an installer (.msi or .exe)")
	switch {
	case errors.Is(err, filepicker.ErrCancelled):
		writeJSON(w, 200, map[string]string{"path": ""})
		return
	case errors.Is(err, filepicker.ErrUnavailable):
		httpErr(w, 501, "no file chooser on this host — type the path instead")
		return
	case err != nil:
		httpErr(w, 500, "%v", err)
		return
	}
	format, ferr := appcatalog.FormatForFile(path)
	if ferr != nil {
		httpErr(w, 400, "%v", ferr)
		return
	}
	writeJSON(w, 200, map[string]any{
		"path": path, "id": appcatalog.DeriveID(path),
		"name": filepath.Base(path), "format": format,
	})
}

func (s *Server) handleBrowse(w http.ResponseWriter, r *http.Request) {
	// Generous: this blocks while a person reads a dialog.
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	path, err := filepicker.PickFolder(ctx, "Select a workspace folder")
	switch {
	case errors.Is(err, filepicker.ErrCancelled):
		writeJSON(w, 200, map[string]string{"path": ""})
		return
	case errors.Is(err, filepicker.ErrUnavailable):
		httpErr(w, 501, "no folder chooser on this host — type the path instead")
		return
	case err != nil:
		httpErr(w, 500, "%v", err)
		return
	}
	writeJSON(w, 200, map[string]string{"path": path})
}

// handleWorkspaceDefaults suggests a name and a place, so creating one is a
// button rather than an interrogation. Both stay editable: the point is to
// remove the blank page, not the choice.
func (s *Server) handleWorkspaceDefaults(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]string{
		"org":    suggestOrgName(),
		"parent": suggestWorkspaceParent(),
	})
}

// suggestOrgName prefers the machine name: on a work bench it is usually the
// shop's name already, and it is a better guess than "Default".
func suggestOrgName() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	if u := os.Getenv("USERNAME"); u != "" {
		return u
	}
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return "My Workspace"
}

// suggestWorkspaceParent picks somewhere the person will actually find it
// again — never an app-data folder, which is where files go to be forgotten
// and never committed.
func suggestWorkspaceParent() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	for _, candidate := range []string{"Documents", "documents"} {
		p := filepath.Join(home, candidate)
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			return filepath.Join(p, "DSKY Workspaces")
		}
	}
	return filepath.Join(home, "DSKY Workspaces")
}

// handleNewWorkspace scaffolds a workspace and opens it.
func (s *Server) handleNewWorkspace(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Org    string `json:"org"`
		Parent string `json:"parent"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpErr(w, 400, "body must include org and parent")
		return
	}
	org := strings.TrimSpace(req.Org)
	parent := strings.TrimSpace(req.Parent)
	if org == "" {
		httpErr(w, 400, "the workspace needs a name")
		return
	}
	if parent == "" {
		parent = suggestWorkspaceParent()
	}
	// The suggested folder will not exist the first time, which is normal
	// rather than an error worth showing anyone.
	if err := os.MkdirAll(parent, 0o755); err != nil {
		httpErr(w, 400, "cannot use %s: %v", parent, err)
		return
	}
	dir := filepath.Join(parent, slugify(org)+"-workspace")
	if _, err := os.Stat(dir); err == nil {
		httpErr(w, 400, "%s already exists — open it instead of creating it again", dir)
		return
	}
	if err := workspace.Scaffold(dir, org); err != nil {
		httpErr(w, 500, "%v", err)
		return
	}
	if err := s.SetWorkspaceDir(dir); err != nil {
		httpErr(w, 500, "created %s but could not open it: %v", dir, err)
		return
	}
	writeJSON(w, 200, map[string]string{"dir": dir})
}

// slugify turns an org name into a folder-safe stem.
var notSlug = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(s string) string {
	out := notSlug.ReplaceAllString(strings.ToLower(s), "-")
	if out = strings.Trim(out, "-"); out == "" {
		return "org"
	}
	return out
}

// handleDisks reports each attached disk with its partition layout. Reading
// needs no elevation, so the page can show this without prompting anyone.
func (s *Server) handleDisks(w http.ResponseWriter, r *http.Request) {
	devs, err := device.List(r.Context())
	if err != nil {
		httpErr(w, 500, "%v", err)
		return
	}
	type partOut struct {
		Number int      `json:"number"`
		SizeGB float64  `json:"size_gb"`
		Type   string   `json:"type"`
		Label  string   `json:"label,omitempty"`
		Mounts []string `json:"mounts,omitempty"`
	}
	type diskOut struct {
		deviceInfo
		Scheme   string    `json:"scheme"`
		Parts    []partOut `json:"parts"`
		UnusedGB float64   `json:"unused_gb"`
		Notes    []string  `json:"notes,omitempty"`
		Error    string    `json:"error,omitempty"`
	}
	out := []diskOut{}
	for _, d := range devs {
		row := diskOut{deviceInfo: deviceInfoOf(d), Parts: []partOut{}}
		if d.Flashable() {
			if l, err := diskutil.Inspect(r.Context(), d); err != nil {
				row.Error = err.Error()
			} else {
				row.Scheme = l.Scheme
				row.Notes = l.Notes
				row.UnusedGB = float64(l.UnusedBytes()) / 1e9
				for _, p := range l.Parts {
					row.Parts = append(row.Parts, partOut{
						Number: p.Number, SizeGB: float64(p.Size) / 1e9,
						Type: p.Type, Label: p.Label, Mounts: p.Mounts,
					})
				}
			}
		}
		out = append(out, row)
	}
	writeJSON(w, 200, map[string]any{"disks": out})
}

// handleDiskPrepare erases a removable disk and gives it one full-size
// volume. Same interlock as a flash: the operator types the device's size.
func (s *Server) handleDiskPrepare(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DeviceID string `json:"device_id"`
		Scheme   string `json:"scheme"`
		FS       string `json:"fs"`
		Label    string `json:"label"`
		Confirm  string `json:"confirm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.DeviceID == "" {
		httpErr(w, 400, "body must include device_id and confirm")
		return
	}
	dev, err := s.findDevice(r.Context(), req.DeviceID)
	if err != nil {
		httpErr(w, 400, "%v", err)
		return
	}
	// Permission is derived from the disk named, never sent by the client: a
	// fixed disk is allowed, and asking for one is what grants it.
	if err := diskutil.Guard(dev, !dev.Routine()); err != nil {
		httpErr(w, 400, "%v", err)
		return
	}
	opts := diskutil.Options{
		Scheme:     diskutil.Scheme(strings.ToLower(req.Scheme)),
		FS:         diskutil.FS(strings.ToLower(req.FS)),
		Label:      req.Label,
		AllowFixed: !dev.Routine(),
	}
	if opts.Scheme == "" {
		opts.Scheme = diskutil.GPT
	}
	if opts.FS == "" {
		opts.FS = diskutil.ExFAT
	}
	if opts.Label == "" {
		opts.Label = "DSKY"
	}
	if err := opts.Validate(dev); err != nil {
		httpErr(w, 400, "%v", err)
		return
	}
	if strings.TrimSpace(req.Confirm) != dev.SizeConfirmation() {
		httpErr(w, 400, "confirmation mismatch: %s is %s GiB — type exactly %q to arm",
			dev.ID, dev.SizeConfirmation(), dev.SizeConfirmation())
		return
	}
	job := s.Reg.New("prepare", fmt.Sprintf("%s → %s %s", dev.ID, opts.FS, opts.Label))
	go func() {
		s.deviceMu.Lock()
		defer s.deviceMu.Unlock()
		if _, err := flashrun.RunPrepare(context.Background(), dev, opts, progressFor(job)); err != nil {
			job.Fail(err)
			return
		}
		job.Finish("prepared — one " + string(opts.FS) + " volume, safe to use")
	}()
	writeJSON(w, 202, map[string]string{"job_id": job.ID})
}

// handleDetect profiles the machine the server runs on, so the page can offer
// "include drivers for this computer" with the actual hardware named.
func (s *Server) handleDetect(w http.ResponseWriter, r *http.Request) {
	h, err := hwdetect.Detect(r.Context())
	if err != nil {
		httpErr(w, 501, "%v", err)
		return
	}
	type dev struct {
		Name  string `json:"name"`
		Brand string `json:"brand,omitempty"`
	}
	resp := struct {
		Vendor      string   `json:"vendor"`
		Model       string   `json:"model"`
		CPU         string   `json:"cpu"`
		Feed        string   `json:"feed,omitempty"`
		FeedName    string   `json:"feed_name,omitempty"`
		GPUs        []dev    `json:"gpus"`
		NICs        []dev    `json:"nics"`
		LookupIDs   []string `json:"lookup_ids"`
		DeviceCount int      `json:"device_count"`
	}{
		Vendor: h.Vendor, Model: h.Model, CPU: h.CPU, Feed: h.KnownVendor(),
		FeedName: catalog.VendorNames[catalog.Vendor(h.KnownVendor())],
		GPUs:     []dev{}, NICs: []dev{},
		LookupIDs: h.DriverHWIDs(), DeviceCount: len(h.Devices),
	}
	for _, g := range h.GPUs {
		resp.GPUs = append(resp.GPUs, dev{Name: g.Name, Brand: g.GPUVendor})
	}
	for _, n := range h.NICs {
		resp.NICs = append(resp.NICs, dev{Name: n.Name})
	}
	writeJSON(w, 200, resp)
}

func (s *Server) handleCapture(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DeviceID string `json:"device_id"`
		Confirm  string `json:"confirm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.DeviceID == "" {
		httpErr(w, 400, "body must include device_id and confirm")
		return
	}
	dev, err := s.findDevice(r.Context(), req.DeviceID)
	if err != nil {
		httpErr(w, 400, "%v", err)
		return
	}
	if dev.System {
		httpErr(w, 400, "refusing to capture the system disk")
		return
	}
	if strings.TrimSpace(req.Confirm) != dev.SizeConfirmation() {
		httpErr(w, 400, "confirmation mismatch: device is %s GiB", dev.SizeConfirmation())
		return
	}
	outPath := filepath.Join(s.Lib.ArtifactsDir(), fmt.Sprintf("clone-%s.img", time.Now().Format("20060102-150405")))
	job := s.Reg.New("clone", fmt.Sprintf("%s → %s", dev.ID, filepath.Base(outPath)))
	go func() {
		s.deviceMu.Lock()
		defer s.deviceMu.Unlock()
		result, err := flashrun.RunClone(context.Background(), dev, outPath, progressFor(job))
		if err != nil {
			job.Fail(err)
			return
		}
		job.Finish(outPath + " (" + result + ")")
	}()
	writeJSON(w, 202, map[string]string{"job_id": job.ID})
}

func (s *Server) findDevice(ctx context.Context, id string) (device.Device, error) {
	devs, err := device.List(ctx)
	if err != nil {
		return device.Device{}, err
	}
	for _, d := range devs {
		if strings.EqualFold(d.ID, id) {
			return d, nil
		}
	}
	return device.Device{}, fmt.Errorf("device %s is no longer attached — refresh and retry", id)
}

// ── events (SSE) ─────────────────────────────────────────────────────────────

// pageOpened and pageClosed bracket a live event stream, which is the only
// reliable signal that somebody is actually looking at the portal.
func (s *Server) pageOpened() {
	s.idleMu.Lock()
	defer s.idleMu.Unlock()
	s.clients++
	s.sawClient = true
	if s.idleTimer != nil {
		s.idleTimer.Stop()
		s.idleTimer = nil
	}
}

func (s *Server) pageClosed() {
	s.idleMu.Lock()
	defer s.idleMu.Unlock()
	if s.clients > 0 {
		s.clients--
	}
	if s.clients == 0 && s.IdleTimeout > 0 {
		s.idleTimer = time.AfterFunc(s.IdleTimeout, s.stopIfIdle)
	}
}

// stopIfIdle ends the session once nobody is watching and nothing is running.
//
// A reload drops the stream and reopens it a moment later, hence the delay
// before this fires at all. A job still running holds the server open however
// long it takes: a flash that outlives its browser tab has to finish, because
// a half-written stick is the worst thing this tool can leave behind.
func (s *Server) stopIfIdle() {
	s.idleMu.Lock()
	if s.clients > 0 {
		s.idleMu.Unlock()
		return
	}
	if s.Reg != nil && s.Reg.Busy() {
		// Check again later rather than giving up on stopping entirely.
		s.idleTimer = time.AfterFunc(s.IdleTimeout, s.stopIfIdle)
		s.idleMu.Unlock()
		return
	}
	s.idleMu.Unlock()
	if s.quit != nil {
		s.quit()
	}
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		httpErr(w, 500, "streaming unsupported")
		return
	}
	s.pageOpened()
	defer s.pageClosed()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	ch, cancel := s.Reg.Subscribe()
	defer cancel()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case ev, open := <-ch:
			if !open {
				return
			}
			b, _ := json.Marshal(ev)
			fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
		}
	}
}

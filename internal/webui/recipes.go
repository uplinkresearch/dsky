package webui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/uplinkresearch/dsky/internal/appcatalog"
	"github.com/uplinkresearch/dsky/internal/oscatalog"
	"github.com/uplinkresearch/dsky/internal/recipe"
)

// ── recipes: open, edit, delete ─────────────────────────────────────────────

type recipeDetail struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Path string `json:"path"`
	// Lines say what the recipe does in the install dialog's words.
	Lines [][2]string `json:"lines"`
	YAML  string      `json:"yaml"`
	// Form is the recipe as install-dialog choices, when the dialog can edit
	// it; NotEditable says why not otherwise.
	Form        *oscatalog.RecipeForm `json:"form,omitempty"`
	NotEditable string                `json:"not_editable,omitempty"`
}

func (s *Server) recipeByID(id string) (*recipe.Recipe, string, error) {
	ws := s.workspace()
	if ws == nil {
		return nil, "", errors.New("no workspace is open")
	}
	r, err := ws.Recipe(id)
	return r, ws.Dir, err
}

func (s *Server) handleRecipeGet(w http.ResponseWriter, r *http.Request) {
	rc, wsDir, err := s.recipeByID(r.URL.Query().Get("id"))
	if err != nil {
		httpErr(w, 404, "%v", err)
		return
	}
	b, _ := os.ReadFile(rc.Path)
	d := recipeDetail{ID: rc.ID, Name: rc.Name, Path: rc.Path, YAML: string(b)}
	form, err := oscatalog.FormFromRecipe(wsDir, rc)
	if err == nil {
		d.Form = &form
	} else {
		d.NotEditable = strings.TrimPrefix(err.Error(), oscatalog.ErrNotEditable.Error()+": ")
	}
	d.Lines = s.describeRecipe(rc, d.Form)
	writeJSON(w, 200, d)
}

// describeRecipe lists a recipe's settings with the labels the install dialog
// uses, so opening one reads like the dialog that made it.
func (s *Server) describeRecipe(rc *recipe.Recipe, form *oscatalog.RecipeForm) [][2]string {
	var out [][2]string
	add := func(k, v string) {
		if v != "" {
			out = append(out, [2]string{k, v})
		}
	}
	osName := rc.OS.Source
	if e, ok := oscatalog.Get(rc.OS.Source); ok {
		osName = e.Name
		if oscatalog.InLibrary(s.Lib, e) {
			osName += " (downloaded)"
		} else {
			osName += " (downloads when set up)"
		}
	}
	add("OS", osName)
	programs := func(ids []string) string {
		var names []string
		for _, id := range ids {
			if a, ok := appcatalog.Get(id); ok {
				names = append(names, a.Name)
			} else {
				names = append(names, strings.TrimPrefix(id, appcatalog.WingetPrefix))
			}
		}
		if len(names) == 0 {
			return "None"
		}
		return strings.Join(names, ", ")
	}

	if w := rc.Windows; w != nil {
		if form != nil {
			add("Edition", form.Edition)
		} else if w.EICfg != nil {
			add("Edition", w.EICfg.Edition)
		}
		if w.Unattend != nil {
			switch w.Unattend.Vars["account_mode"] {
			case "oobe":
				add("Account setup", "Normal Windows setup (OOBE)")
			case "local", "":
				add("Account setup", "Local account, no setup screens")
			default:
				add("Account setup", w.Unattend.Vars["account_mode"])
			}
			if w.Unattend.Vars["bypass_requirements"] == "1" {
				add("TPM / Secure Boot / RAM checks", "Skipped")
			} else {
				add("TPM / Secure Boot / RAM checks", "Kept")
			}
		}
		if w.Debloat.Enabled() {
			add("Remove bloatware", strings.ToUpper(w.Debloat.Preset[:1])+w.Debloat.Preset[1:])
		} else {
			add("Remove bloatware", "Keep everything")
		}
		if form != nil {
			add("Install programs", programs(form.Apps))
		} else {
			var ids []string
			if w.Apps != nil {
				for _, p := range w.Apps.Winget {
					ids = append(ids, appcatalog.WingetPrefix+p)
				}
			}
			add("Install programs", programs(ids))
		}
		var drivers []string
		for _, h := range w.Hardware {
			switch {
			case h.Vendor != "":
				drivers = append(drivers, strings.TrimSpace(h.Vendor+" "+h.Model))
			case len(h.HWIDs) > 0:
				drivers = append(drivers, fmt.Sprintf("%d devices", len(h.HWIDs)))
			}
		}
		if n := len(w.DriverPacks); n > 0 {
			drivers = append(drivers, fmt.Sprintf("%d driver packs listed in the file", n))
		}
		if len(drivers) == 0 {
			add("Drivers", "None")
		} else {
			add("Drivers", strings.Join(drivers, ", "))
		}
		if d := w.Domain; d != nil {
			switch {
			case d.Blob != "":
				add("Join a domain", "One computer, join file "+d.Blob)
			case d.Join != "":
				add("Join a domain", d.Join)
			}
		}
	} else {
		// One describer answers this for the CLI, the picker and this page. It
		// used to be worked out here from "is there an autoinstall section?",
		// so a kickstart — the one recipe shape that clears the disk without
		// asking — came out as "nothing set in advance" with its program list
		// left off the page entirely.
		p := oscatalog.RecipeInstallPlan(rc, form)
		if form != nil {
			add("Install programs", programs(form.Apps))
		}
		add("Installer", p.InstallerLine())
	}
	if ms := rc.Target.MinStick; ms != "" {
		add("Stick", "At least "+strings.Replace(ms, "GiB", " GB", 1))
	}
	return out
}

// handleRecipeUpdate saves install-dialog choices over an existing recipe.
func (s *Server) handleRecipeUpdate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		installRequest
		ID string `json:"id"`
		// KeepDrivers keeps the driver packs the recipe already has, which
		// the dialog can't show as choices.
		KeepDrivers bool `json:"keep_drivers"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID == "" || req.OSID == "" {
		httpErr(w, 400, "body must include id and os_id")
		return
	}
	rc, wsDir, err := s.recipeByID(req.ID)
	if err != nil {
		httpErr(w, 404, "%v", err)
		return
	}
	old, err := oscatalog.FormFromRecipe(wsDir, rc)
	if err != nil {
		httpErr(w, 400, "%v — change it as a file instead", err)
		return
	}
	if req.OSID != old.OSID {
		httpErr(w, 400, "a recipe keeps its OS — save a new recipe for %s", req.OSID)
		return
	}
	if strings.TrimSpace(req.DomainBlob) != "" {
		httpErr(w, 400, "a domain join file is for one computer, so it is not saved into a recipe")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = rc.Name
	}
	e, opts, err := s.installOptions(r.Context(), req.installRequest)
	if err != nil {
		httpErr(w, 400, "%v", err)
		return
	}
	if req.KeepDrivers {
		// Written through as they are, not resolved again: they are already
		// in the recipe, with their manifests in the workspace.
		opts.Kept = old.Hardware
	}
	if s.Reg.BusyExcept("") {
		httpErr(w, 409, "something is running — save the recipe when the activity has finished")
		return
	}
	job := s.Reg.New("recipe", "save recipe "+name)
	go func() {
		path, err := oscatalog.ReplaceRecipe(context.Background(), s.Lib, wsDir, rc.ID, name, e, opts, progressFor(job))
		if err != nil {
			job.Fail(err)
			return
		}
		job.Finish("saved " + path)
	}()
	writeJSON(w, 202, map[string]string{"job_id": job.ID})
}

// handleRecipeFile saves a recipe's file as edited on the page. The new text
// has to load as a recipe with the same id, and the workspace has to load
// with it, or the old file is put back: one broken recipe stops every recipe
// in the workspace from listing.
func (s *Server) handleRecipeFile(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID   string `json:"id"`
		YAML string `json:"yaml"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID == "" {
		httpErr(w, 400, "body must include id and yaml")
		return
	}
	rc, _, err := s.recipeByID(req.ID)
	if err != nil {
		httpErr(w, 404, "%v", err)
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(rc.Path), ".edit-*.yaml.tmp")
	if err != nil {
		httpErr(w, 500, "%v", err)
		return
	}
	tmp.WriteString(req.YAML)
	tmp.Close()
	defer os.Remove(tmp.Name())
	nr, err := recipe.Load(tmp.Name())
	if err != nil {
		httpErr(w, 400, "not saved: %s", strings.TrimPrefix(err.Error(), tmp.Name()+": "))
		return
	}
	if nr.ID != rc.ID {
		httpErr(w, 400, "not saved: the id must stay %s (built images and scripts refer to it)", rc.ID)
		return
	}
	for _, f := range nr.Lint() {
		if f.Severity == "error" {
			httpErr(w, 400, "not saved: %s", f.Message)
			return
		}
	}
	old, err := os.ReadFile(rc.Path)
	if err != nil {
		httpErr(w, 500, "%v", err)
		return
	}
	if err := os.WriteFile(rc.Path, []byte(req.YAML), 0o644); err != nil {
		httpErr(w, 500, "%v", err)
		return
	}
	if ws := s.workspace(); ws != nil {
		if _, err := ws.Recipe(rc.ID); err != nil {
			os.WriteFile(rc.Path, old, 0o644)
			httpErr(w, 400, "not saved: %v", err)
			return
		}
	}
	writeJSON(w, 200, map[string]string{"ok": "1"})
}

func (s *Server) handleRecipeDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID == "" {
		httpErr(w, 400, "body must be {\"id\": \"<recipe>\"}")
		return
	}
	rc, wsDir, err := s.recipeByID(req.ID)
	if err != nil {
		httpErr(w, 404, "%v", err)
		return
	}
	if s.Reg.BusyExcept("") {
		httpErr(w, 409, "something is running — delete it when the activity has finished")
		return
	}
	if err := oscatalog.DeleteRecipe(wsDir, rc); err != nil {
		httpErr(w, 500, "%v", err)
		return
	}
	writeJSON(w, 200, map[string]string{"ok": "1"})
}

// ── downloaded OS images ────────────────────────────────────────────────────

type downloadInfo struct {
	OSID     string `json:"os_id"`
	Name     string `json:"name"`
	Family   string `json:"family"`
	SizeMB   int64  `json:"size_mb"`
	Added    string `json:"added"`
	Filename string `json:"filename"`
}

func (s *Server) downloads() []downloadInfo {
	out := []downloadInfo{}
	for _, d := range oscatalog.DownloadedImages(s.Lib) {
		info := downloadInfo{
			OSID: d.Entry.ID, Name: d.Entry.Name, Family: string(d.Entry.Family),
			SizeMB: d.Library.Size >> 20, Filename: d.Library.Filename,
		}
		if d.Entry.Version != "" {
			info.Name += " " + d.Entry.Version
		}
		if !d.Library.ImportedAt.IsZero() {
			info.Added = d.Library.ImportedAt.Local().Format("2006-01-02")
		}
		out = append(out, info)
	}
	return out
}

func (s *Server) handleDownloadDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OSID string `json:"os_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.OSID == "" {
		httpErr(w, 400, "body must be {\"os_id\": \"<os>\"}")
		return
	}
	e, ok := oscatalog.Get(req.OSID)
	if !ok || !oscatalog.InLibrary(s.Lib, e) {
		httpErr(w, 404, "%s is not downloaded", req.OSID)
		return
	}
	if s.Reg.BusyExcept("") {
		httpErr(w, 409, "something is running — delete it when the activity has finished")
		return
	}
	freed, err := s.Lib.Remove(e.ID)
	if err != nil {
		httpErr(w, 500, "%v", err)
		return
	}
	writeJSON(w, 200, map[string]int64{"freed_mb": freed >> 20})
}

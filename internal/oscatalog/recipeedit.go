package oscatalog

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/uplinkresearch/dsky/internal/appcatalog"
	"github.com/uplinkresearch/dsky/internal/library"
	"github.com/uplinkresearch/dsky/internal/recipe"
)

// RecipeForm is a saved recipe read back into the choices the install dialog
// offers, so it can be opened there and changed.
type RecipeForm struct {
	OSID              string   `json:"os_id"`
	Edition           string   `json:"edition,omitempty"`
	AccountMode       string   `json:"account_mode,omitempty"`
	Debloat           string   `json:"debloat,omitempty"`
	BypassRequirement bool     `json:"bypass_requirement,omitempty"`
	Apps              []string `json:"apps"`
	// ThirdPartyDrivers is Ubuntu's "drivers for this computer": its
	// installer puts on the proprietary ones it finds. Nothing is staged, so
	// unlike Hardware below it is a choice the dialog shows and can change.
	ThirdPartyDrivers bool `json:"third_party_drivers,omitempty"`
	// Hardware is the driver packs already chosen. The dialog can keep them
	// but not show them as its own pickers: they were resolved from whatever
	// computer or models were chosen at the time.
	Hardware []recipe.HardwareSpec `json:"hardware,omitempty"`
}

// ErrNotEditable is why a recipe cannot be changed in the install dialog: it
// was written by hand or uses something the dialog has no control for. It can
// still be opened, and changed as a file.
var ErrNotEditable = errors.New("not editable in the install dialog")

// FormFromRecipe reads a recipe back into the install dialog's choices. Only
// what the dialog could have written comes back; anything else is refused
// rather than lost when the recipe is saved again.
func FormFromRecipe(wsDir string, r *recipe.Recipe) (RecipeForm, error) {
	notEditable := func(why string) (RecipeForm, error) {
		return RecipeForm{}, fmt.Errorf("%w: %s", ErrNotEditable, why)
	}
	if filepath.Clean(r.Path) != filepath.Join(wsDir, "recipes", r.ID+".yaml") {
		return notEditable("its file name is not its id")
	}
	e, ok := Get(r.OS.Source)
	if !ok {
		return notEditable(fmt.Sprintf("it builds from %s, which is not in DSKY's OS list", r.OS.Source))
	}
	f := RecipeForm{OSID: e.ID, Apps: []string{}}
	if e.Family != Windows {
		if r.Windows != nil {
			return notEditable("it has Windows settings on a Linux OS")
		}
		if r.Linux == nil || r.Linux.Autoinstall == nil {
			return f, sameAsDialog(r, f, e)
		}
		if len(r.Linux.Autoinstall.Vars) > 0 || r.Linux.Autoinstall.UserData != ubuntuUserDataFile(r.ID) {
			return notEditable("its Ubuntu answers were written by hand")
		}
		apps, drivers, err := ubuntuProgramsIn(filepath.Join(wsDir, filepath.FromSlash(r.Linux.Autoinstall.UserData)))
		if err != nil {
			return notEditable(err.Error())
		}
		f.Apps, f.ThirdPartyDrivers = apps, drivers
		return f, sameAsDialog(r, f, e)
	}

	w := r.Windows
	if w == nil || w.Unattend == nil || r.OS.SourceMode == recipe.SourceTree {
		return notEditable("it does not use DSKY's Windows setup answers")
	}
	switch {
	case len(w.DriverPacks) > 0 || len(w.WinPEDrivers) > 0:
		return notEditable("it lists driver packs by hand")
	case w.Domain != nil:
		// A join file is one computer's, so the dialog never saves one.
		return notEditable("it joins a domain")
	case w.StatusScreen != nil:
		return notEditable("it has a status screen")
	case w.Firstboot.Mode == "template":
		return notEditable("its first-boot script is a template")
	case w.Debloat != nil && (len(w.Debloat.RemoveApps) > 0 || len(w.Debloat.KeepApps) > 0):
		return notEditable("its bloatware lists were changed by hand")
	}
	vars := w.Unattend.Vars
	for k, v := range genericKeys {
		if vars["edition_key"] == v {
			f.Edition = k
		}
	}
	if f.Edition == "" {
		return notEditable("its edition key is not one of Microsoft's generic keys")
	}
	f.AccountMode = vars["account_mode"]
	if f.AccountMode == "" {
		f.AccountMode = "local"
	}
	f.BypassRequirement = vars["bypass_requirements"] == "1"
	f.Debloat = "off"
	if w.Debloat != nil && w.Debloat.Preset != "" {
		f.Debloat = w.Debloat.Preset
	}
	if w.Apps != nil {
		for _, pkg := range w.Apps.Winget {
			id := appcatalog.WingetPrefix + pkg
			for _, a := range appcatalog.Catalog() {
				if a.Winget != "" && strings.EqualFold(a.Winget, pkg) {
					id = a.ID
					break
				}
			}
			f.Apps = append(f.Apps, id)
		}
	}
	for _, p := range w.Payload {
		c, ok := customBySource(p.Ref)
		if p.Path != "" || !ok {
			return notEditable("it carries files that are not installers added in DSKY")
		}
		f.Apps = append(f.Apps, c.ID)
	}
	f.Hardware = w.Hardware
	return f, sameAsDialog(r, f, e)
}

// sameAsDialog checks that saving f from the dialog writes the recipe r
// already is, so an edit can't quietly drop or change something the dialog
// doesn't show. Compared as decoded YAML: comments and layout don't count.
func sameAsDialog(r *recipe.Recipe, f RecipeForm, e Entry) error {
	opts := Options{Edition: f.Edition, AccountMode: f.AccountMode, Debloat: f.Debloat,
		BypassRequirement: f.BypassRequirement, Apps: f.Apps,
		ThirdPartyDrivers: f.ThirdPartyDrivers}
	if e.Family == Windows {
		opts.defaults(e)
	}
	meta := recipeMeta{ID: r.ID, Name: r.Name, Template: savedTemplate}
	if r.Linux != nil && r.Linux.Autoinstall != nil {
		meta.UserData = r.Linux.Autoinstall.UserData
	}
	onDisk, err := os.ReadFile(r.Path)
	if err != nil {
		return err
	}
	var have, want recipe.Recipe
	if err := yaml.Unmarshal(onDisk, &have); err != nil {
		return err
	}
	if err := yaml.Unmarshal([]byte(recipeYAML(meta, e, opts, f.Hardware)), &want); err != nil {
		return err
	}
	if !reflect.DeepEqual(have, want) {
		return fmt.Errorf("%w: it has settings the install dialog doesn't show", ErrNotEditable)
	}
	return nil
}

func customBySource(ref string) (appcatalog.Custom, bool) {
	for _, c := range appcatalog.CustomApps() {
		if c.SourceID() == ref {
			return c, true
		}
	}
	return appcatalog.Custom{}, false
}

// programsMarker records the picker's choices in the Ubuntu answers, because
// the answers themselves only say what Ubuntu installs: one package can come
// from several programs, and a set expands.
const programsMarker = "# DSKY programs: "

var flatpakInstall = regexp.MustCompile(`flatpak install [^\n]*?([A-Za-z0-9_-]+(?:\.[A-Za-z0-9_-]+){2,})`)

// ubuntuProgramsIn reads which picker programs a DSKY-written Ubuntu answers
// file installs: from the marker, or for files written before it existed, by
// matching each program's packages against what the file installs.
func ubuntuProgramsIn(path string) ([]string, bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, false, fmt.Errorf("its Ubuntu answers could not be read: %v", err)
	}
	text := string(b)
	var doc struct {
		Autoinstall struct {
			Drivers *struct {
				Install bool `yaml:"install"`
			} `yaml:"drivers"`
			Packages []string `yaml:"packages"`
			Snaps    []struct {
				Name string `yaml:"name"`
			} `yaml:"snaps"`
			LateCommands []string `yaml:"late-commands"`
		} `yaml:"autoinstall"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, false, fmt.Errorf("its Ubuntu answers do not parse: %v", err)
	}
	drivers := doc.Autoinstall.Drivers != nil && doc.Autoinstall.Drivers.Install
	for _, line := range strings.Split(text, "\n") {
		if rest, ok := strings.CutPrefix(line, programsMarker); ok {
			return strings.Fields(rest), drivers, nil
		}
	}
	if !strings.Contains(text, "# Generated by DSKY.") {
		return nil, false, errors.New("its Ubuntu answers were written by hand")
	}
	var script strings.Builder
	for _, c := range doc.Autoinstall.LateCommands {
		if enc, ok := strings.CutPrefix(c, "echo "); ok {
			enc, _, _ = strings.Cut(enc, " ")
			if dec, err := base64.StdEncoding.DecodeString(enc); err == nil {
				script.Write(dec)
			}
		}
	}
	flatpaks := map[string]bool{}
	for _, m := range flatpakInstall.FindAllStringSubmatch(script.String(), -1) {
		flatpaks[m[1]] = true
	}
	var snaps []string
	for _, s := range doc.Autoinstall.Snaps {
		snaps = append(snaps, s.Name)
	}
	out := []string{}
	for _, a := range appcatalog.Catalog() {
		u := a.Ubuntu
		if u == nil {
			continue
		}
		repoInstalled := false
		if u.Repo != "" {
			if repo, ok := appcatalog.UbuntuRepoByID(u.Repo); ok {
				repoInstalled = strings.Contains(script.String(), repo.Package)
			}
		}
		if (u.Apt != "" && slices.Contains(doc.Autoinstall.Packages, u.Apt)) ||
			(u.Snap != "" && slices.Contains(snaps, u.Snap)) ||
			(u.Flatpak != "" && flatpaks[u.Flatpak]) || repoInstalled {
			out = append(out, a.ID)
		}
	}
	return out, drivers, nil
}

// ReplaceRecipe saves opts over the existing recipe id, keeping its id so
// built images and anything else that names it still match. The old files are
// put back if the new recipe does not save.
func ReplaceRecipe(ctx context.Context, lib *library.Library, wsDir, id, name string, e Entry, opts Options, progress func(stage string, done, total int64)) (string, error) {
	files := recipeFiles(wsDir, id)
	moved := map[string]string{}
	restore := func() {
		for orig, bak := range moved {
			os.Rename(bak, orig)
		}
	}
	for _, p := range files {
		if _, err := os.Stat(p); err != nil {
			continue
		}
		bak := p + ".dsky-old"
		if err := os.Rename(p, bak); err != nil {
			restore()
			return "", err
		}
		moved[p] = bak
	}
	path, err := SaveRecipe(ctx, lib, wsDir, id, name, e, opts, progress)
	if err != nil {
		for _, p := range files {
			os.Remove(p)
		}
		restore()
		return "", err
	}
	for _, bak := range moved {
		os.Remove(bak)
	}
	return path, nil
}

// DeleteRecipe removes a recipe's file and the Ubuntu answers DSKY wrote for
// it. Manifests and templates stay: other recipes may use them.
func DeleteRecipe(wsDir string, r *recipe.Recipe) error {
	if err := os.Remove(r.Path); err != nil {
		return err
	}
	if r.Linux != nil && r.Linux.Autoinstall != nil && r.Linux.Autoinstall.UserData == ubuntuUserDataFile(r.ID) {
		if err := os.Remove(filepath.Join(wsDir, filepath.FromSlash(ubuntuUserDataFile(r.ID)))); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func recipeFiles(wsDir, id string) []string {
	return []string{
		filepath.Join(wsDir, "recipes", id+".yaml"),
		filepath.Join(wsDir, filepath.FromSlash(ubuntuUserDataFile(id))),
	}
}

// Downloaded is one OS from the list whose image is in the library.
type Downloaded struct {
	Entry   Entry
	Library library.Entry
}

// DownloadedImages lists the OS images already in the library, in the OS
// list's order.
func DownloadedImages(lib *library.Library) []Downloaded {
	var out []Downloaded
	for _, e := range Catalog() {
		if le, err := lib.Resolve(e.ID); err == nil {
			out = append(out, Downloaded{Entry: e, Library: le})
		}
	}
	return out
}

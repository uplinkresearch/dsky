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
	"github.com/uplinkresearch/dsky/internal/workspace"
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
	// Offline is the recipe carrying its programs' installers rather than
	// naming them for winget. The dialog shows it as a tick beside the
	// program picker.
	Offline bool `json:"offline,omitempty"`
	// prepulled and builtAt are what that tick resolved to when the recipe
	// was saved, read back out of the recipe and the manifests beside it.
	// They are not sent to the page: it has no use for a hash, and changing
	// the program list re-downloads anyway. They exist so the round-trip
	// check below compares the recipe against itself rather than against a
	// recipe with the downloads missing.
	prepulled []appcatalog.Prepulled
	builtAt   string
	// ThirdPartyDrivers is Ubuntu's "drivers for this computer": its
	// installer puts on the proprietary ones it finds. Nothing is staged, so
	// unlike Hardware below it is a choice the dialog shows and can change.
	ThirdPartyDrivers bool `json:"third_party_drivers,omitempty"`
	// WiFiSSID is the wireless network the machine joins at first boot, and
	// WiFiPassword says only whether that network needs a passphrase. The
	// passphrase itself is never written into a recipe, so it cannot be read
	// back out of one: the dialog asks for it again.
	WiFiSSID     string `json:"wifi_ssid,omitempty"`
	WiFiPassword bool   `json:"wifi_password,omitempty"`
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
		if r.Linux == nil || (r.Linux.Autoinstall == nil && r.Linux.Kickstart == nil) {
			return f, sameAsDialog(r, f, e)
		}
		// A kickstart recipe is the same round trip with a different answer
		// file. Without this branch every Fedora recipe the dialog saved came
		// back "not editable", so the only way to change its program list was
		// to delete it and build another -- and the marker fedora.go writes so
		// the picks can be read back had no reader anywhere.
		if k := r.Linux.Kickstart; k != nil {
			if len(k.Vars) > 0 || len(k.KernelArgs) > 0 || k.File != fedoraKickstartFile(r.ID) {
				return notEditable("its kickstart was written by hand")
			}
			apps, err := kickstartProgramsIn(filepath.Join(wsDir, filepath.FromSlash(k.File)))
			if err != nil {
				return notEditable(err.Error())
			}
			f.Apps = apps
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
	// Server carries no key at all — its edition is an image on the media —
	// so the recipe names the edition outright and there is nothing to match
	// a key against.
	if ed := vars["server_edition"]; ed != "" {
		if serverImages[ed] == 0 {
			return notEditable("its Windows Server edition is not one this build offers")
		}
		f.Edition = ed
	} else {
		for k, v := range genericKeys {
			if vars["edition_key"] == v {
				f.Edition = k
			}
		}
		if f.Edition == "" {
			return notEditable("its edition key is not one of Microsoft's generic keys")
		}
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
			f.Apps = append(f.Apps, catalogIDFor(pkg))
		}
	}
	// An offline recipe carries the catalog programs' installers as payload.
	// Those refs are read back from the record beside them, not from the
	// operator's own installers, and the programs they stand for go into the
	// picker as the catalog programs they are.
	offline, err := offlinePrograms(wsDir, w)
	if err != nil {
		return notEditable(err.Error())
	}
	fromOffline := map[string]bool{}
	for _, p := range offline {
		fromOffline[p.SourceID()] = true
		f.Apps = append(f.Apps, catalogIDFor(p.WingetID))
	}
	if len(offline) > 0 {
		f.Offline, f.prepulled, f.builtAt = true, offline, w.Apps.BuiltAt
	}
	for _, p := range w.Payload {
		if fromOffline[p.Ref] {
			continue
		}
		c, ok := customBySource(p.Ref)
		if p.Path != "" || !ok {
			return notEditable("it carries files that are not installers added in DSKY")
		}
		f.Apps = append(f.Apps, c.ID)
	}
	if wf := w.WiFi; wf != nil {
		// A literal passphrase means somebody wrote it in by hand. Saving
		// from the dialog would replace it with the variable and lose the
		// value, so the recipe stays a file rather than a form.
		if wf.Password != "" && wf.Password != wifiPasswordVar {
			return notEditable("its wireless passphrase is written into the recipe")
		}
		f.WiFiSSID, f.WiFiPassword = wf.SSID, wf.Password != ""
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
		ThirdPartyDrivers: f.ThirdPartyDrivers, WiFiSSID: f.WiFiSSID,
		Offline: f.Offline, prepulled: f.prepulled, builtAt: f.builtAt}
	if f.WiFiPassword {
		// Only whether there is one matters here: the recipe says
		// "${var:wifi_password}" either way, and the value never reaches it.
		opts.WiFiPassword = wifiPasswordVar
	}
	if e.Family == Windows {
		opts.defaults(e)
	}
	meta := recipeMeta{ID: r.ID, Name: r.Name, Template: savedTemplate}
	if r.Linux != nil {
		// One field, two answer files: recipeYAML turns UserData into
		// linux.kickstart.file for an Anaconda entry and linux.autoinstall
		// for the rest. Reading only the Autoinstall one meant a kickstart
		// recipe was compared against a recipe with no linux: block at all,
		// so it never matched and was always "not editable".
		switch {
		case r.Linux.Autoinstall != nil:
			meta.UserData = r.Linux.Autoinstall.UserData
		case r.Linux.Kickstart != nil:
			meta.UserData = r.Linux.Kickstart.File
		}
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

// kickstartProgramsIn reads which picker programs a DSKY-written kickstart
// installs. Only the marker is consulted: unlike the Ubuntu answers there is
// no older format to reconstruct from, because kickstart recipes have carried
// the marker since the day they existed.
func kickstartProgramsIn(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("its kickstart could not be read: %v", err)
	}
	text := string(b)
	for _, line := range strings.Split(text, "\n") {
		if rest, ok := strings.CutPrefix(line, programsMarker); ok {
			return strings.Fields(rest), nil
		}
	}
	// No marker means this file is not one DSKY wrote: a kickstart is only
	// written when programs were picked, and it is written with the marker in
	// the same call. Reconstructing the list from %packages is what the marker
	// exists to avoid -- a set expands, and one package can come from several
	// programs -- so an unmarked file is refused rather than guessed at.
	return nil, errors.New("its kickstart was written by hand")
}

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

// DeleteRecipe removes a recipe's file and the answers DSKY wrote for it —
// Ubuntu's or Anaconda's. Manifests and templates stay: other recipes may use
// them.
func DeleteRecipe(wsDir string, r *recipe.Recipe) error {
	if err := os.Remove(r.Path); err != nil {
		return err
	}
	if r.Linux == nil {
		return nil
	}
	// Deleting a Fedora recipe used to leave its kickstart in the workspace
	// forever, because only the Ubuntu file was known here.
	var written string
	switch {
	case r.Linux.Autoinstall != nil && r.Linux.Autoinstall.UserData == ubuntuUserDataFile(r.ID):
		written = ubuntuUserDataFile(r.ID)
	case r.Linux.Kickstart != nil && r.Linux.Kickstart.File == fedoraKickstartFile(r.ID):
		written = fedoraKickstartFile(r.ID)
	}
	if written == "" {
		return nil
	}
	if err := os.Remove(filepath.Join(wsDir, filepath.FromSlash(written))); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// recipeFiles is what ReplaceRecipe moves aside so a failed save can be rolled
// back. Both answer files are listed because only one of them exists for any
// given recipe, and leaving the kickstart out meant a Fedora save that failed
// half way restored the old recipe beside the new program list -- media that
// then installed what the abandoned edit asked for, with nothing saying so.
func recipeFiles(wsDir, id string) []string {
	return []string{
		filepath.Join(wsDir, "recipes", id+".yaml"),
		filepath.Join(wsDir, filepath.FromSlash(ubuntuUserDataFile(id))),
		filepath.Join(wsDir, filepath.FromSlash(fedoraKickstartFile(id))),
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

// catalogIDFor is the picker's id for a winget package: the program's own id
// where the list has it, and the typed "winget:Publisher.Package" form where
// it does not.
func catalogIDFor(pkg string) string {
	for _, a := range appcatalog.Catalog() {
		if a.Winget != "" && strings.EqualFold(a.Winget, pkg) {
			return a.ID
		}
	}
	return appcatalog.WingetPrefix + pkg
}

// offlinePrograms reads a saved offline build back out of the recipe: which
// winget package each staged installer is, from the record, and how to run it,
// from the manifest beside it and the step that runs it.
//
// It is read back rather than downloaded again on purpose. Opening a recipe in
// the dialog must not touch the network -- somebody renaming a recipe should
// not be made to wait for a hundred megabytes, and must not silently get a
// newer Chrome than the recipe was saved with.
func offlinePrograms(wsDir string, w *recipe.WindowsSpec) ([]appcatalog.Prepulled, error) {
	recs := w.Apps.OfflineApps()
	if len(recs) == 0 {
		return nil, nil
	}
	staged := map[string]bool{}
	for _, p := range w.Payload {
		staged[p.Ref] = true
	}
	args := map[string][]string{}
	for _, s := range w.Firstboot.Steps {
		switch {
		case s.MSI != nil && s.MSI.Ref != "":
			args[s.MSI.Ref] = s.MSI.Args
		case s.Exe != nil && s.Exe.Ref != "":
			args[s.Exe.Ref] = s.Exe.Args
		}
	}
	var out []appcatalog.Prepulled
	for _, r := range recs {
		if !staged[r.Ref] {
			return nil, fmt.Errorf("it records %s as being on the media, and nothing stages it", r.ID)
		}
		b, err := os.ReadFile(filepath.Join(wsDir, "manifests", r.Ref+".yaml"))
		if err != nil {
			return nil, fmt.Errorf("it records %s as being on the media, and its manifest is missing", r.ID)
		}
		var src struct {
			Format   string `yaml:"format"`
			SHA256   string `yaml:"sha256"`
			Filename string `yaml:"filename"`
		}
		if err := yaml.Unmarshal(b, &src); err != nil {
			return nil, fmt.Errorf("%s's manifest could not be read: %v", r.ID, err)
		}
		p := appcatalog.Prepulled{
			WingetID: r.ID, Version: r.Version, SHA256: src.SHA256,
			Filename: src.Filename, Format: src.Format, Args: args[r.Ref],
		}
		if p.SourceID() != r.Ref {
			return nil, fmt.Errorf("%s is staged as %s, which is not the name DSKY files it under", r.ID, r.Ref)
		}
		out = append(out, p)
	}
	return out, nil
}

// VarsRecipeNeeds lists what a saved recipe cannot be built without: the
// values it references and the workspace does not supply.
//
// It is asked right after saving, because a recipe that keeps passwords out of
// itself -- which is the whole point of keeping them out -- is a recipe that
// will not build until somebody puts them somewhere. The dialog said "saved"
// and nothing else, and the next thing anybody heard was a build refusing to
// start, naming a file they had never opened.
//
// Reading the recipe back rather than working from the options it was saved
// from, because the recipe on disk is what a build will read, and it is
// already loaded back at this point to prove it parses.
func VarsRecipeNeeds(wsDir, id string) []string {
	ws, err := workspace.Load(wsDir)
	if err != nil {
		return nil
	}
	r, err := ws.Recipe(id)
	if err != nil {
		return nil
	}
	return recipe.NeededVars(r, ws.MergedVars(r, nil))
}

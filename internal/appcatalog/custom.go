package appcatalog

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/uplinkresearch/dsky/internal/library"
	"github.com/uplinkresearch/dsky/internal/manifest"
)

// Custom programs: installers the operator supplies.
//
// The built-in list names packages in winget, which covers public software and
// nothing else. The installer that actually matters to an MSP is the one no
// package manager has — the RMM agent built for this customer, a licensed app
// with a site-specific MSI, an in-house tool. Those are the programs worth
// putting on every machine, and before this there was no way to add one
// without hand-authoring a workspace recipe.
//
// The file itself goes into the library, content-addressed like everything
// else, so it is pinned by its own hash and shared between builds. This file
// only records what it is and how to run it silently.
//
// Machine-local on purpose: Quick Install has no workspace, and an
// organisation's agent installer is usually the thing you least want to commit
// to a git repository. Sharing a set between technicians is a library
// export/import job, not a catalog one.

// CustomStoreVersion is the on-disk format of the custom-app store.
const CustomStoreVersion = 1

// CustomCategory is where operator-added programs appear in the pickers,
// which is deliberately one group rather than mixed into the built-in ones:
// "did my agent get added?" should be answerable at a glance.
const CustomCategory = "Your installers"

// Custom is one operator-supplied installer.
type Custom struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Category string    `json:"category,omitempty"`
	Format   string    `json:"format"` // msi | exe
	SHA256   string    `json:"sha256"` // the library blob holding the installer
	Filename string    `json:"filename"`
	Args     []string  `json:"args,omitempty"` // silent-install switches
	Size     int64     `json:"size"`
	AddedAt  time.Time `json:"added_at"`
}

// SourceID is the library/manifest id the installer is filed under. Prefixed
// so an app called "git" cannot collide with a driver pack or an OS image.
func (c Custom) SourceID() string { return "app-" + c.ID }

// RunArgs are the switches to run the installer with. An MSI with no switches
// would open a wizard on a machine nobody is sitting at, so quiet is the
// default; msiexec's own /qn is applied downstream when this is empty.
func (c Custom) RunArgs() []string { return c.Args }

type customStore struct {
	Version int      `json:"version"`
	Apps    []Custom `json:"apps"`
}

var (
	customMu sync.RWMutex
	customs  []Custom
)

// idRe matches what a manifest id may contain, since SourceID becomes one.
var idRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// StorePath is where the custom-app records live, beside the library they
// reference.
func StorePath(root string) string { return filepath.Join(root, "apps.json") }

// LoadCustom reads the operator's programs and makes them part of Catalog().
// Called once at startup. A missing store is the normal first-run state, not
// an error; a corrupt one is reported so it can be fixed rather than silently
// emptying somebody's list.
func LoadCustom(root string) error {
	b, err := os.ReadFile(StorePath(root))
	if err != nil {
		if os.IsNotExist(err) {
			// The list is whatever this root says, and this root says none.
			// Clearing rather than returning early matters when the root
			// changes (--library): the previous root's programs must not
			// linger as if they were still available.
			customMu.Lock()
			customs = nil
			customMu.Unlock()
			return nil
		}
		return err
	}
	var s customStore
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("%s is not readable: %w", StorePath(root), err)
	}
	if s.Version > CustomStoreVersion {
		return fmt.Errorf("%s was written by a newer version of this tool (format %d, this build reads %d)",
			StorePath(root), s.Version, CustomStoreVersion)
	}
	customMu.Lock()
	customs = s.Apps
	customMu.Unlock()
	return nil
}

// CustomApps returns the operator's programs, newest listing order by name.
func CustomApps() []Custom {
	customMu.RLock()
	defer customMu.RUnlock()
	out := make([]Custom, len(customs))
	copy(out, customs)
	return out
}

// GetCustom returns the operator program with this id.
func GetCustom(id string) (Custom, bool) {
	for _, c := range CustomApps() {
		if strings.EqualFold(c.ID, id) {
			return c, true
		}
	}
	return Custom{}, false
}

func saveCustom(root string, list []Custom) error {
	sort.Slice(list, func(i, j int) bool {
		return strings.ToLower(list[i].Name) < strings.ToLower(list[j].Name)
	})
	b, err := json.MarshalIndent(customStore{Version: CustomStoreVersion, Apps: list}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	// Written through a temporary file: a half-written store would lose every
	// program the operator has added, not just the one being changed.
	tmp := StorePath(root) + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, StorePath(root)); err != nil {
		return err
	}
	customMu.Lock()
	customs = list
	customMu.Unlock()
	return nil
}

// CheckCustomID validates everything knowable before the installer is copied
// into the library, so a bad id costs nothing instead of a few hundred
// megabytes of pointless copying.
func CheckCustomID(id, name, format string) error {
	switch {
	case id == "":
		return fmt.Errorf("an id is required (it is what --apps takes)")
	case !idRe.MatchString(id):
		return fmt.Errorf("id %q must be lowercase letters, digits, dot, dash or underscore, starting with a letter or digit", id)
	case name == "":
		return fmt.Errorf("a name is required — it is what the picker shows")
	case format != "msi" && format != "exe":
		return fmt.Errorf("format %q must be msi or exe", format)
	}
	// A built-in id would be ambiguous in --apps and silently shadow the other.
	// One added before the built-in list grew to include its id is already
	// the operator's, and editing or replacing it stays allowed.
	if _, clash := builtinByID(id); clash && !customExists(id) {
		return fmt.Errorf("%q is already a built-in program — choose another id", id)
	}
	return nil
}

// ValidateCustom checks a complete record before it is stored, so a broken
// entry surfaces here rather than as a confusing manifest error several
// gigabytes into a build.
func ValidateCustom(c Custom) error {
	if err := CheckCustomID(c.ID, c.Name, c.Format); err != nil {
		return err
	}
	if c.SHA256 == "" || c.Filename == "" {
		return fmt.Errorf("the installer was not filed in the library")
	}
	return nil
}

// AddCustom stores a program. Without replace, an existing id is an error
// rather than a silent overwrite: re-adding usually means a new version of the
// same installer, and doing that by accident would swap what lands on every
// machine built afterwards.
func AddCustom(root string, c Custom, replace bool) error {
	if err := ValidateCustom(c); err != nil {
		return err
	}
	list := CustomApps()
	for i, existing := range list {
		if strings.EqualFold(existing.ID, c.ID) {
			if !replace {
				return fmt.Errorf("%q already exists (%s) — pass --replace to update it",
					c.ID, existing.Filename)
			}
			if c.AddedAt.IsZero() {
				c.AddedAt = existing.AddedAt
			}
			list[i] = c
			return saveCustom(root, list)
		}
	}
	if c.AddedAt.IsZero() {
		c.AddedAt = time.Now().UTC().Truncate(time.Second)
	}
	return saveCustom(root, append(list, c))
}

// UpdateCustom edits a stored program in place — a name or the silent-install
// switches — without re-importing the installer. Getting the switches wrong is
// the common mistake, and re-copying a 200 MB MSI to fix a typo would be
// silly.
func UpdateCustom(root, id string, edit func(*Custom)) (Custom, error) {
	list := CustomApps()
	for i, c := range list {
		if !strings.EqualFold(c.ID, id) {
			continue
		}
		edit(&list[i])
		if err := ValidateCustom(list[i]); err != nil {
			return Custom{}, err
		}
		return list[i], saveCustom(root, list)
	}
	return Custom{}, fmt.Errorf("no program %q — see `dsky apps`", id)
}

// RemoveCustom forgets a program. The installer stays in the library, which
// `dsky gc` reclaims once nothing references it — so removing the wrong one
// costs a re-add, not a re-download.
func RemoveCustom(root, id string) (Custom, error) {
	list := CustomApps()
	for i, c := range list {
		if strings.EqualFold(c.ID, id) {
			return c, saveCustom(root, append(list[:i:i], list[i+1:]...))
		}
	}
	return Custom{}, fmt.Errorf("no program %q — see `dsky apps`", id)
}

// FormatForFile reports how an installer will be run, from its extension and
// from what is actually inside the file.
//
// The name is not enough. A ScreenConnect client saved as
// ScreenConnect.ClientSetup.msi reached a machine, was handed to msiexec at
// first boot, and came back 1620 -- "this installation package could not be
// opened" -- because whatever had been downloaded was not a Windows Installer
// package. That is a bad moment to find out: the file had been staged onto a
// stick, carried to a bench, and installed onto a machine somebody was
// waiting for. Both formats say what they are in their first bytes, so it is
// checked when the installer is added.
func FormatForFile(path string) (string, error) {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".msi", ".exe":
	default:
		return "", fmt.Errorf("%s is not a .msi or .exe — those are what the first-boot script can run",
			filepath.Base(path))
	}
	head, err := headOf(path)
	if err != nil {
		// Unreadable here is somebody else's error to report; the extension
		// is all this function promised.
		if ext == ".msi" {
			return "msi", nil
		}
		return "exe", nil
	}
	switch ext {
	case ".msi":
		// A Windows Installer package is an OLE compound file.
		if !bytes.HasPrefix(head, []byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1}) {
			return "", fmt.Errorf("%s is named .msi but is not a Windows Installer package (%s) — "+
				"Windows refuses these at first boot with error 1620; re-download it, or add it as the .exe installer",
				filepath.Base(path), describeHead(head))
		}
		return "msi", nil
	default:
		if !bytes.HasPrefix(head, []byte("MZ")) {
			return "", fmt.Errorf("%s is named .exe but is not a Windows program (%s) — re-download it",
				filepath.Base(path), describeHead(head))
		}
		return "exe", nil
	}
}

// headOf reads the first few bytes of a file, which is all any of this needs.
func headOf(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	head := make([]byte, 8)
	n, err := io.ReadFull(f, head)
	if err != nil && n == 0 {
		return nil, err
	}
	return head[:n], nil
}

// describeHead says what the file looks like instead, in the words somebody
// would use to go and find the right one.
func describeHead(head []byte) string {
	switch {
	case bytes.HasPrefix(head, []byte("MZ")):
		return "it is a Windows program; rename it .exe"
	case bytes.HasPrefix(head, []byte("PK\x03\x04")):
		return "it is a zip file"
	case bytes.HasPrefix(head, []byte("<!DO")), bytes.HasPrefix(head, []byte("<htm")), bytes.HasPrefix(head, []byte("<HTM")):
		return "it is a web page — the download was probably an error page"
	case len(head) < 8:
		return "the file is empty or truncated"
	default:
		return "it starts with " + hex.EncodeToString(head)
	}
}

// DeriveID makes a usable picker id from a filename, so the common case needs
// no id to be chosen by hand.
func DeriveID(path string) string {
	base := filepath.Base(path)
	base = strings.TrimSuffix(base, filepath.Ext(base))
	var b strings.Builder
	for _, r := range strings.ToLower(base) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == '.':
			b.WriteRune(r)
		case r == ' ':
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-._")
}

// SplitArgs turns a switch string into arguments. Quoted runs are kept whole,
// since installer switches routinely carry paths and licence keys with spaces.
func SplitArgs(s string) []string {
	var out []string
	var cur strings.Builder
	quote := rune(0)
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '"' || r == '\'':
			quote = r
		case r == ' ' || r == '\t':
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return out
}

// RunLine is the command the first-boot script will run. Worth showing
// wherever an installer is configured: a silent switch that is wrong produces
// a machine sitting on an installer dialog forever, and nobody finds out until
// they walk up to it.
func (c Custom) RunLine() string {
	if c.Format == "msi" {
		args := strings.Join(c.Args, " ")
		if args == "" {
			args = "/qn" // msiexec's default, applied downstream
		}
		return fmt.Sprintf(`msiexec /i "%s" %s`, c.Filename, args)
	}
	return strings.TrimSpace(fmt.Sprintf(`"%s" %s`, c.Filename, strings.Join(c.Args, " ")))
}

// Installer is what a caller knows about an installer being added; the empty
// fields are filled in from the file.
type Installer struct {
	Path     string
	ID       string
	Name     string
	Args     string
	Category string
	Replace  bool
}

// AddInstaller does the whole job: work out the defaults, refuse anything
// wrong before copying gigabytes, file the installer in the library, and
// record it.
//
// One implementation for the command line and the portal both, because the
// order of those steps is the part that matters — validating after the copy
// would waste it, and recording before the copy would leave a program
// pointing at nothing.
//
// progress, if given, is called while the installer is hashed and copied, and
// only once that is actually going to happen — so a caller shows a bar for
// real work rather than on the way to a refusal.
func AddInstaller(root string, lib blobStore, in Installer, progress func(stage string, done, total int64)) (Custom, error) {
	format, err := FormatForFile(in.Path)
	if err != nil {
		return Custom{}, err
	}
	st, err := os.Stat(in.Path)
	if err != nil {
		return Custom{}, err
	}
	if st.IsDir() {
		return Custom{}, fmt.Errorf("%s is a directory — point at the installer file", in.Path)
	}

	c := Custom{
		ID:       firstNonEmpty(in.ID, DeriveID(in.Path)),
		Name:     firstNonEmpty(in.Name, filepath.Base(in.Path)),
		Category: in.Category,
		Format:   format,
		Filename: filepath.Base(in.Path),
		Args:     SplitArgs(in.Args),
	}
	if err := CheckCustomID(c.ID, c.Name, c.Format); err != nil {
		return Custom{}, err
	}
	// Checked here as well as in AddCustom so a duplicate costs nothing: the
	// copy below is the expensive part.
	if existing, ok := GetCustom(c.ID); ok && !in.Replace {
		return Custom{}, fmt.Errorf("%q already exists (%s) — replace it to update",
			c.ID, existing.Filename)
	}

	entry, err := lib.ImportWithProgress(&manifest.Source{
		ID:       c.SourceID(),
		Kind:     manifest.KindPayload,
		Format:   manifest.Format(c.Format),
		Filename: c.Filename,
	}, in.Path, progress)
	if err != nil {
		return Custom{}, err
	}
	c.SHA256, c.Size = entry.SHA256, entry.Size
	if err := AddCustom(root, c, in.Replace); err != nil {
		return Custom{}, err
	}
	// Return what was actually stored, not the copy that went in: AddCustom
	// stamps AddedAt (and preserves the original on a replace), so the local
	// copy is missing it and a caller echoing it back would show a 1-01-01
	// date it never had.
	if stored, ok := GetCustom(c.ID); ok {
		return stored, nil
	}
	return c, nil
}

// blobStore is the slice of the library this package needs, kept narrow so
// the dependency is obvious and the tests need no real library.
//
// The progress form, because an installer can be several hundred megabytes
// and is read twice — once to hash, once to copy — which is long enough that
// silence reads as a hang.
type blobStore interface {
	ImportWithProgress(src *manifest.Source, filePath string,
		progress func(stage string, done, total int64)) (library.Entry, error)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func customExists(id string) bool {
	for _, c := range CustomApps() {
		if strings.EqualFold(c.ID, id) {
			return true
		}
	}
	return false
}

func builtinByID(id string) (App, bool) {
	for _, a := range builtin {
		if strings.EqualFold(a.ID, id) {
			return a, true
		}
	}
	return App{}, false
}

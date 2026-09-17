// Package migrate describes a Windows PC well enough to rebuild it on new
// hardware: what is installed, who it is in the domain, which settings and
// peripherals it has, and — the part that matters — how each of those is to be
// recreated. That description is the manifest, and it is the only thing that
// passes between the five stages of a migration:
//
//	scan (on the old PC) -> resolve -> review (a person approves) -> build -> verify
//
// Everything here is about the file, not the machine: reading it, writing it,
// checking it, and hashing it so that a build refuses a manifest nobody
// approved or one edited after approval. The scanner, resolver, builder and
// runner each live elsewhere and speak only through this type.
//
// Three rules shape the schema, and they are worth keeping in mind before
// adding a field:
//
//   - Nothing is silent. An application the tool cannot place has an entry
//     with status "unmapped", not no entry at all; a setting with no way to
//     apply it on the new machine is still captured and still reported.
//   - Instructions, not state. identity says which domain and which OU to
//     join, never a credential or a machine password; data says which
//     strategy to use, not the files.
//   - The operator decides. Every automatic choice records how it was made
//     (resolved_by) so a review can see what to look at.
package migrate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// SchemaVersion is the manifest format this build writes. It is major.minor:
// a new optional field is a minor bump that older builds still read, and
// anything that changes the meaning of an existing field is a major bump with
// a migration in schema.go. See docs/migrate-schema-changelog.md.
const SchemaVersion = "1.1"

// ManifestName is what the scanner writes beside itself.
const ManifestName = "manifest.json"

// Manifest is one source machine, and what to do about it.
type Manifest struct {
	SchemaVersion string    `json:"schema_version"`
	GeneratedAt   time.Time `json:"generated_at"`
	GeneratedBy   string    `json:"generated_by"`

	Approval    Approval    `json:"approval"`
	Source      Source      `json:"source"`
	Target      Target      `json:"target"`
	Identity    Identity    `json:"identity"`
	Apps        []App       `json:"apps"`
	Settings    []Setting   `json:"settings"`
	Peripherals Peripherals `json:"peripherals"`
	Data        Data        `json:"data"`
	Compat      []Compat    `json:"compat"`
	Notes       []string    `json:"notes"`
}

// Approval is the human step, recorded. ContentHash covers every other
// section, so any later edit to the manifest invalidates it -- see Hash.
type Approval struct {
	Approved    bool       `json:"approved"`
	ApprovedAt  *time.Time `json:"approved_at"`
	ApprovedBy  string     `json:"approved_by"`
	ContentHash string     `json:"content_hash"`
}

// Source is what the machine was. Nothing here is acted on; it is what the
// report shows and what verify compares against.
type Source struct {
	Hostname      string     `json:"hostname"`
	OS            SourceOS   `json:"os"`
	Hardware      Hardware   `json:"hardware"`
	ScanDurationS float64    `json:"scan_duration_s"`
	ScannerUser   string     `json:"scanner_user"`
	Users         []UserProf `json:"users,omitempty"`
}

// SourceOS is the Windows on the source machine.
type SourceOS struct {
	ProductName string `json:"product_name"` // "Windows 10 Pro"
	Build       string `json:"build"`        // "19045.4651"
	Edition     string `json:"edition"`      // "Professional"
	Arch        string `json:"arch"`         // x64 | x86 | arm64
}

// Hardware is the box itself.
type Hardware struct {
	Manufacturer string `json:"manufacturer"`
	Model        string `json:"model"`
	Serial       string `json:"serial"`
	CPU          string `json:"cpu"`
	RAMBytes     int64  `json:"ram_bytes"`
	Disks        []Disk `json:"disks,omitempty"`
}

// Disk is one drive as the source machine sees it.
type Disk struct {
	Index      int    `json:"index"`
	Model      string `json:"model,omitempty"`
	SizeBytes  int64  `json:"size_bytes"`
	Partitions int    `json:"partitions,omitempty"`
	BusType    string `json:"bus_type,omitempty"`
}

// UserProf is a profile found on the source machine. It is how the review
// decides whose data to carry over, so last-logon matters more than the path.
type UserProf struct {
	Account   string     `json:"account"` // DOMAIN\user, or .\user for a local account
	Profile   string     `json:"profile"` // C:\Users\...
	LastLogon *time.Time `json:"last_logon,omitempty"`
	SizeBytes int64      `json:"size_bytes,omitempty"`
	Local     bool       `json:"local,omitempty"`
}

// Target is what to build. The password for LocalAdmin is asked for at build
// time and never written here; see the note on Data.StorePath as well.
type Target struct {
	OSEdition   string          `json:"os_edition"`
	Hostname    string          `json:"hostname"`
	LocalAdmin  string          `json:"local_admin"`
	Timezone    string          `json:"timezone"`
	Locale      string          `json:"locale"`
	DskyOptions json.RawMessage `json:"dsky_options,omitempty"`
}

// Identity is domain membership as instructions. The offline join blob is
// produced at build time and named here; a credential never appears.
type Identity struct {
	DomainFQDN     string       `json:"domain_fqdn"`
	DomainNetBIOS  string       `json:"domain_netbios"`
	ComputerOUDN   string       `json:"computer_ou_dn"`
	ComputerGroups []string     `json:"computer_groups,omitempty"`
	LocalGroups    []LocalGroup `json:"local_groups,omitempty"`
	ODJBlobPath    string       `json:"odj_blob_path,omitempty"`
}

// Domained reports whether this machine is in a domain at all.
func (i Identity) Domained() bool { return strings.TrimSpace(i.DomainFQDN) != "" }

// LocalGroup is a local group and who is in it, with the well-known members
// (BUILTIN\..., NT AUTHORITY\...) left out by the scanner: they are on every
// machine and restoring them says nothing.
type LocalGroup struct {
	Name    string   `json:"name"`
	Members []string `json:"members"`
}

// App source kinds.
const (
	SourceWin32 = "win32"
	SourceAppx  = "appx"
	SourceMSIX  = "msix"
	SourceStore = "store"
)

// Resolution statuses. An empty status means the resolver has not looked at
// this application yet; it is not the same as unmapped, which means it looked
// and found nothing.
const (
	StatusUnset    = ""
	StatusResolved = "resolved"
	StatusUnmapped = "unmapped"
	StatusBlocked  = "blocked"
	StatusDropped  = "dropped"
)

// Install methods. Manual is not an install: it becomes a line on the
// checklist the new machine prints when it is done.
const (
	MethodWinget = "winget"
	MethodMSI    = "msi"
	MethodEXE    = "exe"
	MethodMSIX   = "msix"
	MethodScript = "script"
	MethodManual = "manual"
)

// How a resolution was arrived at.
const (
	ByAuto     = "auto"
	ByMapping  = "mapping_table"
	ByOperator = "operator"
)

// Install order. Runtimes and database engines go first because the things
// that need them fail quietly when they are missing.
const (
	OrderPrereq  = 10
	OrderDefault = 50
	OrderMax     = 99
)

// App is one application found on the source machine.
type App struct {
	ID              string `json:"id"`
	DisplayName     string `json:"display_name"`
	DisplayVersion  string `json:"display_version,omitempty"`
	Publisher       string `json:"publisher,omitempty"`
	SourceKind      string `json:"source_kind"`
	InstallLocation string `json:"install_location,omitempty"`
	Arch            string `json:"arch,omitempty"`
	RegistryKey     string `json:"registry_key,omitempty"`

	Resolution    Resolution     `json:"resolution"`
	ConfigCapture *ConfigCapture `json:"config_capture"`
}

// Resolution is how this application is to be installed on the new machine.
type Resolution struct {
	Status     string  `json:"status"`
	Method     string  `json:"method,omitempty"`
	Ref        string  `json:"ref,omitempty"` // winget id, UNC path, URL, script name
	Confidence float64 `json:"confidence,omitempty"`
	ResolvedBy string  `json:"resolved_by,omitempty"`
	// Note is why, when the answer needs one: "Edge comes with Windows",
	// "needs the SQL Express instance first". It comes from whichever mapping
	// table answered, and it is what the report shows beside an application
	// nobody is installing -- "left behind, decided by mapping_table" is a
	// fact without a reason, which is the sort of line that gets queried.
	Note  string   `json:"note,omitempty"`
	Args  []string `json:"args,omitempty"`
	Order int      `json:"order,omitempty"`
}

// Installable reports whether the runner will try to install this.
func (r Resolution) Installable() bool {
	return r.Status == StatusResolved && r.Method != MethodManual
}

// ConfigCapture is the files and registry keys that hold an application's
// own settings. It is only ever filled in from a curated recipe for a known
// application -- guessing where an arbitrary program keeps its configuration
// produces junk, and junk restored over a fresh install breaks it.
type ConfigCapture struct {
	Paths        []string `json:"paths"`
	RegistryKeys []string `json:"registry_keys"`
}

// Setting is one allowlisted setting: what it was on the old machine, where
// that was read from, and how (or whether) it can be set on the new one.
type Setting struct {
	Key          string                 `json:"key"`
	Value        json.RawMessage        `json:"value"`
	CapturedFrom string                 `json:"captured_from"`
	Apply        map[string]ApplyMethod `json:"apply"`
}

// Appliable reports whether this setting can be applied on the target OS.
// A setting with no method is still in the manifest, and still in the report,
// so that nobody assumes it came across.
func (s Setting) Appliable(osName string) bool {
	m, ok := s.Apply[osName]
	return ok && m.Method != ""
}

// ApplyMethod is how a setting is put back.
type ApplyMethod struct {
	Method string `json:"method"` // registry | powershell | gpo
	Ref    string `json:"ref"`
}

// Setting apply methods.
const (
	ApplyRegistry   = "registry"
	ApplyPowerShell = "powershell"
	ApplyGPO        = "gpo"
)

// Peripherals is the hardware around the machine rather than in it.
type Peripherals struct {
	Printers     []Printer     `json:"printers,omitempty"`
	MappedDrives []MappedDrive `json:"mapped_drives,omitempty"`
	Network      Network       `json:"network"`
}

// Printer is one queue. A network queue needs only its share path; a
// directly-attached IP printer needs the driver and the port.
type Printer struct {
	Name       string `json:"name"`
	DriverName string `json:"driver_name,omitempty"`
	Port       string `json:"port,omitempty"`
	IP         string `json:"ip,omitempty"`
	SharedPath string `json:"shared_path,omitempty"`
}

// MappedDrive is a drive letter a user had. It belongs to a user, not to the
// machine, which is why the runner stages it as a logon script rather than
// mapping it as the local administrator.
type MappedDrive struct {
	Letter     string `json:"letter"`
	UNC        string `json:"unc"`
	Persistent bool   `json:"persistent"`
	User       string `json:"user,omitempty"`
}

// Network is what can be carried across. Wi-Fi keys cannot: they are held for
// the machine, so only the names come over, as something to re-enter.
type Network struct {
	WiFiSSIDs []string   `json:"wifi_ssids,omitempty"`
	StaticIPs []StaticIP `json:"static_ips,omitempty"`
}

// StaticIP is an adapter with a fixed address. It is reported for review
// rather than applied: adapter names and counts differ on the new machine.
type StaticIP struct {
	Adapter string   `json:"adapter"`
	Address string   `json:"address"`
	Prefix  int      `json:"prefix"`
	Gateway string   `json:"gateway,omitempty"`
	DNS     []string `json:"dns,omitempty"`
}

// Data migration strategies.
const (
	DataUSMT      = "usmt"
	DataKFM       = "onedrive_kfm"
	DataRedirect  = "folder_redirection"
	DataNoneStrat = "none"
)

// Data is the plan for the users' own files. StorePath names a share; the
// credentials for it are asked for at build time and never stored.
type Data struct {
	Strategy  string   `json:"strategy"`
	StorePath string   `json:"store_path,omitempty"`
	Users     []string `json:"users,omitempty"`
	Encrypted bool     `json:"encrypted,omitempty"`
}

// Compat severities.
const (
	Blocker = "blocker"
	Warning = "warning"
	Info    = "info"
)

// Compat is something the scanner noticed that a person has to decide about.
// A blocker stops approval until it is acknowledged or the application is
// dropped.
type Compat struct {
	Severity        string `json:"severity"`
	Subject         string `json:"subject"`
	Reason          string `json:"reason"`
	SuggestedAction string `json:"suggested_action,omitempty"`
	Acknowledged    bool   `json:"acknowledged,omitempty"`
}

// ── reading and writing ──────────────────────────────────────────────────────

// Load reads and checks a manifest. Unknown fields are an error: a manifest
// written by a newer DSKY may mean something this build would ignore, and
// ignoring half of a migration plan is worse than refusing it.
func Load(path string) (*Manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	m, err := Parse(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return m, nil
}

// Parse reads a manifest from bytes.
func Parse(b []byte) (*Manifest, error) {
	dec := json.NewDecoder(strings.NewReader(strings.TrimPrefix(string(b), "\ufeff")))
	dec.DisallowUnknownFields()
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("not a manifest DSKY understands: %w", err)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	// A manifest from an older 1.x is this build's shape as soon as it is
	// read: every field this build knows is filled in from here on, and the
	// version it declares has to say so. Stamping it at the other end --
	// when it is written -- would change the file after it was approved, and
	// approval is a hash of the file.
	if supportedVersion(m.SchemaVersion) {
		m.SchemaVersion = SchemaVersion
	}
	return &m, nil
}

// Save writes the manifest, checked first, indented so that a person reading
// or diffing the file can follow it, and replaced in one step so that an
// interrupted write cannot leave half a plan behind.
//
// The version is stamped when a manifest is read, not here, so that writing
// one never changes what was approved.
func (m *Manifest) Save(path string) error {
	if m.SchemaVersion == "" {
		m.SchemaVersion = SchemaVersion
	}
	if err := m.Validate(); err != nil {
		return err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ── checking ─────────────────────────────────────────────────────────────────

var idRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

func supportedVersion(v string) bool {
	major, _, ok := strings.Cut(v, ".")
	return ok && major == "1"
}

// Validate enforces what the format means, in the words a person reading the
// error can act on. It is deliberately structural: whether a plan is a *good*
// plan is the review's job, and advice that is not an error belongs in Lint.
func (m *Manifest) Validate() error {
	fail := func(format string, args ...any) error {
		return fmt.Errorf("manifest: "+format, args...)
	}
	switch {
	case m.SchemaVersion == "":
		return fail("schema_version is required (this build writes %s)", SchemaVersion)
	case !supportedVersion(m.SchemaVersion):
		return fail("schema_version %q is from a later DSKY — update DSKY to read it", m.SchemaVersion)
	case m.GeneratedAt.IsZero():
		return fail("generated_at is required")
	case m.GeneratedBy == "":
		return fail("generated_by is required, so a manifest says what wrote it")
	}

	seen := map[string]bool{}
	for i, a := range m.Apps {
		where := fmt.Sprintf("apps[%d]", i)
		if a.DisplayName != "" {
			where = fmt.Sprintf("apps[%d] (%s)", i, a.DisplayName)
		}
		switch {
		case a.ID == "":
			return fail("%s: id is required", where)
		case !idRe.MatchString(a.ID):
			return fail("%s: id %q must be lower-case letters, digits, dot, dash or underscore", where, a.ID)
		case seen[a.ID]:
			return fail("%s: id %q is used twice", where, a.ID)
		case a.DisplayName == "":
			return fail("%s: display_name is required", where)
		}
		seen[a.ID] = true
		switch a.SourceKind {
		case SourceWin32, SourceAppx, SourceMSIX, SourceStore:
		default:
			return fail("%s: source_kind %q is not one of win32, appx, msix, store", where, a.SourceKind)
		}
		if err := a.Resolution.validate(where, fail); err != nil {
			return err
		}
		if c := a.ConfigCapture; c != nil && len(c.Paths) == 0 && len(c.RegistryKeys) == 0 {
			return fail("%s: config_capture is empty — leave it null instead", where)
		}
	}

	keys := map[string]bool{}
	for i, s := range m.Settings {
		where := fmt.Sprintf("settings[%d]", i)
		switch {
		case s.Key == "":
			return fail("%s: key is required", where)
		case keys[s.Key]:
			return fail("%s: key %q is captured twice", where, s.Key)
		case len(s.Value) == 0:
			return fail("%s (%s): value is required", where, s.Key)
		case s.CapturedFrom == "":
			return fail("%s (%s): captured_from is required, so the report can show where the value came from", where, s.Key)
		}
		keys[s.Key] = true
		for osName, ap := range s.Apply {
			switch ap.Method {
			case "", ApplyRegistry, ApplyPowerShell, ApplyGPO:
			default:
				return fail("%s (%s): apply.%s.method %q is not one of registry, powershell, gpo", where, s.Key, osName, ap.Method)
			}
			if ap.Method != "" && ap.Ref == "" {
				return fail("%s (%s): apply.%s needs a ref saying what to set", where, s.Key, osName)
			}
		}
	}

	switch m.Data.Strategy {
	// Where a USMT store lives and whose files go in it are decisions made in
	// review, not readings taken by a scan, so a manifest without them is
	// still a valid manifest -- just not an approvable one. ReadyToApprove
	// has that rule.
	case DataUSMT, DataKFM, DataRedirect, DataNoneStrat:
	case "":
		return fail("data.strategy is required (usmt, onedrive_kfm, folder_redirection or none)")
	default:
		return fail("data.strategy %q is not one of usmt, onedrive_kfm, folder_redirection, none", m.Data.Strategy)
	}

	for i, c := range m.Compat {
		switch c.Severity {
		case Blocker, Warning, Info:
		default:
			return fail("compat[%d]: severity %q is not one of blocker, warning, info", i, c.Severity)
		}
		if c.Reason == "" {
			return fail("compat[%d] (%s): reason is required", i, c.Subject)
		}
	}

	for i, g := range m.Identity.LocalGroups {
		if g.Name == "" {
			return fail("identity.local_groups[%d]: name is required", i)
		}
	}
	if m.Identity.ComputerOUDN != "" && !m.Identity.Domained() {
		return fail("identity.computer_ou_dn is set but identity.domain_fqdn is empty")
	}

	for i, p := range m.Peripherals.Printers {
		if p.Name == "" {
			return fail("peripherals.printers[%d]: name is required", i)
		}
	}
	for i, d := range m.Peripherals.MappedDrives {
		switch {
		case !driveLetterRe.MatchString(d.Letter):
			return fail("peripherals.mapped_drives[%d]: letter %q should look like \"H:\"", i, d.Letter)
		case !strings.HasPrefix(d.UNC, `\\`):
			return fail("peripherals.mapped_drives[%d] (%s): unc %q is not a \\\\server\\share path", i, d.Letter, d.UNC)
		}
	}

	return m.Approval.validate(fail)
}

var driveLetterRe = regexp.MustCompile(`^[A-Za-z]:$`)

func (r Resolution) validate(where string, fail func(string, ...any) error) error {
	switch r.Status {
	case StatusUnset, StatusUnmapped, StatusDropped, StatusBlocked, StatusResolved:
	default:
		return fail("%s: resolution.status %q is not one of resolved, unmapped, blocked, dropped (or empty for not yet looked at)", where, r.Status)
	}
	switch r.Method {
	case "", MethodWinget, MethodMSI, MethodEXE, MethodMSIX, MethodScript, MethodManual:
	default:
		return fail("%s: resolution.method %q is not one of winget, msi, exe, msix, script, manual", where, r.Method)
	}
	switch r.ResolvedBy {
	case "", ByAuto, ByMapping, ByOperator:
	default:
		return fail("%s: resolution.resolved_by %q is not one of auto, mapping_table, operator", where, r.ResolvedBy)
	}
	if r.Status == StatusResolved {
		if r.Method == "" {
			return fail("%s: resolved, so resolution.method is required", where)
		}
		if r.Method != MethodManual && r.Ref == "" {
			return fail("%s: resolved as %s, so resolution.ref is required", where, r.Method)
		}
		if r.ResolvedBy == "" {
			return fail("%s: resolved, so resolution.resolved_by must say by what", where)
		}
	}
	if r.Confidence < 0 || r.Confidence > 1 {
		return fail("%s: resolution.confidence %v is not between 0 and 1", where, r.Confidence)
	}
	if r.Order < 0 || r.Order > OrderMax {
		return fail("%s: resolution.order %d is not between 0 and %d", where, r.Order, OrderMax)
	}
	return nil
}

func (a Approval) validate(fail func(string, ...any) error) error {
	if !a.Approved {
		return nil
	}
	switch {
	case a.ApprovedBy == "":
		return fail("approval.approved is true but approved_by is empty")
	case a.ApprovedAt == nil || a.ApprovedAt.IsZero():
		return fail("approval.approved is true but approved_at is empty")
	case a.ContentHash == "":
		return fail("approval.approved is true but content_hash is empty")
	}
	return nil
}

// ── reading the plan ─────────────────────────────────────────────────────────

// Counts is the shape of a manifest at a glance: the numbers the report puts
// at the top and the review uses to decide whether there is work left.
type Counts struct {
	Apps, Resolved, Unmapped, Blocked, Dropped, Manual int
	Blockers, UnackedBlockers, Warnings                int
	Settings, SettingsApplied                          int
	Printers, Drives                                   int
}

// Count summarises the manifest for the OS the target will run.
func (m *Manifest) Count(osName string) Counts {
	c := Counts{Apps: len(m.Apps), Settings: len(m.Settings)}
	for _, a := range m.Apps {
		switch a.Resolution.Status {
		case StatusResolved:
			c.Resolved++
			if a.Resolution.Method == MethodManual {
				c.Manual++
			}
		case StatusUnmapped, StatusUnset:
			c.Unmapped++
		case StatusBlocked:
			c.Blocked++
		case StatusDropped:
			c.Dropped++
		}
	}
	for _, x := range m.Compat {
		switch x.Severity {
		case Blocker:
			c.Blockers++
			if !x.Acknowledged {
				c.UnackedBlockers++
			}
		case Warning:
			c.Warnings++
		}
	}
	for _, s := range m.Settings {
		if s.Appliable(osName) {
			c.SettingsApplied++
		}
	}
	c.Printers = len(m.Peripherals.Printers)
	c.Drives = len(m.Peripherals.MappedDrives)
	return c
}

// InstallOrder is the applications to install, in the order to install them:
// prerequisites first, then by name so that two runs of the same manifest
// install in the same order and a log can be compared with an earlier one.
//
// One package installs once. A machine that registered 7-Zip in both hives
// has two applications in the manifest -- both true, both worth showing in
// the report -- resolving to one winget id, and installing it twice would
// waste a minute of somebody's first boot to be told it is already there.
func (m *Manifest) InstallOrder() []App {
	var out []App
	seen := map[string]bool{}
	for _, a := range m.Apps {
		if !a.Resolution.Installable() {
			continue
		}
		key := a.Resolution.Method + "|" + strings.ToLower(a.Resolution.Ref) + "|" + strings.Join(a.Resolution.Args, " ")
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, a)
	}
	sort.SliceStable(out, func(i, j int) bool {
		oi, oj := out[i].Resolution.Order, out[j].Resolution.Order
		if oi == 0 {
			oi = OrderDefault
		}
		if oj == 0 {
			oj = OrderDefault
		}
		if oi != oj {
			return oi < oj
		}
		return strings.ToLower(out[i].DisplayName) < strings.ToLower(out[j].DisplayName)
	})
	return out
}

// Manual is the applications a person has to install or licence by hand, for
// the checklist the new machine prints when the runner has finished.
func (m *Manifest) Manual() []App {
	var out []App
	for _, a := range m.Apps {
		if a.Resolution.Status == StatusResolved && a.Resolution.Method == MethodManual {
			out = append(out, a)
		}
	}
	return out
}

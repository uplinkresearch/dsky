package migrate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Scanning is reading a machine that somebody still works on. Nothing is
// installed, nothing is changed, and the whole of it is one pass of reads
// that finishes in the time it takes to fetch a coffee -- an office PC is
// scanned during business hours, with its user standing there.
//
// The rules that shape what is read:
//
//   - Never Win32_Product. Enumerating it makes the installer of every MSI
//     product on the machine reconfigure itself, which takes minutes and
//     writes to the machine we promised not to touch. The uninstall registry
//     keys say the same thing and cost nothing.
//   - Read, decide later. The scanner records what it saw and what that means
//     for the rebuild; it does not resolve where software comes from (that is
//     the resolver, which needs the network) and it does not decide what to
//     migrate (that is the person reviewing).
//   - A source that fails is a note, not a failure. A machine with a broken
//     WMI repository still has readable registry hives, and a list of
//     applications with a line saying "printers could not be read" is worth
//     far more than no manifest at all.
//
// This file is the part that has no Windows in it: the raw readings come from
// a Collector (scan_windows.go on Windows, an error everywhere else), and
// everything that turns readings into the manifest is here, where it can be
// tested against the shapes real machines produce.

// ScanOptions are the choices a person makes when starting a scan.
type ScanOptions struct {
	// AllUsers loads the registry hives of users who are not signed in, for
	// their per-user applications and mapped drives. It is the slow part of a
	// scan, so it is asked for rather than assumed.
	AllUsers bool
	// ProfileSizes measures each profile. On a machine with a 50 GB profile
	// this doubles the scan, so it is off unless somebody wants the numbers
	// to plan a USMT store.
	ProfileSizes bool
	// Hostname for the new machine, when it is not the old machine's name.
	Hostname string
	// LocalAdmin is the account DSKY creates on the new machine.
	LocalAdmin string
	// Since is how recently a profile must have been used to be offered for
	// data migration. Ninety days is the spec's default: a profile nobody has
	// signed into since last quarter is usually somebody who left.
	Since time.Duration
	// GeneratedBy is what wrote the manifest, version and all: a manifest
	// that cannot say which build scanned the machine is a manifest nobody
	// can reproduce.
	GeneratedBy string
	// Now is the clock, for tests.
	Now func() time.Time
}

func (o *ScanOptions) defaults() {
	if o.GeneratedBy == "" {
		o.GeneratedBy = "dsky-scan"
	}
	if o.Since == 0 {
		o.Since = 90 * 24 * time.Hour
	}
	if o.LocalAdmin == "" {
		o.LocalAdmin = "uplink"
	}
	if o.Now == nil {
		o.Now = time.Now
	}
}

// RawApp is one application as the machine describes itself, before anything
// is decided about it.
type RawApp struct {
	DisplayName      string
	DisplayVersion   string
	Publisher        string
	InstallLocation  string
	RegistryKey      string
	Arch             string // x64 | x86 | arm64, from the hive it was found in
	SourceKind       string // win32 | appx | msix | store
	SystemComponent  bool   // set by Windows on pieces of other products
	WindowsInstaller bool
	User             string // the profile it belongs to, for per-user entries
	Framework        bool   // an Appx runtime other packages depend on
	Inbox            bool   // shipped with Windows
}

// RawIdentity is the machine's place in a domain, as read.
type RawIdentity struct {
	DomainFQDN    string
	DomainNetBIOS string
	OUDN          string
	Groups        []string
	LocalGroups   []LocalGroup
}

// RawSetting is one allowlisted setting as read, with where it came from.
type RawSetting struct {
	Key   string
	Value any
	From  string
}

// RawHints are the things that decide how user data moves.
type RawHints struct {
	OneDriveKFM       bool
	RedirectedFolders []string // "Documents -> \\fs\home\user\Documents"
	USMTPath          string   // where scanstate was found, if it was
}

// Noter is a collector that has smaller failures to report -- one unreadable
// user hive out of five -- which belong in the manifest without making the
// whole scan partial.
type Noter interface {
	Notes() []string
	Timings() map[string]float64
}

// Collector reads one machine. Every method may fail on its own; a scan
// reports what failed and keeps the rest.
type Collector interface {
	OS() (SourceOS, error)
	Hardware() (Hardware, error)
	Apps(allUsers bool) ([]RawApp, error)
	Identity() (RawIdentity, error)
	Printers() ([]Printer, error)
	MappedDrives(allUsers bool) ([]MappedDrive, error)
	Network() (Network, error)
	Settings() ([]RawSetting, error)
	Profiles(sizes bool) ([]UserProf, error)
	Hints() (RawHints, error)
	UnsignedDrivers() ([]string, error)
	Hostname() (string, error)
	ScannerUser() string
}

// Scan reads a machine and returns the manifest, plus what could not be read.
// It is never an error for a single source to fail: the manifest carries a
// note for each, and the caller decides what to make of that (the command
// exits 3, so a script can tell a complete scan from a partial one).
func Scan(c Collector, opts ScanOptions) (*Manifest, []error) {
	opts.defaults()
	started := opts.Now()
	m := &Manifest{
		SchemaVersion: SchemaVersion,
		GeneratedAt:   started.UTC().Truncate(time.Second),
		GeneratedBy:   opts.GeneratedBy,
		Apps:          []App{},
		Settings:      []Setting{},
		Notes:         []string{},
		Compat:        []Compat{},
	}
	var errs []error
	note := func(what string, err error) {
		errs = append(errs, fmt.Errorf("%s: %w", what, err))
		m.Notes = append(m.Notes, fmt.Sprintf("%s could not be read: %v", what, err))
	}

	if host, err := c.Hostname(); err != nil {
		note("the computer name", err)
	} else {
		m.Source.Hostname = host
	}
	m.Source.ScannerUser = c.ScannerUser()

	if os, err := c.OS(); err != nil {
		note("the Windows version", err)
	} else {
		m.Source.OS = os
	}
	if hw, err := c.Hardware(); err != nil {
		note("the hardware", err)
	} else {
		m.Source.Hardware = hw
	}
	if raw, err := c.Apps(opts.AllUsers); err != nil {
		note("the installed applications", err)
	} else {
		m.Apps = AppsFrom(raw)
	}
	if id, err := c.Identity(); err != nil {
		note("the domain membership", err)
	} else {
		m.Identity = IdentityFrom(id, m.Source.Hostname)
	}
	if p, err := c.Printers(); err != nil {
		note("the printers", err)
	} else {
		m.Peripherals.Printers = p
	}
	if d, err := c.MappedDrives(opts.AllUsers); err != nil {
		note("the mapped drives", err)
	} else {
		m.Peripherals.MappedDrives = d
	}
	if n, err := c.Network(); err != nil {
		note("the network configuration", err)
	} else {
		m.Peripherals.Network = n
	}
	if s, err := c.Settings(); err != nil {
		note("the settings", err)
	} else {
		m.Settings = SettingsFrom(s)
	}
	if u, err := c.Profiles(opts.ProfileSizes); err != nil {
		note("the user profiles", err)
	} else {
		m.Source.Users = u
	}
	hints, err := c.Hints()
	if err != nil {
		note("where the user's files are kept", err)
	}
	m.Data = DataPlan(hints, m.Source.Users, opts)

	drivers, err := c.UnsignedDrivers()
	if err != nil {
		note("the driver signatures", err)
	}

	m.Target = Target{
		OSEdition:  m.Source.OS.Edition,
		Hostname:   firstNonEmpty(opts.Hostname, m.Source.Hostname),
		LocalAdmin: opts.LocalAdmin,
	}

	// What the machine itself said it could not read, and how long each
	// reading took: the first belongs in the plan, the second in the log.
	if n, ok := c.(Noter); ok {
		m.Notes = append(m.Notes, n.Notes()...)
	}
	// The target starts as the machine was, settings included: a replacement
	// PC in the wrong time zone is noticed within the hour.
	for _, s := range m.Settings {
		var v string
		if err := json.Unmarshal(s.Value, &v); err != nil {
			continue
		}
		switch s.Key {
		case KeyTimezone:
			m.Target.Timezone = v
		case KeyLocale:
			m.Target.Locale = v
		}
	}
	m.Compat = append(m.Compat, CompatRules(m, drivers, hints)...)
	m.Source.ScanDurationS = opts.Now().Sub(started).Seconds()
	return m, errs
}

func firstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if strings.TrimSpace(x) != "" {
			return x
		}
	}
	return ""
}

// ── applications ─────────────────────────────────────────────────────────────

// appxInbox is what Windows ships and what a fresh Windows 11 will have
// anyway. Migrating these means asking the Store to reinstall software that
// is already there, and listing them means a review with sixty rows nobody
// needs to read.
var appxInbox = regexp.MustCompile(`(?i)^(Microsoft\.(Windows|VCLibs|NET\.|UI\.Xaml|Services\.Store|Advertising|AsyncTextService|BioEnrollment|CredDialogHost|ECApp|LockApp|Media|MicrosoftEdge|OneConnect|Paint|People|SecHealthUI|StorePurchaseApp|Todos|Wallet|Xbox|YourPhone|ZuneMusic|ZuneVideo|GetHelp|Getstarted|MSPaint|Office\.OneNote|SkypeApp|StickyNotes|WebMediaExtensions|WebpImageExtension|HEIFImageExtension|VP9VideoExtensions|ScreenSketch|MixedReality|3DBuilder|Print3D|Messaging|OneDriveSync|People)|windows\.|MicrosoftWindows\.|NcsiUwpApp|InputApp|Windows\.)`)

// windowsUpdate matches the entries Windows' own servicing leaves in the
// uninstall keys. They are not applications anybody installed and a rebuilt
// machine gets its own updates; listing them pads a report a customer reads.
var windowsUpdate = regexp.MustCompile(`(?i)^(security update|update|hotfix|service pack) for |\(KB\d{6,}\)$|^Microsoft Update Health Tools$`)

// AppsFrom turns registry and package readings into the manifest's list: what
// a person would recognise from Settings -> Apps, and nothing else.
func AppsFrom(raw []RawApp) []App {
	out := []App{}
	seen := map[string]bool{}
	ids := map[string]bool{}
	for _, r := range raw {
		name := strings.Join(strings.Fields(r.DisplayName), " ")
		switch {
		case name == "":
			// An uninstall key with no display name is a fragment of another
			// product, not something anybody installed.
			continue
		case r.SystemComponent:
			continue
		case r.Framework, r.Inbox:
			continue
		case appxInbox.MatchString(name) && r.SourceKind != SourceWin32:
			continue
		case windowsUpdate.MatchString(name):
			continue
		}
		// One product is often registered in both hives -- Chrome writes a
		// 64-bit and a 32-bit key for the same install -- and per-user
		// software appears once per signed-in person. It is one application
		// to reinstall either way, and the first reading wins, which is the
		// 64-bit one because that hive is read first.
		key := strings.ToLower(name + "|" + r.DisplayVersion)
		if seen[key] {
			continue
		}
		seen[key] = true
		kind := r.SourceKind
		if kind == "" {
			kind = SourceWin32
		}
		out = append(out, App{
			ID:              uniqueID(appID(name, r.Publisher), ids),
			DisplayName:     name,
			DisplayVersion:  strings.TrimSpace(r.DisplayVersion),
			Publisher:       strings.TrimSpace(r.Publisher),
			SourceKind:      kind,
			InstallLocation: strings.TrimRight(strings.TrimSpace(r.InstallLocation), `\`),
			Arch:            r.Arch,
			RegistryKey:     r.RegistryKey,
			// No resolution yet: an empty status means nobody has looked,
			// which is what the resolver looks for.
			Resolution: Resolution{},
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].DisplayName) < strings.ToLower(out[j].DisplayName)
	})
	return out
}

var idTrim = regexp.MustCompile(`[^a-z0-9]+`)

// appID is a short, stable, readable name for an application: the display
// name reduced to letters and digits. Readable matters because these ids end
// up in mapping tables, compat entries and the verify diff, where somebody
// has to recognise them.
func appID(name, publisher string) string {
	id := idTrim.ReplaceAllString(strings.ToLower(name), "")
	if len(id) > 28 {
		id = id[:28]
	}
	if id == "" || id[0] < 'a' && (id[0] < '0' || id[0] > '9') {
		sum := sha256.Sum256([]byte(name + "|" + publisher))
		id = "app" + hex.EncodeToString(sum[:2])
	}
	return id
}

func uniqueID(id string, taken map[string]bool) string {
	if !taken[id] {
		taken[id] = true
		return id
	}
	for i := 2; ; i++ {
		try := fmt.Sprintf("%s-%d", id, i)
		if !taken[try] {
			taken[try] = true
			return try
		}
	}
}

// ── identity ─────────────────────────────────────────────────────────────────

// wellKnown are the group members every Windows machine has, whether it has
// ever been touched or not: the service identities, the disabled built-in
// accounts, and the two memberships a domain join creates by itself. None of
// them says anything about this machine and none needs recreating -- what
// matters is the domain group somebody added by hand.
//
// The first real machine this ran on produced four local groups, every one of
// them Windows' own defaults, which is noise in a report a customer reads.
var wellKnown = regexp.MustCompile(`(?i)^(NT AUTHORITY\\|NT SERVICE\\|BUILTIN\\|S-1-5-(32|18|19|20)|Everyone$|CREATOR OWNER$)`)

// machineDefault matches a member that is this machine's own built-in account
// (Guest, DefaultAccount, WDAGUtilityAccount, and the local Administrator) or
// a domain's automatic membership.
var machineDefault = regexp.MustCompile(`(?i)\\(Guest|DefaultAccount|WDAGUtilityAccount|Administrator)$|\\Domain (Users|Admins|Computers)$`)

// IdentityFrom cleans up the domain reading. hostname is this machine's own
// name, so that its local accounts can be told from the domain's: a local
// account does not migrate -- the new machine gets its own -- and listing one
// as a group member to recreate would be a promise DSKY cannot keep.
func IdentityFrom(r RawIdentity, hostname string) Identity {
	id := Identity{
		DomainFQDN:     strings.TrimSpace(r.DomainFQDN),
		DomainNetBIOS:  strings.ToUpper(strings.TrimSpace(r.DomainNetBIOS)),
		ComputerOUDN:   strings.TrimSpace(r.OUDN),
		ComputerGroups: r.Groups,
	}
	// A machine in no domain reports its own workgroup as the domain; that is
	// not a domain to join.
	if strings.EqualFold(id.DomainFQDN, "WORKGROUP") || !strings.Contains(id.DomainFQDN, ".") {
		id.DomainFQDN, id.DomainNetBIOS, id.ComputerOUDN, id.ComputerGroups = "", "", "", nil
	}
	local := strings.ToLower(strings.TrimSpace(hostname)) + `\`
	for _, g := range r.LocalGroups {
		var keep []string
		for _, mem := range g.Members {
			mem = strings.TrimSpace(mem)
			if wellKnown.MatchString(mem) || machineDefault.MatchString(mem) {
				continue
			}
			if local != `\` && strings.HasPrefix(strings.ToLower(mem), local) {
				continue
			}
			keep = append(keep, mem)
		}
		if len(keep) == 0 {
			continue
		}
		sort.Strings(keep)
		id.LocalGroups = append(id.LocalGroups, LocalGroup{Name: g.Name, Members: keep})
	}
	sort.Slice(id.LocalGroups, func(i, j int) bool { return id.LocalGroups[i].Name < id.LocalGroups[j].Name })
	return id
}

// inboxPrinter reports whether a print queue is one Windows creates for
// itself. Every Windows has Print to PDF, the XPS writer, Fax and (with
// Office) OneNote; listing them as peripherals to recreate is noise, and on
// the first real machine they outnumbered the two real printers three to one.
func inboxPrinter(p Printer) bool {
	name := strings.ToLower(p.Name)
	switch {
	case strings.Contains(name, "print to pdf"), strings.Contains(name, "xps document writer"),
		name == "fax", strings.HasPrefix(name, "onenote"), strings.Contains(name, "send to onenote"):
		return true
	}
	// A queue whose port is a OneNote package or the fax port is the same
	// thing under a name somebody changed.
	port := strings.ToLower(p.Port)
	return strings.Contains(port, "onenoteim") || port == "shrfax:"
}

// ── settings ─────────────────────────────────────────────────────────────────

// SettingsFrom pairs each captured setting with how it is applied on the
// target, from the allowlist. A setting with no way to apply it is kept:
// the report says it was not migrated, which is the whole point of capturing
// it -- somebody has to know to set it again.
func SettingsFrom(raw []RawSetting) []Setting {
	out := []Setting{}
	for _, r := range raw {
		def, known := Allowlist[r.Key]
		if !known {
			// Not on the allowlist means the scanner should not have read it;
			// dropping it here keeps the two in step.
			continue
		}
		b, err := json.Marshal(r.Value)
		if err != nil {
			continue
		}
		s := Setting{Key: r.Key, Value: b, CapturedFrom: firstNonEmpty(r.From, def.CapturedFrom), Apply: map[string]ApplyMethod{}}
		if def.Apply != nil {
			if m := def.Apply(r.Value); m.Method != "" {
				s.Apply["win11"] = m
			}
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// ── user data ────────────────────────────────────────────────────────────────

// DataPlan decides how the user's files move, from what the machine says
// about where they are. OneDrive's Known Folder Move and folder redirection
// both mean the files are not on this disk at all, and copying them would be
// slower and riskier than letting them come back on their own.
func DataPlan(h RawHints, users []UserProf, opts ScanOptions) Data {
	opts.defaults()
	switch {
	case h.OneDriveKFM:
		return Data{Strategy: DataKFM}
	case len(h.RedirectedFolders) > 0:
		return Data{Strategy: DataRedirect}
	}
	// Otherwise the files are local, and USMT is the way. Whose files: the
	// domain profiles somebody has actually used lately. A local account's
	// profile is not offered -- a new machine has new local accounts.
	cut := opts.Now().Add(-opts.Since)
	var who []string
	for _, u := range users {
		if u.Local || u.LastLogon == nil || u.LastLogon.Before(cut) {
			continue
		}
		who = append(who, u.Account)
	}
	if len(who) == 0 {
		// Nobody has signed in lately: a kiosk, a spare, or a machine already
		// replaced. Nothing to carry, and the report says so.
		return Data{Strategy: DataNoneStrat}
	}
	sort.Strings(who)
	return Data{Strategy: DataUSMT, Users: who}
}

// ── what a person has to decide ──────────────────────────────────────────────

// KnownIncompatible is software that does not work on Windows 11, as a
// pattern and the reason. It is deliberately empty: an entry here stops a
// migration, so each one needs a machine it was seen failing on, not a
// rumour. `dsky migrate scan --known-incompatible <file>` adds a site's own.
var KnownIncompatible = []Mapping{}

// CompatRules is everything the scanner noticed that a person has to decide
// about. Each rule exists because of a way a rebuild goes wrong quietly: a
// 32-bit application that has no 64-bit version, a driver nobody can
// re-download, a static address that the new machine will not have.
func CompatRules(m *Manifest, unsignedDrivers []string, h RawHints) []Compat {
	var out []Compat
	has64 := map[string]bool{}
	for _, a := range m.Apps {
		if a.Arch == "x64" {
			has64[strings.ToLower(baseName(a.DisplayName))] = true
		}
	}
	for _, a := range m.Apps {
		if a.Arch != "x86" || has64[strings.ToLower(baseName(a.DisplayName))] {
			continue
		}
		out = append(out, Compat{
			Severity: Warning, Subject: a.ID,
			Reason:          fmt.Sprintf("%s is 32-bit and no 64-bit version of it is installed here.", a.DisplayName),
			SuggestedAction: "Confirm a 64-bit or Windows 11 version exists before the old machine is retired.",
		})
	}
	for _, a := range m.Apps {
		for _, k := range KnownIncompatible {
			if k.matches(a) {
				out = append(out, Compat{
					Severity: Blocker, Subject: a.ID,
					Reason:          fmt.Sprintf("%s is on the known-incompatible list: %s", a.DisplayName, k.Note),
					SuggestedAction: "Accept the risk or drop this application.",
				})
			}
		}
	}
	for _, d := range unsignedDrivers {
		out = append(out, Compat{
			Severity: Warning, Subject: d,
			Reason:          fmt.Sprintf("An unsigned driver is in use (%s).", d),
			SuggestedAction: "Check whether the new machine needs it at all; Windows 11 may refuse to load it.",
		})
	}
	for _, ip := range m.Peripherals.Network.StaticIPs {
		out = append(out, Compat{
			Severity: Warning, Subject: ip.Adapter,
			Reason: fmt.Sprintf("This machine has a static IP (%s/%d). Adapter names differ on new hardware, so DSKY does not copy the configuration.",
				ip.Address, ip.Prefix),
			SuggestedAction: "Set the address by hand after the rebuild, or move the reservation to DHCP.",
		})
	}
	if len(m.Peripherals.Network.WiFiSSIDs) > 0 {
		out = append(out, Compat{
			Severity: Info, Subject: strings.Join(m.Peripherals.Network.WiFiSSIDs, ", "),
			Reason:          "Wi-Fi network names come across; their passwords cannot be read off this machine.",
			SuggestedAction: "Have the Wi-Fi password to hand when the new machine is set up.",
		})
	}
	// A legacy install location with nothing in the 64-bit hive is the shape
	// of software installed years ago whose installer may no longer exist.
	for _, a := range m.Apps {
		if a.Arch == "x86" && strings.Contains(strings.ToLower(a.InstallLocation), `program files (x86)`) && a.RegistryKey != "" &&
			!strings.Contains(a.RegistryKey, "WOW6432Node") && a.SourceKind == SourceWin32 {
			out = append(out, Compat{
				Severity: Info, Subject: a.ID,
				Reason:          fmt.Sprintf("%s is installed in the 32-bit program folder but registered in the 64-bit hive.", a.DisplayName),
				SuggestedAction: "Verify it installs and runs on Windows 11.",
			})
		}
	}
	switch {
	case h.OneDriveKFM:
		out = append(out, Compat{
			Severity: Info, Subject: m.Source.Hostname,
			Reason:          "This machine's files are in OneDrive with Known Folder Move, so nothing is copied — they come back at first sign-in.",
			SuggestedAction: "Confirm the user has signed into OneDrive before the old machine is retired.",
		})
	case len(h.RedirectedFolders) > 0:
		out = append(out, Compat{
			Severity: Info, Subject: m.Source.Hostname,
			Reason:          "Folders are redirected to the file server (" + strings.Join(h.RedirectedFolders, ", ") + "), so nothing is copied.",
			SuggestedAction: "None — the files are already off this machine.",
		})
	case m.Data.Strategy == DataUSMT && h.USMTPath == "":
		out = append(out, Compat{
			Severity: Blocker, Subject: "USMT",
			Reason:          "This machine's files are local, so they need USMT, and scanstate.exe was not found beside the scanner.",
			SuggestedAction: "Supply USMT from the Windows ADK (--usmt <folder>), or choose a different way to move the files in review.",
		})
	}
	// Licences that are bound to a machine are the most common surprise after
	// a rebuild, and the publisher is the only reliable signal.
	for _, a := range m.Apps {
		if machineBoundLicensor.MatchString(a.Publisher) {
			out = append(out, Compat{
				Severity: Info, Subject: a.ID,
				Reason:          fmt.Sprintf("%s licences from %s are usually tied to the machine.", a.DisplayName, a.Publisher),
				SuggestedAction: "Have the account or licence key to hand; the new machine will need activating.",
			})
		}
	}
	return out
}

// machineBoundLicensor are publishers whose licensing commonly binds to the
// hardware it was activated on. Being on this list only adds a line to the
// report; it never stops anything.
var machineBoundLicensor = regexp.MustCompile(`(?i)^(Adobe|Autodesk|Intuit|Sage|Bentley|Trimble|Chief Architect|SolidWorks|Dassault|Ansys|Esri|Environmental Systems Research)`)

// baseName is a product name without its version or its architecture, so
// that "7-Zip 24.08 (x64 edition)" and "7-Zip 24.08" are recognised as one
// product installed twice. Cutting at the first digit is not enough: 7-Zip
// starts with one, and cutting there left every 7-Zip its own product, which
// is how the first real machine came to be warned that its 32-bit 7-Zip had
// no 64-bit version while the 64-bit one sat beside it in the list.
func baseName(name string) string {
	words := strings.Fields(strings.TrimSpace(name))
	var keep []string
	for i, w := range words {
		low := strings.ToLower(strings.Trim(w, "()[],"))
		// A word that is a version, an architecture or a bracketed aside ends
		// the product's name -- unless it is the first word, which is part of
		// the name however it looks ("7-Zip", "1Password").
		if i > 0 && (versionWord.MatchString(low) || archWord[low]) {
			break
		}
		keep = append(keep, w)
	}
	return strings.TrimRight(strings.TrimSpace(strings.Join(keep, " ")), "-,")
}

var versionWord = regexp.MustCompile(`^v?\d+([._]\d+)*$`)

var archWord = map[string]bool{
	"x64": true, "x86": true, "amd64": true, "arm64": true, "32-bit": true, "64-bit": true,
	"bit": true, "edition": true,
}

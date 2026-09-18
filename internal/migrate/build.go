package migrate

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/uplinkresearch/dsky/internal/appcatalog"
)

// Building is where a plan becomes a drive. Everything before this reads,
// decides or describes; this is the step that costs an operator twenty
// minutes and a stick, so it refuses early and loudly rather than producing
// media that is subtly not what was approved.
//
// What it will not do:
//
//   - Build a plan nobody approved, or one edited after approval. The hash is
//     recomputed here, not trusted.
//   - Build a plan with an application nobody placed. "It will sort itself
//     out at first boot" is how a machine arrives missing the software its
//     owner needs.
//   - Put a domain credential on the drive. The join is an offline join file
//     made for this one machine, as the rest of DSKY already does.

// BuildPlan is the manifest expressed as the choices the rest of DSKY
// already takes: which Windows, which programs, which join file. Nothing in
// here decides anything -- every field comes from the approved manifest.
//
// It is plain data rather than the build pipeline's own options type, and
// deliberately so: the first-boot agent has to be able to read a manifest
// (it is the thing that carries one onto a machine), and the build pipeline
// imports the agent. This package must therefore stay clear of the build
// pipeline, and the command line is where the two are joined.
type BuildPlan struct {
	Edition     string // Pro | Home
	AccountMode string // local
	Debloat     string // off | standard | aggressive
	Locale      string
	Timezone    string
	AdminUser   string
	Hostname    string // empty on an offline join: the join file names it
	DomainBlob  string
	Apps        []string // picker ids, winget:<id>, or an operator's installer id

	Manual   []App    // for the checklist, not installed
	Warnings []string // things the operator should know before the build runs
}

// Preflight is every reason not to build, checked before anything is
// downloaded, written or asked for.
func Preflight(m *Manifest) error {
	if err := m.CheckApproved(); err != nil {
		return err
	}
	c := m.Count("win11")
	if c.Unmapped > 0 {
		for _, a := range m.Apps {
			if s := a.Resolution.Status; s == StatusUnmapped || s == StatusUnset {
				return fmt.Errorf("%q still has nowhere to install from — review the plan again (%d like it)",
					a.DisplayName, c.Unmapped)
			}
		}
	}
	if c.UnackedBlockers > 0 {
		return fmt.Errorf("%d blocker(s) are unanswered — review the plan again", c.UnackedBlockers)
	}
	if m.Identity.Domained() && m.Identity.ComputerOUDN == "" {
		return fmt.Errorf("this machine joins %s but the plan does not say which OU — review it and set one",
			m.Identity.DomainFQDN)
	}
	return nil
}

// PlanBuild turns an approved manifest into the build's choices. odjBlob is
// the join file made for this machine, which the caller provisions (it needs
// a domain-joined Windows machine and is not this package's business).
func PlanBuild(m *Manifest, odjBlob string) (*BuildPlan, error) {
	if err := Preflight(m); err != nil {
		return nil, err
	}
	p := &BuildPlan{
		Edition:     edition(m.Target.OSEdition),
		AccountMode: "local",
		Debloat:     "standard",
		Locale:      m.Target.Locale,
		Timezone:    m.Target.Timezone,
		AdminUser:   m.Target.LocalAdmin,
		Hostname:    m.Target.Hostname,
	}
	if m.Identity.Domained() {
		if odjBlob == "" {
			return nil, fmt.Errorf("this machine joins %s, so the build needs its offline join file", m.Identity.DomainFQDN)
		}
		p.DomainBlob = odjBlob
		// No name from here. The blob names the computer, and it does so
		// even though the agent now applies it after OOBE rather than Setup
		// applying it in specialize: a lab machine built with no name of its
		// own (the answer file's default is "*", meaning random) came back
		// from the agent's join as NEWDESK01, in lab.dsky.local, which is the
		// name its computer account was provisioned for.
		//
		// I was about to send the name from here as belt and braces, on the
		// theory that a machine already running has to be renamed by
		// somebody. The lab says djoin does it. Sending a name too is the
		// untested configuration, so it does not go in.
		p.Hostname = ""
	}

	// Applications: winget packages by id, and the operator's own installers
	// by the name the library knows them as. Anything else is a person's job
	// and goes on the checklist instead.
	seen := map[string]bool{}
	for _, a := range m.InstallOrder() {
		switch a.Resolution.Method {
		case MethodWinget:
			id := appcatalog.WingetPrefix + a.Resolution.Ref
			if !seen[id] {
				seen[id] = true
				p.Apps = append(p.Apps, id)
			}
		case MethodMSI, MethodEXE:
			// Staged from the library, which is where the review's installer
			// paths are copied to before a build (see StageInstallers).
			id := installerID(a)
			if !seen[id] {
				seen[id] = true
				p.Apps = append(p.Apps, id)
			}
		case MethodMSIX, MethodScript:
			p.Warnings = append(p.Warnings,
				fmt.Sprintf("%s installs by %s, which the first-boot agent does not run yet; it is on the checklist instead",
					a.DisplayName, a.Resolution.Method))
			p.Manual = append(p.Manual, a)
		}
	}
	p.Manual = append(p.Manual, m.Manual()...)

	// Everything the plan says will not happen, said once more here, because
	// this is the last moment before a drive is written.
	if m.Data.Strategy == DataUSMT {
		p.Warnings = append(p.Warnings, fmt.Sprintf(
			"the user's files are to be copied with USMT from %s — that runs after this drive has installed Windows, not now",
			m.Data.StorePath))
	}
	// A printer with its own address needs a driver that a new Windows 11
	// probably does not have, and that is the one the operator may have to
	// deal with. A shared queue installs its own driver from the server when
	// somebody signs in, so it is not a warning, it is just later.
	var direct, shared int
	for _, pr := range m.Peripherals.Printers {
		if pr.SharedPath != "" {
			shared++
		} else {
			direct++
		}
	}
	if direct > 0 {
		p.Warnings = append(p.Warnings, fmt.Sprintf(
			"%d printer(s) have their own address, so they need their driver on the new machine; any that cannot be added say so in the first-boot log",
			direct))
	}
	if shared > 0 {
		p.Warnings = append(p.Warnings, fmt.Sprintf(
			"%d printer(s) come from a print server, so they are added at each person's first sign-in and not before", shared))
	}
	// Settings that belong to a person rather than the machine are worth
	// saying out loud: they do not appear until that person signs in, so an
	// operator checking the machine at the bench will not see them.
	if _, perUser := SettingActions(m, "win11"); len(perUser) > 0 {
		p.Warnings = append(p.Warnings, fmt.Sprintf(
			"%d setting(s) belong to whoever signs in, so they are applied at each person's first sign-in and not before",
			len(perUser)))
	}
	sort.Strings(p.Warnings)
	return p, nil
}

func appliableSettings(m *Manifest) int {
	n := 0
	for _, s := range m.Settings {
		if s.Appliable("win11") {
			n++
		}
	}
	return n
}

// installerID is what an operator's installer is called in the library: the
// application's id, which is stable and readable in a build log.
func installerID(a App) string { return "migrate-" + a.ID }

// Installer is one file the build has to have in the library before it runs.
type Installer struct {
	App  App
	ID   string
	Path string
	Args []string
}

// Installers are the files the plan installs from a share or a disk. The
// caller copies each into the library (appcatalog.AddInstaller) so that the
// build stages it and the agent runs it -- the same path an operator's own
// .msi takes when they add one by hand.
func Installers(m *Manifest) []Installer {
	var out []Installer
	seen := map[string]bool{}
	for _, a := range m.InstallOrder() {
		switch a.Resolution.Method {
		case MethodMSI, MethodEXE:
		default:
			continue
		}
		id := installerID(a)
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, Installer{App: a, ID: id, Path: a.Resolution.Ref, Args: a.Resolution.Args})
	}
	return out
}

// DjoinCommand is the provisioning command for this machine, as it must be
// run on a domain-joined Windows computer. It is printed rather than guessed
// at: an operator on a Mac or a Linux box needs to run this somewhere else
// and bring the file back.
func DjoinCommand(m *Manifest, savefile string) string {
	return fmt.Sprintf(`djoin /provision /domain %s /machine %s /machineou "%s" /savefile %s /reuse`,
		m.Identity.DomainFQDN, m.Target.Hostname, m.Identity.ComputerOUDN, savefile)
}

// GroupCommands add the new computer to the groups the old one was in. RSAT
// is not on every workstation, so these are printed for somebody to run where
// the tools are, rather than failed over.
func GroupCommands(m *Manifest) []string {
	// Single quotes, not double: a computer account is NAME$, and PowerShell
	// would read the $ in a double-quoted string as the start of a variable
	// and send the group an empty member. Go's %q would escape the backslash
	// in DOMAIN\Group too, which PowerShell does not undo.
	var out []string
	for _, g := range m.Identity.ComputerGroups {
		out = append(out, fmt.Sprintf(`Add-ADGroupMember -Identity '%s' -Members '%s$'`,
			psQuote(g), psQuote(m.Target.Hostname)))
	}
	return out
}

// edition maps what Windows called itself on the old machine to the edition
// the new one installs. A migration is like for like unless somebody says
// otherwise: a practice on Pro does not want Home, and Enterprise media is
// not something this path builds.
func edition(was string) string {
	switch strings.ToLower(strings.TrimSpace(was)) {
	case "core", "home", "corecountryspecific", "coresinglelanguage":
		return "Home"
	case "enterprise", "enterprises", "education":
		// Both install from the same media as Pro here, and a volume-licence
		// key is the operator's to apply; Pro is the honest default.
		return "Pro"
	default:
		return "Pro"
	}
}

// BlobName is what to call the join file for this machine, so that a folder
// of them stays readable.
func BlobName(m *Manifest) string {
	name := m.Target.Hostname
	if name == "" {
		name = m.Source.Hostname
	}
	return filepath.Base(name) + "-odj.txt"
}

// SettingActions splits the plan's settings into what the agent applies to the
// machine and what waits for whoever signs in.
//
// The split is by hive, and it decides whether a migration is any good. Half
// of what somebody notices about their PC lives in HKCU -- file extensions,
// hidden files, the taskbar search box -- and the agent runs as a local
// administrator nobody will ever use. Applying those there would set them for
// the wrong person and leave the real user with a machine that looks nothing
// like the one they had. Those are staged to run at each person's own first
// sign-in instead; a migrated machine is a domain machine, so the people who
// use it are accounts the agent never sees.
//
// Settings with no way to apply them (the default browser, which cannot be
// set on Windows 11 without forging a hash) are not here at all. They stay in
// the manifest and in the report, which is how "set this again by hand"
// reaches a person.
func SettingActions(m *Manifest, osName string) (machine, perUser []SettingAction) {
	for _, s := range m.Settings {
		how, ok := s.Apply[osName]
		if !ok || how.Method == "" {
			continue
		}
		a := SettingAction{Key: s.Key, Method: how.Method, Ref: how.Ref}
		if how.Method == ApplyRegistry && strings.HasPrefix(how.Ref, `HKCU\`) {
			perUser = append(perUser, a)
			continue
		}
		machine = append(machine, a)
	}
	return machine, perUser
}

// SettingAction is one setting, already turned into the thing that sets it.
// Plain data, for the same reason BuildPlan is: this package must not import
// the agent, because the agent reads manifests.
type SettingAction struct {
	Key    string
	Method string
	Ref    string
}

// PrintersAndDrives are the queues and drive letters the old machine had, for
// the build to hand to the agent.
//
// Nothing is decided here beyond what the scanner already recorded: a queue
// with a share path came from a print server and is a per-user connection, one
// with an address is the machine's. That difference is what decides whether
// anybody has to install a driver by hand, which is why the report says it too.
func PrintersAndDrives(m *Manifest) ([]Printer, []MappedDrive) {
	return m.Peripherals.Printers, m.Peripherals.MappedDrives
}

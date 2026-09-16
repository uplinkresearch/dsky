// Package agent is the first-boot provisioning agent: one compiled program
// that installs drivers, removes consumer apps and installs the operator's
// programs on a freshly imaged machine, driven by a manifest DSKY writes
// beside it on the stick.
//
// It exists because generated scripts kept failing on the machine and nowhere
// else. A single em dash in a generated PowerShell file, written as UTF-8
// without a byte-order mark, was read by Windows PowerShell in a legacy code
// page as a curly quote, ended a string early and stopped every program from
// installing. A driver pack handed the wrong switches printed its usage and
// exited 0, and the generated batch file logged that as success because
// %errorlevel% inside a parenthesised block expands when the block is parsed.
// None of those were mistakes in the installer hooks; all of them were in text
// DSKY generated. Text that is assembled per build cannot be tested per build.
// A compiled program can.
package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

// ManifestName is the manifest's filename beside the agent on the stick.
const ManifestName = "dsky-agent.json"

// AgentName is the agent's own filename, which a payload carries a copy of
// and runs once it is on the machine.
const AgentName = "dsky-agent.exe"

// LogName is the human-readable log, kept at the name earlier versions used
// so `verify.ps1` and everything written about it still find it.
const LogName = "firstboot.log"

// EventsName is the machine-readable log: one JSON object per line.
const EventsName = "dsky-agent.jsonl"

// StateName records the steps already finished, so a reboot or a crash
// resumes rather than starting over.
const StateName = "dsky-agent-state.json"

// Manifest is everything the agent does on the machine. It is written by
// compose from the recipe: the agent decides nothing about what an operator
// asked for, only how to carry it out and how to tell the truth about it.
type Manifest struct {
	// Version guards against an agent meeting a manifest it cannot read.
	Version int `json:"version"`
	// Recipe is the recipe id, for the log and the report.
	Recipe string `json:"recipe"`

	Drivers Drivers  `json:"drivers"`
	Debloat *Debloat `json:"debloat,omitempty"`
	Apps    *Apps    `json:"apps,omitempty"`

	// Steps is the order to run in, using the recipe's own step names
	// ("drivers", "debloat", "apps"). Unknown steps are logged and skipped
	// rather than failing the run.
	Steps []string `json:"steps"`

	// VerifyScript, when set, is run at the end (the status screen paints the
	// result on the lock screen). Relative to the agent's own directory.
	VerifyScript string `json:"verify_script,omitempty"`

	// Mode is how the agent is being used, which decides how it treats the
	// machine it is on. Empty is first boot, so every manifest written before
	// this field existed means what it always meant.
	//
	// The same agent does both jobs. What differs is whose machine it is: at
	// first boot nobody is using it yet, so the agent takes the whole screen,
	// clears every desktop shortcut and turns off the automatic sign-in that
	// setup needed. Deployed onto a machine somebody already uses, any of
	// those would be vandalism.
	Mode string `json:"mode,omitempty"`

	// Build names this particular payload, so running it a second time
	// carries on where the first left off and a newer payload starts fresh.
	// Only standalone payloads carry one; at first boot there is only ever
	// one build on the machine.
	Build string `json:"build,omitempty"`
}

// The modes a manifest can ask for.
const (
	ModeFirstBoot  = "firstboot"
	ModeStandalone = "standalone"
)

// Standalone reports whether this manifest is a payload deployed onto a
// machine that is already in use, rather than a machine's first boot.
func (m *Manifest) Standalone() bool { return m.Mode == ModeStandalone }

// Drivers is the driver material staged beside the agent.
type Drivers struct {
	// Cabs are .cab files expanded into Drivers/<Dir> and then swept.
	Cabs []Cab `json:"cabs,omitempty"`
	// Extracts are vendor self-extracting packs.
	Extracts []Extract `json:"extracts,omitempty"`
	// Exes are vendor installers run directly.
	Exes []Exe `json:"exes,omitempty"`
	// Sweep installs everything under Drivers/ with pnputil. Skipped when
	// nothing extracted, because pnputil on an empty folder fails with 87
	// and reads like a real error.
	Sweep bool `json:"sweep"`
}

// Cab is one .cab of INF drivers.
type Cab struct {
	File string `json:"file"`
	Dir  string `json:"dir"`
}

// Extract is one self-extracting driver pack. Args is what the vendor's
// catalog says; AltArgs is the other switch style that vendor has shipped.
//
// Both are tried because the switches are not stable within a vendor: HP
// SoftPaqs took -pdf -e -s -f"dir" for years and current ones take
// /s /e /f <dir>, and a pack given the wrong style prints its usage and
// exits 0. Success is whether driver files appeared, never the exit code.
type Extract struct {
	File    string   `json:"file"`
	Dir     string   `json:"dir"`
	Args    []string `json:"args"`
	AltArgs []string `json:"alt_args,omitempty"`
}

// Exe is a vendor driver installer run directly.
type Exe struct {
	File string   `json:"file"`
	Args []string `json:"args,omitempty"`
	Log  string   `json:"log,omitempty"`
	// OnlyVendor/OnlyModel run it only on that machine, so one stick can
	// carry installers for several models.
	OnlyVendor string `json:"only_vendor,omitempty"`
	OnlyModel  string `json:"only_model,omitempty"`
	// TimeoutMinutes stops an installer that never returns.
	TimeoutMinutes int `json:"timeout_minutes,omitempty"`
}

// Debloat is the consumer-app removal and the policy writes that go with it.
type Debloat struct {
	Preset string `json:"preset"`
	// Apps are Appx family-name prefixes, matched with a trailing wildcard.
	Apps []string `json:"apps"`
}

// Apps is the program install.
type Apps struct {
	// Winget package ids, in the order the operator listed them.
	Winget []string `json:"winget,omitempty"`
	// Scope is "machine" (try machine scope first) or "user".
	Scope string `json:"scope,omitempty"`
	// Installers are the operator's own .msi/.exe files, staged beside the
	// agent and run after the winget packages: they need no network, so if
	// the machine is offline the one that matters still lands.
	Installers []Installer `json:"installers,omitempty"`
}

// Installer is one operator-supplied installer on the stick.
type Installer struct {
	File string   `json:"file"`
	Args []string `json:"args,omitempty"`
	MSI  bool     `json:"msi,omitempty"`
}

// Timeout returns the exe's timeout, or a sane default.
func (e Exe) Timeout() time.Duration {
	if e.TimeoutMinutes <= 0 {
		return 30 * time.Minute
	}
	return time.Duration(e.TimeoutMinutes) * time.Minute
}

// ManifestVersion is what this agent writes and understands.
const ManifestVersion = 1

// LoadManifest reads and checks a manifest.
func LoadManifest(path string) (*Manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	m, err := parseManifest(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return m, nil
}

// parseManifest reads and checks a manifest's bytes, wherever they came from:
// a file beside the agent, or the copy inside a payload that is one file. The
// checks belong here rather than at the file, so a payload carrying a manifest
// this agent cannot read is refused before it unpacks a gigabyte.
func parseManifest(b []byte) (*Manifest, error) {
	var m Manifest
	dec := json.NewDecoder(newTrimmer(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	if m.Version != ManifestVersion {
		return nil, fmt.Errorf("manifest version %d, this agent reads %d", m.Version, ManifestVersion)
	}
	switch m.Mode {
	case "", ModeFirstBoot, ModeStandalone:
	default:
		// An unknown mode is refused rather than guessed at: guessing
		// "first boot" on a machine somebody uses would take over its
		// screen and delete its owner's shortcuts.
		return nil, fmt.Errorf("unknown mode %q", m.Mode)
	}
	if m.Standalone() && m.Build == "" {
		return nil, errors.New("a standalone payload must name its build")
	}
	return &m, nil
}

// Save writes the manifest for compose to stage.
func (m *Manifest) Save(path string) error {
	m.Version = ManifestVersion
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// Package recipe defines the YAML recipe schema — the unit of composition:
// one OS source + target layout + (for Windows) unattend, driver packs,
// payload, and first-boot behavior.
package recipe

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/uplinkresearch/dsky/internal/drivers/catalog"
)

// OSType selects the composition pipeline.
type OSType string

const (
	OSWindows  OSType = "windows"   // extracted/staged FAT32 install media
	OSLinuxISO OSType = "linux-iso" // hybrid ISO, raw-written
	OSRawImg   OSType = "raw-img"   // appliance image, raw-written (may be compressed)
)

// SourceMode says how a Windows OS source is interpreted.
type SourceMode string

const (
	SourceAuto SourceMode = "auto" // iso for .iso blobs, tree for tree_path
	SourceISO  SourceMode = "iso"
	SourceTree SourceMode = "tree" // captured master directory (already split .swm)
)

// Recipe is one buildable media definition.
type Recipe struct {
	Version int    `yaml:"version"`
	ID      string `yaml:"id"`
	Name    string `yaml:"name,omitempty"`

	OS     OSSpec     `yaml:"os"`
	Target TargetSpec `yaml:"target"`

	Windows *WindowsSpec `yaml:"windows,omitempty"`
	Linux   *LinuxSpec   `yaml:"linux,omitempty"`

	Flash         FlashSpec         `yaml:"flash,omitempty"`
	FirmwareNotes string            `yaml:"firmware_notes,omitempty"`
	Vars          map[string]string `yaml:"vars,omitempty"`

	// Path the recipe was loaded from (not serialized).
	Path string `yaml:"-"`
}

type OSSpec struct {
	Source     string     `yaml:"source"` // manifest/library id (iso mode)
	Type       OSType     `yaml:"type"`
	SourceMode SourceMode `yaml:"source_mode,omitempty"`
	// TreePath points at a captured-master directory (tree mode): a working
	// stick copied to disk. Absolute, or relative to the workspace.
	TreePath string `yaml:"tree_path,omitempty"`
}

type TargetSpec struct {
	Scheme      string `yaml:"scheme,omitempty"`       // mbr (default) | gpt
	Filesystem  string `yaml:"filesystem,omitempty"`   // fat32 (only value in v1)
	VolumeLabel string `yaml:"volume_label,omitempty"` // default ESD-USB
	Size        string `yaml:"size,omitempty"`         // auto (default) | "8GiB"
	MinStick    string `yaml:"min_stick,omitempty"`    // informational check at flash time
	Boot        string `yaml:"boot,omitempty"`         // uefi-only (only value in v1)
}

type WindowsSpec struct {
	EICfg        *EICfg         `yaml:"ei_cfg,omitempty"`
	Unattend     *UnattendSpec  `yaml:"unattend,omitempty"`
	WinPEDrivers []string       `yaml:"winpe_drivers,omitempty"` // source refs → $WinpeDriver$/
	DriverPacks  []DriverPack   `yaml:"driver_packs,omitempty"`
	Hardware     []HardwareSpec `yaml:"hardware,omitempty"` // machines whose packs are resolved from catalogs
	Payload      []PayloadItem  `yaml:"payload,omitempty"`
	Debloat      *DebloatSpec   `yaml:"debloat,omitempty"`
	Apps         *AppsSpec      `yaml:"apps,omitempty"`
	WiFi         *WiFiSpec      `yaml:"wifi,omitempty"`
	Domain       *DomainSpec    `yaml:"domain,omitempty"`
	StatusScreen *StatusScreen  `yaml:"status_screen,omitempty"`
	Firstboot    FirstbootSpec  `yaml:"firstboot,omitempty"`
}

// StatusScreen paints the result of the post-install check onto the machine's
// own lock screen and desktop, so a bench of twenty freshly imaged machines
// can be read from the doorway instead of one login at a time.
//
// A failure screen lists what actually failed rather than showing a generic
// stop sign: the point is to know which machine to pick up AND why, without
// sitting down at it.
type StatusScreen struct {
	// Success is an image shown when the machine matches the build — an org
	// logo, normally. Either a workspace-relative path or a library ref. When
	// empty a plain "READY" screen is drawn instead, so this works with no
	// artwork at all.
	Success    string `yaml:"success,omitempty"`
	SuccessRef string `yaml:"success_ref,omitempty"`
	// Keep leaves the screens up until somebody clears them by hand.
	//
	// By default they clear themselves at the next startup, because the
	// screen answers one question for one person, once: the technician looks
	// at the bench, shuts the machine down, and ships it. Whoever opens the
	// box must not find the imaging signal as their wallpaper — least of all
	// a red one. Set this only for a machine that stays on the bench.
	Keep bool `yaml:"keep,omitempty"`
}

// Enabled reports whether a status screen was asked for.
func (s *StatusScreen) Enabled() bool { return s != nil }

// SuccessImage reports the configured success artwork, if any.
func (s *StatusScreen) SuccessImage() (path, ref string) {
	if s == nil {
		return "", ""
	}
	return s.Success, s.SuccessRef
}

// DomainSpec joins the imaged machine to an Active Directory domain. There are
// two ways, and they are not variations on one thing — they trade off
// differently and suit different jobs.
//
// Offline (a blob from `djoin /provision`) puts no credential on the media at
// all and needs no domain network while Setup runs, because the computer
// account was created in advance and the blob carries its own secret. The cost
// is that a blob belongs to exactly one machine, so it is the answer when you
// have a list of machines, not when you image whatever arrives.
//
// Credentialed (a join account and its password) images any number of machines
// from one stick. The cost is that the password is written into
// autounattend.xml on a FAT32 volume that anyone who picks up the stick can
// read — Windows scrubs it from the copy it keeps on the installed system, but
// nothing scrubs the stick. Use an account delegated to create computer
// objects in one OU, never a domain admin, and keep the value in
// vars.local.yaml rather than in the recipe.
type DomainSpec struct {
	// Join is the domain to join, e.g. corp.example.com (credentialed path).
	Join string `yaml:"join,omitempty"`
	// OU is where the computer object is created, as a distinguished name.
	// Only meaningful on the credentialed path: an offline blob already
	// carries the OU chosen when it was provisioned.
	OU string `yaml:"ou,omitempty"`
	// Username may be user@domain.tld or DOMAIN\user.
	Username string `yaml:"username,omitempty"`
	Password string `yaml:"password,omitempty"`

	// Blob is a workspace-relative file holding the base64 provisioning data
	// from `djoin /provision`; BlobRef names one in the library instead.
	// Either selects the offline path.
	Blob    string `yaml:"blob,omitempty"`
	BlobRef string `yaml:"blob_ref,omitempty"`

	// BlobsBySerial was a folder of offline-join files, one per computer,
	// named after each computer's serial number: one stick for a whole batch.
	// It is withdrawn, and the field is kept only so that a recipe still
	// holding one is refused by name instead of parsing into silence. See
	// validate.
	BlobsBySerial string `yaml:"blobs_by_serial,omitempty"`
}

// validate checks the two paths are not mixed and that each has what it needs.
// A half-configured join is worse than none: Setup carries on into a workgroup
// and the machine looks fine until somebody tries to log in with a domain
// account.
func (d *DomainSpec) validate(fail func(string, ...any) error) error {
	credentialed := d.Join != "" || d.Username != "" || d.Password != ""
	if d.BlobsBySerial != "" {
		// Withdrawn, and refused rather than ignored: a recipe that quietly
		// lost its domain join would erase a batch of disks and hand back
		// workgroup machines that look finished.
		//
		// It joined each computer during Windows Setup, which is the thing a
		// lab machine proved a machine cannot come back from. Windows will
		// not sign a local account in automatically on a PC that has just
		// joined a domain, so the first boot never runs, and Windows resets
		// itself into setup again waiting for somebody to type. A single
		// offline join was fixed by joining after Setup instead; the same
		// change has not been made here, and nothing has run this path since,
		// so it is not offered rather than offered untested.
		return fail("windows.domain.blobs_by_serial has been withdrawn: it joined each computer " +
			"during Windows Setup, and a PC that joins a domain during Setup does not finish " +
			"setting itself up — it waits at a sign-in screen with no drivers and no programs. " +
			"Build one stick per computer with windows.domain.blob (`dsky install --domain-blob`), " +
			"which joins after Setup and is tested")
	}
	switch {
	case d.Offline() && credentialed:
		return fail("windows.domain sets both an offline blob and join credentials — pick one; " +
			"an offline blob already carries the computer account, so credentials are not used")
	case d.Blob != "" && d.BlobRef != "":
		return fail("windows.domain sets both blob and blob_ref — pick one")
	case d.Offline() && d.OU != "":
		return fail("windows.domain.ou has no effect on an offline join — the OU is fixed when " +
			"`djoin /provision /machineou` creates the blob")
	case d.Offline():
		return nil
	case !credentialed:
		return nil // nothing configured at all
	case d.Join == "":
		return fail("windows.domain needs join (the domain to join), e.g. corp.example.com")
	case d.Username == "":
		return fail("windows.domain.join is set but no username — an account that may add " +
			"computers to the domain is required")
	case d.Password == "":
		return fail("windows.domain.join is set but no password — use \"${var:domain_password}\" " +
			"and put the value in vars.local.yaml (gitignored)")
	}
	return nil
}

// Offline reports whether this is an offline domain join.
func (d *DomainSpec) Offline() bool {
	return d != nil && (d.Blob != "" || d.BlobRef != "")
}

// Enabled reports whether any domain join is configured. A withdrawn
// blobs_by_serial counts, so that a recipe carrying one reaches validate and
// is refused by name rather than treated as having no join at all.
func (d *DomainSpec) Enabled() bool {
	return d != nil && (d.Join != "" || d.Offline() || d.BlobsBySerial != "")
}

// AppsSpec installs programs at first boot with winget, Windows' own package
// manager, so nothing large is staged on the media and every installer comes
// from the vendor. It needs the machine to be online at first boot — which is
// what the staged network drivers are for.
type AppsSpec struct {
	Winget []string `yaml:"winget,omitempty"` // package ids, e.g. Google.Chrome
	// Scope "machine" installs for all users where the package allows it
	// (default); "user" installs only for the first account.
	Scope string `yaml:"scope,omitempty"`
	// Offline records the catalog programs whose installers were downloaded
	// when the media was built and ride on it, rather than being fetched
	// from the vendor at first boot. They install as ordinary payload -- a
	// pre-pulled Chrome is staged and run exactly like an operator's own MSI
	// -- so nothing here installs anything. This is the record of which
	// winget package each of those files is, which is what lets the machine
	// be brought up to date once it has the internet.
	Offline []OfflineApp `yaml:"offline,omitempty"`
	// BuiltAt is when the media was made, in RFC 3339. It is the age of
	// every version in Offline, and the only honest thing to show somebody
	// asking whether a machine built from this stick is current.
	BuiltAt string `yaml:"built_at,omitempty"`
}

// OfflineApp ties one staged installer back to the winget package it is.
type OfflineApp struct {
	ID      string `yaml:"id"`                // Google.Chrome
	Version string `yaml:"version,omitempty"` // what the media carries
	Ref     string `yaml:"ref"`               // the payload source holding the file
}

// Enabled reports whether the spec actually installs anything.
func (a *AppsSpec) Enabled() bool { return a != nil && len(a.Winget) > 0 }

// WiFiSpec joins the machine to a wireless network at first boot.
//
// A hands-off install and a machine that can reach the internet were mutually
// exclusive on any laptop without an ethernet port, and nothing said so.
// Making an install hands-off means answering OOBE's questions ahead of time,
// and one of those questions is which network to join — so the answer file
// sets HideWirelessSetupInOOBE, OOBE never asks, and the machine arrives at
// first boot with no way to reach anything.
//
// Everything that runs offline then works perfectly: the account is created,
// the machine is debloated, the drivers go on. Only the programs fail, because
// winget fetches them from the vendors — and a machine that looks completely
// installed is the last one anybody thinks to check.
//
// The profile is applied after the drivers step on purpose: the wireless card
// on a fresh image may have no working driver until the staged pack goes on.
type WiFiSpec struct {
	SSID string `yaml:"ssid"`
	// Password is the WPA2/WPA3 passphrase: 8-63 characters, or 64 hex digits
	// for a raw key. Empty means an open network.
	//
	// It is written to the media as clear text, because that is the only form
	// another machine can import — see GenerateWLANProfile. Use
	// "${var:wifi_password}" and keep the value in vars.local.yaml.
	Password string `yaml:"password,omitempty"`
	// Hidden marks a network that does not broadcast its SSID: Windows will
	// not find one by scanning, so the profile has to say to look for it.
	Hidden bool `yaml:"hidden,omitempty"`
}

// Enabled reports whether a wireless network was configured.
func (w *WiFiSpec) Enabled() bool { return w != nil && strings.TrimSpace(w.SSID) != "" }

// OfflineApps are the pre-pulled programs, or nothing.
func (a *AppsSpec) OfflineApps() []OfflineApp {
	if a == nil {
		return nil
	}
	return a.Offline
}

// DebloatSpec strips consumer junk at first boot while keeping the media
// itself official (update- and activation-safe): provisioned-app removal
// plus ad/telemetry/Copilot/widgets policies. Preset "standard" removes
// promo and gaming apps; "aggressive" also drops Outlook (new), Phone Link,
// Maps, People, and Get Help.
type DebloatSpec struct {
	Preset     string   `yaml:"preset"`                // off | standard | aggressive
	RemoveApps []string `yaml:"remove_apps,omitempty"` // extra Appx family-name prefixes
	KeepApps   []string `yaml:"keep_apps,omitempty"`   // exceptions to the preset lists
}

// Enabled reports whether the spec actually does anything.
func (d *DebloatSpec) Enabled() bool {
	return d != nil && d.Preset != "" && d.Preset != "off"
}

// EICfg pins the edition Windows Setup installs (sources/ei.cfg).
type EICfg struct {
	Edition string `yaml:"edition"`           // e.g. Professional
	Channel string `yaml:"channel,omitempty"` // Retail (default) | OEM | Volume
	VL      bool   `yaml:"vl,omitempty"`
}

type UnattendSpec struct {
	Template string            `yaml:"template"` // workspace-relative
	Vars     map[string]string `yaml:"vars,omitempty"`
}

// InstallMethod is how a driver pack gets installed at first boot.
type InstallMethod string

const (
	InstallSweep        InstallMethod = "pnputil-sweep"      // INF dir under Scripts/Drivers/, swept by pnputil
	InstallExpandSweep  InstallMethod = "expand-then-sweep"  // .cab expanded first, then swept
	InstallExtractSweep InstallMethod = "extract-then-sweep" // vendor self-extracting exe unpacked first, then swept
	InstallExe          InstallMethod = "exe"                // vendor silent installer
)

// HardwareSpec names a machine whose driver packs DSKY should find
// and stage automatically (`dsky drivers resolve`): a vendor model from
// the Dell/Lenovo/HP catalogs, or hardware IDs looked up in the Microsoft
// Update Catalog.
type HardwareSpec struct {
	Vendor string   `yaml:"vendor,omitempty"` // dell | lenovo | hp
	Model  string   `yaml:"model,omitempty"`
	HWIDs  []string `yaml:"hwids,omitempty"`
	OS     string   `yaml:"os,omitempty"` // default win11
}

// DriverPack references driver material by library/manifest Ref or by a
// workspace-relative Path (a directory of INFs kept in the workspace repo).
// Exactly one is set.
type DriverPack struct {
	Ref     string        `yaml:"ref,omitempty"`
	Path    string        `yaml:"path,omitempty"`
	Install InstallMethod `yaml:"install"`
	Args    []string      `yaml:"args,omitempty"`    // exe only
	Log     string        `yaml:"log,omitempty"`     // exe only; log filename
	Extract []string      `yaml:"extract,omitempty"` // extract-then-sweep: silent-extract args, {dir} = destination

	// OnlyVendor and OnlyModel restrict an exe pack to the machine it is for,
	// checked at first boot against what Windows reports about the computer.
	// Set by compose for packs found by model; never read from a recipe.
	OnlyVendor string `yaml:"-"`
	OnlyModel  string `yaml:"-"`
}

// Name is the pack's staging directory name.
func (d DriverPack) Name() string {
	if d.Ref != "" {
		return d.Ref
	}
	return filepath.Base(filepath.FromSlash(d.Path))
}

// PayloadItem stages one extra file into Scripts/. Exactly one of Ref
// (library/manifest id) or Path (workspace-relative file) is set.
type PayloadItem struct {
	Ref  string `yaml:"ref,omitempty"`
	Path string `yaml:"path,omitempty"`
}

type FirstbootSpec struct {
	// Mode "generate" (default) builds the script from Steps; "template"
	// renders Template verbatim for byte-exact control.
	Mode     string `yaml:"mode,omitempty"`
	Template string `yaml:"template,omitempty"`
	Steps    []Step `yaml:"steps,omitempty"`
	Log      string `yaml:"log,omitempty"` // default firstboot.log
}

// Step is one ordered first-boot action. YAML forms:
//
//   - drivers                      # expand cabs, pnputil sweep, run driver exes
//   - debloat                      # run the generated debloat pass (requires windows.debloat)
//   - apps                         # winget-install windows.apps (needs network)
//   - wait: 10s
//   - msi: { ref: x, args: ["/qn"], log: x.log }
//   - exe: { ref: x, args: ["-s"], log: x.log }
//   - cmd: "raw command line"
type Step struct {
	Drivers bool
	Debloat bool
	Apps    bool
	// WiFi is inserted by the renderer when windows.wifi is set, and is not
	// something a recipe writes: see the insertion in GenerateFirstbootCmd.
	WiFi bool
	Wait time.Duration
	MSI  *RunItem
	Exe  *RunItem
	Cmd  string
}

type RunItem struct {
	Ref  string   `yaml:"ref"`
	Args []string `yaml:"args,omitempty"`
	Log  string   `yaml:"log,omitempty"`
}

func (s *Step) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		switch node.Value {
		case "drivers":
			s.Drivers = true
			return nil
		case "debloat":
			s.Debloat = true
			return nil
		case "apps":
			s.Apps = true
			return nil
		}
		return fmt.Errorf("line %d: unknown firstboot step %q (scalar steps: drivers, debloat, apps)", node.Line, node.Value)
	}
	if node.Kind != yaml.MappingNode || len(node.Content) != 2 {
		return fmt.Errorf("line %d: a firstboot step is either a scalar or a single-key map", node.Line)
	}
	key, val := node.Content[0].Value, node.Content[1]
	switch key {
	case "wait":
		d, err := time.ParseDuration(val.Value)
		if err != nil {
			return fmt.Errorf("line %d: wait: %v", node.Line, err)
		}
		s.Wait = d
	case "msi":
		s.MSI = &RunItem{}
		return val.Decode(s.MSI)
	case "exe":
		s.Exe = &RunItem{}
		return val.Decode(s.Exe)
	case "cmd":
		s.Cmd = val.Value
	default:
		return fmt.Errorf("line %d: unknown firstboot step %q", node.Line, key)
	}
	return nil
}

type FlashSpec struct {
	Verify string `yaml:"verify,omitempty"` // readback-sha256 (default) | none
}

// LinuxSpec configures linux-iso recipes.
type LinuxSpec struct {
	Autoinstall *AutoinstallSpec `yaml:"autoinstall,omitempty"`
	Kickstart   *KickstartSpec   `yaml:"kickstart,omitempty"`
}

// KickstartSpec makes an Anaconda installer -- Fedora Server, RHEL,
// AlmaLinux, Rocky -- install itself from answers DSKY appends to the ISO.
//
// It needs no boot option and no rebuilt ISO, which is the whole reason this
// is worth doing: Anaconda looks, by itself, for a filesystem labelled OEMDRV
// holding ks.cfg, and uses it. So the ISO is written unmodified and a small
// labelled partition is appended after it, exactly as CIDATA is for Ubuntu --
// except that Ubuntu also needs its GRUB menu rewritten to pass `autoinstall`,
// and this does not.
type KickstartSpec struct {
	File string            `yaml:"file"` // template: the kickstart
	Vars map[string]string `yaml:"vars,omitempty"`
	// KernelArgs are appended to the installer's kernel line. The common
	// reasons are a serial console on a headless server (console=ttyS0,115200)
	// and inst.text on machines whose graphics the installer cannot drive.
	KernelArgs []string `yaml:"kernel_args,omitempty"`
}

// AutoinstallSpec makes an Ubuntu (subiquity) live-server ISO install
// itself with nobody at the keyboard: a cloud-init NoCloud "CIDATA"
// partition carrying user-data/meta-data is appended to the raw ISO, and
// the ISO's GRUB menu is rewritten in place (same byte length, no ISO
// rebuild) to boot with the `autoinstall` kernel parameter so the installer
// skips its confirmation prompt.
type AutoinstallSpec struct {
	UserData string            `yaml:"user_data"`           // template: #cloud-config with autoinstall:
	MetaData string            `yaml:"meta_data,omitempty"` // template; default sets instance-id
	Vars     map[string]string `yaml:"vars,omitempty"`
	// KernelPatch (default true) rewrites /boot/grub/grub.cfg for
	// zero-touch boot. Off = the installer asks "Continue with autoinstall?".
	KernelPatch *bool `yaml:"kernel_patch,omitempty"`
}

// PatchKernel reports whether the GRUB rewrite is enabled.
func (a *AutoinstallSpec) PatchKernel() bool { return a.KernelPatch == nil || *a.KernelPatch }

// SourceRefs lists every library/manifest id the recipe consumes, so a
// one-shot run can pull what is missing. For Windows tree-mode builds the
// caller may skip os.Source when the tree is present.
func (r *Recipe) SourceRefs() []string {
	seen := map[string]bool{}
	var out []string
	add := func(ref string) {
		if ref != "" && !seen[ref] {
			seen[ref] = true
			out = append(out, ref)
		}
	}
	add(r.OS.Source)
	if w := r.Windows; w != nil {
		for _, ref := range w.WinPEDrivers {
			add(ref)
		}
		for _, d := range w.DriverPacks {
			add(d.Ref)
		}
		for _, p := range w.Payload {
			add(p.Ref)
		}
		for _, s := range w.Firstboot.Steps {
			if s.MSI != nil {
				add(s.MSI.Ref)
			}
			if s.Exe != nil {
				add(s.Exe.Ref)
			}
		}
	}
	return out
}

var idRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// Load reads and validates a recipe file.
func Load(path string) (*Recipe, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r Recipe
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	r.Path = path
	r.applyDefaults()
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return &r, nil
}

func (r *Recipe) applyDefaults() {
	if r.Target.Scheme == "" {
		r.Target.Scheme = "mbr"
	}
	if r.Target.Filesystem == "" {
		r.Target.Filesystem = "fat32"
	}
	if r.Target.VolumeLabel == "" {
		r.Target.VolumeLabel = "ESD-USB"
	}
	if r.Target.Size == "" {
		r.Target.Size = "auto"
	}
	if r.Target.Boot == "" {
		r.Target.Boot = "uefi-only"
	}
	if r.OS.SourceMode == "" {
		r.OS.SourceMode = SourceAuto
	}
	if r.Flash.Verify == "" {
		r.Flash.Verify = "readback-sha256"
	}
	if r.Windows != nil {
		if r.Windows.Firstboot.Mode == "" {
			r.Windows.Firstboot.Mode = "generate"
		}
		if r.Windows.Firstboot.Log == "" {
			r.Windows.Firstboot.Log = "firstboot.log"
		}
	}
}

// Validate enforces structural rules; production-lesson rules live in Lint.
func (r *Recipe) Validate() error {
	fail := func(format string, args ...any) error {
		return fmt.Errorf("%s: %s", r.Path, fmt.Sprintf(format, args...))
	}
	switch {
	case r.Version != 1:
		return fail("version must be 1")
	case r.ID == "" || !idRe.MatchString(r.ID):
		return fail("id %q must be lowercase letters, digits, dot, dash, underscore", r.ID)
	case r.OS.Type != OSWindows && r.OS.Type != OSLinuxISO && r.OS.Type != OSRawImg:
		return fail("os.type must be windows, linux-iso, or raw-img")
	case r.Target.Boot != "uefi-only":
		return fail("target.boot must be uefi-only (legacy BIOS boot is out of scope)")
	case r.Target.Scheme != "mbr" && r.Target.Scheme != "gpt":
		return fail("target.scheme must be mbr or gpt")
	}
	if r.OS.Type == OSWindows {
		if r.Target.Filesystem != "fat32" {
			return fail("target.filesystem must be fat32 for Windows media (WIMs over 4 GiB get split, never NTFS)")
		}
		if r.Windows == nil {
			return fail("os.type windows requires a windows: section")
		}
		switch r.OS.SourceMode {
		case SourceAuto, SourceISO, SourceTree:
		default:
			return fail("os.source_mode must be auto, iso, or tree")
		}
		if r.OS.SourceMode == SourceTree && r.OS.TreePath == "" {
			return fail("os.source_mode tree requires os.tree_path")
		}
		if r.OS.Source == "" && r.OS.TreePath == "" {
			return fail("os.source (manifest id) or os.tree_path is required")
		}
		if d := r.Windows.Debloat; d != nil {
			switch d.Preset {
			case "off", "standard", "aggressive":
			default:
				return fail("windows.debloat.preset must be off, standard, or aggressive")
			}
		}
		fb := r.Windows.Firstboot
		if fb.Mode != "generate" && fb.Mode != "template" {
			return fail("windows.firstboot.mode must be generate or template")
		}
		for i, s := range fb.Steps {
			if s.Debloat && !r.Windows.Debloat.Enabled() {
				return fail("windows.firstboot.steps[%d] is `debloat` but windows.debloat is off/absent", i)
			}
			if s.Apps && !r.Windows.Apps.Enabled() {
				return fail("windows.firstboot.steps[%d] is `apps` but windows.apps lists no packages", i)
			}
		}
		if a := r.Windows.Apps; a != nil && a.Scope != "" && a.Scope != "machine" && a.Scope != "user" {
			return fail("windows.apps.scope must be machine or user")
		}
		if d := r.Windows.Domain; d != nil {
			if err := d.validate(fail); err != nil {
				return err
			}
		}
		if w := r.Windows.WiFi; w != nil {
			if err := ValidateWiFi(w, "windows.wifi.ssid", "windows.wifi.password", fail); err != nil {
				return err
			}
		}
		if s := r.Windows.StatusScreen; s != nil && s.Success != "" && s.SuccessRef != "" {
			return fail("windows.status_screen sets both success and success_ref — pick one")
		}
		if fb.Mode == "template" && fb.Template == "" {
			return fail("windows.firstboot.mode template requires windows.firstboot.template")
		}
		for i, p := range r.Windows.Payload {
			if (p.Ref == "") == (p.Path == "") {
				return fail("windows.payload[%d] needs exactly one of ref or path", i)
			}
		}
		// An offline record names a file on the media and the winget package
		// it is. A record for something nothing stages would have a machine
		// reporting a program it never got, and trying to update it: worse
		// than not recording it, because it reads as an answer.
		staged := map[string]bool{}
		for _, p := range r.Windows.Payload {
			staged[p.Ref] = true
		}
		for i, o := range r.Windows.Apps.OfflineApps() {
			switch {
			case o.ID == "":
				return fail("windows.apps.offline[%d] needs the winget id of the program it is", i)
			case o.Ref == "":
				return fail("windows.apps.offline[%d] (%s) needs the payload ref that carries its installer", i, o.ID)
			case !staged[o.Ref]:
				return fail("windows.apps.offline[%d] (%s) names %s, which windows.payload does not carry", i, o.ID, o.Ref)
			}
		}
		for i, d := range r.Windows.DriverPacks {
			if (d.Ref == "") == (d.Path == "") {
				return fail("windows.driver_packs[%d] needs exactly one of ref or path", i)
			}
			switch d.Install {
			case InstallSweep, InstallExpandSweep, InstallExtractSweep, InstallExe:
			default:
				return fail("windows.driver_packs[%d] install must be pnputil-sweep, expand-then-sweep, extract-then-sweep, or exe", i)
			}
		}
		for i, h := range r.Windows.Hardware {
			byModel := h.Vendor != "" && h.Model != ""
			if !byModel && len(h.HWIDs) == 0 {
				return fail("windows.hardware[%d] needs vendor+model or hwids", i)
			}
			if h.Vendor != "" && !catalog.IsModelFeed(h.Vendor) {
				names := make([]string, len(catalog.ModelFeeds))
				for i, v := range catalog.ModelFeeds {
					names[i] = string(v)
				}
				return fail("windows.hardware[%d] vendor must be one of %s (use hwids for others)", i, strings.Join(names, ", "))
			}
		}
	} else {
		if r.Windows != nil {
			return fail("windows: section is only valid with os.type windows")
		}
		if r.OS.Source == "" {
			return fail("os.source is required")
		}
	}
	if r.Linux != nil {
		if r.OS.Type != OSLinuxISO {
			return fail("linux: section is only valid with os.type linux-iso")
		}
		if a := r.Linux.Autoinstall; a != nil && a.UserData == "" {
			return fail("linux.autoinstall.user_data (template path) is required")
		}
		if k := r.Linux.Kickstart; k != nil && k.File == "" {
			return fail("linux.kickstart.file (template path) is required")
		}
		// Two sets of answers on one stick is a recipe whose author expects
		// one of them to be read, with no way to say which.
		if r.Linux.Autoinstall != nil && r.Linux.Kickstart != nil {
			return fail("linux: autoinstall and kickstart answer different installers — use one")
		}
	}
	if r.Target.Size != "auto" {
		if _, err := ParseSize(r.Target.Size); err != nil {
			return fail("target.size: %v", err)
		}
	}
	if r.Target.MinStick != "" {
		if _, err := ParseSize(r.Target.MinStick); err != nil {
			return fail("target.min_stick: %v", err)
		}
	}
	if v := r.Flash.Verify; v != "readback-sha256" && v != "none" {
		return fail("flash.verify must be readback-sha256 or none")
	}
	return nil
}

var sizeRe = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)\s*([KMGT]i?B|B)?$`)

// ParseSize parses "8GiB", "512MiB", "16GB" (decimal), or plain bytes.
func ParseSize(s string) (int64, error) {
	m := sizeRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, fmt.Errorf("invalid size %q (use e.g. 8GiB, 512MiB, 16GB)", s)
	}
	val, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, err
	}
	mult := float64(1)
	switch m[2] {
	case "", "B":
	case "KiB":
		mult = 1 << 10
	case "MiB":
		mult = 1 << 20
	case "GiB":
		mult = 1 << 30
	case "TiB":
		mult = 1 << 40
	case "KB":
		mult = 1e3
	case "MB":
		mult = 1e6
	case "GB":
		mult = 1e9
	case "TB":
		mult = 1e12
	}
	return int64(val * mult), nil
}

// varRe matches ${var:name} references in recipe strings.
var varRe = regexp.MustCompile(`\$\{var:([A-Za-z0-9_.-]+)\}`)

// ExpandVars substitutes ${var:name} references from vars. Unknown names are
// an error so a missing secret fails the build instead of installing media
// with a literal placeholder.
func ExpandVars(s string, vars map[string]string) (string, error) {
	var missing []string
	out := varRe.ReplaceAllStringFunc(s, func(m string) string {
		name := varRe.FindStringSubmatch(m)[1]
		v, ok := vars[name]
		if !ok {
			missing = append(missing, name)
			return m
		}
		return v
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("undefined var(s) %s — define in workspace vars, vars.local.yaml, or --var", strings.Join(missing, ", "))
	}
	return out, nil
}

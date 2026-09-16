package agent

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const stepDrivers = "drivers"

// pnputilRebootNeeded is what pnputil returns when the drivers are in but
// Windows cannot finish with them until the machine restarts. It prints
// "System reboot is needed to complete install operations!" alongside it.
//
// This is not advice. On an HP EliteBook x360 that had just taken 282 driver
// packages -- storage, Bluetooth, NFC, touch -- the agent read this, carried
// straight on into removing consumer apps, and the machine bugchecked four
// minutes later, ending provisioning with no programs installed. Windows asked
// for a restart; it gets one.
const pnputilRebootNeeded = 259

// countINF reports how many driver files are under dir. This, not an exit
// code, is how the agent decides whether unpacking worked: an HP EliteBook
// pack handed the switch style its own catalog listed printed its usage text,
// unpacked nothing and exited 0.
func countINF(dir string) int {
	n := 0
	filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() && strings.EqualFold(filepath.Ext(d.Name()), ".inf") {
			n++
		}
		return nil
	})
	return n
}

// expandArgs substitutes the destination directory into a vendor's switches
// and drops the quotes around it.
//
// The switches are written for a command line, where "{dir}" keeps a path
// with spaces in one piece and the shell removes the quotes before the
// program sees them. The agent starts the program itself and passes each
// argument whole, so a path needs no quoting -- and quotes left in are part
// of the path as far as the extractor is concerned. An HP pack handed
// /s /e /f "C:\..." with the quotes intact printed its usage text and
// unpacked nothing, exactly as it had when handed the wrong switches.
func expandArgs(args []string, dir string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = strings.ReplaceAll(strings.ReplaceAll(a, "{dir}", dir), `"`, "")
	}
	return out
}

// driversStep unpacks every staged pack and installs what matches this
// machine. Nothing here stops the run: a machine with some drivers is worth
// more than a machine that stopped at the first bad pack.
func (a *Agent) driversStep() {
	d := a.Manifest.Drivers
	driversDir := filepath.Join(a.Dir, "Drivers")

	for _, cab := range d.Cabs {
		src := filepath.Join(a.Dir, cab.File)
		if _, err := os.Stat(src); err != nil {
			a.J.Info(stepDrivers, "%s is not on the stick, skipping", cab.File)
			continue
		}
		dest := filepath.Join(driversDir, cab.Dir)
		os.MkdirAll(dest, 0o755)
		r := run(30*time.Minute, "expand", src, "-F:*", dest)
		a.J.Raw(r.Out)
		if n := countINF(dest); n > 0 {
			a.J.Info(stepDrivers, "%s: expanded %d driver file(s)", cab.File, n)
		} else {
			a.J.FailDetail(stepDrivers, cab.File+": no driver files after expanding", trimOut(r.Out))
		}
	}

	for _, ex := range d.Extracts {
		a.extractPack(ex, driversDir)
	}

	if d.Sweep {
		a.sweep(driversDir)
	}

	for _, exe := range d.Exes {
		a.vendorInstaller(exe)
	}
}

// extractPack unpacks one vendor pack, trying the other switch style when the
// first produces no driver files.
func (a *Agent) extractPack(ex Extract, driversDir string) {
	src := filepath.Join(a.Dir, ex.File)
	if _, err := os.Stat(src); err != nil {
		a.J.Info(stepDrivers, "%s is not on the stick, skipping", ex.File)
		return
	}
	dest := filepath.Join(driversDir, ex.Dir)
	os.MkdirAll(dest, 0o755)
	// Already unpacked on an earlier boot: the files are right there. Watched
	// on an HP EliteBook, Windows restarted the machine part way through
	// installing the drivers -- for its own display driver -- and the agent
	// came back and unpacked the same 1.2 GB pack from scratch before getting
	// to the part it had not finished.
	if n := countINF(dest); n > 0 {
		a.J.Info(stepDrivers, "%s: already unpacked on an earlier boot, %d driver file(s) present", ex.File, n)
		return
	}
	a.UI.Detail("unpacking " + ex.File)

	attempts := [][]string{ex.Args}
	if len(ex.AltArgs) > 0 {
		attempts = append(attempts, ex.AltArgs)
	}
	for i, args := range attempts {
		full := expandArgs(args, dest)
		if i > 0 {
			a.J.Info(stepDrivers, "%s: no driver files appeared, retrying with %s", ex.File, strings.Join(full, " "))
		}
		cmd, cmdArgs := src, full
		if strings.EqualFold(filepath.Ext(src), ".msi") {
			// An MSI is not a program and cannot be started on its own.
			// msiexec's administrative install unpacks it to a folder without
			// installing anything -- which for a driver MSI, Surface's, is
			// its drivers and not the updater tooling that comes with them.
			cmd, cmdArgs = "msiexec", append([]string{"/a", src}, full...)
		}
		r := run(45*time.Minute, cmd, cmdArgs...)
		a.J.Raw(r.Out)
		n := countINF(dest)
		a.J.Info(stepDrivers, "%s: %s exited %d, %d driver file(s) present", ex.File, strings.Join(full, " "), r.Code, n)
		if n > 0 {
			return
		}
	}
	a.J.Fail(stepDrivers, "%s unpacked no driver files", ex.File)
}

// sweep hands everything unpacked to pnputil, which installs only what
// matches this machine — that is what lets one stick carry packs for several
// models. Skipped when nothing unpacked: pnputil on an empty folder exits 87
// and reads like a real failure in the log.
func (a *Agent) sweep(driversDir string) {
	n := countINF(driversDir)
	if n == 0 {
		a.J.Info(stepDrivers, "no driver files to install")
		return
	}
	a.J.Info(stepDrivers, "installing %d driver file(s) with pnputil", n)
	a.UI.Detail("installing " + itoa(n) + " driver files (this is the long part)")
	r := run(60*time.Minute, "pnputil", "/add-driver", filepath.Join(driversDir, "*.inf"), "/subdirs", "/install")
	a.J.Raw(r.Out)
	added := strings.Count(r.Out, "Driver package added successfully")
	if r.Code == pnputilRebootNeeded || strings.Contains(r.Out, "System reboot is needed") {
		a.rebootWanted = "Windows needs a restart to finish installing the drivers"
	}
	switch {
	case added > 0:
		a.J.Info(stepDrivers, "pnputil added %d driver package(s), exit %d", added, r.Code)
	case r.ok():
		a.J.Info(stepDrivers, "pnputil reported nothing to add, exit 0")
	default:
		a.J.FailDetail(stepDrivers, "pnputil installed no driver packages", trimOut(r.Out))
	}
}

// vendorInstaller runs a vendor's own driver installer, on the model it is
// for. A machine that is not that model skips it, so one stick serves a
// mixed bench.
func (a *Agent) vendorInstaller(exe Exe) {
	src := filepath.Join(a.Dir, exe.File)
	if _, err := os.Stat(src); err != nil {
		a.J.Info(stepDrivers, "%s is not on the stick, skipping", exe.File)
		return
	}
	if exe.OnlyModel != "" {
		vendor, model := machineModel()
		if !modelMatches(exe.OnlyVendor, exe.OnlyModel, vendor, model) {
			a.J.Info(stepDrivers, "%s is for %s %s; this machine is %s %s, skipping",
				exe.File, exe.OnlyVendor, exe.OnlyModel, vendor, model)
			return
		}
	}
	args := expandArgs(exe.Args, a.Dir)
	for i, v := range args {
		args[i] = strings.ReplaceAll(v, "{log}", filepath.Join(a.Dir, exe.Log))
	}
	r := run(exe.Timeout(), src, args...)
	a.J.Raw(r.Out)
	switch {
	case r.Err != nil:
		a.J.FailDetail(stepDrivers, exe.File+" did not finish", r.Err.Error())
	case r.ok():
		a.J.Info(stepDrivers, "%s finished, exit 0 (%s)", exe.File, r.Timing.Round(time.Second))
	default:
		// A vendor installer's non-zero codes are its own; record and move on.
		a.J.Info(stepDrivers, "%s exited %d (%s)", exe.File, r.Code, r.Timing.Round(time.Second))
	}
}

// modelMatches compares loosely, the way the model gate script did: vendors
// spell their own names inconsistently between the driver catalog and what
// the machine reports about itself.
func modelMatches(wantVendor, wantModel, gotVendor, gotModel string) bool {
	norm := func(s string) string {
		return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(s))), " ")
	}
	if wantVendor != "" && gotVendor != "" && !strings.Contains(norm(gotVendor), norm(wantVendor)) {
		return false
	}
	wm, gm := norm(wantModel), norm(gotModel)
	return wm != "" && gm != "" && (strings.Contains(gm, wm) || strings.Contains(wm, gm))
}

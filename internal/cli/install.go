package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/uplinkresearch/dsky/internal/appcatalog"
	"github.com/uplinkresearch/dsky/internal/compose"
	"github.com/uplinkresearch/dsky/internal/driverresolve"
	"github.com/uplinkresearch/dsky/internal/drivers/catalog"
	"github.com/uplinkresearch/dsky/internal/hwdetect"
	"github.com/uplinkresearch/dsky/internal/library"
	"github.com/uplinkresearch/dsky/internal/oscatalog"
	"github.com/uplinkresearch/dsky/internal/recipe"
)

func cmdCatalog(ctx context.Context, env *Env, _ []string) error {
	refreshCatalog(ctx, env)
	fmt.Printf("Operating systems (dsky install <id>) — list: %s\n", oscatalog.Source())
	headings := map[oscatalog.Category]string{
		oscatalog.Desktop:   "Desktop",
		oscatalog.Server:    "Server",
		oscatalog.Appliance: "Single-board and appliance",
	}
	for _, cat := range []oscatalog.Category{oscatalog.Desktop, oscatalog.Server, oscatalog.Appliance} {
		first := true
		for _, e := range oscatalog.Catalog() {
			if e.Group() != cat {
				continue
			}
			if first {
				fmt.Printf("\n  %s\n", headings[cat])
				first = false
			}
			tags := string(e.Family)
			if a := e.CPUArch(); a != "amd64" {
				tags += "/" + a
			}
			fmt.Printf("    %-24s %-13s %s\n", e.ID, tags, e.Name)
			detail := fmt.Sprintf("%s — %s", e.Version, e.Notes)
			if len(e.Editions) > 0 {
				detail += "  editions: " + strings.Join(e.Editions, ", ")
			}
			fmt.Printf("      %s\n", detail)
			// On its own line rather than in the tag column, which is too
			// narrow for it and would push every other name out of alignment.
			if e.ImportOnly() && e.ImportFrom != "" {
				fmt.Printf("      bring your own ISO, from %s\n", e.ImportFrom)
			}
		}
	}
	return nil
}

// cmdInstall is Quick Install from the CLI: pick a catalog OS, build media
// with a few options, and flash the attached stick.
func cmdInstall(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	edition := fs.String("edition", "", "Windows edition (Pro, Home, …)")
	account := fs.String("account", "local", "Windows account setup: local | oobe")
	debloat := fs.String("debloat", "standard", "Windows debloat: off | standard | aggressive")
	bypass := fs.Bool("bypass-checks", false, "Windows: skip TPM/Secure Boot/RAM checks")
	withDrivers := fs.Bool("drivers", false, "drivers for this computer: on Windows, detect this machine and stage\n"+
		"its driver packs; on Ubuntu, have the installer put on the proprietary\n"+
		"drivers it finds (NVIDIA and the like)")
	var driversFor stringList
	fs.Var(&driversFor, "drivers-for", "Windows: stage drivers for another machine, e.g.\n"+
		"\"dell:OptiPlex 7010\" (repeatable; one stick can carry several models)")
	apps := fs.String("apps", "", "programs to install with the operating system, on Windows, Ubuntu and\n"+
		"Fedora Server (see `dsky apps`, and `dsky apps --os ubuntu|fedora`)")
	domainBlob := fs.String("domain-blob", "", "Windows: join a domain using a blob from\n"+
		"`djoin /provision` (one machine per blob). A credentialed join belongs\n"+
		"in a workspace recipe, so its password is not left in shell history.")
	domainBlobs := fs.String("domain-blobs", "", "Windows: join a batch of computers from one stick — a folder of\n"+
		"`djoin /provision` files, each named <serial number>.txt")
	iso := fs.String("iso", "", "use an ISO you downloaded instead of fetching it")
	yes := fs.Bool("yes", false, "skip the typed size confirmation")
	buildOnly := fs.Bool("build-only", false, "stop after building; do not flash")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() < 1 || fs.NArg() > 2 {
		return fmt.Errorf("install <os-id> [device] [--edition …] [--account local|oobe] [--debloat …]")
	}
	refreshCatalog(ctx, env)
	e, ok := oscatalog.Get(fs.Arg(0))
	if !ok {
		return fmt.Errorf("unknown OS %q — see `dsky catalog`", fs.Arg(0))
	}
	lib, err := env.library()
	if err != nil {
		return err
	}
	// Said now rather than after the device has been chosen and armed: there
	// is no download to attempt, so nothing later can rescue this.
	if e.ImportOnly() && *iso == "" && !oscatalog.InLibrary(lib, e) {
		return e.ImportOnlyError()
	}

	devArg := ""
	if fs.NArg() == 2 {
		devArg = fs.Arg(1)
	}
	var dev, derr = pickDevice(ctx, devArg)
	if !*buildOnly && derr != nil {
		return derr
	}

	if *iso != "" {
		if err := importISO(lib, e, *iso); err != nil {
			return err
		}
	}

	// Detected hardware and named models are additive: pnputil installs only
	// what matches the machine being imaged, so one stick can carry packs for
	// several models. On Ubuntu, "drivers for this computer" is a different
	// job under the same name — nothing is staged and nothing is detected,
	// because the kernel carries all of it but the proprietary drivers, and
	// Ubuntu's installer finds and installs those itself when asked to.
	thirdParty := false
	var hw []recipe.HardwareSpec
	switch {
	case *withDrivers && e.ThirdPartyDriversSupported():
		thirdParty = true
		fmt.Println("Ubuntu will install the proprietary drivers it finds for this machine (NVIDIA and the like).")
	case *withDrivers:
		var err error
		if hw, err = detectedHardware(ctx, e); err != nil {
			return err
		}
	}
	for _, spec := range driversFor {
		h, err := hardwareForModel(spec, e)
		if err != nil {
			return err
		}
		hw = append(hw, h)
	}

	var appIDs []string
	if strings.TrimSpace(*apps) != "" && e.Family != oscatalog.Windows {
		appIDs = strings.Split(*apps, ",")
		if err := oscatalog.CheckPrograms(e, appIDs); err != nil {
			return err
		}
		if err := printLinuxProgramPlan(os.Stdout, e, appIDs); err != nil {
			return err
		}
	} else if strings.TrimSpace(*apps) != "" {
		appIDs = strings.Split(*apps, ",")
		pkgs, custom, err := appcatalog.Resolve(appIDs)
		if err != nil {
			return err
		}
		// A typed package id is checked now: a misspelling found at first boot
		// is a machine delivered without the program.
		for _, id := range appIDs {
			pkg, typed := strings.CutPrefix(strings.TrimSpace(id), appcatalog.WingetPrefix)
			if !typed {
				continue
			}
			if _, err := appcatalog.LookupWinget(ctx, pkg); errors.Is(err, appcatalog.ErrWingetUnchecked) {
				fmt.Fprintf(os.Stderr, "warning: %s not confirmed in winget (%v)\n", pkg, err)
			} else if err != nil {
				return fmt.Errorf("%s: %w", pkg, err)
			}
		}
		if len(pkgs) > 0 {
			fmt.Printf("Will install %d program(s) with winget at first boot: %s\n",
				len(pkgs), strings.Join(pkgs, ", "))
		}
		// Named separately because they behave differently: these ride on the
		// stick and run with no network, which is usually why they were added.
		for _, c := range custom {
			fmt.Printf("Will stage and run your installer %s (%s, %d MiB)\n", c.ID, c.Filename, c.Size>>20)
		}
	}

	if *domainBlob != "" {
		if e.Family != oscatalog.Windows {
			return fmt.Errorf("--domain-blob is a Windows option")
		}
		// The blob is that computer account's password. Said plainly, because
		// "offline join is the safe one" is easy to over-read, and because the
		// blob is for exactly one machine.
		fmt.Printf("Offline domain join from %s\n", filepath.Base(*domainBlob))
		fmt.Println("  This blob is for one machine and holds its computer-account password —")
		fmt.Println("  treat the stick as carrying a credential, and build one stick per machine.")
	}
	if *domainBlobs != "" {
		if e.Family != oscatalog.Windows {
			return fmt.Errorf("--domain-blobs is a Windows option")
		}
		if *domainBlob != "" {
			return fmt.Errorf("use --domain-blob for one computer or --domain-blobs for a batch, not both")
		}
		blobs, err := compose.SerialBlobs(*domainBlobs)
		if err != nil {
			return err
		}
		fmt.Printf("Domain join by serial number: %d computers\n", len(blobs))
		fmt.Println("  Each PC joins with the file named after its serial number and deletes all of")
		fmt.Println("  them from its own disk. The stick keeps every file — wipe it after the batch.")
	}
	fmt.Printf("Building %s installer media...\n", e.Name)
	prog := &stageProgress{}
	art, err := oscatalog.BuildQuick(ctx, lib, e, oscatalog.Options{
		Edition: *edition, AccountMode: *account, Debloat: *debloat, BypassRequirement: *bypass,
		Hardware: hw, Apps: appIDs, ThirdPartyDrivers: thirdParty,
		DomainBlob: *domainBlob, DomainBlobsDir: *domainBlobs,
	}, prog.report)
	prog.finish()
	if err != nil {
		return err
	}
	if *buildOnly {
		fmt.Printf("built: %s (%d MiB)\n", art.Path, art.Size>>20)
		return nil
	}
	return armAndFlash(ctx, art, dev, *yes)
}

// stringList collects a repeatable string flag.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ", ") }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// hardwareForModel parses a --drivers-for target into a hardware entry.
func hardwareForModel(spec string, e oscatalog.Entry) (recipe.HardwareSpec, error) {
	if e.Family != oscatalog.Windows {
		return recipe.HardwareSpec{}, fmt.Errorf("--drivers-for is a Windows option — Linux ships its drivers in the kernel")
	}
	h, err := driverresolve.SpecForModel(spec, e.DriverOS())
	if err != nil {
		return recipe.HardwareSpec{}, fmt.Errorf("--drivers-for %v", err)
	}
	return h, nil
}

// importISO files a downloaded ISO under the catalog entry's id, skipping the
// work when the image is already there.
func importISO(lib *library.Library, e oscatalog.Entry, path string) error {
	if oscatalog.InLibrary(lib, e) {
		fmt.Printf("%s is already in the library — using that, ignoring --iso.\n", e.ID)
		fmt.Println("  (to replace it: dsky gc, or delete the blob the catalog names)")
		return nil
	}
	if err := oscatalog.CheckISO(path); err != nil {
		return err
	}
	prog := &stageProgress{}
	entry, err := oscatalog.ImportISO(lib, e, path, prog.report)
	prog.finish()
	if err != nil {
		return err
	}
	fmt.Printf("  in library as %s: %d MiB, sha256 %s\n", entry.ID, entry.Size>>20, entry.SHA256)
	return nil
}

// detectedHardware profiles this machine into the hardware entries Quick
// Install should hunt driver packs for. Whatever the catalogs turn out not to
// carry is dropped during the build, not here.
func detectedHardware(ctx context.Context, e oscatalog.Entry) ([]recipe.HardwareSpec, error) {
	if e.Family != oscatalog.Windows {
		return nil, fmt.Errorf("--drivers is a Windows option — Linux ships its drivers in the kernel")
	}
	h, err := hwdetect.Detect(ctx)
	if err != nil {
		return nil, err
	}
	fmt.Printf("Detected %s %s (%s)\n", h.Vendor, h.Model, h.CPU)
	ids := h.DriverHWIDs()
	if v := h.KnownVendor(); v != "" {
		fmt.Printf("  %s drivers for this model, plus %d GPU/network hardware ID(s)\n", catalog.VendorNames[catalog.Vendor(v)], len(ids))
	} else {
		fmt.Printf("  no per-model feed for this maker; %d GPU/network hardware ID(s) to look up\n", len(ids))
	}
	hw := driverresolve.SpecsFor(h, e.DriverOS())
	if len(hw) == 0 {
		return nil, fmt.Errorf("nothing to resolve: this machine's maker has no drivers by model and no PCI GPU or network device was detected")
	}
	return hw, nil
}

// printLinuxProgramPlan says what --apps will put on the machine, in the last
// thing shown before the disk is erased.
//
// What an id means depends on the OS: `docker` is docker.io on Ubuntu and
// moby-engine on Fedora. This resolved every Linux entry with Ubuntu's table
// and printed the word "Ubuntu" over it, so a Fedora stick announced Ubuntu
// and a package name it would never install. Releases were resolved and never
// printed at all, so `--apps dsky` listed nothing and installed DSKY anyway.
func printLinuxProgramPlan(w io.Writer, e oscatalog.Entry, appIDs []string) error {
	fmt.Fprintf(w, "The stick will install %s with these programs, and erase the computer's disk:\n", e.Name)
	var flatpaks, repos, releases []string
	if e.AppTarget() == appcatalog.TargetFedora {
		plan, err := appcatalog.ResolveFedora(appIDs)
		if err != nil {
			return err
		}
		if len(plan.Dnf) > 0 {
			fmt.Fprintf(w, "  from Fedora: %s\n", strings.Join(plan.Dnf, ", "))
		}
		flatpaks, repos, releases = plan.Flatpaks, plan.Repos, plan.Releases
	} else {
		plan, err := appcatalog.ResolveUbuntu(appIDs)
		if err != nil {
			return err
		}
		if len(plan.Apt) > 0 {
			fmt.Fprintf(w, "  from Ubuntu: %s\n", strings.Join(plan.Apt, ", "))
		}
		for _, sn := range plan.Snaps {
			fmt.Fprintf(w, "  snap: %s\n", sn.Name)
		}
		flatpaks, repos, releases = plan.Flatpaks, plan.Repos, plan.Releases
	}
	for _, f := range flatpaks {
		fmt.Fprintf(w, "  Flathub, on first boot: %s\n", f)
	}
	for _, r := range repos {
		fmt.Fprintf(w, "  vendor repository, on first boot: %s\n", r)
	}
	for _, r := range releases {
		fmt.Fprintf(w, "  published release, on first boot: %s\n", r)
	}
	if e.AppTarget() == appcatalog.TargetUbuntu && e.Group() != oscatalog.Server {
		fmt.Fprintln(w, "  Ubuntu shows its review screen and waits for Install before erasing anything.")
	} else {
		// Neither Ubuntu Server's identity section nor DSKY's kickstart carries
		// an account, so both installers stop once to ask for one.
		fmt.Fprintln(w, "  No questions except the account, which the installer asks for on screen.")
	}
	return nil
}

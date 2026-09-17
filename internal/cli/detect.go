package cli

import (
	"context"
	"flag"
	"fmt"

	"github.com/uplinkresearch/dsky/internal/driverresolve"
	"github.com/uplinkresearch/dsky/internal/drivers/catalog"
	"github.com/uplinkresearch/dsky/internal/hwdetect"
	"github.com/uplinkresearch/dsky/internal/oscatalog"
)

// cmdDetect profiles this machine and shows what Quick Install would hunt
// drivers for. With --resolve it actually goes and gets them, caching into the
// same place `install --drivers` reads, so the real build downloads nothing
// twice.
func cmdDetect(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("detect", flag.ContinueOnError)
	resolve := fs.Bool("resolve", false, "look the drivers up in the catalogs and download them now")
	osID := fs.String("os", "windows-11", "which catalog OS to resolve drivers for")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	h, err := hwdetect.Detect(ctx)
	if err != nil {
		return err
	}
	fmt.Println("This machine:")
	fmt.Printf("  make/model:  %s %s\n", h.Vendor, h.Model)
	if v := h.KnownVendor(); v != "" {
		fmt.Printf("  driver feed: %s (drivers for this model)\n", catalog.VendorNames[catalog.Vendor(v)])
	} else {
		fmt.Println("  driver feed: none — drivers resolve per-device via the Microsoft Update Catalog")
	}
	fmt.Printf("  cpu:         %s\n", h.CPU)
	for _, g := range h.GPUs {
		brand := g.GPUVendor
		if brand == "" {
			brand = "unknown"
		}
		fmt.Printf("  gpu:         %s [%s]  %s\n", g.Name, brand, g.HardwareID)
	}
	for _, n := range h.NICs {
		fmt.Printf("  network:     %s  %s\n", n.Name, n.HardwareID)
	}
	fmt.Printf("  devices:     %d total\n", len(h.Devices))

	ids := h.DriverHWIDs()
	fmt.Printf("\nDrivers would be looked up for %d device(s):\n", len(ids))
	for _, id := range ids {
		fmt.Printf("  %s\n", id)
	}
	if !*resolve {
		fmt.Println("\nBuild media with those drivers staged:")
		fmt.Println("  dsky install windows-11 --drivers")
		fmt.Println("Or see what the catalogs actually have, and cache it now:")
		fmt.Println("  dsky detect --resolve")
		return nil
	}

	e, ok := oscatalog.Get(*osID)
	if !ok {
		return fmt.Errorf("unknown OS %q — see `dsky catalog`", *osID)
	}
	if e.Family != oscatalog.Windows {
		return fmt.Errorf("--resolve applies to Windows — Linux ships its drivers in the kernel")
	}
	lib, err := env.library()
	if err != nil {
		return err
	}
	hw := driverresolve.SpecsFor(h, e.DriverOS())
	if len(hw) == 0 {
		return fmt.Errorf("nothing to resolve: no maker with drivers by model and no PCI GPU or network device detected")
	}
	fmt.Printf("\nResolving %s drivers...\n", e.Name)
	prog := &stageProgress{}
	res, err := oscatalog.PlanDrivers(ctx, lib, e, hw, prog.report)
	prog.finish()
	if err != nil {
		return err
	}
	fmt.Printf("\n%d driver pack(s) ready in the library, %d MiB total:\n", len(res.Packs), res.Bytes>>20)
	for _, id := range res.Packs {
		size := ""
		if le, err := lib.Resolve(id); err == nil {
			size = fmt.Sprintf("  (%d MiB)", le.Size>>20)
		}
		fmt.Printf("  %s%s\n", id, size)
	}
	if res.Bytes > 2<<30 {
		fmt.Printf("\nThat is a lot of drivers — GPU packages are most of it. Media with\n")
		fmt.Printf("them staged needs a 16 GB or larger stick.\n")
	}
	if len(res.Missing) > 0 {
		fmt.Printf("\n%d device(s) with no catalog pack (Windows Update covers these after install):\n", len(res.Missing))
		for _, m := range res.Missing {
			fmt.Printf("  %s\n", m)
		}
	}
	fmt.Println("\nThese are cached — `dsky install " + e.ID + " --drivers` will reuse them.")
	return nil
}

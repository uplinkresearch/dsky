package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/uplinkresearch/dsky/internal/drivers"
	"github.com/uplinkresearch/dsky/internal/helpers"
	"github.com/uplinkresearch/dsky/internal/manifest"

	"github.com/uplinkresearch/dsky/internal/hidewin"
)

func cmdDrivers(ctx context.Context, env *Env, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("drivers: need inspect, add, or scan")
	}
	switch args[0] {
	case "inspect":
		if len(args) != 2 {
			return fmt.Errorf("drivers inspect <dir|.zip|.cab|.inf|.exe>")
		}
		return driversInspect(ctx, env, args[1])
	case "add":
		return driversAdd(ctx, env, args[1:])
	case "search":
		return driversSearch(ctx, env, args[1:])
	case "models":
		return driversModels(ctx, env, args[1:])
	case "resolve":
		return driversResolve(ctx, env, args[1:])
	case "scan":
		return driversScan(ctx)
	default:
		return fmt.Errorf("drivers: unknown subcommand %q (inspect, add, search, models, resolve, scan)", args[0])
	}
}

// materializeForInspect turns any pack shape into a directory of INFs (or
// explains why it can't).
func materializeForInspect(ctx context.Context, env *Env, src string) (dir string, cleanup func(), err error) {
	cleanup = func() {}
	st, err := os.Stat(src)
	if err != nil {
		return "", cleanup, err
	}
	if st.IsDir() {
		return src, cleanup, nil
	}
	switch strings.ToLower(filepath.Ext(src)) {
	case ".inf":
		return filepath.Dir(src), cleanup, nil
	case ".zip":
		tmp, err := os.MkdirTemp("", "dsky-drivers-")
		if err != nil {
			return "", cleanup, err
		}
		cleanup = func() { os.RemoveAll(tmp) }
		if err := helpers.ExpandZip(src, tmp); err != nil {
			cleanup()
			return "", func() {}, err
		}
		return tmp, cleanup, nil
	case ".cab":
		if runtime.GOOS != "windows" {
			return "", cleanup, fmt.Errorf("inspecting .cab needs Windows (expand.exe); the pack can still be added with `drivers add`")
		}
		tmp, err := os.MkdirTemp("", "dsky-drivers-")
		if err != nil {
			return "", cleanup, err
		}
		cleanup = func() { os.RemoveAll(tmp) }
		out, err := hidewin.Cmd(exec.CommandContext(ctx, "expand", src, "-F:*", tmp)).CombinedOutput()
		if err != nil {
			cleanup()
			return "", func() {}, fmt.Errorf("expand: %v\n%s", err, out)
		}
		return tmp, cleanup, nil
	case ".exe":
		return "", cleanup, fmt.Errorf("%s is a vendor installer — INFs are packed inside it.\nAdd it as an exe pack with silent flags instead, e.g.:\n  dsky drivers add --id my-wifi --exe-args \"-q -s\" %s", filepath.Base(src), src)
	default:
		return "", cleanup, fmt.Errorf("unsupported pack type %s (dir, .zip, .cab, .inf, .exe)", filepath.Ext(src))
	}
}

func driversInspect(ctx context.Context, env *Env, src string) error {
	dir, cleanup, err := materializeForInspect(ctx, env, src)
	if err != nil {
		return err
	}
	defer cleanup()
	infs, err := drivers.ScanDir(dir)
	if err != nil {
		return err
	}
	if len(infs) == 0 {
		return fmt.Errorf("no .inf files found under %s", src)
	}
	fmt.Printf("%d INF(s) in %s:\n\n", len(infs), src)
	for _, inf := range infs {
		fmt.Printf("  %s\n", inf.File)
		fmt.Printf("    class %-14s provider %-24s ver %s (%s)\n", inf.Class, inf.Provider, inf.DriverVer, inf.DriverDate)
		if len(inf.Devices) > 0 {
			max := len(inf.Devices)
			if max > 3 {
				max = 3
			}
			fmt.Printf("    devices: %s", strings.Join(inf.Devices[:max], " · "))
			if len(inf.Devices) > max {
				fmt.Printf(" (+%d more)", len(inf.Devices)-max)
			}
			fmt.Println()
		}
		max := len(inf.HardwareIDs)
		if max > 4 {
			max = 4
		}
		fmt.Printf("    %d hardware IDs: %s", len(inf.HardwareIDs), strings.Join(inf.HardwareIDs[:max], "  "))
		if len(inf.HardwareIDs) > max {
			fmt.Printf("  …")
		}
		fmt.Println()
		fmt.Println()
	}
	fmt.Println("Installable by pnputil sweep. Stage it with `dsky drivers add --id <name> " + src + "`")
	return nil
}

var packIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

func driversAdd(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("drivers add", flag.ContinueOnError)
	id := fs.String("id", "", "pack id (lowercase, e.g. intel-lan-31.2.2)")
	exeArgs := fs.String("exe-args", "", "silent flags for a vendor .exe pack (e.g. \"-q -s\")")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 || *id == "" || !packIDRe.MatchString(*id) {
		return fmt.Errorf("drivers add --id <pack-id> [--exe-args \"...\"] <dir|.zip|.cab|.exe>")
	}
	src := fs.Arg(0)
	ws, err := env.workspace()
	if err != nil {
		return err
	}

	st, err := os.Stat(src)
	if err != nil {
		return err
	}
	if st.IsDir() {
		// INF directory → copied into the workspace repo (small, reviewable).
		infs, err := drivers.ScanDir(src)
		if err != nil {
			return err
		}
		if len(infs) == 0 {
			return fmt.Errorf("%s holds no .inf files — a pnputil pack needs INFs", src)
		}
		dest := filepath.Join(ws.Dir, "Drivers", *id)
		if _, err := os.Stat(dest); err == nil {
			return fmt.Errorf("%s already exists", dest)
		}
		if err := copyTree(src, dest); err != nil {
			return err
		}
		fmt.Printf("Copied %d file(s), %d INF(s) into Drivers\\%s\n\n", countFiles(dest), len(infs), *id)
		fmt.Println("Add to the recipe's windows.driver_packs:")
		fmt.Printf("    - { path: Drivers/%s, install: pnputil-sweep }\n", *id)
		return nil
	}

	// Single-file packs → library + pinned manifest.
	lib, err := env.library()
	if err != nil {
		return err
	}
	var format manifest.Format
	var install, extra string
	switch strings.ToLower(filepath.Ext(src)) {
	case ".zip":
		format, install = manifest.FormatZip, "pnputil-sweep"
	case ".cab":
		format, install = manifest.FormatCab, "expand-then-sweep"
	case ".exe":
		format, install = manifest.FormatExe, "exe"
		if *exeArgs == "" {
			return fmt.Errorf("a vendor .exe pack needs its silent flags: --exe-args \"-q -s\" (check the vendor's docs)")
		}
		extra = fmt.Sprintf(", args: [%s]", quoteArgs(*exeArgs))
	default:
		return fmt.Errorf("drivers add takes a directory, .zip, .cab, or .exe")
	}
	srcMan := &manifest.Source{ID: *id, Kind: manifest.KindDriverPack, Format: format, Filename: filepath.Base(src)}
	entry, err := lib.Import(srcMan, src)
	if err != nil {
		return err
	}
	manPath := filepath.Join(ws.Dir, "manifests", *id+".yaml")
	if _, err := os.Stat(manPath); err == nil {
		return fmt.Errorf("%s already exists", manPath)
	}
	if err := os.MkdirAll(filepath.Dir(manPath), 0o755); err != nil {
		return err
	}
	man := fmt.Sprintf("id: %s\nkind: driver-pack\nformat: %s\n# No URL: added from a local file. Re-import on other machines:\n#   dsky sources import %s %s\nsha256: %s\nfilename: %s\n",
		*id, format, *id, filepath.Base(src), entry.SHA256, entry.Filename)
	if err := os.WriteFile(manPath, []byte(man), 0o644); err != nil {
		return err
	}
	fmt.Printf("In library (%d MiB, sha256 pinned) with manifest manifests\\%s.yaml\n\n", entry.Size>>20, *id)
	fmt.Println("Add to the recipe's windows.driver_packs:")
	fmt.Printf("    - { ref: %s, install: %s%s }\n", *id, install, extra)
	return nil
}

func quoteArgs(s string) string {
	parts := strings.Fields(s)
	for i, p := range parts {
		parts[i] = fmt.Sprintf("%q", p)
	}
	return strings.Join(parts, ", ")
}

// driversScan lists this machine's devices with driver problems and their
// hardware IDs — what to hunt packs for.
func driversScan(ctx context.Context) error {
	if runtime.GOOS != "windows" {
		return fmt.Errorf("drivers scan inspects the local Windows machine; run it on the target hardware")
	}
	script := `Get-CimInstance Win32_PnPEntity | Where-Object { $_.ConfigManagerErrorCode -ne 0 } | ForEach-Object { ($_.Name, $_.ConfigManagerErrorCode -join "|") + "|" + (($_.HardwareID | Select-Object -First 2) -join " ") }`
	out, err := hidewin.Cmd(exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", script)).Output()
	if err != nil {
		return fmt.Errorf("querying PnP devices: %w", err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	problems := 0
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		parts := strings.SplitN(l, "|", 3)
		if len(parts) < 3 {
			continue
		}
		problems++
		fmt.Printf("  %-45s (error %s)\n", parts[0], parts[1])
		if parts[2] != "" {
			fmt.Printf("      %s\n", parts[2])
		}
	}
	if problems == 0 {
		fmt.Println("No devices with driver problems — this machine is fully driven.")
		return nil
	}
	fmt.Printf("\n%d device(s) need drivers. Look each hardware ID up in the Microsoft Update Catalog:\n", problems)
	fmt.Println("  dsky drivers search mscatalog \"<hardware-id>\" --add")
	fmt.Println("or, for a maker with drivers by model, take all of this model's:")
	fmt.Println("  dsky detect --resolve")
	return nil
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.Create(target)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	})
}

func countFiles(dir string) int {
	n := 0
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			n++
		}
		return nil
	})
	return n
}

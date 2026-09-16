package cli

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/uplinkresearch/dsky/internal/compose"
	"github.com/uplinkresearch/dsky/internal/device"
	"github.com/uplinkresearch/dsky/internal/elevate"
	"github.com/uplinkresearch/dsky/internal/flashrun"
)

func cmdFlash(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("flash", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "skip the typed size confirmation (scripted use)")
	rebuild := fs.Bool("rebuild", false, "ignore the artifact cache")
	all := fs.Bool("all", false, "write every attached USB stick at once")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("flash <recipe-id|artifact.img> <device> [<device> …] [--all] [--yes]")
	}
	what := fs.Arg(0)

	// Resolve targets first — no point composing for a bad one.
	devs, err := pickDevices(ctx, fs.Args()[1:], *all)
	if err != nil {
		return err
	}

	var art *compose.Artifact
	if looksLikeImagePath(what) {
		var composed bool
		art, composed, err = artifactForPath(what)
		if err != nil {
			return err
		}
		if !composed {
			describeForeignImage(art)
		}
	} else {
		ws, err := env.workspace()
		if err != nil {
			return err
		}
		lib, err := env.library()
		if err != nil {
			return err
		}
		art, err = buildArtifact(ctx, env, ws, lib, what, *rebuild, false)
		if err != nil {
			return err
		}
	}
	return armAndFlashMany(ctx, art, devs, *yes)
}

// cmdClone reads a working stick into an image, and optionally writes that
// image straight back out to a batch of blanks — the "make twenty of this
// one" workflow, which is why it is worth having as a single command.
func cmdClone(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("clone", flag.ContinueOnError)
	out := fs.String("out", "", "output image path (default: library artifacts dir)")
	var to stringList
	fs.Var(&to, "to", "after reading, write the image to this stick (repeatable, or --to all)")
	yes := fs.Bool("yes", false, "skip the typed size confirmation")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("clone <device> [--out file.img] [--to <device> …]")
	}
	devs, err := device.List(ctx)
	if err != nil {
		return err
	}
	dev, err := matchDevice(devs, fs.Arg(0))
	if err != nil {
		return err
	}
	if dev.System {
		return fmt.Errorf("refusing to clone the system disk")
	}
	outPath := *out
	if outPath == "" {
		lib, err := env.library()
		if err != nil {
			return err
		}
		name := strings.Map(func(r rune) rune {
			if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' {
				return r
			}
			return '-'
		}, dev.Model)
		outPath = filepath.Join(lib.ArtifactsDir(), fmt.Sprintf("clone-%s-%s.img", name, time.Now().Format("20060102-150405")))
	}

	// Resolve the write targets before reading anything: a typo in --to
	// should not cost the operator a full read of the master first. The
	// source is excluded so a stray "all" cannot overwrite what it just read.
	var targets []device.Device
	if len(to) > 0 {
		targets, err = cloneTargets(ctx, to, dev)
		if err != nil {
			return err
		}
	}

	fmt.Println("About to READ this device (read-only, through its last partition):")
	fmt.Println(" ", dev.String())
	fmt.Println("  to:", outPath)
	if err := confirmSize(dev, *yes); err != nil {
		return err
	}
	if !elevate.IsElevated() {
		fmt.Println("elevating clone worker —", elevate.Hint())
	}
	prog := &stageProgress{}
	result, err := flashrun.RunClone(ctx, dev, outPath, prog.report)
	prog.finish()
	if err != nil {
		return err
	}
	fmt.Printf("Read into %s (%s)\n", outPath, result)
	if len(targets) == 0 {
		fmt.Printf("\nWrite it to blanks with:\n  dsky flash %s <device> [<device> …]\n", filepath.Base(outPath))
		return nil
	}

	art, err := compose.LoadArtifact(compose.MetaPath(outPath))
	if err != nil {
		return fmt.Errorf("no artifact metadata beside the image: %w", err)
	}
	return armAndFlashMany(ctx, art, targets, *yes)
}

// cloneTargets resolves --to, refusing the source so a clone cannot eat the
// master it was taken from.
func cloneTargets(ctx context.Context, to []string, src device.Device) ([]device.Device, error) {
	all := len(to) == 1 && strings.EqualFold(to[0], "all")
	args := to
	if all {
		args = nil
	}
	devs, err := pickDevices(ctx, args, all)
	if err != nil {
		return nil, err
	}
	var out []device.Device
	for _, d := range devs {
		if d.ID == src.ID {
			if !all {
				return nil, fmt.Errorf("%s is the device being cloned — it cannot also be a target", d.ID)
			}
			continue // --all: quietly leave the master out
		}
		out = append(out, d)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no target sticks left after excluding the source")
	}
	return out, nil
}

// confirmSize is the typed-size interlock: the operator must type the
// target's exact size to arm a destructive write.
func confirmSize(dev device.Device, skip bool) error {
	if skip {
		return nil
	}
	want := dev.SizeConfirmation()
	fmt.Printf("Type the device size (%s) to confirm: ", want)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return fmt.Errorf("confirmation aborted: %w", err)
	}
	if strings.TrimSpace(line) != want {
		return fmt.Errorf("confirmation mismatch — aborting, nothing written")
	}
	return nil
}

// cmdFlashWorker runs inside the elevated relaunch; exit code is the result.
func cmdFlashWorker(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("flash-worker", flag.ContinueOnError)
	jobPath := fs.String("job", "", "job file")
	progPath := fs.String("progress", "", "progress file")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	return flashrun.Worker(ctx, *jobPath, *progPath)
}

// matchDevice resolves a user-typed device argument: the exact ID, or the
// platform shorthand (a disk number on Windows, sdX on Linux, diskN on mac).
func matchDevice(devs []device.Device, arg string) (device.Device, error) {
	norm := strings.ToLower(strings.TrimSpace(arg))
	for _, d := range devs {
		if strings.EqualFold(d.ID, arg) {
			return d, nil
		}
		short := strings.ToLower(strings.TrimPrefix(strings.TrimPrefix(d.ID, `\\.\`), "/dev/"))
		if short == norm || short == "r"+norm || strings.TrimPrefix(short, "physicaldrive") == norm {
			return d, nil
		}
	}
	var ids []string
	for _, d := range devs {
		if d.Flashable() {
			ids = append(ids, d.ID)
		}
	}
	if len(ids) == 0 {
		return device.Device{}, fmt.Errorf("no device matches %q and no flashable devices are attached", arg)
	}
	return device.Device{}, fmt.Errorf("no device matches %q — flashable: %s", arg, strings.Join(ids, ", "))
}

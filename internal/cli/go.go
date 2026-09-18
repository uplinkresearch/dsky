package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/uplinkresearch/dsky/internal/compose"
	"github.com/uplinkresearch/dsky/internal/device"
	"github.com/uplinkresearch/dsky/internal/elevate"
	"github.com/uplinkresearch/dsky/internal/flashrun"
	"github.com/uplinkresearch/dsky/internal/helpers"
	"github.com/uplinkresearch/dsky/internal/library"
	"github.com/uplinkresearch/dsky/internal/manifest"
	"github.com/uplinkresearch/dsky/internal/recipe"
	"github.com/uplinkresearch/dsky/internal/workspace"
)

// cmdGo is the one-shot pipeline — "compose this": pull whatever pinned
// sources are missing, build, pick the attached USB stick, arm, flash,
// verify. `dsky <recipe>` and a bare `dsky` in a one-recipe
// workspace both land here.
func cmdGo(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("go", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "skip the typed size confirmation (scripted use)")
	rebuild := fs.Bool("rebuild", false, "ignore the artifact cache")
	tofu := fs.Bool("pin-tofu", false, "accept unpinned sources on first download (prints the hash to pin)")
	buildOnly := fs.Bool("build-only", false, "stop after building; do not flash")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() < 1 || fs.NArg() > 2 {
		return fmt.Errorf("go <recipe-id|artifact.img> [device] [--yes] [--build-only]")
	}
	what := fs.Arg(0)
	devArg := ""
	if fs.NArg() == 2 {
		devArg = fs.Arg(1)
	}

	var dev device.Device
	if !*buildOnly {
		d, err := pickDevice(ctx, devArg)
		if err != nil {
			return err
		}
		dev = d
	}

	var art *compose.Artifact
	if looksLikeImagePath(what) {
		a, composed, err := artifactForPath(what)
		if err != nil {
			return err
		}
		if !composed {
			describeForeignImage(a)
		}
		art = a
	} else {
		ws, err := env.workspace()
		if err != nil {
			return err
		}
		lib, err := env.library()
		if err != nil {
			return err
		}
		r, err := ws.Recipe(what)
		if err != nil {
			return err
		}
		if err := resolveHardware(ctx, ws, lib, r); err != nil {
			return err
		}
		if err := ensureSources(ctx, ws, lib, r, *tofu); err != nil {
			return err
		}
		art, err = buildArtifact(ctx, env, ws, lib, what, *rebuild, false)
		if err != nil {
			return err
		}
	}
	if *buildOnly {
		fmt.Printf("built: %s (%d MiB)\n", art.Path, art.Size>>20)
		return nil
	}
	return armAndFlash(ctx, art, dev, *yes)
}

// pickDevice resolves an explicit device argument, or — the common case —
// the single flashable USB stick attached. Anything ambiguous stops.
func pickDevice(ctx context.Context, arg string) (device.Device, error) {
	devs, err := device.List(ctx)
	if err != nil {
		return device.Device{}, err
	}
	if arg != "" {
		dev, err := matchDevice(devs, arg)
		if err != nil {
			return device.Device{}, err
		}
		if !dev.Flashable() {
			return device.Device{}, fmt.Errorf("%s is not flashable (bus=%s, system=%v) — `dsky devices` shows valid targets", dev.ID, dev.Bus, dev.System)
		}
		return dev, nil
	}
	var usable []device.Device
	for _, d := range devs {
		if d.Flashable() {
			usable = append(usable, d)
		}
	}
	switch len(usable) {
	case 1:
		return usable[0], nil
	case 0:
		return device.Device{}, fmt.Errorf("no USB stick attached — plug one in and re-run (`dsky devices` lists targets)")
	default:
		var ids []string
		for _, d := range usable {
			ids = append(ids, d.String())
		}
		return device.Device{}, fmt.Errorf("%d USB sticks attached; name the target:\n  %s", len(usable), strings.Join(ids, "\n  "))
	}
}

// pickDevices resolves the targets for a write: named devices, every attached
// stick with --all, or — the common case — the single one plugged in.
func pickDevices(ctx context.Context, args []string, all bool) ([]device.Device, error) {
	if all && len(args) > 0 {
		return nil, fmt.Errorf("--all writes every attached stick; do not also name devices")
	}
	if all {
		devs, err := device.List(ctx)
		if err != nil {
			return nil, err
		}
		var usable []device.Device
		for _, d := range devs {
			if d.Flashable() {
				usable = append(usable, d)
			}
		}
		if len(usable) == 0 {
			return nil, fmt.Errorf("no USB sticks attached")
		}
		return usable, nil
	}
	if len(args) <= 1 {
		arg := ""
		if len(args) == 1 {
			arg = args[0]
		}
		d, err := pickDevice(ctx, arg)
		if err != nil {
			return nil, err
		}
		return []device.Device{d}, nil
	}
	devs, err := device.List(ctx)
	if err != nil {
		return nil, err
	}
	var out []device.Device
	seen := map[string]bool{}
	for _, a := range args {
		d, err := matchDevice(devs, a)
		if err != nil {
			return nil, err
		}
		if !d.Flashable() {
			return nil, fmt.Errorf("%s is not flashable (bus=%s, system=%v)", d.ID, d.Bus, d.System)
		}
		if seen[d.ID] {
			return nil, fmt.Errorf("%s named twice", d.ID)
		}
		seen[d.ID] = true
		out = append(out, d)
	}
	return out, nil
}

// ensureSources pulls every pinned source the recipe needs that is not in
// the library yet. Windows tree-mode builds skip the ISO when the master
// tree is present on this machine.
func ensureSources(ctx context.Context, ws *workspace.Workspace, lib *library.Library, r *recipe.Recipe, tofu bool) error {
	skipOS := false
	if r.OS.Type == recipe.OSWindows && r.OS.TreePath != "" {
		tp := r.OS.TreePath
		if !filepath.IsAbs(tp) {
			tp = filepath.Join(ws.Dir, filepath.FromSlash(tp))
		}
		if st, err := os.Stat(tp); err == nil && st.IsDir() {
			skipOS = true
		}
	}
	for _, ref := range r.SourceRefs() {
		if skipOS && ref == r.OS.Source {
			continue
		}
		if _, err := lib.Resolve(ref); err == nil {
			continue
		}
		src, err := ws.Source(ref)
		if err != nil {
			return fmt.Errorf("%s is not in the library and has no manifest — add one, or `dsky sources import %s <file>`", ref, ref)
		}
		if src.URL == "" && src.Provider == "" {
			return fmt.Errorf("%s is not in the library and its manifest has no url/provider — `dsky sources import %s <file>`", ref, ref)
		}
		fmt.Printf("pulling %s...\n", ref)
		prog := &stageProgress{}
		resolver := func(ctx context.Context, s *manifest.Source) (string, error) {
			prog.report("resolving download URL", 0, -1)
			return helpers.ResolveFidoURL(ctx, lib.HelpersDir(), s.Fido)
		}
		entry, err := lib.Pull(ctx, src, tofu, resolver, func(done, total int64) {
			prog.report("download", done, total)
		}, func(s string) { prog.report(s, 0, -1) })
		prog.finish()
		var unpinned *library.ErrUnpinned
		if errors.As(err, &unpinned) {
			return fmt.Errorf("%s downloaded but is unpinned (sha256 %s) — add that sha256 to its manifest, or re-run with --pin-tofu", ref, unpinned.SHA256)
		}
		if err != nil {
			return err
		}
		fmt.Printf("  %s in library (%d MiB)\n", entry.ID, entry.Size>>20)
		if src.SHA256 == "" {
			fmt.Printf("  PIN IT: add `sha256: %s` to manifests/%s.yaml\n", entry.SHA256, ref)
		}
	}
	return nil
}

// armAndFlash shows the target, takes the typed-size confirmation, and
// runs the (elevated) flash with progress.
func armAndFlash(ctx context.Context, art *compose.Artifact, dev device.Device, yes bool) error {
	return armAndFlashMany(ctx, art, []device.Device{dev}, yes)
}

// armAndFlashMany arms and writes one or more sticks under a single
// elevation. One stick keeps the familiar typed-size interlock; several are
// confirmed by typing how many, since making someone type twenty sizes would
// only teach them to reach for --yes.
func armAndFlashMany(ctx context.Context, art *compose.Artifact, devs []device.Device, yes bool) error {
	// A payload is a zip to be run on a machine, not a disk image. Written
	// raw to a stick it would wipe the stick and boot nothing, so it is
	// refused before any confirmation is even offered.
	if art.Kind == "payload" {
		return fmt.Errorf("%s is a payload, not bootable media — copy the zip onto a machine and run it there (README.txt inside says how)",
			filepath.Base(art.Path))
	}
	fmt.Println()
	if len(devs) == 1 {
		fmt.Println("About to WIPE this device:")
	} else {
		fmt.Printf("About to WIPE these %d devices:\n", len(devs))
	}
	for _, d := range devs {
		fmt.Println(" ", d.String())
		if len(d.Mounts) > 0 {
			fmt.Println("    currently mounted at:", strings.Join(d.Mounts, ", "))
		}
	}
	fmt.Printf("  writing: %s (%d MiB, verify %s)\n", filepath.Base(art.Path), art.Size>>20, art.Verify)

	if len(devs) == 1 {
		if err := confirmSize(devs[0], yes); err != nil {
			return err
		}
	} else if err := confirmCount(len(devs), yes); err != nil {
		return err
	}

	if !elevate.IsElevated() {
		fmt.Println("elevating flash worker —", elevate.Hint())
	}
	prog := newMultiProgress(devs)
	err := flashrun.RunFlashMany(ctx, art, devs, prog.report)
	prog.finish()
	if err != nil {
		return err
	}
	if len(devs) == 1 {
		fmt.Printf("Done. %s is written and verified — safe to remove.\n", devs[0].ID)
	} else {
		fmt.Printf("Done. All %d are written and verified — safe to remove.\n", len(devs))
	}
	return nil
}

// confirmCount is the multi-stick interlock: the operator has just been shown
// every target and types how many they meant.
func confirmCount(n int, yes bool) error {
	if yes {
		return nil
	}
	fmt.Printf("\nThis PERMANENTLY ERASES all %d. Type %d to confirm: ", n, n)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return fmt.Errorf("reading confirmation: %w", err)
	}
	if strings.TrimSpace(line) != fmt.Sprint(n) {
		return fmt.Errorf("confirmation mismatch — nothing was written")
	}
	return nil
}

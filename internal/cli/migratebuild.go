package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/uplinkresearch/dsky/internal/appcatalog"
	"github.com/uplinkresearch/dsky/internal/compose"
	"github.com/uplinkresearch/dsky/internal/migrate"
	"github.com/uplinkresearch/dsky/internal/oscatalog"
)

// Building a migration is the point where this feature stops being a
// description and starts writing to a drive, so it is the command that says
// the most before it does anything. What it does is deliberately little of
// its own: the plan becomes the same choices `dsky install` already takes,
// and the media comes out of the pipeline that has been building Windows
// sticks all along.
//
// The one thing it adds is the domain join. `djoin /provision` creates the
// computer account in advance and writes a file that carries that account's
// password; it needs a domain-joined Windows machine to run on, so on
// anything else this command prints the exact command to run over there and
// asks for the file back. No domain credential ever reaches the drive.
func migrateBuild(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("migrate build", flag.ContinueOnError)
	blob := fs.String("odj-blob", "", "the offline domain join file for this machine (made by djoin /provision)")
	provision := fs.Bool("provision", false, "run djoin /provision here (needs a domain-joined Windows computer)")
	iso := fs.String("iso", "", "a Windows 11 ISO you already have, instead of fetching one")
	target := fs.String("device", "", "write to this drive when the build is done")
	buildOnly := fs.Bool("build-only", false, "build the image and stop, without writing a drive")
	yes := fs.Bool("yes", false, "skip the typed size confirmation when writing a drive")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("migrate build <manifest.json> [--odj-blob file | --provision] [--iso file] [--device X: | --build-only]")
	}
	m, err := migrate.Load(fs.Arg(0))
	if err != nil {
		return err
	}
	// Everything that makes this plan unbuildable, before a byte is fetched.
	if err := migrate.Preflight(m); err != nil {
		return err
	}

	blobPath := *blob
	if m.Identity.Domained() && blobPath == "" {
		blobPath, err = provisionJoin(ctx, m, *provision, filepath.Dir(fs.Arg(0)))
		if err != nil {
			return err
		}
	}
	plan, err := migrate.PlanBuild(m, blobPath)
	if err != nil {
		return err
	}

	lib, err := env.library()
	if err != nil {
		return err
	}
	// The installers the plan names are copied into the library, which is how
	// every other operator-supplied .msi reaches a stick. A missing one stops
	// the build here rather than at somebody's first boot.
	for _, in := range migrate.Installers(m) {
		if _, err := os.Stat(in.Path); err != nil {
			return fmt.Errorf("%s installs from %s, which is not readable from here: %w",
				in.App.DisplayName, in.Path, err)
		}
		if _, err := appcatalog.AddInstaller(env.libraryRoot(), lib, appcatalog.Installer{
			Path: in.Path, ID: in.ID, Name: in.App.DisplayName,
			Args: strings.Join(in.Args, " "), Category: "Migration", Replace: true,
		}, nil); err != nil {
			return fmt.Errorf("staging %s: %w", in.App.DisplayName, err)
		}
	}

	// What is about to happen, in the words of the plan.
	fmt.Printf("%s — rebuilding as %s\n", m.Source.Hostname, describeTarget(m))
	fmt.Printf("  %d program(s) at first boot, %d by hand afterwards\n", len(plan.Apps), len(plan.Manual))
	if m.Identity.Domained() {
		fmt.Printf("  joins %s from %s\n", m.Identity.DomainFQDN, filepath.Base(blobPath))
	}
	for _, w := range plan.Warnings {
		fmt.Printf("  note: %s\n", w)
	}
	for _, cmd := range migrate.GroupCommands(m) {
		fmt.Printf("  afterwards, where RSAT is: %s\n", cmd)
	}

	entry, ok := oscatalog.Get("windows-11")
	if !ok {
		return fmt.Errorf("the catalog has no windows-11 entry")
	}
	opts := oscatalog.Options{
		Edition: plan.Edition, AccountMode: plan.AccountMode, Debloat: plan.Debloat,
		Locale: plan.Locale, Timezone: plan.Timezone, AdminUser: plan.AdminUser,
		Hostname: plan.Hostname, DomainBlob: plan.DomainBlob, Apps: plan.Apps,
		BypassRequirement: true, // a replacement PC is new hardware; this costs nothing and saves a rebuild
	}
	if *iso != "" {
		if err := importISO(lib, entry, *iso); err != nil {
			return err
		}
	}
	prog := &stageProgress{}
	art, err := oscatalog.BuildQuick(ctx, lib, entry, opts, prog.report)
	prog.finish()
	if err != nil {
		return err
	}
	fmt.Printf("built: %s (%d MiB)\n", art.Path, art.Size>>20)

	if *buildOnly || *target == "" {
		fmt.Println("write it to a drive with: dsky flash", art.Path, "<device>")
		return nil
	}
	devs, err := pickDevices(ctx, []string{*target}, false)
	if err != nil {
		return err
	}
	return armAndFlashMany(ctx, art, devs, *yes)
}

// provisionJoin makes the computer account and returns the file that carries
// it. On a domain-joined Windows machine it runs djoin; anywhere else it says
// exactly what to run and where to put the result, because that is a two
// minute job on another computer and a dead end otherwise.
func provisionJoin(ctx context.Context, m *migrate.Manifest, run bool, dir string) (string, error) {
	save := filepath.Join(dir, migrate.BlobName(m))
	cmd := migrate.DjoinCommand(m, save)
	if !run || runtime.GOOS != "windows" {
		where := "a domain-joined Windows computer"
		if runtime.GOOS == "windows" && !run {
			where = "this computer (add --provision to run it here)"
		}
		return "", fmt.Errorf(`this machine joins %s, so it needs an offline join file.

Run this on %s:

  %s

then build again with --odj-blob %s

The file carries one computer account's password: it is for this machine only,
and it is used up the first time it is used`, m.Identity.DomainFQDN, where, cmd, save)
	}
	fmt.Println("provisioning the computer account:", cmd)
	out, err := exec.CommandContext(ctx, "djoin", "/provision",
		"/domain", m.Identity.DomainFQDN,
		"/machine", m.Target.Hostname,
		"/machineou", m.Identity.ComputerOUDN,
		"/savefile", save, "/reuse").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("djoin: %v\n%s", err, strings.TrimSpace(string(out)))
	}
	if err := compose.CheckODJBlob(save); err != nil {
		return "", err
	}
	fmt.Println("wrote", save)
	return save, nil
}

func describeTarget(m *migrate.Manifest) string {
	bits := []string{"Windows 11 " + m.Target.OSEdition}
	if m.Target.Hostname != "" {
		bits = append(bits, m.Target.Hostname)
	}
	if m.Target.Timezone != "" {
		bits = append(bits, m.Target.Timezone)
	}
	return strings.Join(bits, ", ")
}

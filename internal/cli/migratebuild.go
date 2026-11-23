package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/uplinkresearch/dsky/internal/agent"
	"github.com/uplinkresearch/dsky/internal/appcatalog"
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
// It does not join a domain. A machine that was in one arrives in a
// workgroup under the plan's name, and the build says so up front, with the
// OU and groups, for whoever joins it by hand.
func migrateBuild(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("migrate build", flag.ContinueOnError)
	iso := fs.String("iso", "", "a Windows 11 ISO you already have, instead of fetching one")
	target := fs.String("device", "", "write to this drive when the build is done")
	buildOnly := fs.Bool("build-only", false, "build the image and stop, without writing a drive")
	passStdin := fs.Bool("admin-password-stdin", false, "read the local administrator's password from stdin")
	yes := fs.Bool("yes", false, "skip the typed size confirmation when writing a drive")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("migrate build <manifest.json> [--iso file] [--device X: | --build-only]")
	}
	m, err := migrate.Load(fs.Arg(0))
	if err != nil {
		return err
	}
	// Everything that makes this plan unbuildable, before a byte is fetched.
	if err := migrate.Preflight(m); err != nil {
		return err
	}

	plan, err := migrate.PlanBuild(m)
	if err != nil {
		return err
	}
	adminPass, err := adminPassword(m, *passStdin, fs.Arg(0))
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
	for _, w := range plan.Warnings {
		fmt.Printf("  note: %s\n", w)
	}

	entry, ok := oscatalog.Get("windows-11")
	if !ok {
		return fmt.Errorf("the catalog has no windows-11 entry")
	}
	// The plan's settings, split into what the machine gets now and what
	// waits for the person who signs in. The split is migrate's to make; the
	// agent only carries it out.
	machineSet, perUserSet := migrate.SettingActions(m, "win11")
	var settings *agent.Settings
	if len(machineSet) > 0 || len(perUserSet) > 0 {
		settings = &agent.Settings{
			Machine: agentActions(machineSet),
			PerUser: agentActions(perUserSet),
		}
	}
	printers, drives := migrate.PrintersAndDrives(m)
	opts := oscatalog.Options{
		Edition: plan.Edition, AccountMode: plan.AccountMode, Debloat: plan.Debloat,
		Locale: plan.Locale, Timezone: plan.Timezone, AdminUser: plan.AdminUser,
		AdminPassword: adminPass,
		Hostname:      plan.Hostname, Apps: plan.Apps,
		Settings:          settings,
		Printers:          agentPrinters(printers),
		Drives:            agentDrives(drives),
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

// adminPassword is the new machine's local administrator password. A migration
// asks for one, and a domain machine will not build without it.
//
// The reason is what the machine becomes. It replaces one that was in a
// domain, so it is about to be joined to that domain, and a passwordless
// administrator on a domain member means anybody who walks past it gets an
// administrator's desktop -- on a machine the domain trusts. On a standalone machine the same
// account is a local one on a box in front of you, which is why the quick
// install has always allowed it and still does.
//
// It is not a flag value. This repository already says why, about the other
// password it handles: a password passed to a command lands in shell history
// and in the scrollback of whoever is watching. So it comes in on stdin or
// from the environment, and it is never written down -- not in the manifest,
// which is a document people mail around, and not in the recipe the build
// leaves in the library.
func adminPassword(m *migrate.Manifest, fromStdin bool, manifestFile string) (string, error) {
	if fromStdin {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", fmt.Errorf("reading the administrator password from stdin: %w", err)
		}
		if p := strings.Trim(string(b), "\r\n"); p != "" {
			return p, nil
		}
		return "", fmt.Errorf("--admin-password-stdin was given but stdin was empty")
	}
	if p := os.Getenv("DSKY_ADMIN_PASSWORD"); p != "" {
		return p, nil
	}
	if !m.Identity.Domained() {
		return "", nil
	}
	return "", fmt.Errorf(`this machine replaces one in %s, so %s needs a password.

It is about to be joined to that domain, and a domain PC with a
passwordless administrator is an administrator's desktop for anybody
who walks past it.

Give it one of these two ways, so that it stays out of your shell history:

  printf %%s 'the-password' | dsky migrate build %s --admin-password-stdin ...
  DSKY_ADMIN_PASSWORD=... dsky migrate build ...

It is not written to the plan or kept in the library`,
		m.Identity.DomainFQDN, orUser(m.Target.LocalAdmin), filepath.Base(manifestFile))
}

// orUser names the account in a message when the plan does not.
func orUser(name string) string {
	if name == "" {
		return "the local administrator"
	}
	return name
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

// agentActions is the same list, in the type the agent reads. Two types for
// one idea, and deliberately: internal/migrate must not import the agent,
// because the agent is what reads a manifest on the machine.
func agentActions(in []migrate.SettingAction) []agent.Action {
	var out []agent.Action
	for _, a := range in {
		out = append(out, agent.Action{Key: a.Key, Method: a.Method, Ref: a.Ref})
	}
	return out
}

func agentPrinters(in []migrate.Printer) []agent.Printer {
	var out []agent.Printer
	for _, p := range in {
		out = append(out, agent.Printer{
			Name: p.Name, SharedPath: p.SharedPath, IP: p.IP,
			Port: p.Port, DriverName: p.DriverName,
		})
	}
	return out
}

func agentDrives(in []migrate.MappedDrive) []agent.MappedDrive {
	var out []agent.MappedDrive
	for _, d := range in {
		out = append(out, agent.MappedDrive{Letter: d.Letter, UNC: d.UNC})
	}
	return out
}

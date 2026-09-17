package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/uplinkresearch/dsky/internal/buildinfo"
	"github.com/uplinkresearch/dsky/internal/migrate"
)

// Replacing a Windows 10 PC with a new machine is the job this covers: scan
// the old one, decide what comes across, build media that recreates it. The
// manifest is the whole of it -- every verb here either writes that file,
// checks it, or renders it -- so these three commands work on a file and need
// neither a workspace nor the machine being replaced.
//
// The verbs that need more than a file (scan, resolve, review, build, verify)
// arrive with the milestones in docs/plan-migrate.md; until then this command
// says so rather than pretending.
func cmdMigrate(ctx context.Context, env *Env, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("migrate: need scan, validate, report or schema (see docs/plan-migrate.md)")
	}
	switch args[0] {
	case "scan":
		return migrateScan(ctx, args[1:])
	case "validate":
		return migrateValidate(args[1:])
	case "report":
		return migrateReport(args[1:])
	case "schema":
		if len(args) != 1 {
			return fmt.Errorf("migrate schema")
		}
		_, err := os.Stdout.Write(migrate.JSONSchema())
		return err
	case "resolve", "review", "build", "verify":
		return fmt.Errorf("migrate %s is not built yet — scan, validate, report and schema are; see docs/plan-migrate.md", args[0])
	default:
		return fmt.Errorf("migrate: no such thing as %q (scan, validate, report, schema)", args[0])
	}
}

// migrateScan reads the machine it runs on. It is the one command in this
// family that touches a computer rather than a file, and the computer it
// touches is somebody's working PC -- so it reads, writes its two files
// wherever it was told, and leaves nothing behind.
//
// A scan that could not read part of the machine still writes its manifest
// and exits 3, so that a script driving a fleet can tell a complete reading
// from a partial one without parsing the output.
func migrateScan(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("migrate scan", flag.ContinueOnError)
	out := fs.String("out", ".", "where to write manifest.json and report.html")
	allUsers := fs.Bool("all-users", false, "include users who are not signed in (slower: their registry hives are loaded)")
	sizes := fs.Bool("profile-sizes", false, "measure each profile, to size a USMT store (slow on large profiles)")
	usmt := fs.String("usmt", "", "folder holding scanstate.exe, from the Windows ADK")
	hostname := fs.String("hostname", "", "name for the new machine (default: this machine's name)")
	admin := fs.String("local-admin", "uplink", "the local administrator DSKY creates on the new machine")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("migrate scan [--out dir] [--all-users] [--profile-sizes] [--usmt folder]")
	}
	res, err := migrate.ScanToFiles(ctx, *out, migrate.ScanOptions{
		AllUsers: *allUsers, ProfileSizes: *sizes, Hostname: *hostname, LocalAdmin: *admin,
	}, *usmt, "dsky "+buildinfo.Version, os.Stdout)
	if err != nil {
		return err
	}
	if len(res.Unread) > 0 {
		return exitError{code: 3, err: fmt.Errorf("%d part(s) of this machine could not be read; the manifest says which", len(res.Unread))}
	}
	return nil
}

// migrateValidate is the answer to "is this file a migration plan DSKY will
// act on?" -- worth its own command because a manifest is edited by hand more
// often than anything else in DSKY, and because a plan that is fine except
// for one thing should say which thing.
func migrateValidate(args []string) error {
	fs := flag.NewFlagSet("migrate validate", flag.ContinueOnError)
	quiet := fs.Bool("quiet", false, "say nothing when the manifest is good")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("migrate validate <manifest.json> [--quiet]")
	}
	m, err := migrate.Load(fs.Arg(0))
	if err != nil {
		return err
	}
	if *quiet {
		return nil
	}
	c := m.Count("win11")
	fmt.Printf("%s — %s %s, %s\n", m.Source.Hostname,
		m.Source.Hardware.Manufacturer, m.Source.Hardware.Model, m.Source.OS.ProductName)
	// Manual work is counted apart from the installs: it is on the checklist
	// the new machine prints, not in the run.
	fmt.Printf("  %d applications: %d will install, %d with no source, %d blocked, %d left behind, %d by hand\n",
		c.Apps, len(m.InstallOrder()), c.Unmapped, c.Blocked, c.Dropped, c.Manual)
	fmt.Printf("  %d of %d settings can be applied on Windows 11\n", c.SettingsApplied, c.Settings)
	if m.Identity.Domained() {
		fmt.Printf("  joins %s", m.Identity.DomainFQDN)
		if m.Identity.ComputerOUDN != "" {
			fmt.Printf(" in %s", m.Identity.ComputerOUDN)
		}
		fmt.Println()
	}
	fmt.Printf("  user files: %s\n", m.Data.Strategy)
	if c.Blockers > 0 || c.Warnings > 0 {
		fmt.Printf("  %d blocker(s) (%d unanswered), %d warning(s)\n", c.Blockers, c.UnackedBlockers, c.Warnings)
	}
	switch err := m.CheckApproved(); {
	case err == nil:
		fmt.Printf("  approved by %s — ready to build\n", m.Approval.ApprovedBy)
	default:
		fmt.Printf("  %v\n", err)
		if why := m.ReadyToApprove(); why != nil {
			fmt.Printf("  before it can be approved: %v\n", why)
		}
	}
	return nil
}

// migrateReport renders the manifest as the page a customer reads. It is
// generated every time rather than kept: a report that disagrees with the
// plan is worse than no report.
func migrateReport(args []string) error {
	fs := flag.NewFlagSet("migrate report", flag.ContinueOnError)
	out := fs.String("out", "", "write here instead of beside the manifest (- for standard output)")
	osName := fs.String("os", "win11", "the Windows the new machine will run: win11 or win10")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("migrate report <manifest.json> [--out report.html] [--os win11]")
	}
	m, err := migrate.Load(fs.Arg(0))
	if err != nil {
		return err
	}
	if *out == "-" {
		return migrate.Report(os.Stdout, m, *osName, "dsky "+buildinfo.Version)
	}
	path := *out
	if path == "" {
		path = filepath.Join(filepath.Dir(fs.Arg(0)), "report.html")
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := migrate.Report(f, m, *osName, "dsky "+buildinfo.Version); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	fmt.Println("wrote", path)
	return nil
}

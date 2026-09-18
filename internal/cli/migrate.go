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
		return fmt.Errorf("migrate: need scan, resolve, review, build, validate, report or schema (see docs/plan-migrate.md)")
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
	case "resolve":
		return migrateResolve(args[1:])
	case "review":
		return migrateReview(args[1:])
	case "build":
		return migrateBuild(ctx, env, args[1:])
	case "verify":
		return fmt.Errorf("migrate verify is not built yet — scan, resolve, review, build, validate, report and schema are; see docs/plan-migrate.md")
	default:
		return fmt.Errorf("migrate: no such thing as %q (scan, resolve, review, build, validate, report, schema)", args[0])
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

// migrateResolve works out where each application comes from on the new
// machine, and writes what it found back into the manifest. It runs on the
// operator's own computer, not the one being replaced: this is the step that
// needs the site's table and, one day, an index of every package there is.
//
// It never decides anything twice. An application somebody settled in an
// earlier review keeps that decision, so running this again after adding an
// entry to the site's table only fills what is still blank.
func migrateResolve(args []string) error {
	fs := flag.NewFlagSet("migrate resolve", flag.ContinueOnError)
	site := fs.String("site", "", "the site's mapping table (default: mappings.json beside the manifest)")
	strict := fs.Bool("strict", false, "exit non-zero if anything is left with nowhere to install from")
	dry := fs.Bool("dry-run", false, "say what would be resolved without writing the manifest")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("migrate resolve <manifest.json> [--site mappings.json] [--strict] [--dry-run]")
	}
	path := fs.Arg(0)
	m, err := migrate.Load(path)
	if err != nil {
		return err
	}
	sitePath := *site
	if sitePath == "" {
		sitePath = filepath.Join(filepath.Dir(path), migrate.MappingName)
	}
	table, err := migrate.LoadTable(sitePath)
	if err != nil {
		return err
	}
	if len(table.Entries) > 0 {
		fmt.Printf("%d decision(s) from %s\n", len(table.Entries), sitePath)
	}

	res := migrate.Resolve(m, migrate.SiteTable(table), migrate.GlobalTable(),
		migrate.NameMatch{Known: migrate.KnownApps()})
	fmt.Println(res.Summary())
	for _, a := range m.Apps {
		switch {
		case a.Resolution.Status == migrate.StatusResolved && a.Resolution.ResolvedBy == migrate.ByAuto && a.Resolution.Confidence < 1:
			fmt.Printf("  %-44s %s (%.0f%% sure)\n", short(a.DisplayName), a.Resolution.Ref, a.Resolution.Confidence*100)
		case a.Resolution.Status == migrate.StatusUnmapped && a.Resolution.Ref != "":
			fmt.Printf("  %-44s maybe %s (%.0f%%) — the review asks\n", short(a.DisplayName), a.Resolution.Ref, a.Resolution.Confidence*100)
		case a.Resolution.Status == migrate.StatusUnset, a.Resolution.Status == migrate.StatusUnmapped:
			fmt.Printf("  %-44s nowhere to install from\n", short(a.DisplayName))
		}
	}
	if *dry {
		return nil
	}
	// Approval covers the plan, and the plan just changed; a manifest that
	// was approved before this ran has to be looked at again.
	if m.Approval.Approved {
		m.Unapprove()
		fmt.Println("this manifest was approved before; that approval is cleared because the plan changed")
	}
	if err := m.Save(path); err != nil {
		return err
	}
	fmt.Println("wrote", path)
	if *strict && res.Left > 0 {
		return exitError{code: 3, err: fmt.Errorf("%d application(s) still have nowhere to install from", res.Left)}
	}
	return nil
}

// short keeps a long product name inside the column it is printed in.
func short(s string) string {
	if len(s) <= 44 {
		return s
	}
	return s[:41] + "..."
}

// migrateReview is the step that cannot be skipped: a person reads the plan,
// settles what the resolver could not, and approves it. Everything they
// decide is offered to the site's table, so the next machine at that customer
// asks fewer questions than this one did.
func migrateReview(args []string) error {
	fs := flag.NewFlagSet("migrate review", flag.ContinueOnError)
	site := fs.String("site", "", "the site's mapping table (default: mappings.json beside the manifest)")
	by := fs.String("by", "", "who is approving (default: this account)")
	auto := fs.Bool("auto-approve", false, "approve without asking, and only if there is nothing to ask about")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("migrate review <manifest.json> [--site mappings.json] [--by name] [--auto-approve]")
	}
	path := fs.Arg(0)
	m, err := migrate.Load(path)
	if err != nil {
		return err
	}
	who := *by
	if who == "" {
		who = whoami()
	}

	if *auto {
		if err := migrate.AutoApprove(m, who); err != nil {
			return err
		}
		if err := m.Save(path); err != nil {
			return err
		}
		fmt.Printf("approved by %s without asking: nothing was outstanding\n", who)
		return nil
	}

	sitePath := *site
	if sitePath == "" {
		sitePath = filepath.Join(filepath.Dir(path), migrate.MappingName)
	}
	table, err := migrate.LoadTable(sitePath)
	if err != nil {
		return err
	}
	r := &migrate.Review{
		In: os.Stdin, Out: os.Stdout, By: who,
		Site: table, SitePath: sitePath,
		Save: func(t *migrate.Table) error { return t.Save(sitePath) },
	}
	changed, err := r.Run(m)
	if err != nil {
		return err
	}
	if !changed {
		fmt.Println("nothing changed; the manifest is as it was")
		return nil
	}
	if err := m.Save(path); err != nil {
		return err
	}
	fmt.Println("wrote", path)
	if len(table.Entries) > 0 {
		fmt.Printf("%s holds %d decision(s) for this site\n", sitePath, len(table.Entries))
	}
	return nil
}

// whoami is who to record as having approved a plan, when nobody said.
func whoami() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	if u := os.Getenv("USERNAME"); u != "" {
		return u
	}
	return "operator"
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

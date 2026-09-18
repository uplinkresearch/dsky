package cli

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

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
		return fmt.Errorf("migrate: need scan, resolve, review, build, capture, restore, result, verify, validate, report or schema (see docs/plan-migrate.md)")
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
	case "result":
		return migrateResult(args[1:])
	case "capture":
		return migrateUSMT(ctx, args[1:], true)
	case "restore":
		return migrateUSMT(ctx, args[1:], false)
	case "verify":
		return migrateVerify(ctx, args[1:])
	default:
		return fmt.Errorf("migrate: no such thing as %q (scan, resolve, review, build, capture, restore, result, verify, validate, report, schema)", args[0])
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

// migrateResult renders the record a machine wrote about its own build.
//
// The record is JSON on the machine, which is the right thing for the agent to
// write and the wrong thing to hand anybody. This turns it into the page that
// gets stapled to the work order -- and with the plan alongside it, the page
// can say "ArcGIS Pro" where the machine could only say which file it ran.
func migrateResult(args []string) error {
	fs := flag.NewFlagSet("migrate result", flag.ContinueOnError)
	plan := fs.String("plan", "", "the approved manifest, so programs are named the way people name them")
	out := fs.String("out", "", "write the page here (default: beside the record)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("migrate result <dsky-migrate-result.json> [--plan manifest.json] [--out page.html]")
	}
	rec, err := migrate.LoadRecord(fs.Arg(0))
	if err != nil {
		return err
	}
	var m *migrate.Manifest
	if *plan != "" {
		if m, err = migrate.Load(*plan); err != nil {
			return err
		}
	}
	path := *out
	if path == "" {
		path = strings.TrimSuffix(fs.Arg(0), ".json") + ".html"
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := migrate.RecordReport(f, rec, m, buildinfo.Version); err != nil {
		return err
	}

	// The counts, so somebody running this over a bench of machines does not
	// have to open every page to find the one with a problem.
	var failed, waiting int
	for _, o := range rec.Outcomes {
		switch o.State {
		case migrate.StateFailed:
			failed++
		case migrate.StateAtSignIn:
			waiting++
		}
	}
	fmt.Printf("%s — %d done, %d waiting for the person who will use it, %d need a hand\n",
		rec.Machine, len(rec.Outcomes)-failed-waiting, waiting, failed)
	fmt.Println("wrote", path)
	if failed > 0 {
		// A non-zero exit so a script over a bench can find these without
		// reading anything.
		return errQuiet{n: failed}
	}
	return nil
}

// errQuiet carries a count out as an exit code without printing twice.
type errQuiet struct{ n int }

func (e errQuiet) Error() string {
	return fmt.Sprintf("%d thing(s) on this machine need a hand", e.n)
}

// migrateVerify reads the new machine back and asks whether it is the machine
// the plan described.
//
// Everything else in this feature reports on its own work, and all of it can
// be right while the machine is still wrong: a program installs and does not
// run, a setting is applied and then overwritten by the program installed
// after it, a queue is added to an account nobody uses. This is the one
// reading taken from the finished machine by something that had no part in
// building it.
func migrateVerify(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("migrate verify", flag.ContinueOnError)
	plan := fs.String("plan", "", "the approved manifest this machine was built from (required)")
	scan := fs.String("scan", "", "a scan of the new machine; omit to scan the machine this runs on")
	site := fs.String("mappings", "", "the site's mapping table, to offer aliases for programs found under another name")
	out := fs.String("out", "", "also write the comparison as a page here")
	yes := fs.Bool("yes", false, "accept every offered alias without asking")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *plan == "" || fs.NArg() != 0 {
		return fmt.Errorf("migrate verify --plan manifest.json [--scan newscan.json] [--mappings site.json] [--out page.html]")
	}
	approved, err := migrate.Load(*plan)
	if err != nil {
		return err
	}
	// A plan nobody approved describes nothing, so there is nothing to
	// compare against. Said here rather than after a scan that takes a minute.
	if err := approved.CheckApproved(); err != nil {
		return fmt.Errorf("this plan cannot be verified against: %w", err)
	}

	after, err := scanOrLoad(ctx, *scan)
	if err != nil {
		return err
	}
	v := migrate.Verify(approved, after)

	fmt.Printf("%s — %s\n", after.Source.Hostname, v.Summary())
	for _, m := range v.Missing {
		if m.Nearest != nil {
			fmt.Printf("  missing: %s — but %s is here (%.0f%% alike)\n",
				m.Wanted.DisplayName, m.Nearest.DisplayName, m.Confidence*100)
			continue
		}
		fmt.Printf("  missing: %s\n", m.Wanted.DisplayName)
	}
	for _, d := range v.Settings {
		fmt.Printf("  %s: the plan asked for %s, this machine has %s\n", d.Key, d.Wanted, d.Found)
	}
	for _, d := range v.Waiting {
		fmt.Printf("  %s: waiting — it belongs to a person and is applied at their first sign-in\n", d.Key)
	}
	for _, line := range v.Identity {
		fmt.Println("  " + line)
	}

	if *site != "" {
		if err := offerAliases(v, *site, *yes); err != nil {
			return err
		}
	}
	if *out != "" {
		f, err := os.Create(*out)
		if err != nil {
			return err
		}
		defer f.Close()
		if err := migrate.VerifyReport(f, v, buildinfo.Version); err != nil {
			return err
		}
		fmt.Println("wrote", *out)
	}
	if !v.OK() {
		return errQuiet{n: len(v.Missing) + len(v.Settings) + len(v.Identity)}
	}
	return nil
}

// scanOrLoad reads the machine, or a scan of it somebody already took.
func scanOrLoad(ctx context.Context, path string) (*migrate.Manifest, error) {
	if path != "" {
		return migrate.Load(path)
	}
	dir, err := os.MkdirTemp("", "dsky-verify-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	res, err := migrate.ScanToFiles(ctx, dir, migrate.ScanOptions{}, "", buildinfo.Version, os.Stderr)
	if err != nil {
		return nil, err
	}
	return res.Manifest, nil
}

// offerAliases writes the near misses into the site's table, so that the next
// machine resolves them without anybody noticing.
//
// This is the quiet payoff of the whole mapping table: a program installed
// under a name nobody expected looks exactly like a program that failed to
// install, and telling the two apart by hand is what nobody has time for on
// the twentieth machine.
func offerAliases(v *migrate.Verification, path string, all bool) error {
	near := v.Aliases()
	if len(near) == 0 {
		return nil
	}
	t, err := migrate.LoadTable(path)
	if err != nil {
		return err
	}
	in := bufio.NewReader(os.Stdin)
	added := 0
	for _, m := range near {
		if !all {
			fmt.Printf("\n%s was not found, but %s is here (%.0f%% alike).\n",
				m.Wanted.DisplayName, m.Nearest.DisplayName, m.Confidence*100)
			fmt.Printf("Remember %q as the same program, so the next machine resolves it? [y/N] ",
				m.Nearest.DisplayName)
			line, _ := in.ReadString('\n')
			if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "y") {
				continue
			}
		}
		// Matched on the name the program has NOW, resolved the way the plan
		// resolved it. That is the direction that helps: the next machine to
		// be scanned will show the new name, and what it needs is the known
		// way to install it. The other way round -- the old name with
		// whatever a fresh scan happened to say -- writes an entry that
		// matches something already resolved and carries a resolution nobody
		// decided.
		t.Remember(*m.Nearest, m.Wanted.Resolution, m.Wanted.ConfigCapture, "verify")
		added++
	}
	if added == 0 {
		return nil
	}
	if err := t.Save(path); err != nil {
		return err
	}
	fmt.Printf("remembered %d program(s) in %s\n", added, path)
	return nil
}

// migrateUSMT moves somebody's files: capture on the old machine before it is
// retired, restore on the new one once it is built.
//
// Both are run by a person rather than by the first-boot agent, and the reason
// is the key. An encrypted store needs one, and the rule this feature does not
// bend is that a secret never rides on the USB — so the key is typed on the
// machine in front of somebody and never goes near the media. It is read the
// same way the administrator password is, and for the same reason: a password
// on a command line is in the shell history and in the process list.
func migrateUSMT(ctx context.Context, args []string, capturing bool) error {
	what := "restore"
	if capturing {
		what = "capture"
	}
	fs := flag.NewFlagSet("migrate "+what, flag.ContinueOnError)
	usmt := fs.String("usmt", "", "the folder holding scanstate.exe and loadstate.exe, from the Windows ADK")
	keyStdin := fs.Bool("key-stdin", false, "read the store's encryption key from stdin")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("migrate %s <manifest.json> --usmt <adk folder> [--key-stdin]", what)
	}
	m, err := migrate.Load(fs.Arg(0))
	if err != nil {
		return err
	}
	// The plan says whose files move and where they go, and both were
	// approved by somebody who knew whose machine it was. An unapproved plan
	// has not had that conversation.
	if err := m.CheckApproved(); err != nil {
		return fmt.Errorf("this plan cannot move anybody's files: %w", err)
	}
	u, err := migrate.NewUSMT(*usmt)
	if err != nil {
		return err
	}
	key, err := usmtKey(m, *keyStdin)
	if err != nil {
		return err
	}
	if capturing {
		return u.Capture(ctx, m, key, os.Stdout)
	}
	return u.Restore(ctx, m, key, os.Stdout)
}

// usmtKey reads the store's encryption key, when there is one to read.
func usmtKey(m *migrate.Manifest, fromStdin bool) (string, error) {
	if !m.Data.Encrypted {
		return "", nil
	}
	if fromStdin {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", fmt.Errorf("reading the store key from stdin: %w", err)
		}
		if k := strings.Trim(string(b), "\r\n"); k != "" {
			return k, nil
		}
		return "", fmt.Errorf("--key-stdin was given but stdin was empty")
	}
	if k := os.Getenv("DSKY_USMT_KEY"); k != "" {
		return k, nil
	}
	return "", fmt.Errorf(`this plan's store is encrypted, so it needs its key.

Give it one of these two ways, so that it stays out of your shell history:

  printf %%s 'the-key' | dsky migrate %s <manifest.json> --usmt <folder> --key-stdin
  DSKY_USMT_KEY=... dsky migrate ... --usmt <folder>

The key is not in the plan and never rides on the stick: it unlocks every
document the old machine had`, "capture|restore")
}

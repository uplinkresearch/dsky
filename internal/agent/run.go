package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Agent is one run on one machine.
type Agent struct {
	// Dir is where the agent, the manifest and the staged files live on the
	// imaged machine (C:\Windows\Setup\Scripts).
	Dir      string
	Manifest *Manifest
	J        *Journal
	State    *State
	UI       *screen
	Opts     RunOptions

	// ownShortcuts are the desktop shortcuts that were there before a
	// standalone payload started: the owner's, and never touched.
	ownShortcuts map[string]bool

	// rebootWanted is set by a step that Windows told to restart the
	// machine, with the reason in the operator's words. It is acted on
	// between steps, never in the middle of one.
	rebootWanted string
}

// Apply does everything the manifest asks for, in the recipe's order, and
// records what happened. It returns an error only when it could not start:
// a step that fails is logged and the rest still run, because a machine with
// drivers and no Spotify is worth more than a machine with neither.
// RunOptions are how the agent was started, as opposed to what the manifest
// asks it to do.
type RunOptions struct {
	// Quiet runs without a window: from a script, or from a remote tool with
	// nobody at the machine to read one.
	Quiet bool
	// Unattended says nobody is at the machine, so a standalone payload may
	// restart it by itself when Windows asks. A first boot always may: nobody
	// is using a machine that is still being set up.
	Unattended bool
	// From is the file the payload is inside, when that is not this program.
	// It is not passed on to a later run: by then the payload is unpacked on
	// the machine and the file it came in may be gone.
	From string
}

// Args are the options as they appear on the command line, for starting the
// agent again with the same ones -- after a restart, or from its own copy.
func (o RunOptions) Args() []string {
	var a []string
	if o.Quiet {
		a = append(a, "--quiet")
	}
	if o.Unattended {
		a = append(a, "--unattended")
	}
	return a
}

// Apply does everything the manifest asks for, in the recipe's order, and
// records what happened. It returns how many problems there were -- the
// agent's exit code, so a remote tool can tell success from failure -- and an
// error only when it could not start: a step that fails is logged and the
// rest still run, because a machine with drivers and no Spotify is worth more
// than a machine with neither.
func Apply(dir string, opts RunOptions) (int, error) {
	m, err := LoadManifest(filepath.Join(dir, ManifestName))
	if err != nil {
		return 0, err
	}

	// The window next, before anything slower: at first sign-in the desktop
	// is already up by the time Windows runs the agent, and every moment
	// after that is somebody looking at a machine that appears to be finished.
	var ui *screen
	if !opts.Quiet {
		ui = openScreenFn(machineName(machineModel()), !m.Standalone())
	}
	defer ui.Close()

	j, err := OpenJournal(dir)
	if err != nil {
		// Windows says "Access is denied." and leaves the operator to work
		// out whose. C:\Windows\Setup\Scripts is writable only by an
		// administrator, and the agent needs to write there before it can
		// say anything at all -- including this.
		if os.IsPermission(err) {
			return 0, fmt.Errorf("%s cannot be written to: run this from a PowerShell or Command Prompt "+
				"started with Run as administrator", dir)
		}
		return 0, err
	}
	defer j.Close()

	a := &Agent{Dir: dir, Manifest: m, J: j, State: LoadState(dir), Opts: opts}
	start := time.Now()
	machine := machineName(machineModel())
	if m.Standalone() {
		a.J.Info("", "agent starting payload %s (recipe %s) on %s, which is already in use", m.Build, m.Recipe, machine)
	} else {
		a.J.Info("", "first-boot agent starting for recipe %s on %s (%s)", m.Recipe, machine, runtime.GOOS)
	}

	// What the person at the machine sees. A nil screen -- no window, or not
	// Windows -- costs nothing: every call on it does nothing.
	a.UI = ui
	if a.UI == nil {
		a.J.Info("", "no status window; the log is the only record")
	}

	// On a machine somebody already uses, the shortcuts on its desktops are
	// theirs. Only the ones that appear while the payload runs are ours to
	// take away. Taken once, on the first run of this payload, so a run that
	// resumes after a restart does not adopt the installers' icons as the
	// owner's.
	if m.Standalone() {
		a.ownShortcuts = a.State.OwnShortcuts(currentShortcuts)
	}

	// The display stays on and the machine stays awake while there is work
	// to do. It is let go before the finish screen: a finished machine left
	// on a bench for a week should sleep like any other.
	release := keepAwakeFn()
	defer release()

	// Before anything else, arrange to be started again. Everything below
	// can be interrupted -- a restart Windows asks for, a machine that
	// crashes under a driver, somebody closing the lid -- and the state
	// file that lets this run carry on is worth nothing if nothing ever
	// runs the agent a second time.
	if err := ensureResumeFn(a); err != nil {
		a.J.FailDetail("", "this machine will not carry on by itself if it restarts before the end", err.Error())
	}

	for _, step := range m.Steps {
		if a.State.Finished(step) {
			a.J.Info(step, "already done on an earlier boot, skipping")
			// Still on the checklist. Watched in the VM after a crash, the
			// window came back listing only the step it resumed, and the
			// drivers and removal done before the restart had vanished --
			// which reads as never having happened.
			a.UI.Doing(step, "")
			a.UI.Finished(step, 0)
			continue
		}
		before := len(a.J.Failures())
		a.UI.Doing(step, "")
		switch step {
		case stepDomain:
			a.domainStep()
		case stepDrivers:
			a.driversStep()
		case stepDebloat:
			a.debloatStep()
		case stepApps:
			a.appsStep()
		case stepSettings:
			a.settingsStep()
		case stepPrinters:
			a.printersStep()
		default:
			a.J.Info(step, "no such step in this agent, skipping")
			continue
		}
		a.UI.Finished(step, len(a.J.Failures())-before)
		a.State.Finish(step)
		// A restart is taken between steps, with the finished step
		// recorded, so the machine comes back and carries on at the next
		// one rather than repeating this one.
		if a.rebootWanted != "" && a.restartAndResume() {
			return len(a.J.Failures()), nil
		}
	}

	if failures := a.J.Failures(); len(failures) > 0 {
		a.J.Info("", "finished in %s with %d problem(s): %s",
			time.Since(start).Round(time.Second), len(failures), strings.Join(failures, "; "))
	} else {
		a.J.Info("", "finished in %s with nothing to report", time.Since(start).Round(time.Second))
	}

	// The post-install check paints the result on the lock screen, so a bench
	// of machines can be read from the doorway. It runs last, because it
	// reports on everything above.
	if m.VerifyScript != "" {
		a.UI.Doing("checking", "reading back what this machine actually has")
		script := filepath.Join(dir, m.VerifyScript)
		if _, err := os.Stat(script); err == nil {
			r := run(15*time.Minute, "powershell", "-NoProfile", "-NonInteractive",
				"-ExecutionPolicy", "Bypass", "-File", script)
			a.J.Raw(r.Out)
			a.J.Info("", "post-install check exited %d", r.Code)
		} else {
			a.J.Fail("", "the post-install check %s is not on the machine", m.VerifyScript)
		}
		a.UI.Finished("checking", 0)
	}

	// Last of all, once nothing else can put one there: no desktop
	// shortcuts. An install that ran in the signed-in user's session can
	// leave an icon behind after its step was recorded as done.
	a.tidyDesktop()

	// Finished for good: stop asking to be started again, and hand the
	// machine over without the automatic sign-in provisioning needed.
	a.finishUp()
	a.J.Info("", "agent done")

	release()

	// The last thing on the screen is what this machine got, and it stays
	// there until somebody says they have seen it. Nothing is waiting on
	// this: the work is over.
	a.UI.Summary(summaryHeading(a.J.Failures()), summaryLines(a.J.Failures(), buildTook(a.State, start)))

	// An installer that puts its icon on the desktop after it has exited --
	// Spotify does -- would otherwise leave one behind on a machine that has
	// been swept twice. Nothing is waiting on this: the work is done.
	done := make(chan struct{})
	go a.keepDesktopClear(done)
	a.UI.WaitDismiss()
	close(done)
	a.tidyDesktop()
	return len(a.J.Failures()), nil
}

// buildTook is how long the machine has been being set up, across restarts.
// This run's own clock starts at the last boot: watched in the VM, a machine
// crashed part way through and finished fifty minutes after it began, and the
// finish screen said "Set up in 10 minutes".
func buildTook(st *State, thisRun time.Time) time.Duration {
	if began, err := time.Parse(time.RFC3339, st.Started); err == nil && began.Before(thisRun) {
		return time.Since(began)
	}
	return time.Since(thisRun)
}

// summaryHeading is the first thing read from across a room.
func summaryHeading(failures []string) string {
	if len(failures) == 0 {
		return "This machine is ready"
	}
	return "Finished, with " + itoa(len(failures)) + " problem(s)"
}

// summaryLines say what went wrong, plainly, and never more than fits.
func summaryLines(failures []string, took time.Duration) []string {
	if len(failures) == 0 {
		return []string{"Everything the build asked for is installed.",
			"Set up in " + shortDur(took) + "."}
	}
	const most = 8
	out := make([]string, 0, most+2)
	for i, f := range failures {
		if i == most {
			out = append(out, "and "+itoa(len(failures)-most)+" more, in firstboot.log")
			break
		}
		out = append(out, f)
	}
	return append(out, "Everything else is installed. The full record is in firstboot.log.")
}

// machineName is what the machine calls itself, said once.
//
// Vendors are inconsistent about whether the model already carries the maker's
// name. HP's firmware reports the manufacturer as "HP" and the model as "HP
// EliteBook x360 1040 G8 Notebook PC", so joining them put "HP HP EliteBook
// x360 1040 G8 Notebook PC" on the screen in front of whoever is setting the
// machine up. Dell reports "Dell Inc." and "OptiPlex 3070", which needs both.
func machineName(vendor, model string) string {
	vendor, model = strings.TrimSpace(vendor), strings.TrimSpace(model)
	switch {
	case model == "":
		if vendor == "" {
			return "unknown machine"
		}
		return vendor
	case vendor == "":
		return model
	}
	// "HP" against "HP EliteBook ...": the model already says who made it.
	first, _, _ := strings.Cut(model, " ")
	if strings.EqualFold(first, vendor) || strings.EqualFold(first, strings.TrimSuffix(vendor, ".")) {
		return model
	}
	return vendor + " " + model
}

// Main is the agent's entry point, kept here so the command is three lines
// and the behaviour is testable.
func Main(args []string) error {
	// No arguments at all is somebody double-clicking a payload that is one
	// file, which is the way it is meant to be started; options with no
	// command is a script running that same file, where "apply" is the only
	// thing it could have meant. Anywhere else it is somebody who does not
	// know what this program is, and the usage line is the answer.
	if len(args) == 0 || strings.HasPrefix(args[0], "--") {
		carried, err := carriedPayload(fromOption(args))
		if err != nil {
			return err
		}
		if carried != nil {
			defer carried.Close()
		}
		if carried == nil {
			if len(args) > 0 {
				return fmt.Errorf("unknown command %q (this agent carries no payload; try: dsky-agent apply %s)", args[0], strings.Join(args, " "))
			}
			return fmt.Errorf("usage: dsky-agent apply [dir] [--quiet] [--unattended] | dsky-agent scan [dir] [--all-users] | dsky-agent verify [dir] | dsky-agent user-install <job> <result>")
		}
		args = append([]string{"apply"}, args...)
	}
	switch args[0] {
	case "apply":
		dir, opts, err := parseApplyArgs(args[1:])
		if err != nil {
			return err
		}
		// A payload carried inside a file is what to run, unless a directory
		// was named -- which is how the copy on the machine is started, and
		// how a resume after a restart carries on. The file is usually this
		// one; with --from it is the payload this agent was unpacked out of,
		// so that the elevation prompt could name the agent instead.
		if dir == "" {
			carried, err := carriedPayload(opts.From)
			if err != nil {
				return err
			}
			if carried != nil {
				defer carried.Close()
				opts.From = ""
				problems, err := startAttached(carried, opts)
				if err != nil {
					return err
				}
				exitWith(problems)
				return nil
			}
			if opts.From != "" {
				return fmt.Errorf("%s carries no payload", opts.From)
			}
		}
		if dir == "" {
			exe, err := os.Executable()
			if err != nil {
				return err
			}
			dir = filepath.Dir(exe)
		}
		m, err := LoadManifest(filepath.Join(dir, ManifestName))
		if err != nil {
			return err
		}
		if m.Standalone() {
			ranElsewhere, problems, err := startStandalone(dir, m, opts)
			if err != nil {
				return err
			}
			if ranElsewhere {
				exitWith(problems)
				return nil
			}
		}
		problems, err := Apply(dir, opts)
		if err != nil {
			return err
		}
		exitWith(problems)
		return nil
	case "verify":
		dir := ""
		if len(args) > 1 {
			dir = args[1]
		}
		if dir == "" {
			exe, err := os.Executable()
			if err != nil {
				return err
			}
			dir = filepath.Dir(exe)
		}
		checks, err := Verify(dir)
		if err != nil {
			return err
		}
		fmt.Printf("DSKY post-install check\n\n")
		if Report(checks, os.Stdout) {
			fmt.Println("\nThis machine matches the build.")
			return nil
		}
		fmt.Println("\nThis machine does not match the build; see the lines marked FAIL.")
		os.Exit(1)
		return nil
	case "scan":
		// Reading the PC that is being replaced. This is the agent rather
		// than dsky.exe because this is the binary that rides on a stick:
		// one file, no installation, run from D:\ on somebody's desk while
		// they watch.
		return scanThisMachine(args[1:])
	case "user-install":
		// The unelevated half of an install that refuses an administrator.
		if len(args) != 3 {
			return fmt.Errorf("usage: dsky-agent user-install <job.json> <result.json>")
		}
		return RunUserJob(args[1], args[2])
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

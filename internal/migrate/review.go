package migrate

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// The review is the one step that cannot be automated away, and the spec says
// so plainly: the tool proposes, a person approves. What it can do is make
// that person's part short. By the time they sit down, the scan has found
// everything and the resolver has placed what it could; the review is the
// blockers nobody has answered, the applications nobody could place, and the
// two or three facts about the new machine that a scan cannot know.
//
// Three rules shape it:
//
//   - Ask in the order that can stop the build. Blockers first: an
//     unanswered one makes everything after it pointless.
//   - Every answer is offered back to the site's table, defaulting to yes.
//     The expensive part of a migration is working out where a practice's
//     software comes from, and nobody should pay it twice.
//   - Nothing is decided by silence. Enter on a question with a default
//     takes the default and says so; a question with no sensible default
//     keeps asking.

// Review runs the questions over a manifest.
type Review struct {
	In  io.Reader
	Out io.Writer
	// Site is the table answers are remembered in, and SitePath is where it
	// is written. A review with no table still works; it just teaches nothing
	// to the next machine.
	Site     *Table
	SitePath string
	// By is who is approving, recorded in the manifest.
	By string
	// Save is called when the site's table gains an entry, so the caller
	// decides where it goes (and tests can keep it in memory).
	Save func(*Table) error

	in       *bufio.Reader
	remember bool // the answer to "save to the site's table?" last time
}

// Run asks everything that needs asking and, if the operator agrees and
// nothing is outstanding, approves the manifest. It returns whether the
// manifest was changed, so the caller knows to write it.
func (r *Review) Run(m *Manifest) (changed bool, err error) {
	r.in = bufio.NewReader(r.In)
	r.remember = true

	r.say("%s — %s, %s", m.Source.Hostname, machineName(m.Source.Hardware), m.Source.OS.ProductName)
	c := m.Count("win11")
	r.say("%d applications: %d will install, %d with nowhere to install from, %d left behind, %d by hand.",
		c.Apps, len(m.InstallOrder()), c.Unmapped, c.Dropped, c.Manual)
	r.say("")

	for _, step := range []func(*Manifest) (bool, error){
		r.blockers, r.unplaced, r.userFiles, r.identity, r.notes,
	} {
		did, err := step(m)
		if err != nil {
			return changed, err
		}
		changed = changed || did
	}
	return r.approve(m, changed)
}

// ── the questions ────────────────────────────────────────────────────────────

// blockers must each be answered: accepted with the risk understood, or the
// application dropped. Nothing else can approve a plan.
func (r *Review) blockers(m *Manifest) (bool, error) {
	var changed bool
	for i := range m.Compat {
		c := &m.Compat[i]
		if c.Severity != Blocker || c.Acknowledged {
			continue
		}
		r.say("BLOCKER  %s", m.subjectName(c.Subject))
		r.say("  %s", c.Reason)
		if c.SuggestedAction != "" {
			r.say("  suggested: %s", c.SuggestedAction)
		}
		app := m.appByID(c.Subject)
		options := "[a] accept the risk and carry on  [s] leave it for now"
		if app != nil {
			options = "[a] accept the risk and install it anyway  [d] drop " + app.DisplayName + "  [s] leave it for now"
		}
		switch r.ask(options, "a", "d", "s") {
		case "a":
			c.Acknowledged = true
			changed = true
			// "Accept the risk" has to mean something the plan says out loud.
			// For the missing migration tool it means the files stay behind,
			// and a plan that still claimed it would copy them would be a
			// promise nothing could keep -- noticed, if at all, as an empty
			// Documents folder on somebody's new machine.
			if c.Subject == "USMT" && m.Data.Strategy == DataUSMT {
				m.Data = Data{Strategy: DataNoneStrat}
				m.Notes = append(m.Notes,
					"The user's files will NOT be copied: USMT was not supplied and the blocker was accepted in review.")
				r.say("  accepted — and the files will not be copied, which the report now says.")
				break
			}
			r.say("  accepted.")
		case "d":
			if app != nil {
				app.Resolution = Resolution{Status: StatusDropped, ResolvedBy: ByOperator,
					Note: "dropped in review: " + c.Reason}
				c.Acknowledged = true
				changed = true
				r.rememberDecision(*app)
				r.say("  %s will not be installed.", app.DisplayName)
			}
		default:
			r.say("  left unanswered; this plan cannot be approved until it is.")
		}
		r.say("")
	}
	return changed, nil
}

// unplaced is the heart of it: one application at a time, with whatever the
// resolver suggested already typed in.
func (r *Review) unplaced(m *Manifest) (bool, error) {
	var changed bool
	for i := range m.Apps {
		a := &m.Apps[i]
		if a.Resolution.Status != StatusUnmapped && a.Resolution.Status != StatusUnset {
			continue
		}
		r.say("%s%s", a.DisplayName, versionSuffix(a.DisplayVersion))
		if a.Publisher != "" {
			r.say("  from %s", a.Publisher)
		}
		if a.InstallLocation != "" {
			r.say("  installed in %s", a.InstallLocation)
		}
		options := []string{"[w] a winget package id", "[f] an installer on a share or disk",
			"[m] somebody installs it by hand", "[d] leave it behind", "[s] decide later"}
		keys := []string{"w", "f", "m", "d", "s"}
		if a.Resolution.Ref != "" {
			options = append([]string{fmt.Sprintf("[y] yes, %s (%.0f%% sure)", a.Resolution.Ref, a.Resolution.Confidence*100)}, options...)
			keys = append([]string{"y"}, keys...)
		}
		switch r.ask(strings.Join(options, "  "), keys...) {
		case "y":
			a.Resolution.Status = StatusResolved
			a.Resolution.ResolvedBy = ByOperator
			if a.Resolution.Method == "" {
				a.Resolution.Method = MethodWinget
			}
			a.Resolution.Order = prerequisiteOrder(a.DisplayName)
			changed = true
		case "w":
			id := r.line("winget id (for example 7zip.7zip)")
			if id == "" {
				break
			}
			a.Resolution = Resolution{Status: StatusResolved, Method: MethodWinget, Ref: id,
				ResolvedBy: ByOperator, Confidence: 1, Order: prerequisiteOrder(a.DisplayName)}
			changed = true
		case "f":
			path := r.line(`path to the installer (\\server\share\setup.exe)`)
			if path == "" {
				break
			}
			method := MethodEXE
			if strings.EqualFold(pathExt(path), ".msi") {
				method = MethodMSI
			}
			args := r.line("silent switches, if any (/qn, /S /v/qn)")
			a.Resolution = Resolution{Status: StatusResolved, Method: method, Ref: path,
				Args: strings.Fields(args), ResolvedBy: ByOperator, Confidence: 1,
				Order: prerequisiteOrder(a.DisplayName)}
			changed = true
		case "m":
			note := r.line("what the person has to do")
			a.Resolution = Resolution{Status: StatusResolved, Method: MethodManual, Ref: note,
				ResolvedBy: ByOperator, Confidence: 1, Order: OrderMax}
			changed = true
		case "d":
			why := r.line("why, for the report (enter to skip)")
			a.Resolution = Resolution{Status: StatusDropped, ResolvedBy: ByOperator, Note: why}
			changed = true
		default:
			a.Resolution.Status = StatusUnmapped
			r.say("  left for later.")
			r.say("")
			continue
		}
		r.rememberDecision(*a)
		r.say("")
	}
	return changed, nil
}

// userFiles settles what a scan can only propose: where a USMT store lives
// and whose files go in it.
func (r *Review) userFiles(m *Manifest) (bool, error) {
	if m.Data.Strategy != DataUSMT {
		return false, nil
	}
	var changed bool
	if len(m.Data.Users) > 0 {
		r.say("Files to carry over for %s.", strings.Join(m.Data.Users, ", "))
	}
	if m.Data.StorePath == "" {
		r.say("They are on this machine's disk, so they need somewhere to be staged.")
		if path := r.line(`the share to stage them through (\\server\migration$\PC01), or enter to decide later`); path != "" {
			m.Data.StorePath = path
			changed = true
		}
	}
	if m.Data.StorePath != "" && !m.Data.Encrypted {
		if r.yes("Encrypt the store (recommended: it holds somebody's documents)?", true) {
			m.Data.Encrypted = true
			changed = true
		}
	}
	r.say("")
	return changed, nil
}

// identity shows where the new machine lands in the domain, and lets it be
// moved: a replacement often goes into a different OU from the machine it
// replaces, and that is a decision, not a reading.
func (r *Review) identity(m *Manifest) (bool, error) {
	if !m.Identity.Domained() {
		return false, nil
	}
	r.say("Joins %s as %s", m.Identity.DomainFQDN, m.Target.Hostname)
	if m.Identity.ComputerOUDN != "" {
		r.say("  into %s", m.Identity.ComputerOUDN)
	}
	if !r.yes("Change where it joins or what it is called?", false) {
		r.say("")
		return false, nil
	}
	var changed bool
	if name := r.line("name for the new machine (enter to keep " + m.Target.Hostname + ")"); name != "" {
		m.Target.Hostname = name
		changed = true
	}
	if ou := r.line("OU it should join (enter to keep the one above)"); ou != "" {
		m.Identity.ComputerOUDN = ou
		changed = true
	}
	r.say("")
	return changed, nil
}

func (r *Review) notes(m *Manifest) (bool, error) {
	note := r.line("Anything to note on the report for whoever reads it? (enter to skip)")
	if note == "" {
		return false, nil
	}
	m.Notes = append(m.Notes, note)
	r.say("")
	return true, nil
}

// approve is the last question, and it is only asked when there is nothing
// left outstanding.
func (r *Review) approve(m *Manifest, changed bool) (bool, error) {
	if why := m.ReadyToApprove(); why != nil {
		r.say("Not ready to approve: %v", why)
		r.say("Run the review again when that is settled.")
		return changed, nil
	}
	c := m.Count("win11")
	r.say("Ready: %d to install, %d by hand, %d left behind.", len(m.InstallOrder()), c.Manual, c.Dropped)
	if !r.yes(fmt.Sprintf("Approve this plan as %s?", r.who()), true) {
		return changed, nil
	}
	if err := m.Approve(r.who()); err != nil {
		return changed, err
	}
	r.say("Approved. Build it with: dsky migrate build <manifest>")
	return true, nil
}

// ── asking ───────────────────────────────────────────────────────────────────

func (r *Review) say(format string, args ...any) {
	fmt.Fprintf(r.Out, format+"\n", args...)
}

// ask offers one-letter choices and keeps asking until it gets one of them.
// The first is the default, because a person going quickly through a list of
// applications should be able to hold down Enter and get the safe answer.
func (r *Review) ask(prompt string, keys ...string) string {
	for {
		fmt.Fprintf(r.Out, "  %s: ", prompt)
		line, err := r.in.ReadString('\n')
		got := strings.ToLower(strings.TrimSpace(line))
		if got == "" && len(keys) > 0 {
			return keys[len(keys)-1] // enter means "decide later", never a guess
		}
		for _, k := range keys {
			if got == k {
				return k
			}
		}
		if err != nil {
			return keys[len(keys)-1]
		}
		r.say("  %q is not one of those.", got)
	}
}

// line asks for a value and returns it trimmed.
func (r *Review) line(prompt string) string {
	fmt.Fprintf(r.Out, "  %s: ", prompt)
	line, _ := r.in.ReadString('\n')
	return strings.TrimSpace(line)
}

// yes asks a yes/no question with a default.
func (r *Review) yes(prompt string, def bool) bool {
	suffix := " [y/N]"
	if def {
		suffix = " [Y/n]"
	}
	for {
		fmt.Fprintf(r.Out, "  %s%s: ", prompt, suffix)
		line, err := r.in.ReadString('\n')
		got := strings.ToLower(strings.TrimSpace(line))
		switch got {
		case "":
			return def
		case "y", "yes":
			return true
		case "n", "no":
			return false
		}
		if err != nil {
			return def
		}
		// Silently asking the same question again reads like the program is
		// stuck. Say what was not understood.
		r.say("  %q is not yes or no.", got)
	}
}

func (r *Review) who() string {
	if r.By == "" {
		return "operator"
	}
	return r.By
}

// rememberDecision offers the answer to the site's table. The default is yes:
// the expensive part of a migration is working out where a practice's own
// software comes from, and the next machine at that practice should not pay
// for it again.
func (r *Review) rememberDecision(a App) {
	if r.Site == nil || r.Save == nil {
		return
	}
	switch a.Resolution.Status {
	case StatusResolved, StatusDropped:
	default:
		return
	}
	if !r.yes("Remember this for the rest of this site?", r.remember) {
		r.remember = false
		return
	}
	r.remember = true
	r.Site.Remember(a, a.Resolution, a.ConfigCapture, r.who())
	if err := r.Save(r.Site); err != nil {
		r.say("  (could not write the site's table: %v)", err)
	}
}

// ── small helpers ────────────────────────────────────────────────────────────

func (m *Manifest) appByID(id string) *App {
	for i := range m.Apps {
		if m.Apps[i].ID == id {
			return &m.Apps[i]
		}
	}
	return nil
}

func versionSuffix(v string) string {
	if v == "" {
		return ""
	}
	return " " + v
}

func pathExt(p string) string {
	if i := strings.LastIndex(p, "."); i >= 0 {
		return p[i:]
	}
	return ""
}

// AutoApprove is the path a repeat machine at a known site takes: no
// questions, and only when there is nothing to ask about. It refuses exactly
// what the review would refuse.
func AutoApprove(m *Manifest, by string) error {
	if why := m.ReadyToApprove(); why != nil {
		return fmt.Errorf("cannot approve without asking: %v", why)
	}
	return m.Approve(by)
}

// Plural is "1 application" / "3 applications", for the review's summaries.
func Plural(n int, one, many string) string {
	if n == 1 {
		return strconv.Itoa(n) + " " + one
	}
	return strconv.Itoa(n) + " " + many
}

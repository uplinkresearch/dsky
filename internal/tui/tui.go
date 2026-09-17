// Package tui is the full-screen terminal wizard: pick an OS, set a few
// options, pick the stick, confirm, watch it build and flash. Same Quick
// Install pipeline the CLI and the web portal drive — this is the third face
// on it, for people who live in a terminal and would rather not remember
// flags.
package tui

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/uplinkresearch/dsky/internal/appcatalog"
	"github.com/uplinkresearch/dsky/internal/compose"
	"github.com/uplinkresearch/dsky/internal/device"
	"github.com/uplinkresearch/dsky/internal/driverresolve"
	"github.com/uplinkresearch/dsky/internal/drivers/catalog"
	"github.com/uplinkresearch/dsky/internal/flashrun"
	"github.com/uplinkresearch/dsky/internal/hwdetect"
	"github.com/uplinkresearch/dsky/internal/library"
	"github.com/uplinkresearch/dsky/internal/oscatalog"
	"github.com/uplinkresearch/dsky/internal/recipe"
)

// Adaptive so the wizard stays legible on light and dark terminals alike.
var (
	cTitle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "#1a4fa0", Dark: "#7aa2f7"})
	cDim   = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#6a6a6a", Dark: "#8a8a8a"})
	cSel   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "#0b6b3a", Dark: "#9ece6a"})
	cWarn  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "#a33", Dark: "#f7768e"})
	cOK    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "#0b6b3a", Dark: "#9ece6a"})
)

type stage int

const (
	stageOS stage = iota
	stageISO
	stageOptions
	stageApps
	stageDevice
	stageConfirm
	stageRunning
	stageDone
)

// choice is one multiple-value option row.
type choice struct {
	label  string
	values []string
	labels []string // display text per value
	idx    int
}

func (c *choice) value() string { return c.values[c.idx] }
func (c *choice) text() string  { return c.labels[c.idx] }
func (c *choice) next(d int) {
	c.idx = (c.idx + d + len(c.values)) % len(c.values)
}

type progressMsg struct {
	stage string
	done  int64
	total int64
}
type detectedMsg struct {
	hw  *hwdetect.Hardware
	err error
}
type devicesMsg struct {
	devs []device.Device
	err  error
}
type doneMsg struct {
	art *compose.Artifact
	err error
}

type model struct {
	ctx context.Context
	lib *library.Library

	stage  stage
	cursor int
	err    error

	entries []oscatalog.Entry
	osIdx   int

	// needISO is set when the chosen OS is not in the library yet and its
	// image has to be fetched — the point at which offering a downloaded one
	// is useful rather than clutter.
	needISO bool
	// mustISO is set for an import-only entry: the path is not optional,
	// because there is no download to fall back on.
	mustISO bool
	isoPath string

	opts    []*choice
	drivers bool
	hw      *hwdetect.Hardware
	hwErr   error

	apps      []appcatalog.App
	appTarget appcatalog.Target
	appPick   map[string]bool
	appIdx    int
	appOrder  []string

	devs   []device.Device
	devIdx int
	// buildOnly is chosen when no stick is attached (or the operator asks):
	// the image is composed and left in the library.
	buildOnly bool

	confirm string

	ch       chan tea.Msg
	curStage string
	pct      int
	art      *compose.Artifact
	finished bool
}

// Run starts the wizard.
func Run(ctx context.Context, lib *library.Library) error {
	m := newModel(ctx, lib)
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithContext(ctx))
	_, err := p.Run()
	return err
}

func newModel(ctx context.Context, lib *library.Library) *model {
	m := &model{
		ctx: ctx, lib: lib,
		entries: oscatalog.Catalog(),
		appPick: map[string]bool{},
		ch:      make(chan tea.Msg, 64),
	}
	m.buildAppList()
	return m
}

// buildAppList narrows the program list to what the chosen operating system
// can actually install: winget packages and the operator's own installers on
// Windows, apt, snaps, Flathub and vendor repositories on Ubuntu. Offering a
// program that cannot run there is worse than not offering it. When the
// target changes the picks are dropped, so a Windows-only program cannot ride
// along to Ubuntu on a mind changed at the first screen.
func (m *model) buildAppList() {
	target := m.entry().AppTarget()
	m.apps = nil
	for _, a := range appcatalog.Catalog() {
		if a.InstallsOn(target) {
			m.apps = append(m.apps, a)
		}
	}
	m.appIdx = 0
	if m.appTarget != target {
		m.appTarget = target
		m.appPick = map[string]bool{}
		m.appOrder = nil
	}
}

func (m *model) Init() tea.Cmd { return listDevices(m.ctx) }

func listDevices(ctx context.Context) tea.Cmd {
	return func() tea.Msg {
		devs, err := device.List(ctx)
		return devicesMsg{devs: devs, err: err}
	}
}

func detectHardware(ctx context.Context) tea.Cmd {
	return func() tea.Msg {
		hw, err := hwdetect.Detect(ctx)
		return detectedMsg{hw: hw, err: err}
	}
}

// waitFor turns the job channel into a stream of Bubble Tea messages; each
// message re-arms it.
func waitFor(ch chan tea.Msg) tea.Cmd {
	return func() tea.Msg { return <-ch }
}

func (m *model) entry() oscatalog.Entry { return m.entries[m.osIdx] }

func (m *model) buildOptions() {
	e := m.entry()
	m.buildAppList()
	m.opts = nil
	// Answered again for every operating system picked, never carried over.
	// An entry with no options screen never clears it otherwise, so a Yes
	// given to Windows or Ubuntu was still a Yes after changing to a distro
	// that has no such question — and the build then wrote Ubuntu's answers
	// onto, say, a Fedora stick.
	m.drivers = false
	if e.Family != oscatalog.Windows {
		// Ubuntu has one thing to choose: its installer can put on the
		// proprietary drivers the kernel does not carry. Everything else
		// Windows asks about — edition, account, bloatware — has no Ubuntu
		// counterpart, so the screen has one line rather than five.
		if e.ThirdPartyDriversSupported() {
			m.opts = append(m.opts, &choice{
				label:  "Drivers for this computer",
				values: []string{"no", "yes"},
				labels: []string{"No", "Yes — the proprietary ones Ubuntu finds (NVIDIA…)"},
			})
		}
		return
	}
	eds := e.Editions
	if len(eds) == 0 {
		eds = []string{"Pro"}
	}
	m.opts = append(m.opts,
		&choice{label: "Edition", values: eds, labels: eds},
		&choice{
			label:  "Account setup",
			values: []string{"local", "oobe"},
			labels: []string{"Local account, no setup screens", "Normal Windows setup (OOBE)"},
		},
		&choice{
			label:  "Remove bloatware",
			values: []string{"standard", "aggressive", "off"},
			labels: []string{"Standard", "Aggressive", "Keep everything"},
		},
		&choice{
			label:  "Drivers for this computer",
			values: []string{"no", "yes"},
			labels: []string{"No", "Yes — detect and stage them"},
		},
		&choice{
			label:  "Skip TPM / Secure Boot checks",
			values: []string{"no", "yes"},
			labels: []string{"No", "Yes (older or virtual hardware)"},
		},
	)
}

func (m *model) opt(label string) string {
	for _, o := range m.opts {
		if o.label == label {
			return o.value()
		}
	}
	return ""
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		return m.key(msg)
	case devicesMsg:
		m.devs = nil
		for _, d := range msg.devs {
			if d.Flashable() {
				m.devs = append(m.devs, d)
			}
		}
		if m.devIdx >= len(m.devs) {
			m.devIdx = 0
		}
		return m, nil
	case detectedMsg:
		m.hw, m.hwErr = msg.hw, msg.err
		return m, nil
	case progressMsg:
		m.curStage = msg.stage
		if msg.total > 0 {
			m.pct = int(msg.done * 100 / msg.total)
		} else {
			m.pct = -1
		}
		return m, waitFor(m.ch)
	case doneMsg:
		m.stage, m.finished = stageDone, true
		m.art, m.err = msg.art, msg.err
		return m, nil
	}
	return m, nil
}

func (m *model) key(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "ctrl+c", "q":
		if m.stage != stageRunning {
			return m, tea.Quit
		}
		return m, nil // never abandon a half-written stick on a stray key
	case "esc":
		if m.stage <= stageOS || m.stage >= stageRunning {
			return m, nil
		}
		m.err = nil
		m.stage = m.prevStage(m.stage)
		return m, nil
	}

	switch m.stage {
	case stageOS:
		return m.keyOS(k)
	case stageISO:
		return m.keyISO(k)
	case stageOptions:
		return m.keyOptions(k)
	case stageApps:
		return m.keyApps(k)
	case stageDevice:
		return m.keyDevice(k)
	case stageConfirm:
		return m.keyConfirm(k)
	case stageDone:
		if k.String() == "enter" {
			return m, tea.Quit
		}
	}
	return m, nil
}

// prevStage is the screen before this one for the operating system chosen.
// Going back has to skip exactly what going forward skipped: Windows walks
// every screen, while a Linux entry has no options to set, may have nothing
// to download, and only offers programs where its installer takes a list.
func (m *model) prevStage(s stage) stage {
	e := m.entry()
	for s > stageOS {
		s--
		switch s {
		case stageISO:
			// Windows is offered the prompt whenever the image is not in the
			// library, since Microsoft's downloads are rate-limited. A Linux
			// entry only sees it when there is nothing to download at all.
			if m.needISO && (e.Family == oscatalog.Windows || m.mustISO) {
				return s
			}
		case stageOptions:
			if len(m.opts) > 0 {
				return s
			}
		case stageApps:
			if e.ProgramsSupported() {
				return s
			}
		default:
			return s
		}
	}
	return stageOS
}

func (m *model) keyOS(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "up", "k":
		if m.osIdx > 0 {
			m.osIdx--
		}
	case "down", "j":
		if m.osIdx < len(m.entries)-1 {
			m.osIdx++
		}
	case "enter":
		m.buildOptions()
		e := m.entry()
		// An import-only entry has nothing to download, so it goes through the
		// ISO step whatever family it is — and there the path is required
		// rather than merely offered.
		m.needISO = !oscatalog.InLibrary(m.lib, e)
		m.mustISO = m.needISO && e.ImportOnly()
		if e.Family != oscatalog.Windows && !m.mustISO {
			// Ubuntu has options to set and takes a program list, through its
			// installer's own answers. Everything else goes straight to the
			// stick, because there is nothing to ask it.
			switch {
			case len(m.opts) > 0:
				m.cursor = 0
				m.stage = stageOptions
			case e.ProgramsSupported():
				m.stage = stageApps
			default:
				m.stage = stageDevice
				return m, listDevices(m.ctx)
			}
			return m, nil
		}
		m.cursor = 0
		m.stage = stageOptions
		if m.needISO {
			m.stage, m.isoPath = stageISO, ""
		}
		return m, detectHardware(m.ctx)
	}
	return m, nil
}

// keyISO edits the ISO path. Paths are long and get pasted, and a terminal
// delivers a paste as one multi-rune key event, so the whole run is taken
// rather than a single character.
func (m *model) keyISO(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.Type {
	case tea.KeyBackspace:
		if r := []rune(m.isoPath); len(r) > 0 {
			m.isoPath = string(r[:len(r)-1])
		}
	case tea.KeySpace:
		m.isoPath += " "
	case tea.KeyRunes:
		m.isoPath += string(k.Runes)
	case tea.KeyEnter:
		// Pasted paths often arrive wrapped in quotes.
		m.isoPath = strings.Trim(strings.TrimSpace(m.isoPath), `"'`)
		if m.isoPath == "" && m.mustISO {
			m.err = m.entry().ImportOnlyError()
			return m, nil
		}
		if m.isoPath != "" {
			if err := oscatalog.CheckISO(m.isoPath); err != nil {
				m.err = err
				return m, nil
			}
		}
		m.err = nil
		// Linux entries have no options to set, so the empty screen is
		// skipped: to the programs where they can take a list, and to the
		// stick where they cannot.
		if len(m.opts) == 0 {
			if m.entry().ProgramsSupported() {
				m.stage = stageApps
				return m, nil
			}
			m.stage = stageDevice
			return m, listDevices(m.ctx)
		}
		m.stage = stageOptions
	}
	return m, nil
}

func (m *model) keyOptions(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if m.cursor < len(m.opts)-1 {
			m.cursor++
		}
	case "left", "h":
		m.opts[m.cursor].next(-1)
	case "right", "l", " ":
		m.opts[m.cursor].next(1)
	case "enter":
		m.drivers = m.opt("Drivers for this computer") == "yes"
		m.stage = stageApps
		m.appIdx = 0
	}
	return m, nil
}

func (m *model) keyApps(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "up", "k":
		if m.appIdx > 0 {
			m.appIdx--
		}
	case "down", "j":
		if m.appIdx < len(m.apps)-1 {
			m.appIdx++
		}
	case " ", "x":
		id := m.apps[m.appIdx].ID
		if m.appPick[id] {
			delete(m.appPick, id)
			for i, o := range m.appOrder {
				if o == id {
					m.appOrder = append(m.appOrder[:i], m.appOrder[i+1:]...)
					break
				}
			}
		} else {
			m.appPick[id] = true
			m.appOrder = append(m.appOrder, id)
		}
	case "enter":
		m.stage = stageDevice
		return m, listDevices(m.ctx)
	}
	return m, nil
}

func (m *model) keyDevice(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "up", "k":
		if m.devIdx > 0 {
			m.devIdx--
		}
	case "down", "j":
		if m.devIdx < len(m.devs)-1 {
			m.devIdx++
		}
	case "r":
		return m, listDevices(m.ctx)
	case "b":
		m.buildOnly = true
		m.stage = stageRunning
		return m, tea.Batch(m.start(), waitFor(m.ch))
	case "enter":
		if len(m.devs) == 0 {
			return m, nil
		}
		m.buildOnly = false
		m.confirm = ""
		m.stage = stageConfirm
	}
	return m, nil
}

func (m *model) keyConfirm(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "backspace":
		if n := len(m.confirm); n > 0 {
			m.confirm = m.confirm[:n-1]
		}
	case "enter":
		want := m.devs[m.devIdx].SizeConfirmation()
		if strings.TrimSpace(m.confirm) != want {
			m.err = fmt.Errorf("that is not %q — nothing was written", want)
			return m, nil
		}
		m.err = nil
		m.stage = stageRunning
		return m, tea.Batch(m.start(), waitFor(m.ch))
	default:
		if s := k.String(); len(s) == 1 {
			m.confirm += s
		}
	}
	return m, nil
}

// start runs the same pipeline the CLI and portal run, on its own goroutine,
// reporting through the channel.
func (m *model) start() tea.Cmd {
	e := m.entry()
	opts := oscatalog.Options{
		Edition:           m.opt("Edition"),
		AccountMode:       m.opt("Account setup"),
		Debloat:           m.opt("Remove bloatware"),
		BypassRequirement: m.opt("Skip TPM / Secure Boot checks") == "yes",
		Apps:              append([]string(nil), m.appOrder...),
	}
	// "Drivers for this computer" means two different jobs. On Windows it
	// stages the packs this machine's hardware needs; on Ubuntu it tells the
	// installer to fetch the proprietary drivers itself, with nothing staged
	// and nothing detected.
	var hw []recipe.HardwareSpec
	switch {
	case m.drivers && e.Family != oscatalog.Windows:
		opts.ThirdPartyDrivers = true
	case m.drivers && m.hw != nil:
		hw = driverresolve.SpecsFor(m.hw, e.DriverOS())
	}
	opts.Hardware = hw

	dev := device.Device{}
	if !m.buildOnly && len(m.devs) > 0 {
		dev = m.devs[m.devIdx]
	}
	buildOnly := m.buildOnly
	ch := m.ch
	ctx := m.ctx
	lib := m.lib
	isoPath := m.isoPath

	return func() tea.Msg {
		go func() {
			progress := func(stage string, done, total int64) {
				select {
				case ch <- progressMsg{stage: stage, done: done, total: total}:
				default: // never block the build on a slow UI
				}
			}
			if isoPath != "" && !oscatalog.InLibrary(lib, e) {
				progress("importing "+filepath.Base(isoPath), 0, -1)
				if _, err := oscatalog.ImportISO(lib, e, isoPath, progress); err != nil {
					ch <- doneMsg{err: err}
					return
				}
			}
			art, err := oscatalog.BuildQuick(ctx, lib, e, opts, progress)
			if err != nil {
				ch <- doneMsg{err: err}
				return
			}
			if buildOnly {
				ch <- doneMsg{art: art}
				return
			}
			if err := flashrun.RunFlash(ctx, art, dev, progress); err != nil {
				ch <- doneMsg{art: art, err: err}
				return
			}
			ch <- doneMsg{art: art}
		}()
		return progressMsg{stage: "starting", total: -1}
	}
}

func (m *model) View() string {
	var b strings.Builder
	b.WriteString(cTitle.Render("DSKY") + cDim.Render("  ·  install an operating system") + "\n\n")
	switch m.stage {
	case stageOS:
		b.WriteString(m.viewOS())
	case stageISO:
		b.WriteString(m.viewISO())
	case stageOptions:
		b.WriteString(m.viewOptions())
	case stageApps:
		b.WriteString(m.viewApps())
	case stageDevice:
		b.WriteString(m.viewDevice())
	case stageConfirm:
		b.WriteString(m.viewConfirm())
	case stageRunning:
		b.WriteString(m.viewRunning())
	case stageDone:
		b.WriteString(m.viewDone())
	}
	if m.err != nil && m.stage != stageDone {
		b.WriteString("\n" + cWarn.Render("error: "+m.err.Error()) + "\n")
	}
	return b.String()
}

func (m *model) viewOS() string {
	var b strings.Builder
	b.WriteString("Which operating system?\n\n")
	for i, e := range m.entries {
		cur := "  "
		name := e.Name
		if i == m.osIdx {
			cur = cSel.Render("> ")
			name = cSel.Render(name)
		}
		detail := e.Version
		if e.ImportOnly() {
			detail += "  (bring your own ISO)"
		}
		b.WriteString(fmt.Sprintf("%s%-28s %s\n", cur, name, cDim.Render(detail)))
	}
	b.WriteString("\n" + cDim.Render("↑/↓ choose · enter continue · q quit") + "\n")
	return b.String()
}

func (m *model) viewISO() string {
	e := m.entry()
	var b strings.Builder
	b.WriteString(cSel.Render(e.Name) + " is not in the library yet.\n\n")
	if m.mustISO {
		b.WriteString("  There is no link to fetch — the vendor puts its images behind an\n")
		b.WriteString("  account, so you supply the ISO.\n\n")
		if e.ImportFrom != "" {
			b.WriteString("  Download it from " + cSel.Render(e.ImportFrom) + "\n\n")
		}
		b.WriteString("  Then give the path to the file:\n\n")
	} else {
		b.WriteString("  It can be fetched from Microsoft, but that is rate-limited to\n")
		b.WriteString("  roughly one download per day per address, and is often refused.\n\n")
		b.WriteString("  If you already have an ISO, give its path. Leave it blank to try\n")
		b.WriteString("  the download.\n\n")
	}
	b.WriteString("  " + cSel.Render(m.isoPath+"▌") + "\n")
	b.WriteString("\n" + cDim.Render("enter to continue · esc back") + "\n")
	return b.String()
}

func (m *model) viewOptions() string {
	var b strings.Builder
	b.WriteString("Options for " + cSel.Render(m.entry().Name) + "\n\n")
	for i, o := range m.opts {
		cur := "  "
		if i == m.cursor {
			cur = cSel.Render("> ")
		}
		b.WriteString(fmt.Sprintf("%s%-30s %s\n", cur, o.label, cSel.Render("‹ "+o.text()+" ›")))
		if o.label == "Drivers for this computer" && o.value() == "yes" {
			b.WriteString("      " + cDim.Render(m.hwSummary()) + "\n")
		}
	}
	b.WriteString("\n" + cDim.Render("↑/↓ move · ←/→ change · enter continue · esc back") + "\n")
	return b.String()
}

func (m *model) hwSummary() string {
	switch {
	case m.hwErr != nil:
		return "detection unavailable: " + m.hwErr.Error()
	case m.hw == nil:
		return "detecting this computer…"
	}
	name := strings.TrimSpace(m.hw.Vendor + " " + m.hw.Model)
	n := len(m.hw.DriverHWIDs())
	s := fmt.Sprintf("%s — %d device(s) to look up", name, n)
	if v := m.hw.KnownVendor(); v != "" {
		s += ", plus " + catalog.VendorNames[catalog.Vendor(v)] + "'s drivers for this model"
	}
	return s
}

func (m *model) viewApps() string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("Programs to install at first boot (%d selected)\n\n", len(m.appPick)))
	lastCat := ""
	// Keep the window short on small terminals: show a slice around the cursor.
	start, end := 0, len(m.apps)
	if end > 14 {
		start = m.appIdx - 6
		if start < 0 {
			start = 0
		}
		end = start + 14
		if end > len(m.apps) {
			end = len(m.apps)
			start = end - 14
		}
	}
	for i := start; i < end; i++ {
		a := m.apps[i]
		if a.Category != lastCat {
			lastCat = a.Category
			b.WriteString("  " + cDim.Render(strings.ToUpper(a.Category)) + "\n")
		}
		mark := "[ ]"
		if m.appPick[a.ID] {
			mark = cOK.Render("[x]")
		}
		cur := "  "
		name := a.Name
		if i == m.appIdx {
			cur = cSel.Render("> ")
			name = cSel.Render(name)
		}
		b.WriteString(fmt.Sprintf("%s%s %s\n", cur, mark, name))
	}
	b.WriteString("\n" + cDim.Render("space toggle · ↑/↓ move · enter continue · esc back") + "\n")
	b.WriteString(cDim.Render("Installed with winget at first boot — the machine needs to be online then.") + "\n")
	return b.String()
}

func (m *model) viewDevice() string {
	var b strings.Builder
	b.WriteString("Which stick?\n\n")
	if len(m.devs) == 0 {
		b.WriteString("  " + cWarn.Render("No USB stick attached.") + "\n")
		b.WriteString("  " + cDim.Render("Plug one in and press r to rescan, or press b to build the image only.") + "\n")
	}
	for i, d := range m.devs {
		cur := "  "
		line := d.String()
		if i == m.devIdx {
			cur = cSel.Render("> ")
			line = cSel.Render(line)
		}
		b.WriteString(cur + line + "\n")
		if len(d.Mounts) > 0 {
			b.WriteString("    " + cDim.Render("mounted at "+strings.Join(d.Mounts, ", ")) + "\n")
		}
	}
	b.WriteString("\n" + cDim.Render("↑/↓ choose · enter continue · r rescan · b build only · esc back") + "\n")
	return b.String()
}

func (m *model) viewConfirm() string {
	d := m.devs[m.devIdx]
	var b strings.Builder
	b.WriteString(cWarn.Render("This ERASES the device below.") + "\n\n")
	b.WriteString("  " + d.String() + "\n")
	if len(d.Mounts) > 0 {
		b.WriteString("  " + cDim.Render("mounted at "+strings.Join(d.Mounts, ", ")) + "\n")
	}
	b.WriteString("\n  Installing " + cSel.Render(m.entry().Name))
	if m.drivers {
		b.WriteString(cDim.Render(" + drivers for this computer"))
	}
	if n := len(m.appPick); n > 0 {
		b.WriteString(cDim.Render(fmt.Sprintf(" + %d program(s)", n)))
	}
	b.WriteString("\n\n")
	b.WriteString(fmt.Sprintf("  Type the stick size (%s) to arm: %s\n",
		cWarn.Render(d.SizeConfirmation()), cSel.Render(m.confirm+"▌")))
	b.WriteString("\n" + cDim.Render("enter to write · esc back") + "\n")
	return b.String()
}

func (m *model) viewRunning() string {
	var b strings.Builder
	b.WriteString("Working…\n\n")
	b.WriteString("  " + cSel.Render(m.curStage) + "\n")
	if m.pct >= 0 {
		const w = 40
		fill := m.pct * w / 100
		b.WriteString("  [" + strings.Repeat("█", fill) + strings.Repeat("·", w-fill) +
			fmt.Sprintf("] %d%%\n", m.pct))
	}
	b.WriteString("\n" + cDim.Render("Do not unplug the stick. q is ignored while writing.") + "\n")
	return b.String()
}

func (m *model) viewDone() string {
	var b strings.Builder
	if m.err != nil {
		b.WriteString(cWarn.Render("Failed: "+m.err.Error()) + "\n")
	} else if m.buildOnly {
		b.WriteString(cOK.Render("Built.") + "\n")
		if m.art != nil {
			b.WriteString(fmt.Sprintf("\n  %s (%d MiB)\n", m.art.Path, m.art.Size>>20))
		}
	} else {
		b.WriteString(cOK.Render("Done — written and verified. Safe to remove.") + "\n")
		b.WriteString("\n  " + cDim.Render("Boot the target machine from this stick.") + "\n")
	}
	b.WriteString("\n" + cDim.Render("enter to exit") + "\n")
	return b.String()
}

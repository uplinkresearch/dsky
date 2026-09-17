package tui

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/uplinkresearch/dsky/internal/device"
	"github.com/uplinkresearch/dsky/internal/library"
	"github.com/uplinkresearch/dsky/internal/oscatalog"
)

func newTestModel(t *testing.T) *model {
	t.Helper()
	lib, err := library.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return newModel(context.Background(), lib)
}

// press sends one keypress the way the terminal would.
func press(t *testing.T, m *model, key string) {
	t.Helper()
	var k tea.KeyMsg
	switch key {
	case "enter":
		k = tea.KeyMsg{Type: tea.KeyEnter}
	case "up":
		k = tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		k = tea.KeyMsg{Type: tea.KeyDown}
	case "left":
		k = tea.KeyMsg{Type: tea.KeyLeft}
	case "right":
		k = tea.KeyMsg{Type: tea.KeyRight}
	case "esc":
		k = tea.KeyMsg{Type: tea.KeyEscape}
	case "space":
		k = tea.KeyMsg{Type: tea.KeySpace}
	case "backspace":
		k = tea.KeyMsg{Type: tea.KeyBackspace}
	default:
		k = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
	}
	m.Update(k)
}

func typeText(t *testing.T, m *model, s string) {
	t.Helper()
	for _, r := range s {
		press(t, m, string(r))
	}
}

// selectWindows walks the OS list to the first Windows entry and enters it.
func selectWindows(t *testing.T, m *model) {
	t.Helper()
	for i, e := range m.entries {
		if e.Family == oscatalog.Windows {
			m.osIdx = i
			break
		}
	}
	press(t, m, "enter")
	// A fresh library holds no Windows image, so the wizard asks for one
	// before anything else; blank means "try the download".
	if m.stage == stageISO {
		press(t, m, "enter")
	}
	if m.stage != stageOptions {
		t.Fatalf("expected the options stage, got %v", m.stage)
	}
}

// TestISOStageOnlyWhenNeeded: the prompt is worth showing when the image has
// to be fetched and is noise otherwise, so it must not appear for Linux
// (whose mirrors are not rate-limited) at all.
func TestISOStageOnlyWhenNeeded(t *testing.T) {
	m := newTestModel(t)
	for i, e := range m.entries {
		if e.Family == oscatalog.Windows {
			m.osIdx = i
			break
		}
	}
	press(t, m, "enter")
	if m.stage != stageISO {
		t.Fatalf("Windows with an empty library should ask for an ISO, got stage %v", m.stage)
	}

	// A path that is not an ISO is refused, and the wizard stays put.
	typeText(t, m, "not-an.iso")
	press(t, m, "enter")
	if m.stage != stageISO || m.err == nil {
		t.Errorf("a bad ISO path was accepted (stage %v, err %v)", m.stage, m.err)
	}

	// Blank continues to the options, leaving the fetch to the build.
	for range len([]rune(m.isoPath)) {
		press(t, m, "backspace")
	}
	press(t, m, "enter")
	if m.stage != stageOptions || m.isoPath != "" {
		t.Errorf("blank ISO did not continue (stage %v, path %q)", m.stage, m.isoPath)
	}

	// Linux never sees the prompt.
	l := newTestModel(t)
	for i, e := range l.entries {
		if e.Family == oscatalog.Linux {
			l.osIdx = i
			break
		}
	}
	press(t, l, "enter")
	if l.stage == stageISO {
		t.Error("Linux should not be asked for an ISO")
	}
}

// TestISOPasteAndQuotes: paths get pasted, which a terminal delivers as one
// multi-rune event, and they often arrive wrapped in quotes.
func TestISOPasteAndQuotes(t *testing.T) {
	m := newTestModel(t)
	m.stage = stageISO
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(`"C:\Users\me\Win11.iso"`)})
	if m.isoPath != `"C:\Users\me\Win11.iso"` {
		t.Fatalf("paste lost characters: %q", m.isoPath)
	}
	press(t, m, "enter") // fails CheckISO (no such file), but must strip quotes first
	if strings.HasPrefix(m.isoPath, `"`) || strings.HasSuffix(m.isoPath, `"`) {
		t.Errorf("quotes were not stripped: %q", m.isoPath)
	}
}

// TestWizardReachesConfirm walks the whole wizard and checks the choices made
// along the way are the ones handed to the build.
func TestWizardReachesConfirm(t *testing.T) {
	m := newTestModel(t)
	selectWindows(t, m)

	// Account setup → OOBE (second value).
	m.cursor = 1
	press(t, m, "right")
	if got := m.opt("Account setup"); got != "oobe" {
		t.Errorf("account setup = %q, want oobe", got)
	}
	// Drivers → yes.
	m.cursor = 3
	press(t, m, "right")
	press(t, m, "enter")
	if m.stage != stageApps {
		t.Fatalf("expected the apps stage, got %v", m.stage)
	}
	if !m.drivers {
		t.Error("drivers option did not carry into the model")
	}

	// Pick the first two programs.
	press(t, m, "space")
	press(t, m, "down")
	press(t, m, "space")
	if len(m.appOrder) != 2 {
		t.Fatalf("picked %d programs, want 2", len(m.appOrder))
	}
	first := m.appOrder[0]

	// Toggling off removes it and keeps the rest in order.
	press(t, m, "space")
	if len(m.appOrder) != 1 || m.appOrder[0] != first {
		t.Errorf("after untoggling, appOrder = %v, want just %q", m.appOrder, first)
	}

	press(t, m, "enter")
	if m.stage != stageDevice {
		t.Fatalf("expected the device stage, got %v", m.stage)
	}

	// No real sticks in a test: inject one.
	m.devs = []device.Device{{ID: "/dev/fake", Model: "Test Stick", SizeBytes: 16 * 1000 * 1000 * 1000}}
	press(t, m, "enter")
	if m.stage != stageConfirm {
		t.Fatalf("expected the confirm stage, got %v", m.stage)
	}
}

// TestConfirmGate is the safety property: the stick is only armed when the
// operator types its exact size.
func TestConfirmGate(t *testing.T) {
	m := newTestModel(t)
	selectWindows(t, m)
	press(t, m, "enter") // options → apps
	press(t, m, "enter") // apps → device
	m.devs = []device.Device{{ID: "/dev/fake", Model: "Test Stick", SizeBytes: 16 * 1000 * 1000 * 1000}}
	press(t, m, "enter") // device → confirm

	want := m.devs[0].SizeConfirmation()
	if want == "" {
		t.Fatal("device reported no size confirmation string")
	}

	// Wrong size: refused, and the wizard stays on the confirm screen.
	typeText(t, m, "0.1")
	press(t, m, "enter")
	if m.stage != stageConfirm {
		t.Fatalf("a wrong size advanced past confirm (stage %v)", m.stage)
	}
	if m.err == nil {
		t.Error("a wrong size produced no error")
	}

	// Correct size arms it.
	for range len(m.confirm) {
		press(t, m, "backspace")
	}
	typeText(t, m, want)
	press(t, m, "enter")
	if m.stage != stageRunning {
		t.Fatalf("the exact size did not arm the write (stage %v)", m.stage)
	}
}

// TestQuitIgnoredWhileWriting: q must not abandon a half-written stick.
func TestQuitIgnoredWhileWriting(t *testing.T) {
	m := newTestModel(t)
	m.stage = stageRunning
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if cmd != nil {
		t.Error("q while writing returned a command (quit) — it must be ignored")
	}
	if m.stage != stageRunning {
		t.Errorf("q while writing changed the stage to %v", m.stage)
	}
	// It must still quit from a safe screen.
	m.stage = stageOS
	if _, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")}); cmd == nil {
		t.Error("q on the first screen did not quit")
	}
}

// TestLinuxSkipsWindowsStages: a Linux ISO whose installer takes no answers
// has no editions or programs to choose, so the wizard goes straight to the
// stick — and comes straight back.
func TestLinuxSkipsWindowsStages(t *testing.T) {
	m := newTestModel(t)
	found := false
	for i, e := range m.entries {
		if e.Family == oscatalog.Linux && !e.ProgramsSupported() {
			m.osIdx, found = i, true
			break
		}
	}
	if !found {
		t.Skip("no Linux entry without programs in the catalog")
	}
	// A drivers answer left over from an operating system that asks the
	// question must not follow one that does not: this entry gets no options
	// screen to clear it, and the build would write Ubuntu's answers onto it.
	m.drivers = true
	press(t, m, "enter")
	if m.stage != stageDevice {
		t.Fatalf("Linux should skip to the device stage, got %v", m.stage)
	}
	if m.drivers {
		t.Error("a drivers answer carried over to an entry that never asks")
	}
	if len(m.opts) != 0 {
		t.Errorf("Linux built %d Windows options", len(m.opts))
	}
	// Backing out must not land on a screen this entry never saw.
	press(t, m, "esc")
	if m.stage != stageOS {
		t.Errorf("esc from the device stage landed on %v, not the OS list", m.stage)
	}
}

// Ubuntu's installer does take a program list, so the wizard offers one — and
// offers the programs Ubuntu can install, not Windows's.
func TestUbuntuOffersPrograms(t *testing.T) {
	m := newTestModel(t)
	found := false
	for i, e := range m.entries {
		if e.Family == oscatalog.Linux && e.ProgramsSupported() {
			m.osIdx, found = i, true
			break
		}
	}
	if !found {
		t.Skip("no Ubuntu entry in the catalog")
	}
	press(t, m, "enter")
	// Ubuntu's one option: the proprietary drivers its installer can fetch.
	// None of Windows's — there is no edition or bloatware to choose.
	if m.stage != stageOptions {
		t.Fatalf("Ubuntu should ask about drivers first, got stage %v", m.stage)
	}
	if len(m.opts) != 1 || m.opts[0].label != "Drivers for this computer" {
		t.Fatalf("Ubuntu options are %+v", m.opts)
	}
	press(t, m, "right") // yes
	press(t, m, "enter")
	if !m.drivers {
		t.Error("the drivers choice did not stick")
	}
	if m.stage != stageApps {
		t.Fatalf("Ubuntu should offer programs, got stage %v", m.stage)
	}
	if len(m.apps) == 0 {
		t.Fatal("the program list is empty for Ubuntu")
	}
	for _, a := range m.apps {
		if a.Ubuntu == nil {
			t.Errorf("%s is offered for Ubuntu but has no Ubuntu source", a.ID)
		}
	}
	if strings.TrimSpace(m.View()) == "" {
		t.Error("the programs screen rendered nothing for Ubuntu")
	}
	// A pick reaches the list handed to the build, and the way back out is
	// the way in, in reverse.
	press(t, m, "space")
	if len(m.appOrder) != 1 || m.appOrder[0] != m.apps[0].ID {
		t.Errorf("picking a program did not reach the order: %v", m.appOrder)
	}
	press(t, m, "enter")
	if m.stage != stageDevice {
		t.Fatalf("programs should lead to the stick, got %v", m.stage)
	}
	press(t, m, "esc")
	if m.stage != stageApps {
		t.Errorf("esc from the stick should come back to the programs, got %v", m.stage)
	}
	press(t, m, "esc")
	if m.stage != stageOptions {
		t.Errorf("esc from the programs should come back to the options, got %v", m.stage)
	}
	press(t, m, "esc")
	if m.stage != stageOS {
		t.Errorf("esc from the options should come back to the OS list, got %v", m.stage)
	}

	// Changing to Windows must not carry an Ubuntu-only pick across: a
	// program that cannot install where it is going fails the whole build.
	for i, e := range m.entries {
		if e.Family == oscatalog.Windows {
			m.osIdx = i
			break
		}
	}
	press(t, m, "enter")
	if len(m.appOrder) != 0 {
		t.Errorf("an Ubuntu program survived the change to Windows: %v", m.appOrder)
	}
	if m.drivers {
		t.Error("the drivers answer survived the change of operating system")
	}
	for _, a := range m.apps {
		if !a.InstallsOnWindows() {
			t.Errorf("%s is offered for Windows but cannot install there", a.ID)
		}
	}
}

// TestViewsRender makes sure no screen panics or comes out blank.
func TestViewsRender(t *testing.T) {
	m := newTestModel(t)
	m.devs = []device.Device{{ID: "/dev/fake", Model: "Test Stick", SizeBytes: 16 * 1000 * 1000 * 1000}}
	selectWindows(t, m)
	for _, st := range []stage{stageOS, stageISO, stageOptions, stageApps, stageDevice, stageConfirm, stageRunning, stageDone} {
		m.stage = st
		out := m.View()
		if strings.TrimSpace(out) == "" {
			t.Errorf("stage %v rendered nothing", st)
		}
	}
}

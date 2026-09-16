package agent

import (
	"strings"
	"sync"
	"time"
)

// What somebody standing at the machine sees while it is being set up.
//
// Until now they saw nothing: the machine signed itself in, put up an ordinary
// desktop, and then spent twenty minutes installing drivers and removing apps
// invisibly. There was no way to tell "still working" from "died four minutes
// ago" -- which is exactly what happened to an HP EliteBook, and the only
// reason anybody found out was reading a log file afterwards. Now that the
// agent also restarts the machine on purpose, silence is worse still.
//
// The screen never blocks the work. Everything below is safe on a nil screen,
// so a machine where the window cannot be created is provisioned exactly as
// before, with a line in the log to say the window did not open.

// stepState is how a step is drawn.
type stepState int

const (
	stepDoing stepState = iota
	stepDone
	stepProblem
)

// stepLine is one line of the checklist.
type stepLine struct {
	Name    string
	Detail  string
	State   stepState
	Started time.Time
}

// choice is what the person at the machine decided about a restart.
type choice string

const (
	choiceNow  choice = "now"  // restart it now
	choiceWait choice = "wait" // not yet
	choiceNone choice = ""     // nobody was there; the countdown ran out
)

// waitAgain is how long "Wait" holds the machine before asking again.
const waitAgain = 5 * time.Minute

// screen is the state behind the window, and the only thing the rest of the
// agent touches.
type screen struct {
	mu       sync.Mutex
	machine  string
	heading  string
	sub      string
	steps    []stepLine
	note     string
	buttons  []string
	summary  []string
	finished bool

	clicked  chan int
	dismiss  chan struct{}
	dismissD sync.Once

	w window // the platform's window, nil when there is none
}

// window is what a platform provides: something that can show the screen and
// be told it has changed.
type window interface {
	refresh()
	close()
}

// openScreenFn is how the run opens its window, as a variable so the package's
// tests never put a real one up. On Windows they did: Apply opened a
// full-screen window on the CI runner and then waited, forever, for somebody
// to press Finish.
var openScreenFn = openScreen

// openScreen puts the window up. It returns nil when there can be no window --
// another operating system, a session with no desktop, or a window that would
// not create -- and every method below does nothing on a nil screen.
func openScreen(machine string) *screen {
	s := &screen{
		machine: machine,
		heading: "Setting up this machine",
		sub:     "This takes a while. Please don't turn it off.",
		clicked: make(chan int, 4),
		dismiss: make(chan struct{}),
	}
	w, err := newWindow(s)
	if err != nil || w == nil {
		return nil
	}
	s.w = w
	return s
}

// Doing says what the agent has started on, and closes off whatever it was
// doing before.
func (s *screen) Doing(step, detail string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	for i := range s.steps {
		if s.steps[i].State == stepDoing {
			s.steps[i].State = stepDone
		}
	}
	s.steps = append(s.steps, stepLine{Name: step, Detail: detail, State: stepDoing, Started: time.Now()})
	s.mu.Unlock()
	s.w.refresh()
}

// Detail replaces the note under the step in progress, for the long ones: a
// driver sweep is eighteen minutes of nothing otherwise.
func (s *screen) Detail(detail string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	for i := range s.steps {
		if s.steps[i].State == stepDoing {
			s.steps[i].Detail = detail
		}
	}
	s.mu.Unlock()
	s.w.refresh()
}

// Finished closes off the step in progress, marking it as having had problems
// if it did.
func (s *screen) Finished(step string, problems int) {
	if s == nil {
		return
	}
	s.mu.Lock()
	for i := range s.steps {
		if s.steps[i].Name == step && s.steps[i].State == stepDoing {
			// The detail described the work in progress. Left on a finished
			// line it reads as still happening: watched in the VM, a done
			// step said "OK Drivers - installing 264 driver files (this is
			// the long part)".
			s.steps[i].Detail = ""
			if problems > 0 {
				s.steps[i].State = stepProblem
				s.steps[i].Detail = "see the list at the end"
			} else {
				s.steps[i].State = stepDone
			}
		}
	}
	s.mu.Unlock()
	s.w.refresh()
}

// AskRestart counts down in front of whoever is there, and gives them the
// decision. Nobody there means the countdown runs out and the machine
// restarts, which is what an unattended bench needs; somebody there can take
// it now or hold it off.
func (s *screen) AskRestart(reason string, d time.Duration) choice {
	if s == nil {
		return choiceNone
	}
	for {
		deadline := time.Now().Add(d)
		s.mu.Lock()
		s.buttons = []string{"Restart now", "Wait " + shortDur(waitAgain)}
		s.mu.Unlock()

		tick := time.NewTicker(time.Second)
		for {
			left := time.Until(deadline)
			if left <= 0 {
				tick.Stop()
				s.clearButtons()
				return choiceNone
			}
			s.mu.Lock()
			s.note = "Restarting in " + shortDur(left) + " to " + reason + ". It carries on by itself afterwards."
			s.mu.Unlock()
			s.w.refresh()

			select {
			case n := <-s.clicked:
				tick.Stop()
				s.clearButtons()
				if n == 0 {
					return choiceNow
				}
				// Waiting: say so, hold, then ask again.
				s.mu.Lock()
				s.note = "Restart held for " + shortDur(waitAgain) + "."
				s.mu.Unlock()
				s.w.refresh()
				select {
				case <-time.After(waitAgain):
				case <-s.clicked:
					return choiceNow
				}
				d = 30 * time.Second
			case <-tick.C:
				continue
			}
			break
		}
	}
}

func (s *screen) clearButtons() {
	s.mu.Lock()
	s.buttons = nil
	s.note = ""
	s.mu.Unlock()
	s.w.refresh()
}

// Restarting is the last thing the screen says before the machine goes down.
func (s *screen) Restarting(reason string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.note = "Restarting now to " + reason + ". This machine carries on by itself."
	s.buttons = nil
	s.mu.Unlock()
	s.w.refresh()
}

// Summary is the end of the job: what went in, what did not, and a button.
// The window stays until somebody dismisses it, so a bench of machines can be
// read from the doorway.
func (s *screen) Summary(heading string, lines []string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.finished = true
	s.heading = heading
	s.sub = s.machine
	s.summary = lines
	s.note = ""
	s.buttons = []string{"Finish"}
	s.mu.Unlock()
	s.w.refresh()
}

// WaitDismiss holds until the finish button is pressed. The work is over by
// then, so nothing is being held up.
func (s *screen) WaitDismiss() {
	if s == nil {
		return
	}
	select {
	case <-s.clicked:
	case <-s.dismiss:
	}
}

// Close takes the window away.
func (s *screen) Close() {
	if s == nil {
		return
	}
	s.w.close()
}

// dismissed is called by the window when it is closed from the keyboard, so
// WaitDismiss does not hold a finished machine forever.
func (s *screen) dismissed() {
	s.dismissD.Do(func() { close(s.dismiss) })
}

// shortDur is a duration the way somebody standing at a machine would say it.
func shortDur(d time.Duration) string {
	sec := int(d.Round(time.Second) / time.Second)
	switch {
	case sec >= 120:
		return itoa((sec+59)/60) + " minutes"
	case sec >= 60:
		return "1 minute"
	default:
		return itoa(sec) + " seconds"
	}
}

// isShellFlyoutImage reports whether a program path is the Windows 11 Start
// menu or its search box. Kept apart from the Windows calls that find the
// path, so the decision -- which is the part that must never send an Escape to
// the wrong program -- is testable anywhere.
func isShellFlyoutImage(path string) bool {
	base := strings.ToLower(path)
	if i := strings.LastIndexAny(base, `\/`); i >= 0 {
		base = base[i+1:]
	}
	switch base {
	case "startmenuexperiencehost.exe", "searchhost.exe", "searchapp.exe":
		return true
	}
	return false
}

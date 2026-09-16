package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// State records which steps have finished, so a machine that reboots or
// crashes part way through carries on rather than starting over. A step is
// marked done only when it completed; a step that failed is left undone so a
// later run can try it again.
type State struct {
	path    string
	Started string          `json:"started"`
	Done    map[string]bool `json:"done"`
	// Reboots counts restarts this agent asked for, so a step that asks
	// every time cannot put the machine in a loop.
	Reboots int `json:"reboots,omitempty"`

	// OwnerShortcuts are the desktop shortcuts a standalone payload found
	// when it first ran: the machine owner's, never to be removed. Recorded
	// once, before anything is installed, so a run that resumes after a
	// restart does not take the installers' icons for the owner's.
	OwnerShortcuts []string `json:"owner_shortcuts,omitempty"`
	Snapshotted    bool     `json:"shortcuts_recorded,omitempty"`
}

// LoadState reads the state beside the agent, or starts a fresh one.
func LoadState(dir string) *State {
	s := &State{path: filepath.Join(dir, StateName), Done: map[string]bool{}}
	if b, err := os.ReadFile(s.path); err == nil {
		var prev State
		if json.Unmarshal(b, &prev) == nil && prev.Done != nil {
			s.Done = prev.Done
			s.Started = prev.Started
			s.Reboots = prev.Reboots
			s.OwnerShortcuts = prev.OwnerShortcuts
			s.Snapshotted = prev.Snapshotted
		}
	}
	if s.Started == "" {
		s.Started = time.Now().Format(time.RFC3339)
	}
	return s
}

// Finished reports whether a step has already completed.
func (s *State) Finished(step string) bool { return s.Done[step] }

// Finish marks a step complete and saves immediately, because the next thing
// that happens may be a reboot.
func (s *State) Finish(step string) {
	s.Done[step] = true
	s.save()
}

// OwnShortcuts returns the shortcuts that belong to the machine's owner,
// recording them the first time it is asked.
func (s *State) OwnShortcuts(list func() []string) map[string]bool {
	if !s.Snapshotted {
		s.OwnerShortcuts = list()
		s.Snapshotted = true
		s.save()
	}
	own := make(map[string]bool, len(s.OwnerShortcuts))
	for _, p := range s.OwnerShortcuts {
		own[strings.ToLower(p)] = true
	}
	return own
}

// CountReboot records that the agent is about to restart the machine. It is
// saved before the restart is asked for, because the next thing that happens
// is the machine going down.
func (s *State) CountReboot() {
	s.Reboots++
	s.save()
}

func (s *State) save() {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return
	}
	os.WriteFile(s.path, append(b, '\n'), 0o644)
}

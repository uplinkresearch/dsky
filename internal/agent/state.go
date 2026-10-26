package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// State records which steps have finished, so a machine that reboots or
// crashes part way through carries on rather than starting over.
//
// It belongs to one run, not to the machine. A payload lives in a folder
// named for its build, so the same payload run twice is the same folder --
// and the state a finished run left there is the first thing the next run
// reads. Carrying on from it is right for a run that was interrupted and
// wrong for one that was over: somebody who runs a payload again is asking
// for the work to be done, and skipping every step to show them a finish
// screen is the machine claiming an install it never attempted. So a run
// that reaches its end says so, and the run after that starts from nothing.
//
// A step is recorded as finished when it ran to its end, whether or not
// parts of it failed. It is what a resume can act on: the log and the record
// beside it name what failed, and repeating an hour of driver packs to retry
// the one that is broken costs the machine more than it wins back.
type State struct {
	path    string
	Started string          `json:"started"`
	Done    map[string]bool `json:"done"`
	// Completed is when the run that wrote this reached its end -- not a
	// restart it is coming back from, the end. Empty means a run that is
	// still going or one that was interrupted, which is what a resume is
	// allowed to carry on from.
	Completed string `json:"completed,omitempty"`
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

// LoadState reads the state beside the agent exactly as it was left, or
// starts a fresh one. This is the record of what happened on this machine --
// what `dsky-agent verify` reads back -- so it reports a finished run's steps
// as finished however long ago that run was. A run that is about to do work
// wants StateForRun instead.
func LoadState(dir string) *State {
	s := &State{path: filepath.Join(dir, StateName), Done: map[string]bool{}}
	if b, err := os.ReadFile(s.path); err == nil {
		var prev State
		if json.Unmarshal(b, &prev) == nil && prev.Done != nil {
			s.Done = prev.Done
			s.Started = prev.Started
			s.Completed = prev.Completed
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

// StateForRun is the state a run starts from: what an interrupted run left
// behind, or nothing at all when the last run reached its end.
//
// The machine owner's shortcuts are the one thing carried across either way.
// They were read before any payload installed anything, and reading them
// again now would take the icons the last run installed for the owner's own
// -- and then leave them on the desktop forever, which is the opposite of
// what the list is for.
func StateForRun(dir string) *State {
	s := LoadState(dir)
	if s.Completed == "" {
		return s
	}
	return &State{
		path:           s.path,
		Started:        time.Now().Format(time.RFC3339),
		Done:           map[string]bool{},
		OwnerShortcuts: s.OwnerShortcuts,
		Snapshotted:    s.Snapshotted,
	}
}

// Finished reports whether a step has already completed.
func (s *State) Finished(step string) bool { return s.Done[step] }

// Finish marks a step complete and saves immediately, because the next thing
// that happens may be a reboot.
func (s *State) Finish(step string) {
	s.Done[step] = true
	s.save()
}

// Complete records that the run is over: not part way through a step, not
// on its way into a restart it will come back from -- finished. Said at the
// same moment the resume task is taken away, because they are the same fact:
// nothing is going to carry this run on, so the next one starts its own.
func (s *State) Complete() {
	s.Completed = time.Now().Format(time.RFC3339)
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

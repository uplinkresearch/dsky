package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// ResultName is the machine-readable record of what this build was asked to
// do and what happened, left on the machine it happened to.
const ResultName = "dsky-migrate-result.json"

// Result is that record.
//
// It is JSON and nothing else. The agent does not render a page: DSKY has the
// templates, on a machine with a person in front of it, and an imaging agent
// that grows a template engine to describe itself is an imaging agent that
// fails in a new way. What this has to be is exact, and readable by the thing
// that later checks a machine against the plan it was built from.
type Result struct {
	Machine  string    `json:"machine"`
	Recipe   string    `json:"recipe,omitempty"`
	Agent    string    `json:"agent_version,omitempty"`
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished,omitempty"`
	Outcomes []Outcome `json:"outcomes"`
	Problems []string  `json:"problems,omitempty"`
}

// Outcome is one thing the plan asked for.
type Outcome struct {
	Kind  string `json:"kind"`  // app | setting | printer | drive | domain
	Name  string `json:"name"`  // the plan's own name for it
	State string `json:"state"` // done | failed | at-sign-in
	Note  string `json:"note,omitempty"`
}

// Outcome states.
const (
	// StateDone happened, on this machine, and was checked where checking
	// was possible.
	StateDone = "done"
	// StateFailed did not happen. The note says what somebody has to do.
	StateFailed = "failed"
	// StateAtSignIn is neither: it belongs to a person and is waiting for
	// them to sign in. Recording it as done would be a lie and recording it
	// as failed would send somebody looking for a fault. A migrated machine
	// is full of these, which is why the state exists.
	StateAtSignIn = "at-sign-in"
)

// record adds one outcome. Kept in memory and written after every step, so a
// machine that restarts mid-build -- which a domain join guarantees -- comes
// back with what it had already done still in the file.
func (a *Agent) record(kind, name, state, note string) {
	a.outcomes = append(a.outcomes, Outcome{Kind: kind, Name: name, State: state, Note: note})
}

func (a *Agent) done(kind, name string)          { a.record(kind, name, StateDone, "") }
func (a *Agent) failed(kind, name, why string)   { a.record(kind, name, StateFailed, why) }
func (a *Agent) atSignIn(kind, name, why string) { a.record(kind, name, StateAtSignIn, why) }

// writeResult saves the record, merging with anything an earlier boot left.
//
// Merging rather than appending: a step that ran before a restart is not run
// again, but a step that was interrupted part-way is, and its earlier
// outcomes would otherwise appear twice. Last word wins, because the later
// run is the one that finished.
func (a *Agent) writeResult(machine string, started time.Time, done bool) {
	path := filepath.Join(a.Dir, ResultName)
	r := Result{Machine: machine, Started: started}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &r)
	}
	if a.Manifest != nil {
		r.Recipe = a.Manifest.Recipe
	}
	r.Outcomes = mergeOutcomes(r.Outcomes, a.outcomes)
	r.Problems = a.J.Failures()
	if done {
		r.Finished = time.Now()
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return
	}
	// Best effort, deliberately: a machine that is otherwise fine must not be
	// reported as failed because a log could not be written.
	_ = os.WriteFile(path, append(b, '\n'), 0o644)
}

// mergeOutcomes keeps one entry per thing, preferring the newer run's word.
func mergeOutcomes(old, new []Outcome) []Outcome {
	at := map[string]int{}
	out := make([]Outcome, 0, len(old)+len(new))
	for _, o := range old {
		at[o.Kind+"\x00"+o.Name] = len(out)
		out = append(out, o)
	}
	for _, o := range new {
		if i, ok := at[o.Kind+"\x00"+o.Name]; ok {
			out[i] = o
			continue
		}
		at[o.Kind+"\x00"+o.Name] = len(out)
		out = append(out, o)
	}
	return out
}

// Outcome kinds.
const (
	KindApp     = "app"
	KindSetting = "setting"
	KindPrinter = "printer"
	KindDrive   = "drive"
	KindDomain  = "domain"
)

// Package jobs is the shared progress bus: builds and flashes publish stage
// events; the web UI's SSE stream and any other listener subscribe.
package jobs

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Event is one progress update. Terminal events have Final set (Err empty on
// success).
type Event struct {
	JobID string `json:"job_id"`
	Kind  string `json:"kind"` // "build" | "flash" | "capture"
	Title string `json:"title"`
	Stage string `json:"stage"`
	Done  int64  `json:"done"`
	Total int64  `json:"total"` // -1 while unknown
	Err   string `json:"error,omitempty"`
	Final bool   `json:"final,omitempty"`
	// Result carries a small payload on success (e.g. artifact path).
	Result string `json:"result,omitempty"`
	// Device is the disk this job is about, when it is about one: a flash
	// names the stick it writes, a capture the disk it reads. Empty for work
	// that touches no disk — a download, a build, an update.
	//
	// It is carried here rather than left inside Title because the page acts
	// on it: a finished run is cleared when its disk is unplugged, since the
	// record was only ever a note about that stick. Reading the name back out
	// of a title would mean parsing a sentence that is written differently for
	// every kind of job and differently again on Windows.
	Device string `json:"device,omitempty"`
	At     int64  `json:"at"` // unix millis
}

// Registry fans events out to subscribers and remembers each job's latest
// state so a page that connects late still renders current jobs.
type Registry struct {
	mu     sync.Mutex
	nextID atomic.Int64
	subs   map[chan Event]struct{}
	latest map[string]Event
}

func NewRegistry() *Registry {
	return &Registry{
		subs:   map[chan Event]struct{}{},
		latest: map[string]Event{},
	}
}

// Subscribe returns a buffered event channel and a cancel func. The current
// state of every known job is replayed first.
func (r *Registry) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 256)
	r.mu.Lock()
	r.subs[ch] = struct{}{}
	for _, ev := range r.latest {
		select {
		case ch <- ev:
		default:
		}
	}
	r.mu.Unlock()
	return ch, func() {
		r.mu.Lock()
		if _, ok := r.subs[ch]; ok {
			delete(r.subs, ch)
			close(ch)
		}
		r.mu.Unlock()
	}
}

// Busy reports whether any job is still running. Terminal events carry Final,
// so anything without it is still going.
//
// Used to decide whether the server may stop when the last page closes: a
// flash that outlives its browser tab must finish, and a half-written stick is
// the worst thing this tool can leave behind.
func (r *Registry) Busy() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, ev := range r.latest {
		if !ev.Final {
			return true
		}
	}
	return false
}

// BusyExcept reports whether any job other than id is still running.
//
// Busy cannot answer this, because a job is always unfinished from inside
// itself: a job that guards on Busy is really guarding on its own existence
// and so never proceeds. That is not a hypothetical — the self-update guarded
// on Busy to avoid restarting mid-job, and silently never restarted.
func (r *Registry) BusyExcept(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for jobID, ev := range r.latest {
		if jobID != id && !ev.Final {
			return true
		}
	}
	return false
}

func (r *Registry) publish(ev Event) {
	ev.At = time.Now().UnixMilli()
	r.mu.Lock()
	r.latest[ev.JobID] = ev
	for ch := range r.subs {
		select {
		case ch <- ev:
		default: // slow subscriber: drop rather than block progress
		}
	}
	r.mu.Unlock()
}

// Job is one tracked operation.
type Job struct {
	ID    string
	Kind  string
	Title string
	// Device is the disk this job is about, or empty. See Event.Device.
	Device string
	reg    *Registry
}

// New starts tracking a job that is not about any one disk.
func (r *Registry) New(kind, title string) *Job { return r.NewOn(kind, title, "") }

// NewOn starts tracking a job that is about one disk, and emits its initial
// event. device is the same id the page knows the disk by.
func (r *Registry) NewOn(kind, title, device string) *Job {
	j := &Job{
		ID:     fmt.Sprintf("%s-%d", kind, r.nextID.Add(1)),
		Kind:   kind,
		Title:  title,
		Device: device,
		reg:    r,
	}
	j.Progress("queued", 0, -1)
	return j
}

// event fills in everything constant about this job, so a field added to
// Event cannot reach one publisher and miss another.
func (j *Job) event() Event {
	return Event{JobID: j.ID, Kind: j.Kind, Title: j.Title, Device: j.Device}
}

// Progress publishes a non-terminal update.
func (j *Job) Progress(stage string, done, total int64) {
	ev := j.event()
	ev.Stage, ev.Done, ev.Total = stage, done, total
	j.reg.publish(ev)
}

// Finish publishes the terminal success event.
func (j *Job) Finish(result string) {
	ev := j.event()
	ev.Stage, ev.Final, ev.Result = "done", true, result
	j.reg.publish(ev)
}

// Fail publishes the terminal failure event.
func (j *Job) Fail(err error) {
	ev := j.event()
	ev.Stage, ev.Final, ev.Err = "error", true, err.Error()
	j.reg.publish(ev)
}

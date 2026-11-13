package cli

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/uplinkresearch/dsky/internal/device"
)

// multiProgress renders progress for several sticks being written at once.
//
// It deliberately does not redraw a row per device with cursor movement:
// that breaks the moment output is piped to a file or a CI log, which is
// exactly where someone imaging twenty sticks wants a record. Instead one
// status line is rewritten in place, and each stick prints a durable line
// when it finishes or fails — so the scrollback ends up being the report.
type multiProgress struct {
	mu     sync.Mutex
	total  int
	done   map[string]int64
	size   map[string]int64
	state  map[string]string // "" | "done" | "failed"
	stage  string
	lastNo int
	live   bool
}

func newMultiProgress(devs []device.Device) *multiProgress {
	m := &multiProgress{
		total: len(devs),
		done:  map[string]int64{},
		size:  map[string]int64{},
		state: map[string]string{},
	}
	for _, d := range devs {
		m.state[d.ID] = ""
	}
	return m
}

func (m *multiProgress) report(devID, stage string, doneBytes, total int64) {
	// A stage may carry a second line for the page to show under it (see
	// splitStage in index.html). A status line that is redrawn in place has
	// room for one, so the terminal shows the stage itself.
	stage, _, _ = strings.Cut(stage, "\n")
	m.mu.Lock()
	defer m.mu.Unlock()

	// Device-less events (elevation prompts) are just the headline.
	if devID == "" {
		if stage != m.stage {
			m.clearLine()
			fmt.Println(stage + "...")
			m.stage = stage
		}
		return
	}
	switch {
	case stage == "done":
		if m.state[devID] == "" {
			m.state[devID] = "done"
			m.clearLine()
			fmt.Printf("  %-24s written and verified\n", devID)
		}
	case strings.HasPrefix(stage, "error"):
		if m.state[devID] != "failed" {
			m.state[devID] = "failed"
			m.clearLine()
			fmt.Printf("  %-24s FAILED: %s\n", devID, strings.TrimPrefix(stage, "error: "))
		}
	default:
		if total > 0 {
			m.done[devID], m.size[devID] = doneBytes, total
		}
		m.stage = stage
	}
	m.draw()
}

// draw rewrites the single status line when the rounded percentage moves, so
// a long write does not scroll thousands of near-identical lines.
func (m *multiProgress) draw() {
	var sumDone, sumSize int64
	for id, s := range m.size {
		sumSize += s
		sumDone += m.done[id]
	}
	pct := 0
	if sumSize > 0 {
		pct = int(sumDone * 100 / sumSize)
	}
	finished, failed := 0, 0
	for _, s := range m.state {
		switch s {
		case "done":
			finished++
		case "failed":
			failed++
		}
	}
	if pct == m.lastNo && m.live {
		return
	}
	m.lastNo = pct
	parts := []string{fmt.Sprintf("%d%%", pct)}
	if working := m.total - finished - failed; working > 0 {
		parts = append(parts, fmt.Sprintf("%d writing", working))
	}
	if finished > 0 {
		parts = append(parts, fmt.Sprintf("%d done", finished))
	}
	if failed > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", failed))
	}
	label := m.stage
	if m.total > 1 {
		label = fmt.Sprintf("%d sticks", m.total)
	}
	fmt.Printf("\r%-70s", fmt.Sprintf("  %s: %s", label, strings.Join(parts, ", ")))
	m.live = true
}

func (m *multiProgress) clearLine() {
	if m.live {
		fmt.Printf("\r%-70s\r", "")
		m.live = false
	}
}

// finish closes the status line and, when several sticks were written,
// summarises which ones made it.
func (m *multiProgress) finish() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.clearLine()
	if m.total <= 1 {
		return
	}
	var failed []string
	for id, s := range m.state {
		if s == "failed" {
			failed = append(failed, id)
		}
	}
	sort.Strings(failed)
	if len(failed) > 0 {
		fmt.Printf("%d of %d failed: %s\n", len(failed), m.total, strings.Join(failed, ", "))
	}
}

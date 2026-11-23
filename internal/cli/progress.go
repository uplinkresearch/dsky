package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/mattn/go-isatty"
)

// Progress on the command line.
//
// Every long operation in this tool reports through one callback — pulling a
// 7 GB ISO, composing an image, writing a stick, fetching driver packs,
// replacing the binary — so there is one renderer for all of them and they all
// look the same.
//
// Two shapes, chosen by where the output is going:
//
//   - A terminal gets a bar rewritten in place, with the transferred size, a
//     smoothed rate and an estimate. Nothing scrolls.
//   - Anything else — a pipe, a log file, a CI job, the elevated worker's
//     captured output — gets one durable line per 10%. A bar redrawn with
//     carriage returns turns a log into a single unreadable line, and a log is
//     exactly what someone imaging twenty sticks wants to keep.
//
// The rate is measured over a trailing window rather than between the last two
// callbacks: USB writes and HTTP reads arrive in bursts, and an instantaneous
// rate swings between zero and absurd, which reads as a broken display.
const (
	barWidth   = 22
	lineWidth  = 78 // fits an 80-column terminal without wrapping
	rateWindow = 3 * time.Second
	minRedraw  = 100 * time.Millisecond
)

type sample struct {
	at   time.Time
	done int64
}

type stageProgress struct {
	mu sync.Mutex

	// out defaults to stdout; tests supply their own. forceTTY makes the
	// terminal decision explicit for tests, since a test binary's stdout is
	// never a terminal and the two renderings differ on purpose.
	out      io.Writer
	forceTTY *bool

	stage    string
	started  time.Time
	lastDraw time.Time
	lastPct  int // last percentage rendered (tty) or logged (non-tty)
	live     bool
	tty      bool
	ttyKnown bool

	samples []sample
}

func (p *stageProgress) printf(format string, args ...any) {
	w := p.out
	if w == nil {
		w = os.Stdout
	}
	fmt.Fprintf(w, format, args...)
}

// report is the callback handed to every long operation.
func (p *stageProgress) report(stage string, done, total int64) {
	// A stage may carry a second line for the page to show under it (see
	// splitStage in index.html). A status line that is redrawn in place has
	// room for one, so the terminal shows the stage itself.
	stage, _, _ = strings.Cut(stage, "\n")
	p.mu.Lock()
	defer p.mu.Unlock()

	if !p.ttyKnown {
		switch {
		case p.forceTTY != nil:
			p.tty = *p.forceTTY
		default:
			p.tty = isatty.IsTerminal(os.Stdout.Fd()) || isatty.IsCygwinTerminal(os.Stdout.Fd())
		}
		p.ttyKnown = true
	}

	if stage != p.stage {
		p.endLine()
		p.stage = stage
		p.started = time.Now()
		p.lastPct = -1
		p.samples = p.samples[:0]
		if !p.tty {
			// Announce the stage even when there is no measurable progress, so
			// a log says what it was doing when it stopped.
			p.printf("%s...\n", stage)
		}
	}

	now := time.Now()
	if done > 0 {
		p.samples = append(p.samples, sample{now, done})
		for len(p.samples) > 1 && now.Sub(p.samples[0].at) > rateWindow {
			p.samples = p.samples[1:]
		}
	}

	if !p.tty {
		p.logLine(done, total)
		return
	}
	// Redraw at a human rate, but never skip 100%: that frame is the one that
	// leaves the line reading "done" rather than "97%".
	pct := percent(done, total)
	if now.Sub(p.lastDraw) < minRedraw && pct != 100 && p.live {
		return
	}
	p.lastDraw = now
	p.draw(done, total)
}

// logLine emits a durable line every 10%, for output that is not a terminal.
func (p *stageProgress) logLine(done, total int64) {
	if total <= 0 {
		return // indeterminate: the stage announcement above is all there is
	}
	pct := percent(done, total)
	if pct/10 == p.lastPct/10 && pct != 100 {
		return
	}
	p.lastPct = pct
	p.printf("  %3d%%  %s of %s%s\n", pct, humanBytes(done), humanBytes(total), p.rateSuffix(done, total))
}

func (p *stageProgress) draw(done, total int64) {
	var line string
	if total > 0 {
		pct := percent(done, total)
		filled := barWidth * pct / 100
		bar := strings.Repeat("#", filled) + strings.Repeat("-", barWidth-filled)
		line = fmt.Sprintf("%s [%s] %3d%%  %s/%s%s",
			p.stage, bar, pct, humanBytes(done), humanBytes(total), p.rateSuffix(done, total))
	} else if done > 0 {
		// Size unknown (a server that sends no length, a compressed image
		// expanding): show what has moved rather than a bar that would lie.
		line = fmt.Sprintf("%s  %s%s", p.stage, humanBytes(done), p.rateSuffix(done, 0))
	} else {
		line = p.stage + "..."
	}
	// Truncate by runes, not bytes: a stage name can carry a non-ASCII
	// character (a distro name, a path), and cutting one in half emits a
	// broken sequence that some terminals render as a replacement glyph and
	// others as nothing at all. The ellipsis is one rune but three bytes, so
	// the byte length has to be budgeted for as well.
	if r := []rune(line); len(r) > lineWidth {
		line = string(r[:lineWidth-1]) + "…"
	}
	p.printf("\r%-*s", lineWidth, line)
	p.live = true
}

// rateSuffix renders the smoothed rate and, when the total is known, an
// estimate. Both are omitted until there is enough of a window to mean
// anything — a made-up "ETA 4h" in the first second is worse than nothing.
func (p *stageProgress) rateSuffix(done, total int64) string {
	if len(p.samples) < 2 {
		return ""
	}
	first, last := p.samples[0], p.samples[len(p.samples)-1]
	elapsed := last.at.Sub(first.at).Seconds()
	moved := last.done - first.done
	if elapsed < 0.5 || moved <= 0 {
		return ""
	}
	rate := float64(moved) / elapsed
	out := "  " + humanBytes(int64(rate)) + "/s"
	if total > done {
		eta := time.Duration(float64(total-done)/rate) * time.Second
		out += "  ETA " + humanDuration(eta)
	}
	return out
}

// endLine closes an in-place line so the next print starts clean.
func (p *stageProgress) endLine() {
	if p.live {
		p.printf("\r%-*s\r", lineWidth, "")
		p.live = false
	}
}

// finish closes the display. Callers defer it, so it must be safe to call
// twice and safe to call when nothing was ever reported.
func (p *stageProgress) finish() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.endLine()
	p.stage = ""
}

func percent(done, total int64) int {
	if total <= 0 {
		return 0
	}
	pct := int(done * 100 / total)
	if pct > 100 {
		return 100
	}
	if pct < 0 {
		return 0
	}
	return pct
}

// humanBytes uses binary units, which is what every other size in this tool
// reports (a stick's capacity, an artifact's size) — mixing the two would make
// a 7.3 GiB image look like it does not fit an 8 GB stick.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit && exp < 3; x /= unit {
		div *= unit
		exp++
	}
	v := float64(n) / float64(div)
	format := "%.1f %ciB"
	if v >= 100 {
		format = "%.0f %ciB"
	}
	return fmt.Sprintf(format, v, "KMGT"[exp])
}

func humanDuration(d time.Duration) string {
	switch {
	case d < time.Second:
		return "<1s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func newProgress(tty bool) (*stageProgress, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return &stageProgress{out: buf, forceTTY: &tty}, buf
}

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"}, {512, "512 B"}, {1024, "1.0 KiB"},
		{1536, "1.5 KiB"}, {1 << 20, "1.0 MiB"},
		{150 << 20, "150 MiB"}, // three digits drop the decimal
		{3 * 1 << 30, "3.0 GiB"},
		{7_870_967_808, "7.3 GiB"}, // the Nobara ISO
		{5 << 40, "5.0 TiB"},
	}
	for _, tc := range cases {
		if got := humanBytes(tc.in); got != tc.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestHumanDuration(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{500 * time.Millisecond, "<1s"},
		{45 * time.Second, "45s"},
		{131 * time.Second, "2m11s"},
		{time.Hour + 3*time.Minute, "1h03m"},
	}
	for _, tc := range cases {
		if got := humanDuration(tc.in); got != tc.want {
			t.Errorf("humanDuration(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestPercentNeverLies: a server that under-reports Content-Length, or a
// compressed image that expands past its stated size, would otherwise render a
// bar wider than the bar.
func TestPercentNeverLies(t *testing.T) {
	cases := []struct {
		done, total int64
		want        int
	}{
		{0, 100, 0}, {50, 100, 50}, {100, 100, 100},
		{150, 100, 100}, // over-run clamps
		{-5, 100, 0},    // nonsense clamps
		{50, 0, 0},      // unknown total
		{50, -1, 0},
	}
	for _, tc := range cases {
		if got := percent(tc.done, tc.total); got != tc.want {
			t.Errorf("percent(%d,%d) = %d, want %d", tc.done, tc.total, got, tc.want)
		}
	}
}

// TestTerminalDrawsOneLine: a terminal must not scroll. Every frame is written
// over the same line, so the output contains carriage returns and no newlines
// until the display is closed.
func TestTerminalDrawsOneLine(t *testing.T) {
	p, buf := newProgress(true)
	total := int64(10 << 20)
	for i := int64(0); i <= 10; i++ {
		p.report("downloading Ubuntu", i*total/10, total)
		time.Sleep(2 * time.Millisecond)
	}
	out := buf.String()
	if strings.Contains(out, "\n") {
		t.Errorf("terminal output contains a newline, so it would scroll:\n%q", out)
	}
	if !strings.Contains(out, "\r") {
		t.Error("terminal output never rewrites its line")
	}
	if !strings.Contains(out, "[") || !strings.Contains(out, "#") {
		t.Errorf("no bar was drawn:\n%q", out)
	}
	// The final frame must read 100%, not 90-something: that is the frame left
	// on screen, and a bar stuck at 97% reads as a hang.
	last := out[strings.LastIndex(out, "\r"):]
	if !strings.Contains(last, "100%") {
		t.Errorf("last frame is not 100%%: %q", last)
	}
	p.finish()
	if !strings.HasSuffix(buf.String(), "\r") {
		t.Error("finish did not clear the line")
	}
}

// TestNonTerminalLogsDurableLines: piping to a file or a CI log must produce a
// readable record, not one enormous line full of carriage returns.
func TestNonTerminalLogsDurableLines(t *testing.T) {
	p, buf := newProgress(false)
	total := int64(1 << 30)
	for i := int64(0); i <= 100; i++ {
		p.report("downloading Fedora", i*total/100, total)
	}
	p.finish()
	out := buf.String()
	if strings.Contains(out, "\r") {
		t.Errorf("log output rewrites lines:\n%q", out)
	}
	if !strings.Contains(out, "downloading Fedora...") {
		t.Error("the stage was never announced in the log")
	}
	lines := strings.Count(out, "\n")
	// One stage line plus roughly one per 10%: enough to follow, not a flood.
	if lines < 8 || lines > 16 {
		t.Errorf("got %d log lines, want about 12:\n%s", lines, out)
	}
	if !strings.Contains(out, "100%") {
		t.Error("the log never records completion")
	}
}

// TestStageChangeStartsClean: stages follow one another (download, then build,
// then write), and the previous bar must not be left half-overwritten.
func TestStageChangeStartsClean(t *testing.T) {
	p, buf := newProgress(true)
	p.report("downloading something with a long name", 500, 1000)
	p.report("verifying", 10, 1000)
	out := buf.String()
	if strings.Count(out, "\r") < 2 {
		t.Errorf("the line was not cleared between stages:\n%q", out)
	}
	if !strings.Contains(out, "verifying") {
		t.Error("the new stage was not drawn")
	}
}

// TestIndeterminateShowsMovementNotAFalseBar: some sources report no length.
// Drawing a bar then would mean inventing a denominator.
func TestIndeterminateShowsMovementNotAFalseBar(t *testing.T) {
	p, buf := newProgress(true)
	p.report("resolving download URL", 0, -1)
	if got := buf.String(); !strings.Contains(got, "resolving download URL...") {
		t.Errorf("indeterminate stage not shown: %q", got)
	}
	buf.Reset()
	p.report("downloading", 5<<20, -1)
	out := buf.String()
	if strings.Contains(out, "[") {
		t.Errorf("drew a bar with no known total:\n%q", out)
	}
	if !strings.Contains(out, "5.0 MiB") {
		t.Errorf("did not show how much had moved:\n%q", out)
	}
}

// TestRateAppearsOnlyWhenMeaningful: an estimate produced from two samples a
// millisecond apart is noise, and a wildly wrong "ETA 4h" in the first second
// is worse than showing nothing.
func TestRateAppearsOnlyWhenMeaningful(t *testing.T) {
	p, _ := newProgress(true)
	if got := p.rateSuffix(0, 1000); got != "" {
		t.Errorf("rate shown with no samples: %q", got)
	}
	now := time.Now()
	p.samples = []sample{{now, 0}, {now.Add(100 * time.Millisecond), 1 << 20}}
	if got := p.rateSuffix(1<<20, 10<<20); got != "" {
		t.Errorf("rate shown from a 100ms window: %q", got)
	}
	p.samples = []sample{{now, 0}, {now.Add(2 * time.Second), 20 << 20}}
	got := p.rateSuffix(20<<20, 100<<20)
	if !strings.Contains(got, "/s") {
		t.Errorf("no rate over a 2s window: %q", got)
	}
	if !strings.Contains(got, "ETA") {
		t.Errorf("no estimate when the total is known: %q", got)
	}
}

// TestFinishIsSafeToRepeat: callers defer finish and several also call it on
// the error path.
func TestFinishIsSafeToRepeat(t *testing.T) {
	p, _ := newProgress(true)
	p.finish()
	p.report("working", 1, 2)
	p.finish()
	p.finish()
}

// TestLineNeverWraps: a stage name can be long (a driver pack id, an asset
// filename) and a wrapped bar leaves debris on screen every frame.
//
// Measured in runes, because that is what a terminal column is and what Go's
// width-padded %s counts. Measuring bytes would fail on the ellipsis alone.
func TestLineNeverWraps(t *testing.T) {
	p, buf := newProgress(true)
	for _, stage := range []string{
		strings.Repeat("a very long stage name ", 10),
		"downloading Fedora-Workstation-Live-44-1.7.x86_64.iso from dl.fedoraproject.org",
		"downloading Nobara — codecs, drivers and gaming tweaks — 44 (2026-09-02)",
	} {
		buf.Reset()
		p.report(stage, 1, 2)
		for _, frame := range strings.Split(buf.String(), "\r") {
			if n := len([]rune(frame)); n > lineWidth {
				t.Errorf("frame is %d columns, over the %d budget: %q", n, lineWidth, frame)
			}
		}
	}
}

// TestTruncationKeepsCharactersWhole: cutting a multi-byte character in half
// emits a broken sequence that renders as a replacement glyph or as nothing.
func TestTruncationKeepsCharactersWhole(t *testing.T) {
	p, buf := newProgress(true)
	p.report(strings.Repeat("é", 200), 1, 2)
	out := buf.String()
	if strings.ContainsRune(out, '�') {
		t.Errorf("truncation split a character:\n%q", out)
	}
	if !strings.HasSuffix(strings.TrimRight(out, " "), "…") {
		t.Errorf("an over-long line was not marked as truncated:\n%q", out)
	}
}

// A note from the download arrives as its own stage, between progress reports
// on the one it interrupts. What matters is that it appears, that the download
// carries on afterwards, and that nothing is left half-drawn.
func TestDownloadNoteReadsAsItsOwnStage(t *testing.T) {
	p, buf := newProgress(false)
	p.report("download", 1<<20, 8<<20)
	p.report("mirrors.edge.kernel.org stopped sending (unexpected EOF); trying it again in 2s", 0, -1)
	p.report("download", 2<<20, 8<<20)
	p.finish()

	out := buf.String()
	for _, want := range []string{"download...", "trying it again in 2s", "8.0 MiB"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// The download is announced again after the interruption rather than
	// silently resuming under the note's heading.
	if strings.Count(out, "download...") != 2 {
		t.Errorf("download announced %d times, want 2:\n%s", strings.Count(out, "download..."), out)
	}
	if strings.Contains(out, "\r") {
		t.Errorf("carriage returns in a non-tty log:\n%q", out)
	}
}

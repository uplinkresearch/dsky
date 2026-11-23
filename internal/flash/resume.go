package flash

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"syscall"
	"time"

	"github.com/uplinkresearch/dsky/internal/compose"
	"github.com/uplinkresearch/dsky/internal/device"
	"github.com/uplinkresearch/dsky/internal/stream"
)

// A stick that drops off the bus partway through a write used to end the run
// with whatever the OS said about it, which on Windows is "A device which does
// not exist was specified" -- true, and no help to anybody holding the stick.
// The stick was usually back a second later. Cheap and small sticks reset
// themselves when they get hot under a long write, and front ports and tired
// hubs drop them when the power sags.
//
// So a run now rides it out: it waits for the stick, makes sure the stick that
// came back is the one it was writing, backs up a little and carries on, more
// gently. It gives up after a few of these, because a stick that keeps leaving
// is not one to install from, and the readback at the end still decides
// whether what is on it is right. Nothing here weakens that check; it only
// stops one reset from throwing away twenty minutes of writing.

// These are variables so the tests can run the whole thing against a fake
// device in milliseconds rather than minutes. openTarget, the other seam, is
// in clone.go.
var (
	listDevices = device.List

	// waitForStick is how long a run waits for a stick that left to come back.
	// A reset takes a second or two; a minute covers a slow re-enumeration
	// without leaving somebody staring at a run that is never going to finish.
	waitForStick = 60 * time.Second
	pollEvery    = time.Second

	// maxDrops is the disconnect that ends the run. The ones before it are
	// waited out.
	maxDrops = 3

	// rewind is how far before the failed write a resumed run starts again.
	// A write the stick acknowledged just before it reset is not one to take
	// its word for.
	rewind int64 = 64 << 20

	// A write is slow enough to slow down for when it takes more than
	// slowFloor and more than slowFactor times the run's usual, once the run
	// has a usual. The floor keeps an ordinary hiccup from counting.
	slowFloor  = 5 * time.Second
	slowFactor = 8
	slowAfter  = 8 // chunks written before "usual" means anything

	// Gently means smaller requests with a breath between them. It costs a
	// couple of minutes on a Windows image, which is cheap next to starting
	// over.
	gentlePiece = 1 << 20
	gentlePause = 20 * time.Millisecond
)

// isGone reports an error that means the disk is no longer there, as opposed
// to the disk refusing the request.
func isGone(err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	for _, g := range goneErrnos {
		if errno == g {
			return true
		}
	}
	return false
}

// goneError is a write or read that found the stick missing. It is caught by
// the run and waited out; it reaches the user only if the run gives up.
type goneError struct {
	at  int64
	err error
}

func (g *goneError) Error() string {
	return fmt.Sprintf("flash: stick disconnected at %d MiB: %v", g.at>>20, g.err)
}
func (g *goneError) Unwrap() error { return g.err }

// run is one stick's write and readback, with what it needs to pick itself up
// after the stick resets.
type run struct {
	ctx      context.Context
	dev      device.Device
	t        Target
	devSize  int64
	total    int64 // the image size for progress; -1 while unknown
	progress Progress

	drops int

	// head is what the write is called on screen; detail is the line under
	// it. They travel as one stage string, "head\ndetail": the page shows the
	// detail where its patter would be, and the terminal shows the head.
	head, detail string
	gentle       bool
	// redoUntil is where the stick left, while the stretch before it is
	// being written again; zero otherwise.
	redoUntil int64

	// usual is the run's typical time for one chunk, once it has one.
	usual  time.Duration
	chunks int
}

func (r *run) stage() string {
	if r.detail == "" {
		return r.head
	}
	return r.head + "\n" + r.detail
}

// write streams the image onto the stick from the start, and again from a
// little before wherever the stick left, until it is all on or the run gives
// up. It returns how much was written and the hash of exactly those bytes.
func (r *run) write(art *compose.Artifact) (int64, string, error) {
	r.head = "write"
	var start int64
	for {
		written, sum, err := r.writeFrom(art, start)
		if err == nil {
			// A flush is the stick being asked to finish what it was given,
			// which is when a stick that is about to reset most often does.
			err = r.t.Sync()
			if err == nil {
				return written, sum, nil
			}
			if !isGone(err) {
				return written, "", fmt.Errorf("flash: flushing device: %w", err)
			}
			err = &goneError{at: written, err: err}
		}
		var gone *goneError
		if !errors.As(err, &gone) {
			return written, "", err
		}
		if err := r.reconnect("", gone.at, r.total); err != nil {
			return written, "", err
		}
		start = gone.at - rewind
		if start < 0 {
			start = 0
		}
		start -= start % chunkSize
		r.gentle = true
		r.redoUntil = gone.at
		r.head = fmt.Sprintf("write · same stick is back · resuming from %s MiB, gently", commas(start>>20))
		redo := "Starting again from the beginning."
		if gone.at > start {
			redo = fmt.Sprintf("Rewriting the last %d MiB in case it never landed.", (gone.at-start)>>20)
		}
		r.detail = fmt.Sprintf("%s Disconnects so far: %d of %d.", redo, r.drops, maxDrops)
	}
}

// writeFrom writes the image from offset start to its end. The hash covers the
// whole image, so the part before start is read again and hashed without being
// written: a compressed image cannot be sought, and the bytes on the stick
// before start are the ones already there.
func (r *run) writeFrom(art *compose.Artifact, start int64) (int64, string, error) {
	reader, err := stream.Open(art.Path, art.Compress)
	if err != nil {
		return start, "", fmt.Errorf("flash: %w", err)
	}
	defer reader.Close()
	hash := sha256.New()
	if start > 0 {
		if _, err := io.CopyN(hash, reader, start); err != nil {
			return start, "", fmt.Errorf("flash: reading the image again to resume: %w", err)
		}
	}
	if start > 0 || r.drops > 0 {
		r.progress(r.stage(), start, r.total)
	}
	buf := alignedBuffer(chunkSize)
	written := start
	for {
		if err := r.ctx.Err(); err != nil {
			return written, "", fmt.Errorf("flash: canceled after %d MiB: %w", written>>20, err)
		}
		n, rerr := io.ReadFull(reader, buf)
		if n > 0 {
			chunk := buf[:n]
			if pad := (SectorSize - n%SectorSize) % SectorSize; pad != 0 {
				chunk = buf[:n+pad]
				for i := n; i < n+pad; i++ {
					chunk[i] = 0
				}
			}
			if written+int64(len(chunk)) > r.devSize {
				return written, "", fmt.Errorf("flash: image is larger than %s (%d MiB) — wrong device or truncated stick", r.dev.ID, r.devSize>>20)
			}
			if werr := r.put(chunk, written); werr != nil {
				if isGone(werr) {
					return written, "", &goneError{at: written, err: werr}
				}
				return written, "", fmt.Errorf("flash: write failed at %d MiB: %w%s", written>>20, werr, writeRefusedHint(werr))
			}
			hash.Write(chunk)
			written += int64(len(chunk))
			// Once the stretch written before the stick left has been
			// written again, the run is simply writing, gently.
			if r.redoUntil > 0 && written > r.redoUntil {
				r.redoUntil = 0
				r.head = "write · writing gently"
				r.detail = fmt.Sprintf("The stick disconnected %s, so the rest goes on in smaller pieces. Disconnects so far: %d of %d.",
					times(r.drops), r.drops, maxDrops)
			}
			r.progress(r.stage(), written, r.total)
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			return written, hex.EncodeToString(hash.Sum(nil)), nil
		}
		if rerr != nil {
			return written, "", fmt.Errorf("flash: reading artifact: %w", rerr)
		}
	}
}

// put writes one chunk: whole and timed ordinarily, in small pieces with a
// pause between them once the run is being gentle.
func (r *run) put(p []byte, off int64) error {
	if !r.gentle {
		t0 := time.Now()
		if _, err := r.t.WriteAt(p, off); err != nil {
			return err
		}
		r.timed(time.Since(t0))
		return nil
	}
	for len(p) > 0 {
		// Every piece starts a whole number of mebibytes into a buffer that
		// is page aligned, so the unbuffered Windows handle takes each one.
		n := min(len(p), gentlePiece)
		if _, err := r.t.WriteAt(p[:n], off); err != nil {
			return err
		}
		p, off = p[n:], off+int64(n)
		select {
		case <-r.ctx.Done():
			return r.ctx.Err()
		case <-time.After(gentlePause):
		}
	}
	return nil
}

// timed notices a stick that has started to struggle. A stick that is
// overheating usually slows right down before it resets; easing off then is
// the one chance there is to keep it from resetting at all.
func (r *run) timed(d time.Duration) {
	r.chunks++
	if r.chunks > slowAfter && d > slowFloor && d > time.Duration(slowFactor)*r.usual {
		r.gentle = true
		r.head = "write · stick slowing down, writing gently from here"
		r.detail = fmt.Sprintf("Stick took %s for one piece (usually %s). Smaller pieces, short pauses.",
			roundDur(d), roundDur(r.usual))
		return
	}
	if r.usual == 0 {
		r.usual = d
	} else {
		r.usual = (r.usual*7 + d) / 8
	}
}

// verify reads back what was written and compares its hash, waiting out a
// stick that leaves part way through. A read needs no rewind: what is read
// after the stick returns is read from the stick, which is the whole point.
func (r *run) verify(written int64, want string) error {
	r.progress("verify", 0, written)
	buf := alignedBuffer(chunkSize)
	vhash := sha256.New()
	stage := "verify"
	var read int64
	for read < written {
		if err := r.ctx.Err(); err != nil {
			return err
		}
		n := int64(chunkSize)
		if read+n > written {
			n = written - read
		}
		if _, err := r.t.ReadAt(buf[:n], read); err != nil {
			if !isGone(err) {
				return fmt.Errorf("flash: readback failed at %d MiB: %w", read>>20, err)
			}
			if err := r.reconnect("verify · ", read, written); err != nil {
				return err
			}
			stage = fmt.Sprintf("verify\nThe same stick is back; reading on from %s MiB. Disconnects so far: %d of %d.", commas(read>>20), r.drops, maxDrops)
			continue
		}
		vhash.Write(buf[:n])
		read += n
		r.progress(stage, read, written)
	}
	if hex.EncodeToString(vhash.Sum(nil)) != want {
		return fmt.Errorf("flash: VERIFY FAILED on %s — device returned different data than written (failing or counterfeit stick). Do not use this stick", r.dev.ID)
	}
	return nil
}

// reconnect lets go of a stick that has left, waits for the same stick to come
// back, and opens it again. prefix keeps the stage saying which half of the
// run it interrupted; done and total keep the bar where it was.
func (r *run) reconnect(prefix string, done, total int64) error {
	r.drops++
	if r.t != nil {
		// Holding the handle of a device that has gone can keep Windows from
		// tearing it down, and then it never comes back under a new one.
		r.t.Close()
		r.t = nil
	}
	if r.drops >= maxDrops {
		return fmt.Errorf("the stick disconnected %d times, so DSKY stopped. Replace this stick: one that keeps dropping out can't be trusted for an install. Try another stick, in a port on the back of the PC", r.drops)
	}
	// Disk numbers and device names are handed out again on arrival, so the
	// only way to know the stick that came back is this one is to ask it. A
	// stick with no serial number cannot answer, and writing the rest of an
	// image onto a disk on a guess is how somebody loses a different disk.
	if r.dev.Serial == "" {
		return fmt.Errorf("the stick disconnected at %d MiB. It reports no serial number, so DSKY can't be sure a stick that comes back is the same one, and won't write to it. Try another stick, in a port on the back of the PC", done>>20)
	}

	t0 := time.Now()
	var lastErr error
	for {
		r.progress(fmt.Sprintf("%sstick disconnected · waiting for it to come back (%d of %d) · %s\nNothing will be written until the same stick (serial and size) is back.",
			prefix, r.drops, maxDrops, clock(time.Since(t0))), done, total)
		if dev, ok := r.sameStick(); ok {
			if err := r.reopen(dev); err == nil {
				return nil
			} else {
				lastErr = err
			}
		}
		if time.Since(t0) >= waitForStick {
			break
		}
		select {
		case <-r.ctx.Done():
			return fmt.Errorf("flash: canceled while waiting for the stick: %w", r.ctx.Err())
		case <-time.After(pollEvery):
		}
	}
	msg := fmt.Sprintf("the stick disconnected at %d MiB and did not come back within %s. Check it is still plugged in, then try another stick, in a port on the back of the PC", done>>20, roundDur(waitForStick))
	if lastErr != nil {
		msg += fmt.Sprintf(" (it came back but could not be opened: %v)", lastErr)
	}
	return errors.New(msg)
}

// sameStick looks for the stick this run was writing: same serial number, same
// bus, the same size give or take the enumerator's rounding, and only one of
// it. Two matches means two sticks DSKY cannot tell apart, and it waits rather
// than pick one.
func (r *run) sameStick() (device.Device, bool) {
	devs, err := listDevices(r.ctx)
	if err != nil {
		return device.Device{}, false // the bus is still settling
	}
	var found []device.Device
	for _, d := range devs {
		if d.Serial == r.dev.Serial && d.Bus == r.dev.Bus && within(d.SizeBytes, r.dev.SizeBytes, enumTolerance) {
			found = append(found, d)
		}
	}
	if len(found) != 1 {
		return device.Device{}, false
	}
	return found[0], true
}

// reopen opens the stick that came back, under the same policy the run started
// with, and checks it is the size the run was writing to.
func (r *run) reopen(dev device.Device) error {
	if err := CheckTarget(dev, false); err != nil {
		return err
	}
	t, err := openTarget(r.ctx, dev)
	if err != nil {
		return err
	}
	if size, err := t.Size(); err == nil && size > 0 && size != r.devSize {
		t.Close()
		return fmt.Errorf("%s now reports %d bytes, not %d", dev.ID, size, r.devSize)
	}
	r.t, r.dev = t, dev
	return nil
}

func within(a, b, tol int64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d <= tol
}

func times(n int) string {
	if n == 1 {
		return "once"
	}
	return fmt.Sprintf("%d times", n)
}

func clock(d time.Duration) string {
	s := int(d.Seconds())
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

func roundDur(d time.Duration) string {
	if d >= time.Second {
		return d.Round(time.Second).String()
	}
	return d.Round(10 * time.Millisecond).String()
}

func commas(n int64) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

package flash

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uplinkresearch/dsky/internal/compose"
	"github.com/uplinkresearch/dsky/internal/device"
)

// resetStick is a USB stick that leaves the bus at chosen offsets and comes
// back, the way the SanDisk in the field did. Each open is a fresh handle;
// once a handle has seen the stick leave, it is dead for good, as a Windows
// handle to a removed device is.
type resetStick struct {
	mu  sync.Mutex
	buf []byte
	// dropWrites and dropReads are offsets at which the stick resets, each
	// used once, in order.
	dropWrites, dropReads []int64
	opens                 int
	pieces                []int // size of every write, to see gentle mode
	// slowAt makes the write of the chunk at this offset take slowFor.
	slowAt  int64
	slowFor time.Duration
}

type resetHandle struct {
	s    *resetStick
	dead bool
}

func goneNow() error {
	return &os.PathError{Op: "write", Path: "fake", Err: goneErrnos[0]}
}

func (h *resetHandle) WriteAt(p []byte, off int64) (int, error) {
	s := h.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if h.dead {
		return 0, goneNow()
	}
	if len(s.dropWrites) > 0 && off <= s.dropWrites[0] && s.dropWrites[0] < off+int64(len(p)) {
		at := s.dropWrites[0]
		s.dropWrites = s.dropWrites[1:]
		h.dead = true
		// The last megabyte it acknowledged before it went never landed.
		// A resume that did not back up would leave this hole for the
		// readback to find.
		lost := max(at-1<<20, 0)
		clear(s.buf[lost:at])
		return 0, goneNow()
	}
	if s.slowAt > 0 && off == s.slowAt {
		time.Sleep(s.slowFor)
	}
	s.pieces = append(s.pieces, len(p))
	return copy(s.buf[off:], p), nil
}

func (h *resetHandle) ReadAt(p []byte, off int64) (int, error) {
	s := h.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if h.dead {
		return 0, goneNow()
	}
	if len(s.dropReads) > 0 && off <= s.dropReads[0] && s.dropReads[0] < off+int64(len(p)) {
		s.dropReads = s.dropReads[1:]
		h.dead = true
		return 0, goneNow()
	}
	return copy(p, s.buf[off:]), nil
}

func (h *resetHandle) Size() (int64, error) { return int64(len(h.s.buf)), nil }
func (h *resetHandle) Sync() error          { return nil }
func (h *resetHandle) Finalize() error      { return nil }
func (h *resetHandle) Close() error         { return nil }

// rig wires a resetStick in as the only disk, with the waits shrunk to
// milliseconds. listed is what enumeration returns while the stick is back;
// it defaults to the same stick under a new disk number, which is what
// Windows hands out on arrival.
type rig struct {
	stick  *resetStick
	dev    device.Device
	listed []device.Device
	art    *compose.Artifact
	image  []byte
	stages []string
}

func newRig(t *testing.T, imageMiB int) *rig {
	t.Helper()
	img := make([]byte, imageMiB<<20)
	for i := range img {
		img[i] = byte(i/SectorSize%251 + 1)
	}
	path := filepath.Join(t.TempDir(), "img.raw")
	if err := os.WriteFile(path, img, 0o644); err != nil {
		t.Fatal(err)
	}
	r := &rig{
		stick: &resetStick{buf: make([]byte, 64<<20)},
		dev: device.Device{ID: `\\.\PhysicalDrive1`, Bus: "usb", Removable: true,
			Serial: "4C530001", SizeBytes: 64 << 20},
		art:   &compose.Artifact{Path: path, Size: int64(len(img)), Verify: "readback-sha256"},
		image: img,
	}
	back := r.dev
	back.ID = `\\.\PhysicalDrive2`
	r.listed = []device.Device{back}

	saved := []any{openTarget, listDevices, waitForStick, pollEvery, rewind, gentlePause, slowFloor, slowAfter, slowFactor}
	t.Cleanup(func() {
		openTarget = saved[0].(func(context.Context, device.Device) (Target, error))
		listDevices = saved[1].(func(context.Context) ([]device.Device, error))
		waitForStick, pollEvery = saved[2].(time.Duration), saved[3].(time.Duration)
		rewind, gentlePause = saved[4].(int64), saved[5].(time.Duration)
		slowFloor, slowAfter, slowFactor = saved[6].(time.Duration), saved[7].(int), saved[8].(int)
	})
	openTarget = func(_ context.Context, d device.Device) (Target, error) {
		if d.Serial != r.dev.Serial {
			return nil, fmt.Errorf("opened %s (serial %q), which is not the stick", d.ID, d.Serial)
		}
		r.stick.opens++
		return &resetHandle{s: r.stick}, nil
	}
	listDevices = func(context.Context) ([]device.Device, error) { return r.listed, nil }
	waitForStick, pollEvery = 50*time.Millisecond, time.Millisecond
	rewind, gentlePause = 2<<20, 0
	return r
}

func (r *rig) flash() error {
	return Flash(context.Background(), r.art, r.dev, func(stage string, _, _ int64) {
		if len(r.stages) == 0 || r.stages[len(r.stages)-1] != stage {
			r.stages = append(r.stages, stage)
		}
	})
}

func (r *rig) said(sub string) bool {
	for _, s := range r.stages {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// The field failure, twice over: the stick resets once while being written
// and once while being read back. Both are waited out, the stick is found
// again under its new disk number, and what ends up on it is the image --
// including the megabyte the stick acknowledged and then lost.
func TestFlashRidesOutAStickThatResets(t *testing.T) {
	r := newRig(t, 24)
	r.stick.dropWrites = []int64{9 << 20}
	r.stick.dropReads = []int64{13 << 20}

	if err := r.flash(); err != nil {
		t.Fatalf("flash: %v\nstages: %q", err, r.stages)
	}
	if !bytes.Equal(r.stick.buf[:len(r.image)], r.image) {
		t.Error("the stick does not hold the image after resuming")
	}
	if r.stick.opens != 3 {
		t.Errorf("opened the stick %d times, want 3 (start, after each reset)", r.stick.opens)
	}
	for _, want := range []string{
		"waiting for it to come back (1 of 3)",
		"resuming from 4 MiB, gently",
		"verify · stick disconnected · waiting for it to come back (2 of 3)",
		"Nothing will be written until the same stick",
	} {
		if !r.said(want) {
			t.Errorf("never said %q; said %q", want, r.stages)
		}
	}
}

// A stick that keeps leaving is not one to install from: the third time ends
// the run, in words somebody holding the stick can act on.
func TestFlashStopsAtTheThirdDisconnect(t *testing.T) {
	r := newRig(t, 24)
	r.stick.dropWrites = []int64{5 << 20, 9 << 20, 13 << 20}
	err := r.flash()
	if err == nil || !strings.Contains(err.Error(), "the stick disconnected 3 times, so DSKY stopped") {
		t.Fatalf("got %v, want the stick-disconnected-3-times stop", err)
	}
}

// The safety property: nothing is written to a disk that cannot be shown to
// be the same stick. A different serial number, or two sticks that both
// match, is waited on and then given up on -- never opened.
func TestFlashNeverResumesOntoAnotherDisk(t *testing.T) {
	for name, listed := range map[string][]device.Device{
		"different serial": {{ID: `\\.\PhysicalDrive2`, Bus: "usb", Serial: "OTHER", SizeBytes: 64 << 20}},
		"two that match": {
			{ID: `\\.\PhysicalDrive2`, Bus: "usb", Serial: "4C530001", SizeBytes: 64 << 20},
			{ID: `\\.\PhysicalDrive3`, Bus: "usb", Serial: "4C530001", SizeBytes: 64 << 20},
		},
		"different size": {{ID: `\\.\PhysicalDrive2`, Bus: "usb", Serial: "4C530001", SizeBytes: 256 << 20}},
		"system disk":    {{ID: `\\.\PhysicalDrive0`, Bus: "usb", Serial: "4C530001", SizeBytes: 64 << 20, System: true}},
		"gone for good":  nil,
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, 24)
			r.listed = listed
			r.stick.dropWrites = []int64{9 << 20}
			err := r.flash()
			if err == nil || !strings.Contains(err.Error(), "did not come back") {
				t.Fatalf("got %v, want a did-not-come-back failure", err)
			}
			if r.stick.opens != 1 {
				t.Errorf("opened a disk %d times after the stick left", r.stick.opens-1)
			}
		})
	}
}

// Without a serial number there is no telling the stick that comes back from
// any other, so the run stops at once rather than waiting to guess.
func TestFlashWithoutASerialDoesNotResume(t *testing.T) {
	r := newRig(t, 24)
	r.dev.Serial = ""
	r.listed[0].Serial = ""
	r.stick.dropWrites = []int64{9 << 20}
	err := r.flash()
	if err == nil || !strings.Contains(err.Error(), "no serial number") {
		t.Fatalf("got %v, want the no-serial stop", err)
	}
}

// A stick that suddenly takes far longer than usual to take a chunk is
// written gently from then on: in small pieces, and the screen says so.
func TestFlashSlowsDownForAStrugglingStick(t *testing.T) {
	r := newRig(t, 48)
	slowFloor, slowAfter, slowFactor = 20*time.Millisecond, 2, 2
	r.stick.slowAt, r.stick.slowFor = 20<<20, 80*time.Millisecond

	if err := r.flash(); err != nil {
		t.Fatalf("flash: %v", err)
	}
	if !r.said("stick slowing down, writing gently from here") {
		t.Errorf("never said it was slowing down; said %q", r.stages)
	}
	last := r.stick.pieces[len(r.stick.pieces)-1]
	if last > gentlePiece {
		t.Errorf("still writing %d-byte pieces after slowing down", last)
	}
	if !bytes.Equal(r.stick.buf[:len(r.image)], r.image) {
		t.Error("the stick does not hold the image")
	}
}

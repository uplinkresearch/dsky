// Package flash writes artifacts to raw USB devices: aligned chunked
// writes, streaming decompression, a stale-GPT tail wipe, and readback
// verification. Safety gates: only device.Flashable() targets, size checks,
// and the caller-supplied typed confirmation upstream.
package flash

import (
	"context"
	"errors"
	"fmt"
	"io"
	"unsafe"

	"github.com/uplinkresearch/dsky/internal/compose"
	"github.com/uplinkresearch/dsky/internal/device"
	"github.com/uplinkresearch/dsky/internal/stream"
)

// clearForWrite removes the partition table of a disk about to be overwritten,
// where the platform needs that before raw writes are allowed (Windows; see
// winTarget.ClearLayout). Never call it on a disk being read.
func clearForWrite(t Target) {
	if c, ok := t.(interface{ ClearLayout() }); ok {
		c.ClearLayout()
	}
}

// SectorSize is the only supported logical sector size; Target opens verify
// this and refuse 4Kn devices with a clear error.
const SectorSize = 512

// chunkSize is the aligned write unit.
const chunkSize = 4 << 20

// tailWipe is how much is zeroed at the end of the device (stale GPT backup
// headers from the stick's previous life confuse firmware).
const tailWipe = 2 << 20

// alignedBuffer returns a buffer whose first byte sits on a 4096-byte
// boundary. Windows opens a raw disk unbuffered, and an unbuffered write has
// to come from memory aligned to the sector size; Go's heap promises nothing
// of the sort. Over-allocating and slicing forward costs one page and makes
// every write legal wherever it lands.
func alignedBuffer(n int) []byte {
	raw := make([]byte, n+4096)
	skip := (4096 - int(uintptr(unsafe.Pointer(&raw[0]))%4096)) % 4096
	return raw[skip : skip+n]
}

// wipeTail zeroes the end of the device in aligned pieces.
//
// In pieces because a raw device handle is not a file: Windows opens it
// unbuffered, where every request has to be a whole number of sectors, and
// drivers differ in how large a single request they will take. A megabyte at
// a time is comfortable for all of them and still only two writes.
func wipeTail(t Target, devSize int64) error {
	// A megabyte at a time, and on refusal the same ground again in
	// sector-sized pieces: USB storage drivers cap how much they take in one
	// request, and the cap is not something the device advertises.
	for _, piece := range []int64{1 << 20, 64 << 10, SectorSize} {
		err := wipeRange(t, devSize-tailWipe, devSize, piece)
		if err == nil {
			return nil
		}
		if piece == SectorSize {
			return err
		}
	}
	return nil
}

func wipeRange(t Target, from, to, piece int64) error {
	zeros := alignedBuffer(int(piece))
	for off := from; off < to; off += piece {
		n := piece
		if rest := to - off; rest < n {
			n = rest
		}
		if _, err := t.WriteAt(zeros[:n], off); err != nil {
			return fmt.Errorf("%d bytes at offset %d of %d: %w", n, off, to, err)
		}
	}
	return nil
}

// writeRefusedHint explains a device that took no data at all.
//
// A write that reports success having moved nothing is what Windows returns
// for a device that is not ready, and Go turns it into an unexpected end of
// file -- which reads like a problem with the image rather than the stick.
// One was traced to a counterfeit drive claiming 58 GB while holding a
// fraction of that: a plain PowerShell write to it failed the same way, with
// DSKY nowhere near it.
func writeRefusedHint(err error) string {
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		return ""
	}
	return " — the device accepted no data. That is usually a stick that is" +
		" failing, counterfeit (claiming more space than it has), or was" +
		" pulled out. Try another stick, and a port on the machine itself" +
		" rather than a hub."
}

// enumTolerance is how far the enumerator's idea of a disk's size may sit from
// the size the open device reports before they are taken to be different disks.
const enumTolerance = 64 << 20

// Progress receives stage updates; total is -1 while unknown (compressed
// sources).
type Progress func(stage string, done, total int64)

// Target is an open raw device.
type Target interface {
	Size() (int64, error)
	WriteAt(p []byte, off int64) (int, error)
	ReadAt(p []byte, off int64) (int, error)
	Sync() error
	// Finalize refreshes the OS's view of the device after writing.
	Finalize() error
	Close() error
}

// ErrFixedDisk is returned when a target is writable but is not a removable
// stick, and the caller has not said it means to write to a fixed disk.
var ErrFixedDisk = errors.New("target is a fixed disk")

// CheckTarget is the write policy in one place.
//
// The system disk is refused always and unconditionally. A fixed disk is
// allowed only when the caller passes allowFixed, which exists so that
// permission has to be carried explicitly from the place that actually asked a
// human, rather than being the default anything inherits. Every caller written
// before fixed disks were allowed passes false and keeps its old behaviour.
func CheckTarget(dev device.Device, allowFixed bool) error {
	if !dev.Writable() {
		return fmt.Errorf("refusing %s: it hosts the running OS", dev.ID)
	}
	if !dev.Routine() && !allowFixed {
		return fmt.Errorf("refusing %s: %w (bus=%s, removable=%v) — this needs an explicit confirmation the caller did not give",
			dev.ID, ErrFixedDisk, dev.Bus, dev.Removable)
	}
	return nil
}

// Flash writes the artifact to dev and verifies by readback unless the
// artifact says verify none. progress may be nil.
func Flash(ctx context.Context, art *compose.Artifact, dev device.Device, progress Progress) error {
	if progress == nil {
		progress = func(string, int64, int64) {}
	}
	if err := CheckTarget(dev, false); err != nil {
		return err
	}
	if art.MinStick > 0 && dev.SizeBytes > 0 && dev.SizeBytes < art.MinStick {
		return fmt.Errorf("flash: %s is %d MiB, recipe requires at least %d MiB", dev.ID, dev.SizeBytes>>20, art.MinStick>>20)
	}

	// Opened here only to fail before the stick is touched if the image is
	// missing or unreadable; the write opens it again for itself, and again
	// if it has to resume.
	reader, err := stream.Open(art.Path, art.Compress)
	if err != nil {
		return fmt.Errorf("flash: %w", err)
	}
	reader.Close()
	total := art.Size
	if art.Compress != "" && art.Compress != "none" {
		total = -1
	}

	progress("open", 0, total)
	t, err := openTarget(ctx, dev)
	if err != nil {
		return err
	}
	// The run may swap t for the same stick reopened after a reset, so the
	// handle closed at the end is whichever one it holds by then.
	r := &run{ctx: ctx, dev: dev, t: t, total: total, progress: progress}
	defer func() {
		if r.t != nil {
			r.t.Close()
		}
	}()
	// The open device's own size report is authoritative where available
	// (Windows WMI enumeration under-reports by CHS-geometry rounding, up
	// to a few MiB). The enumerator's size still cross-checks that the
	// same physical device is attached: a swapped stick differs by far
	// more than one cylinder.
	devSize := dev.SizeBytes
	if tSize, err := t.Size(); err == nil && tSize > 0 {
		switch {
		case devSize == 0:
			devSize = tSize
		case tSize < devSize || tSize-devSize > enumTolerance:
			return fmt.Errorf("flash: %s size mismatch: enumeration saw %d, device reports %d — device changed since listing; replug and re-run `dsky devices`", dev.ID, devSize, tSize)
		default:
			devSize = tSize
		}
	} else if devSize == 0 {
		return fmt.Errorf("flash: cannot determine size of %s", dev.ID)
	}
	if devSize%SectorSize != 0 {
		return fmt.Errorf("flash: %s reports size %d, not a multiple of %d", dev.ID, devSize, SectorSize)
	}
	if total > 0 && total > devSize {
		return fmt.Errorf("flash: artifact is %d MiB but %s holds only %d MiB", total>>20, dev.ID, devSize>>20)
	}

	// ── Tail wipe (stale GPT backup headers) ─────────────────────────────
	// Done BEFORE the image write: once the new partition table lands,
	// Windows re-enumerates the disk and blocks late raw writes near the
	// end ("Incorrect function" — found on the first physical stick). At
	// this point the old table is still in place and every old volume is
	// locked, so end-of-disk writes are permitted. The image write may
	// later overlap this region harmlessly.
	//
	// Also before deleting the partition table, not after: on Windows a
	// build stopped here with "unexpected EOF" writing the last two
	// megabytes, straight after IOCTL_DISK_DELETE_DRIVE_LAYOUT. Deleting
	// the layout makes Windows re-read the disk, and a write issued into
	// that moment is refused. Nothing needs the table gone first: the wipe
	// only zeroes bytes.
	// Best effort, and deliberately not fatal. The wipe removes a stale
	// partition table from the stick's previous life, which is a kindness to
	// the firmware that boots it next -- the image about to be written
	// provides the table that matters. A Windows machine refuses these
	// writes (unexplained as yet; the device reports the size being written
	// to, and the offsets are sector-aligned), and refusing to write the
	// stick at all over a defensive cleanup is the wrong trade.
	if err := wipeTail(t, devSize); err != nil {
		progress("could not clear the end of the stick, continuing: "+err.Error(), 0, -1)
	}

	clearForWrite(t)

	// ── Write, then verify ───────────────────────────────────────────────
	// Both ride out a stick that resets partway through; see resume.go.
	r.devSize = devSize
	written, wantHash, err := r.write(art)
	if err != nil {
		return err
	}
	if written == 0 {
		return fmt.Errorf("flash: artifact was empty")
	}
	if art.Verify != "none" {
		if err := r.verify(written, wantHash); err != nil {
			return err
		}
	}

	if err := r.t.Finalize(); err != nil {
		return fmt.Errorf("flash: finalizing device: %w", err)
	}
	progress("done", written, written)
	return nil
}

package flash

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/uplinkresearch/dsky/internal/device"
)

type unixTarget struct {
	f *os.File
}

// OpenTarget unmounts every mounted partition of the device, then opens it
// O_RDWR|O_EXCL — the kernel refuses O_EXCL while anything on the disk is
// still mounted, a free interlock.
func OpenTarget(ctx context.Context, dev device.Device) (Target, error) {
	if err := unmountAll(ctx, dev.ID); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(dev.ID, os.O_RDWR|unix.O_EXCL, 0)
	if err != nil {
		if os.IsPermission(err) {
			return nil, ErrNeedsElevation
		}
		return nil, fmt.Errorf("flash: opening %s (still mounted?): %w", dev.ID, err)
	}
	return &unixTarget{f: f}, nil
}

func unmountAll(ctx context.Context, devPath string) error {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 || !strings.HasPrefix(fields[0], devPath) {
			continue
		}
		if out, err := exec.CommandContext(ctx, "umount", fields[1]).CombinedOutput(); err != nil {
			return fmt.Errorf("flash: unmounting %s: %v\n%s", fields[1], err, out)
		}
	}
	return sc.Err()
}

func (t *unixTarget) Size() (int64, error) {
	return t.f.Seek(0, 2)
}

func (t *unixTarget) WriteAt(p []byte, off int64) (int, error) { return t.f.WriteAt(p, off) }
func (t *unixTarget) ReadAt(p []byte, off int64) (int, error)  { return t.f.ReadAt(p, off) }

// Sync writes everything out and then throws away what Linux remembers of the
// device, because the readback verify that follows reads through this same
// descriptor.
//
// The block node is opened buffered -- unlike Windows, which sets
// FILE_FLAG_NO_BUFFERING, and macOS, which opens /dev/rdiskN -- and both of
// those say in their own comments that they do it so the verify reads the
// stick rather than the memory of having written to it. Linux had neither. On
// a machine with more RAM than the image is large, which is every machine that
// matters here, a 6 GiB write stays resident, fsync leaves those pages clean
// and valid, and the verify hashes them straight back out of RAM. It would
// have passed a counterfeit stick that stored nothing at all -- the exact
// failure the verify exists to catch, reported as "written and verified".
//
// BLKFLSBUF is what `blockdev --flushbufs` does: flush, then invalidate. After
// it the verify has to go to the device. It needs the elevation flashing
// already has, and it only applies to block devices -- an image file written
// as a target has no device lying underneath it to disagree, so there the file
// is the truth and there is nothing to invalidate.
func (t *unixTarget) Sync() error {
	if err := t.f.Sync(); err != nil {
		return err
	}
	st, err := t.f.Stat()
	if err != nil {
		return err
	}
	if !needsCacheDrop(st.Mode()) {
		return nil
	}
	if err := unix.IoctlSetInt(int(t.f.Fd()), unix.BLKFLSBUF, 0); err != nil {
		// Refused rather than ignored: carrying on would run a verify that
		// cannot fail, and printing "verified" on the strength of it is worse
		// than not verifying at all.
		return fmt.Errorf("flash: could not drop the kernel's cache of %s, so a readback could not be trusted: %w", t.f.Name(), err)
	}
	return nil
}

func (t *unixTarget) Finalize() error {
	// Re-read the partition table so /dev nodes reflect the new layout.
	_ = unix.IoctlSetInt(int(t.f.Fd()), unix.BLKRRPART, 0)
	return nil
}

func (t *unixTarget) Close() error { return t.f.Close() }

// needsCacheDrop reports whether this target has a device underneath it that
// could disagree with what Linux remembers writing. A regular file -- an image
// written as a target -- is its own truth, and a character device is not a
// whole-disk block node.
func needsCacheDrop(m os.FileMode) bool {
	return m&os.ModeDevice != 0 && m&os.ModeCharDevice == 0
}

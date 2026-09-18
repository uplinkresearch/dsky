// Package diskutil inspects and re-prepares removable disks: the "why is my
// 64 GB stick showing 3 GB" problem.
//
// That problem is largely one this tool causes. Writing a hybrid installer
// ISO leaves a partition layout Windows will not mount, or mounts as a small
// read-only volume, and the stick looks broken until someone knows to run
// diskpart. Owning the fix is fair.
//
// Two operations, deliberately separate:
//
//   - Inspect reads geometry and the partition table. It never needs
//     elevation, because making someone approve a prompt just to look at a
//     disk teaches them to approve prompts.
//   - Prepare rewrites the partition table and makes one full-size,
//     formatted volume. It is destructive, so it runs through the same
//     elevated worker and the same typed-size interlock as flashing.
//
// Scope is every disk except the one the OS is running from. Removable media
// is the ordinary case and needs no ceremony; a fixed disk is reachable too,
// but only when the caller passes allowFixed — permission that has to be
// carried explicitly from wherever a human was actually asked, and which the
// zero value never grants.
//
// The running system disk is refused unconditionally and is not a confirmation
// anyone can click through.
package diskutil

import (
	"context"
	"fmt"
	"strings"

	"github.com/uplinkresearch/dsky/internal/device"
)

// Scheme is the partition table to write.
type Scheme string

const (
	GPT Scheme = "gpt"
	MBR Scheme = "mbr"
)

// FS is the filesystem to create in the single partition.
type FS string

const (
	ExFAT FS = "exfat" // default: no 4 GiB file limit, read by everything current
	FAT32 FS = "fat32" // maximum compatibility, 4 GiB per-file ceiling
	NTFS  FS = "ntfs"  // Windows-only in practice
)

// Partition is one entry found on a disk.
type Partition struct {
	Number int
	Offset int64
	Size   int64
	Type   string   // scheme-specific type name or GUID
	Label  string   // volume label, when the OS could read one
	Mounts []string // drive letters or mount points
}

// Layout is what Inspect found.
type Layout struct {
	Device device.Device
	Scheme string // "gpt" | "mbr" | "none" | "unknown"
	Parts  []Partition
	// Usable is how much of the disk the partitions actually cover. The gap
	// between this and the device size is the whole point of the report: it
	// is what someone is looking at when a 64 GB stick reads as 3 GB.
	Usable int64
	// Notes explain, in plain words, anything that would make the OS behave
	// oddly with this disk.
	Notes []string
}

// UnusedBytes is capacity no partition claims.
func (l Layout) UnusedBytes() int64 {
	if l.Usable > l.Device.SizeBytes {
		return 0
	}
	return l.Device.SizeBytes - l.Usable
}

// Options configure Prepare.
type Options struct {
	Scheme Scheme
	FS     FS
	Label  string
	// AllowFixed lets Prepare erase a disk that is not removable media. It
	// defaults to false so that a caller written before fixed disks were
	// permitted cannot start erasing them by inheriting a wider policy.
	AllowFixed bool
}

func (o *Options) defaults() {
	if o.Scheme == "" {
		o.Scheme = GPT
	}
	if o.FS == "" {
		o.FS = ExFAT
	}
	if o.Label == "" {
		o.Label = "DSKY"
	}
}

// Validate rejects options the platform tooling would refuse anyway, with a
// better explanation than it would give.
func (o Options) Validate(dev device.Device) error {
	switch o.Scheme {
	case GPT, MBR:
	default:
		return fmt.Errorf("scheme must be gpt or mbr, got %q", o.Scheme)
	}
	switch o.FS {
	case ExFAT, NTFS:
	case FAT32:
		// Windows' own formatter refuses FAT32 over 32 GB. The limit is in
		// the tool, not the filesystem, but we cannot format past it either.
		if dev.SizeBytes > 32<<30 {
			return fmt.Errorf("FAT32 cannot be created on a %.0f GB disk by Windows' formatter (32 GB limit) — use exFAT",
				float64(dev.SizeBytes)/1e9)
		}
	default:
		return fmt.Errorf("filesystem must be exfat, fat32, or ntfs, got %q", o.FS)
	}
	if len(o.Label) > 32 {
		return fmt.Errorf("label is %d characters; keep it to 32 or fewer", len(o.Label))
	}
	for _, r := range o.Label {
		if strings.ContainsRune(`\/:*?"<>|`, r) {
			return fmt.Errorf("label cannot contain %q", r)
		}
	}
	return nil
}

// Guard is the policy gate shared by every destructive operation here.
//
// allowFixed carries a human's explicit decision down from wherever it was
// actually made. It is a parameter rather than a package setting so that it
// cannot be switched on once and then silently apply to a later call nobody
// was looking at.
func Guard(dev device.Device, allowFixed bool) error {
	if !dev.Writable() {
		return fmt.Errorf("refusing %s: it hosts the running OS", dev.ID)
	}
	if !dev.Routine() && !allowFixed {
		return fmt.Errorf("refusing %s: this is a fixed disk (bus=%s), not removable media — "+
			"it can be erased, but only after confirming which disk it is", dev.ID, dev.Bus)
	}
	return nil
}

// Inspect reads the disk's partition table without elevation.
func Inspect(ctx context.Context, dev device.Device) (*Layout, error) {
	l, err := inspect(ctx, dev)
	if err != nil {
		return nil, err
	}
	l.Notes = append(l.Notes, diagnose(*l)...)
	return l, nil
}

// diagnose turns a layout into the sentences someone actually needs: why a
// stick looks the wrong size, or why the OS will not mount it.
func diagnose(l Layout) []string {
	var notes []string
	switch {
	case len(l.Parts) == 0:
		notes = append(notes, "No partition table — most systems will not mount this until it is prepared.")
	case l.UnusedBytes() > l.Device.SizeBytes/10:
		notes = append(notes, fmt.Sprintf(
			"%.1f GB of %.1f GB is outside any partition. This is what a stick looks like after an installer image was written to it.",
			float64(l.UnusedBytes())/1e9, float64(l.Device.SizeBytes)/1e9))
	}
	for _, p := range l.Parts {
		t := strings.ToLower(p.Type)
		if strings.Contains(t, "iso9660") || strings.Contains(t, "0x00") {
			notes = append(notes, fmt.Sprintf("Partition %d looks like installer media, not a normal volume.", p.Number))
		}
	}
	if len(l.Parts) > 0 && len(mountsOf(l)) == 0 {
		notes = append(notes, "No volume is mounted — the filesystem is one this system cannot read, or the table is damaged.")
	}
	if len(notes) > 0 {
		notes = append(notes, "Prepare wipes the disk and gives it one full-size volume this system can use.")
	}
	return notes
}

func mountsOf(l Layout) []string {
	var out []string
	for _, p := range l.Parts {
		out = append(out, p.Mounts...)
	}
	return out
}

// Prepare wipes the disk and lays down one full-size formatted volume. It
// must run elevated; callers reach it through the flash worker.
func Prepare(ctx context.Context, dev device.Device, opts Options, progress func(stage string)) error {
	opts.defaults()
	if err := Guard(dev, opts.AllowFixed); err != nil {
		return err
	}
	if err := opts.Validate(dev); err != nil {
		return err
	}
	// After Guard, because a device that should never have been offered is a
	// different complaint from one that changed underneath. Last thing before
	// the disk is written to, on every platform: Windows reassigns disk
	// numbers on hotplug as readily as Linux reuses /dev/sdb.
	if err := ConfirmSameDisk(ctx, dev); err != nil {
		return err
	}
	if progress == nil {
		progress = func(string) {}
	}
	return prepare(ctx, dev, opts, progress)
}

// listDevices is device.List, as a variable so a test can describe a stick
// being swapped during the password prompt rather than need somebody to do it.
var listDevices = device.List

// ConfirmSameDisk re-identifies a disk immediately before it is written to.
//
// The device travels from the page to the elevated worker as JSON, and between
// choosing it and the work starting there is a password prompt: seconds, and
// sometimes much longer, in which somebody can unplug the stick and put
// something else in. The kernel reuses the name. Everything downstream --
// wipefs, sfdisk, diskpart -- takes that name and believes it.
//
// So the identity is checked against a fresh enumeration rather than against
// the struct that made the trip: serial first, because that is what actually
// names a device, then size. A disk that has become the system's since it was
// listed is refused too, since this is the last moment anything can say no.
//
// Flash has carried a size cross-check against the opened handle for a while
// (internal/flash, enumTolerance); Prepare had nothing at all.
func ConfirmSameDisk(ctx context.Context, want device.Device) error {
	now, err := listDevices(ctx)
	if err != nil {
		return fmt.Errorf("re-checking %s before writing to it: %w", want.ID, err)
	}
	for _, d := range now {
		if d.ID != want.ID {
			continue
		}
		switch {
		case want.Serial != "" && d.Serial != "" && d.Serial != want.Serial:
			return fmt.Errorf("refusing %s: it was serial %s when it was chosen and is %s now — something was unplugged and another device took the name",
				want.ID, want.Serial, d.Serial)
		case want.SizeBytes > 0 && d.SizeBytes > 0 && d.SizeBytes != want.SizeBytes:
			return fmt.Errorf("refusing %s: it was %d MiB when it was chosen and is %d MiB now — something was unplugged and another device took the name",
				want.ID, want.SizeBytes>>20, d.SizeBytes>>20)
		case d.System:
			return fmt.Errorf("refusing %s: it holds the running system", want.ID)
		}
		return nil
	}
	return fmt.Errorf("refusing to write to %s: it is not there any more", want.ID)
}

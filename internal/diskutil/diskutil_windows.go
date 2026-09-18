package diskutil

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/uplinkresearch/dsky/internal/device"
	"github.com/uplinkresearch/dsky/internal/winwmi"

	"github.com/uplinkresearch/dsky/internal/hidewin"
)

type win32Partition struct {
	DeviceID       string
	DiskIndex      uint32
	Index          uint32
	StartingOffset uint64
	Size           uint64
	Type           string
	Bootable       bool
}

type win32LogicalDisk struct {
	DeviceID   string
	VolumeName string
	FileSystem string
}

// inspect reads the partition table through WMI, which needs no elevation —
// the point being that looking at a disk should never cost a UAC prompt.
func inspect(_ context.Context, dev device.Device) (*Layout, error) {
	l := &Layout{Device: dev, Scheme: "unknown"}
	if dev.Index < 0 {
		return nil, fmt.Errorf("no Windows disk number for %s", dev.ID)
	}

	var parts []win32Partition
	q := fmt.Sprintf("SELECT DeviceID, DiskIndex, Index, StartingOffset, Size, Type, Bootable "+
		"FROM Win32_DiskPartition WHERE DiskIndex = %d", dev.Index)
	if err := winwmi.Query(q, &parts); err != nil {
		return nil, fmt.Errorf("reading partitions: %w", err)
	}
	for _, p := range parts {
		part := Partition{
			Number: int(p.Index) + 1,
			Offset: int64(p.StartingOffset),
			Size:   int64(p.Size),
			Type:   p.Type,
		}
		part.Mounts, part.Label = volumesFor(p.DeviceID)
		l.Usable += part.Size
		l.Parts = append(l.Parts, part)
		// Win32_DiskPartition's Type string carries the scheme.
		if strings.Contains(strings.ToUpper(p.Type), "GPT") {
			l.Scheme = "gpt"
		} else if l.Scheme == "unknown" {
			l.Scheme = "mbr"
		}
	}
	if len(l.Parts) == 0 {
		l.Scheme = "none"
	}
	return l, nil
}

// volumesFor maps a partition to its drive letters and label. WMI models
// this as an association, so it needs its own query per partition.
func volumesFor(partitionID string) ([]string, string) {
	var disks []win32LogicalDisk
	q := fmt.Sprintf(
		"ASSOCIATORS OF {Win32_DiskPartition.DeviceID='%s'} WHERE AssocClass = Win32_LogicalDiskToPartition",
		strings.ReplaceAll(partitionID, `'`, `\'`))
	if err := winwmi.Query(q, &disks); err != nil {
		return nil, ""
	}
	var mounts []string
	label := ""
	for _, d := range disks {
		mounts = append(mounts, d.DeviceID)
		if label == "" {
			label = d.VolumeName
		}
	}
	return mounts, label
}

// prepare drives diskpart. Its script language is unlovely, but it is built
// into every Windows including Server Core, it is what the OS itself uses,
// and it handles the volume-arrival timing that makes a hand-rolled
// "partition then format" race.
func prepare(ctx context.Context, dev device.Device, opts Options, progress func(string)) error {
	fsName := map[FS]string{ExFAT: "exfat", FAT32: "fat32", NTFS: "ntfs"}[opts.FS]
	scheme := "gpt"
	if opts.Scheme == MBR {
		scheme = "mbr"
	}
	style, styleErr := partitionStyle(dev.Index)

	progress(fmt.Sprintf("preparing %s as %s/%s", dev.ID, scheme, fsName))
	// clean fails with "Access is denied" on a drive whose volume Windows has
	// mounted -- which is most sticks. Seen on Windows 11 with a stick mounted
	// as D:: the first run failed at clean and took the volume down with it,
	// and the same run a moment later went straight through. clean is safe to
	// repeat, so the partition run is tried again before giving up.
	// The style is read again each time: a run that failed part way may
	// already have converted the disk, and converting it twice is the very
	// refusal this script is written to avoid.
	if err := confirmDiskNumber(dev); err != nil {
		return err
	}
	var err error
	for attempt := 1; attempt <= 3; attempt++ {
		if attempt > 1 {
			style, styleErr = partitionStyle(dev.Index)
		}
		if _, err = runDiskpart(ctx, partitionScript(dev.Index, scheme, style, styleErr == nil)); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
	if err != nil {
		return err
	}

	// The new partition's volume arrives a moment after diskpart has made it
	// -- on a removable drive, noticeably after -- and formatting before it
	// has arrived fails with "There is no volume selected". Seen on Windows
	// 11 with the steps typed one at a time, several seconds apart, so a
	// script running them back to back would meet it every time. The format
	// is its own run, repeated until the volume is there.
	progress("formatting " + dev.ID + " as " + fsName)
	fmtScript := formatScript(dev.Index, fsName, opts.Label)
	var lastErr error
	for attempt := 1; attempt <= 10; attempt++ {
		if _, err := runDiskpart(ctx, fmtScript); err == nil {
			progress("prepared " + dev.ID)
			return nil
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return lastErr
}

// runDiskpart runs one script and says whether it worked. diskpart stops a
// script at the first command that fails and exits non-zero, but some failures
// only show in its output with a zero exit, so both are checked.
func runDiskpart(ctx context.Context, script string) (string, error) {
	f, err := os.CreateTemp("", "dsky-diskpart-*.txt")
	if err != nil {
		return "", err
	}
	path := f.Name()
	defer os.Remove(path)
	if _, err := f.WriteString(script); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	out, err := hidewin.Cmd(exec.CommandContext(ctx, "diskpart", "/s", path)).CombinedOutput()
	text := string(out)
	if err != nil {
		return text, fmt.Errorf("diskpart: %v\n%s", err, strings.TrimSpace(text))
	}
	low := strings.ToLower(text)
	for _, bad := range []string{"access is denied", "no disk selected", "the format did not complete",
		"diskpart has encountered an error", "is not valid", "there is no volume selected"} {
		if strings.Contains(low, bad) {
			return text, fmt.Errorf("diskpart reported a problem:\n%s", strings.TrimSpace(text))
		}
	}
	return text, nil
}

// msftDisk is the Storage module's view of a disk, which -- unlike
// Win32_DiskPartition -- knows the partition style of a disk with no
// partitions on it.
type msftDisk struct {
	Number         uint32
	PartitionStyle uint16 // 0 raw, 1 MBR, 2 GPT
	SerialNumber   string
	Size           uint64
}

// confirmDiskNumber checks that disk number N is still the device that was
// chosen, before a script containing `clean` is written for it.
//
// Windows hands out disk numbers on hotplug the way Linux hands out /dev/sdb,
// and the number travelled here inside a job that waited for a UAC prompt. The
// Storage module is asked rather than diskpart, because diskpart's `detail
// disk` prints in whatever language Windows was installed in and a check that
// works only in English is not a check.
//
// Size is compared with a tolerance: the Storage module and the enumeration
// that filled dev do not always agree to the byte on the same disk.
func confirmDiskNumber(dev device.Device) error {
	var disks []msftDisk
	q := fmt.Sprintf("SELECT Number, SerialNumber, Size FROM MSFT_Disk WHERE Number = %d", dev.Index)
	if err := winwmi.QueryNamespace(q, &disks, `root\Microsoft\Windows\Storage`); err != nil {
		return fmt.Errorf("re-checking disk %d before cleaning it: %w", dev.Index, err)
	}
	if len(disks) == 0 {
		return fmt.Errorf("refusing to clean disk %d: it is not there any more", dev.Index)
	}
	return sameDisk(dev, disks[0])
}

// sameDisk is the comparison on its own, so it can be tested without a disk.
func sameDisk(dev device.Device, got msftDisk) error {
	if s := strings.TrimSpace(got.SerialNumber); s != "" && dev.Serial != "" && s != dev.Serial {
		return fmt.Errorf("refusing to clean disk %d: it was serial %s when it was chosen and is %s now — something was unplugged and another disk took the number",
			dev.Index, dev.Serial, s)
	}
	const tolerance = 64 << 20
	if got.Size > 0 && dev.SizeBytes > 0 {
		if d := int64(got.Size) - dev.SizeBytes; d > tolerance || d < -tolerance {
			return fmt.Errorf("refusing to clean disk %d: it was %d MiB when it was chosen and is %d MiB now — something was unplugged and another disk took the number",
				dev.Index, dev.SizeBytes>>20, got.Size>>20)
		}
	}
	return nil
}

// partitionStyle reports a disk's partition style as diskpart would convert
// it: "mbr", "gpt", or "" for a disk with none.
func partitionStyle(index int) (string, error) {
	var disks []msftDisk
	q := fmt.Sprintf("SELECT Number, PartitionStyle FROM MSFT_Disk WHERE Number = %d", index)
	if err := winwmi.QueryNamespace(q, &disks, `root\Microsoft\Windows\Storage`); err != nil {
		return "", err
	}
	if len(disks) == 0 {
		return "", fmt.Errorf("no disk %d in the storage module", index)
	}
	return map[uint16]string{1: "mbr", 2: "gpt"}[disks[0].PartitionStyle], nil
}

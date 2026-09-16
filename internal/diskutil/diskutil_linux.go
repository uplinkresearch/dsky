package diskutil

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/uplinkresearch/dsky/internal/device"
)

// lsblkDisk mirrors the shape `lsblk --json` returns.
type lsblkDisk struct {
	Name     string      `json:"name"`
	Size     int64       `json:"size"`
	PTType   string      `json:"pttype"`
	Children []lsblkPart `json:"children"`
}

type lsblkPart struct {
	Name         string `json:"name"`
	Size         int64  `json:"size"`
	FSType       string `json:"fstype"`
	Label        string `json:"label"`
	MountPoint   string `json:"mountpoint"`
	PartTypeName string `json:"parttypename"`
	Start        int64  `json:"start"`
}

// inspect uses lsblk, which reports the partition table without root.
func inspect(ctx context.Context, dev device.Device) (*Layout, error) {
	l := &Layout{Device: dev, Scheme: "unknown"}
	out, err := exec.CommandContext(ctx, "lsblk", "--json", "--bytes",
		"-o", "NAME,SIZE,PTTYPE,FSTYPE,LABEL,MOUNTPOINT,PARTTYPENAME,START", dev.ID).Output()
	if err != nil {
		return nil, fmt.Errorf("lsblk: %w", err)
	}
	var doc struct {
		BlockDevices []lsblkDisk `json:"blockdevices"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil, fmt.Errorf("parsing lsblk output: %w", err)
	}
	if len(doc.BlockDevices) == 0 {
		return nil, fmt.Errorf("lsblk reported nothing for %s", dev.ID)
	}
	d := doc.BlockDevices[0]
	switch strings.ToLower(d.PTType) {
	case "gpt":
		l.Scheme = "gpt"
	case "dos":
		l.Scheme = "mbr"
	case "":
		l.Scheme = "none"
	default:
		l.Scheme = d.PTType
	}
	for i, c := range d.Children {
		p := Partition{
			Number: i + 1,
			Offset: c.Start * 512,
			Size:   c.Size,
			Type:   firstNonEmpty(c.PartTypeName, c.FSType),
			Label:  c.Label,
		}
		if c.MountPoint != "" {
			p.Mounts = []string{c.MountPoint}
		}
		l.Usable += p.Size
		l.Parts = append(l.Parts, p)
	}
	return l, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// prepare writes a fresh table with sfdisk and formats with the matching
// mkfs. Both are shelled out to rather than reimplemented: these are the
// tools the distribution already trusts with its own disks.
func prepare(ctx context.Context, dev device.Device, opts Options, progress func(string)) error {
	mkfs, args := linuxMkfs(opts)
	if _, err := exec.LookPath(mkfs); err != nil {
		return fmt.Errorf("%s is not installed — it creates the %s filesystem (try your package manager)", mkfs, opts.FS)
	}
	if _, err := exec.LookPath("sfdisk"); err != nil {
		return fmt.Errorf("sfdisk is not installed — it writes the partition table (package util-linux)")
	}

	// Nothing on the disk may be in use. A mounted partition keeps the kernel
	// from re-reading the new table, so the "new" partition would still be the
	// old, mounted one and the format below would find it busy — which is how
	// Erase and prepare failed on a stick the desktop had mounted as WIN11.
	progress("unmounting " + dev.ID)
	if err := unmountDisk(ctx, dev.ID); err != nil {
		return err
	}
	// The old filesystem's signature has to go too. A new partition that
	// starts where the old one did would otherwise show the same filesystem,
	// and the desktop mounts it again the moment the kernel sees it.
	for _, part := range diskPartitions(ctx, dev.ID) {
		exec.CommandContext(ctx, "wipefs", "--all", part).Run()
	}
	exec.CommandContext(ctx, "wipefs", "--all", dev.ID).Run()

	label, script := sfdiskScript(opts)
	progress(fmt.Sprintf("writing a %s table on %s", label, dev.ID))
	cmd := exec.CommandContext(ctx, "sfdisk", "--wipe", "always", "--wipe-partitions", "always", dev.ID)
	cmd.Stdin = strings.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("sfdisk: %v\n%s", err, strings.TrimSpace(string(out)))
	}
	// Let the kernel pick up the new table before formatting into it.
	exec.CommandContext(ctx, "blockdev", "--rereadpt", dev.ID).Run()
	exec.CommandContext(ctx, "partprobe", dev.ID).Run()
	exec.CommandContext(ctx, "udevadm", "settle").Run()

	part := partitionPath(dev.ID, 1)
	progress(fmt.Sprintf("formatting %s as %s", part, opts.FS))
	// Desktops automount new partitions, and one can still get in between the
	// table landing and the format. Unmount and try again rather than fail.
	var out []byte
	var err error
	for attempt := 1; attempt <= 5; attempt++ {
		_ = unmountDisk(ctx, dev.ID)
		out, err = exec.CommandContext(ctx, mkfs, append(args, part)...).CombinedOutput()
		if err == nil || !strings.Contains(strings.ToLower(string(out)), "busy") {
			break
		}
		exec.CommandContext(ctx, "udevadm", "settle").Run()
		time.Sleep(time.Second)
	}
	if err != nil {
		return fmt.Errorf("%s: %v\n%s", mkfs, err, strings.TrimSpace(string(out)))
	}
	progress("prepared " + dev.ID)
	return nil
}

// unmountDisk unmounts every mounted filesystem on the disk or its partitions.
func unmountDisk(ctx context.Context, disk string) error {
	b, err := os.ReadFile("/proc/self/mounts")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || !onDisk(f[0], disk) {
			continue
		}
		// Mount points escape spaces as \040.
		mnt := strings.ReplaceAll(f[1], `\040`, " ")
		if out, err := exec.CommandContext(ctx, "umount", mnt).CombinedOutput(); err != nil {
			return fmt.Errorf("unmounting %s from %s (close anything using the stick and retry): %v\n%s",
				f[0], mnt, err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

// onDisk reports whether a device node is the disk or one of its partitions:
// /dev/sda and /dev/sda1 for /dev/sda, but not /dev/sdaa.
func onDisk(node, disk string) bool {
	if node == disk {
		return true
	}
	if !strings.HasPrefix(node, disk) {
		return false
	}
	rest := strings.TrimPrefix(node, disk)
	// A disk whose name ends in a digit separates its partitions with "p"
	// (/dev/nvme0n1p2); without it, /dev/nvme0n10 is a different disk.
	if last := disk[len(disk)-1]; last >= '0' && last <= '9' {
		if !strings.HasPrefix(rest, "p") {
			return false
		}
		rest = rest[1:]
	}
	if rest == "" {
		return false
	}
	for _, r := range rest {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// diskPartitions lists the disk's partition nodes as the kernel sees them now.
func diskPartitions(ctx context.Context, disk string) []string {
	out, err := exec.CommandContext(ctx, "lsblk", "-lnpo", "NAME,TYPE", disk).Output()
	if err != nil {
		return nil
	}
	var parts []string
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[1] == "part" {
			parts = append(parts, f[0])
		}
	}
	return parts
}

func linuxMkfs(o Options) (string, []string) {
	switch o.FS {
	case NTFS:
		return "mkfs.ntfs", []string{"--quick", "--label", o.Label}
	case FAT32:
		return "mkfs.vfat", []string{"-F", "32", "-n", o.Label}
	default:
		return "mkfs.exfat", []string{"-n", o.Label}
	}
}

// partitionPath handles the two naming conventions: /dev/sdb1 but
// /dev/nvme0n1p1 and /dev/mmcblk0p1.
func partitionPath(disk string, n int) string {
	last := disk[len(disk)-1]
	if last >= '0' && last <= '9' {
		return fmt.Sprintf("%sp%d", disk, n)
	}
	return fmt.Sprintf("%s%d", disk, n)
}

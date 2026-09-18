package device

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// lsblk -J output shapes.
type lsblkOut struct {
	Blockdevices []lsblkDev `json:"blockdevices"`
}

type lsblkDev struct {
	Name       string      `json:"name"`
	Model      *string     `json:"model"`
	Serial     *string     `json:"serial"`
	Size       json.Number `json:"size"`
	Tran       *string     `json:"tran"`
	RM         bool        `json:"rm"`
	Type       string      `json:"type"`
	Mountpoint *string     `json:"mountpoint"`
	// Mountpoints is every place this node is mounted. MOUNTPOINT, singular,
	// is only the first of them, and on a root filesystem with more than one
	// -- btrfs subvolumes, a bind-heavy LVM, ZFS -- the first is frequently
	// not "/". On the machine this was found on, the LUKS root reported
	// "/mnt/data" and neither "/" nor "/home" was visible at all; that disk
	// was marked as the system's only because /boot happened to be a separate
	// partition on it. Move /boot inside the root filesystem, as systemd-boot
	// layouts do, and the running system's own disk stops looking like one.
	//
	// util-linux has had the plural since 2.37 (2021). Older ones omit the
	// field, and the singular is still read for them.
	Mountpoints []*string  `json:"mountpoints"`
	Children    []lsblkDev `json:"children"`
}

func list(ctx context.Context) ([]Device, error) {
	if isWSL() {
		return nil, fmt.Errorf("device: running under WSL, which cannot see USB block devices — use the Windows dsky.exe instead")
	}
	cmd := exec.CommandContext(ctx, "lsblk", "-J", "-b", "-o", "NAME,MODEL,SERIAL,SIZE,TRAN,RM,TYPE,MOUNTPOINT,MOUNTPOINTS")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("device: lsblk: %w", err)
	}
	var parsed lsblkOut
	if err := json.Unmarshal(out, &parsed); err != nil {
		return nil, fmt.Errorf("device: parsing lsblk output: %w", err)
	}
	var devs []Device
	for _, d := range parsed.Blockdevices {
		if d.Type != "disk" || virtualDisk(d.Name) {
			continue
		}
		size, _ := strconv.ParseInt(d.Size.String(), 10, 64)
		// A disk of no size is a kernel placeholder, not a disk. Loading the
		// nbd module -- which is how a virtual machine's disk gets read --
		// creates sixteen of them, and they filled the device list on a
		// machine whose real stick was not plugged in, burying the one line
		// that said so.
		if size == 0 {
			continue
		}
		dev := Device{
			ID:        "/dev/" + d.Name,
			Index:     -1,
			Model:     strings.TrimSpace(deref(d.Model)),
			Serial:    strings.TrimSpace(deref(d.Serial)),
			SizeBytes: size,
			Bus:       strings.ToLower(deref(d.Tran)),
			Removable: d.RM,
		}
		collectMounts(d, &dev)
		devs = append(devs, dev)
	}
	return devs, nil
}

// virtualDisk reports whether a name belongs to a kernel construct rather
// than hardware: network block devices, loopbacks, RAM disks, compressed swap,
// device-mapper and software RAID. None of them is something to write an
// installer to, and listing them only makes the real disk harder to find.
func virtualDisk(name string) bool {
	for _, prefix := range []string{"nbd", "loop", "ram", "zram", "dm-", "md", "zd"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// systemMount is a mount that makes the disk under it the running system's.
// /efi is here because systemd-boot puts the ESP there rather than at
// /boot/efi, and a machine laid out that way was the one this list failed on.
func systemMount(mp string) bool {
	switch mp {
	case "/", "/boot", "/boot/efi", "/efi", "/home", "/var", "/usr", "[SWAP]":
		return true
	}
	return false
}

// collectMounts walks children marking mounts; a system mount anywhere on the
// disk marks it as the system disk.
func collectMounts(d lsblkDev, dev *Device) {
	for _, mp := range mountsOf(d) {
		dev.Mounts = append(dev.Mounts, mp)
		if systemMount(mp) {
			dev.System = true
		}
	}
	for _, c := range d.Children {
		collectMounts(c, dev)
	}
}

// mountsOf is every place a node is mounted, preferring the plural field and
// falling back to the singular on a util-linux too old to have it.
func mountsOf(d lsblkDev) []string {
	var out []string
	for _, p := range d.Mountpoints {
		if mp := deref(p); mp != "" {
			out = append(out, mp)
		}
	}
	if len(out) > 0 {
		return out
	}
	if mp := deref(d.Mountpoint); mp != "" {
		return []string{mp}
	}
	return nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func isWSL() bool {
	b, err := os.ReadFile("/proc/sys/kernel/osrelease")
	return err == nil && strings.Contains(strings.ToLower(string(b)), "microsoft")
}

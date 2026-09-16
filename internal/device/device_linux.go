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
	Children   []lsblkDev  `json:"children"`
}

func list(ctx context.Context) ([]Device, error) {
	if isWSL() {
		return nil, fmt.Errorf("device: running under WSL, which cannot see USB block devices — use the Windows dsky.exe instead")
	}
	cmd := exec.CommandContext(ctx, "lsblk", "-J", "-b", "-o", "NAME,MODEL,SERIAL,SIZE,TRAN,RM,TYPE,MOUNTPOINT")
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

// collectMounts walks children marking mounts; "/", "/boot", "/home" (or
// swap) anywhere on the disk marks it as the system disk.
func collectMounts(d lsblkDev, dev *Device) {
	if mp := deref(d.Mountpoint); mp != "" {
		dev.Mounts = append(dev.Mounts, mp)
		if mp == "/" || mp == "/boot" || mp == "/boot/efi" || mp == "/home" || mp == "[SWAP]" {
			dev.System = true
		}
	}
	for _, c := range d.Children {
		collectMounts(c, dev)
	}
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

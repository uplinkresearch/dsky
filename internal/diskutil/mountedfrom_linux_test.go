package diskutil

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The interlock before a disk is re-partitioned asks "is anything on this disk
// in use?" and answered by comparing device names, which an encrypted or LVM
// root does not share with its disk: / mounts from /dev/mapper/root, and
// /dev/mapper/root has nothing in common with /dev/nvme0n1. So on the majority
// of laptops the check found nothing, said the disk was idle, and the caller
// went on to wipefs and re-partition it.
//
// The relationship is a fact the kernel publishes, so it is read rather than
// guessed at: slaves, down to a partition or a disk.
func TestBuiltOnWalksTheSlavesChain(t *testing.T) {
	root := t.TempDir()
	old := sysfsBlock
	sysfsBlock = root
	t.Cleanup(func() { sysfsBlock = old })

	// dm-1 (a logical volume) on dm-0 (a LUKS container) on nvme0n1p2.
	for path, slaves := range map[string][]string{
		"dm-1":      {"dm-0"},
		"dm-0":      {"nvme0n1p2"},
		"nvme0n1p2": nil,
		"nvme0n1":   nil,
		"sdb1":      nil,
		"md0":       {"sdb1", "sdc1"},
		"sdc1":      nil,
	} {
		if err := os.MkdirAll(filepath.Join(root, path, "slaves"), 0o755); err != nil {
			t.Fatal(err)
		}
		for _, s := range slaves {
			if err := os.MkdirAll(filepath.Join(root, path, "slaves", s), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}

	cases := []struct {
		name, disk string
		want       bool
	}{
		{"dm-1", "nvme0n1", true}, // LVM on LUKS on a partition
		{"dm-0", "nvme0n1", true}, // LUKS on a partition
		{"nvme0n1p2", "nvme0n1", true},
		{"nvme0n1", "nvme0n1", true},
		{"dm-1", "sda", false}, // a different disk entirely
		{"md0", "sdb", true},   // one leg of a mirror is still this disk
		{"md0", "sdd", false},
		{"nvme0n1p2", "nvme0n10", false}, // not a prefix match
	}
	for _, c := range cases {
		if got := builtOn(c.name, c.disk, 0); got != c.want {
			t.Errorf("builtOn(%q, %q) = %v, want %v", c.name, c.disk, got, c.want)
		}
	}
}

// A chain that points at itself must not be walked forever by a loop running
// as root.
func TestBuiltOnStopsOnACycle(t *testing.T) {
	root := t.TempDir()
	old := sysfsBlock
	sysfsBlock = root
	t.Cleanup(func() { sysfsBlock = old })

	if err := os.MkdirAll(filepath.Join(root, "dm-0", "slaves", "dm-0"), 0o755); err != nil {
		t.Fatal(err)
	}
	done := make(chan bool, 1)
	go func() { done <- builtOn("dm-0", "nvme0n1", 0) }()
	select {
	case got := <-done:
		if got {
			t.Error("a cycle reported a disk it never reached")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("builtOn did not return on a cycle")
	}
}

package flash

import (
	"os"
	"path/filepath"
	"testing"
)

// The readback verify reads through the same descriptor the write used, and on
// Linux the block node is opened buffered -- unlike Windows, which sets
// FILE_FLAG_NO_BUFFERING, and macOS, which opens /dev/rdiskN. So Sync has to
// invalidate the kernel's cache of the device, or the verify hashes what it
// just wrote straight back out of RAM and a counterfeit stick passes.
//
// Exercising BLKFLSBUF needs a real block device and root. What is pinned here
// is the decision: which targets have a device underneath that could disagree.
func TestNeedsCacheDrop(t *testing.T) {
	cases := []struct {
		what string
		mode os.FileMode
		want bool
	}{
		{"a block device — the stick", os.ModeDevice, true},
		{"a character device", os.ModeDevice | os.ModeCharDevice, false},
		{"a regular file — an image written as a target", 0, false},
		{"a directory", os.ModeDir, false},
	}
	for _, c := range cases {
		if got := needsCacheDrop(c.mode); got != c.want {
			t.Errorf("%s: needsCacheDrop = %v, want %v", c.what, got, c.want)
		}
	}
}

// An image file target must still sync cleanly: there is no device under it to
// invalidate, and failing there would break writing to a file.
func TestSyncLeavesImageFilesAlone(t *testing.T) {
	p := filepath.Join(t.TempDir(), "image.img")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(make([]byte, 1<<20)); err != nil {
		t.Fatal(err)
	}
	if err := (&unixTarget{f: f}).Sync(); err != nil {
		t.Fatalf("an image file should sync without a BLKFLSBUF: %v", err)
	}
}

// The exact line that failed on a laptop: a stick holding an Ubuntu ISO, whose
// label has spaces, mounted where /proc/mounts writes those spaces as \040.
func TestUnescapeMount(t *testing.T) {
	cases := map[string]string{
		`/run/media/dgb/Ubuntu\04026.04.1\040LTS\040amd64`: "/run/media/dgb/Ubuntu 26.04.1 LTS amd64",
		`/mnt/data`:            "/mnt/data",
		`/mnt/a\011b`:          "/mnt/a\tb",
		`/mnt/back\134slash`:   `/mnt/back\slash`,
		`/mnt/line\012break`:   "/mnt/line\nbreak",
		`/run/media/x/NO NAME`: "/run/media/x/NO NAME",
	}
	for in, want := range cases {
		if got := unescapeMount(in); got != want {
			t.Errorf("unescapeMount(%q) = %q, want %q", in, got, want)
		}
	}
}

// Unmounting somebody else's filesystems before writing is not a small
// mistake to make quietly, and a prefix match makes it: /dev/sdaa1 begins
// with /dev/sda.
func TestPartitionOf(t *testing.T) {
	cases := []struct {
		source, disk string
		want         bool
	}{
		{"/dev/sda", "/dev/sda", true},
		{"/dev/sda1", "/dev/sda", true},
		{"/dev/sda12", "/dev/sda", true},
		{"/dev/sdaa1", "/dev/sda", false},
		{"/dev/sdb1", "/dev/sda", false},
		{"/dev/nvme0n1p3", "/dev/nvme0n1", true},
		{"/dev/nvme0n10p1", "/dev/nvme0n1", false},
		{"/dev/mmcblk0p1", "/dev/mmcblk0", true},
		{"/dev/mapper/root", "/dev/sda", false},
		{"/dev/sdap", "/dev/sda", false},
	}
	for _, c := range cases {
		if got := partitionOf(c.source, c.disk); got != c.want {
			t.Errorf("partitionOf(%q, %q) = %v, want %v", c.source, c.disk, got, c.want)
		}
	}
}

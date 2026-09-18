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

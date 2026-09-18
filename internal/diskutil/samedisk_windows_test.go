package diskutil

import (
	"strings"
	"testing"

	"github.com/uplinkresearch/dsky/internal/device"
)

// partitionScript carries `clean`, which is unrecoverable on the wrong disk.
// It used to carry `detail disk` above it under a comment promising to refuse
// if diskpart disagreed about what the disk was -- but both went into one
// script, so clean ran regardless and nothing read what detail printed.
//
// The Storage module answers the same question in numbers rather than in
// translated prose, which is the point: a check that only works on an English
// Windows is not a check.
func TestSameDisk(t *testing.T) {
	stick := device.Device{Index: 2, Serial: "AA11", SizeBytes: 64 << 30}

	cases := []struct {
		what    string
		got     msftDisk
		wantErr string
	}{
		{"unchanged", msftDisk{Number: 2, SerialNumber: "AA11", Size: 64 << 30}, ""},
		{"another disk took the number", msftDisk{Number: 2, SerialNumber: "BB22", Size: 64 << 30}, "serial"},
		{"same serial, wildly different size", msftDisk{Number: 2, SerialNumber: "AA11", Size: 4 << 40}, "MiB"},
		// The two sources do not always agree to the byte on one disk, so a
		// small difference must not refuse a perfectly good stick.
		{"a few MiB apart", msftDisk{Number: 2, SerialNumber: "AA11", Size: 64<<30 + (32 << 20)}, ""},
		// Plenty of cheap sticks report no serial at all; that cannot be a
		// refusal on its own, and the size still has to agree.
		{"no serial reported", msftDisk{Number: 2, Size: 64 << 30}, ""},
		{"no serial and the wrong size", msftDisk{Number: 2, Size: 1 << 40}, "MiB"},
		{"serial padded with spaces", msftDisk{Number: 2, SerialNumber: "  AA11 ", Size: 64 << 30}, ""},
	}
	for _, c := range cases {
		err := sameDisk(stick, c.got)
		switch {
		case c.wantErr == "" && err != nil:
			t.Errorf("%s: refused a disk that had not changed: %v", c.what, err)
		case c.wantErr != "" && err == nil:
			t.Errorf("%s: accepted it", c.what)
		case c.wantErr != "" && !strings.Contains(err.Error(), c.wantErr):
			t.Errorf("%s: %v\n  wanted a message mentioning %q", c.what, err, c.wantErr)
		}
	}
}

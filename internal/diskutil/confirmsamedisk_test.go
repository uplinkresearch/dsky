package diskutil

import (
	"context"
	"strings"
	"testing"

	"github.com/uplinkresearch/dsky/internal/device"
)

// Between choosing a disk and the elevated worker starting there is a password
// prompt, and the device makes that trip as JSON. Everything downstream took
// the name in that JSON and believed it, so a stick unplugged during the
// prompt -- with the kernel handing /dev/sdb to whatever went in next -- was
// written to as though nothing had happened.
func TestConfirmSameDisk(t *testing.T) {
	stick := device.Device{ID: "/dev/sdb", Serial: "AA11", SizeBytes: 64 << 30, Bus: "usb", Removable: true}

	cases := []struct {
		what    string
		present []device.Device
		wantErr string
	}{{
		what:    "still the same stick",
		present: []device.Device{stick},
	}, {
		what: "unplugged, and something else took the name",
		present: []device.Device{{ID: "/dev/sdb", Serial: "BB22", SizeBytes: 4 << 40,
			Bus: "usb", Removable: true}},
		wantErr: "serial",
	}, {
		what: "same serial reported, different size: still not the disk that was chosen",
		present: []device.Device{{ID: "/dev/sdb", Serial: "AA11", SizeBytes: 2 << 40,
			Bus: "usb", Removable: true}},
		wantErr: "MiB",
	}, {
		what:    "gone entirely",
		present: []device.Device{{ID: "/dev/sdc", Serial: "AA11", SizeBytes: 64 << 30}},
		wantErr: "not there any more",
	}, {
		// The fresh enumeration is the one with the fixed mountpoint reading,
		// so a disk that has become the system's since it was listed is caught
		// here even if the listing that offered it did not know.
		what: "it is the system disk now",
		present: []device.Device{{ID: "/dev/sdb", Serial: "AA11", SizeBytes: 64 << 30,
			Bus: "usb", Removable: true, System: true}},
		wantErr: "running system",
	}, {
		// A device with no serial is common enough -- plenty of cheap sticks
		// report none -- and must not be refused for it; the size still has to
		// agree.
		what:    "no serial either time",
		present: []device.Device{{ID: "/dev/sdb", SizeBytes: 64 << 30, Bus: "usb", Removable: true}},
	}}

	for _, c := range cases {
		listDevices = func(context.Context) ([]device.Device, error) { return c.present, nil }
		want := stick
		if c.what == "no serial either time" {
			want.Serial = ""
		}
		err := ConfirmSameDisk(context.Background(), want)
		switch {
		case c.wantErr == "" && err != nil:
			t.Errorf("%s: refused a disk that had not changed: %v", c.what, err)
		case c.wantErr != "" && err == nil:
			t.Errorf("%s: accepted it", c.what)
		case c.wantErr != "" && !strings.Contains(err.Error(), c.wantErr):
			t.Errorf("%s: %v\n  wanted a message mentioning %q", c.what, err, c.wantErr)
		}
	}
	listDevices = device.List
}

package device

import (
	"encoding/json"
	"strings"
	"testing"
)

// The disk holding the running system must never be offered for writing. It
// was decided by lsblk's MOUNTPOINT, singular, which is only the first of a
// node's mountpoints -- so on a root filesystem with several, "/" is
// frequently not the one reported.
//
// The first case below is a real machine: a LUKS root whose first mountpoint
// is /mnt/data, with / and /home behind it. That disk was marked as the
// system's only because /boot happened to be a separate partition on it. The
// second case is the same machine with the ESP at /efi and no separate /boot,
// which is what systemd-boot does -- and there the whole disk came back
// writable.
func TestSystemDiskIsSeenThroughEveryMountpoint(t *testing.T) {
	cases := []struct {
		what string
		json string
		want bool
	}{{
		what: "LUKS root, /boot separate: / is not the first mountpoint",
		json: `{"blockdevices":[{"name":"nvme0n1","type":"disk","size":"1000204886016","rm":false,
			"children":[
			  {"name":"nvme0n1p1","type":"part","size":"1073741824","rm":false,"mountpoint":"/boot","mountpoints":["/boot"]},
			  {"name":"nvme0n1p2","type":"part","size":"999130070016","rm":false,
			    "children":[{"name":"root","type":"crypt","size":"999130070016","rm":false,
			      "mountpoint":"/mnt/data","mountpoints":["/mnt/data","/home","/var/log","/"]}]}]}]}`,
		want: true,
	}, {
		what: "the same machine with the ESP at /efi and /boot inside the root",
		json: `{"blockdevices":[{"name":"nvme0n1","type":"disk","size":"1000204886016","rm":false,
			"children":[
			  {"name":"nvme0n1p1","type":"part","size":"1073741824","rm":false,"mountpoint":"/efi","mountpoints":["/efi"]},
			  {"name":"nvme0n1p2","type":"part","size":"999130070016","rm":false,
			    "children":[{"name":"root","type":"crypt","size":"999130070016","rm":false,
			      "mountpoint":"/mnt/data","mountpoints":["/mnt/data","/home","/"]}]}]}]}`,
		want: true,
	}, {
		what: "a util-linux too old for MOUNTPOINTS still reports the singular",
		json: `{"blockdevices":[{"name":"sda","type":"disk","size":"500107862016","rm":false,
			"children":[{"name":"sda1","type":"part","size":"500106784768","rm":false,"mountpoint":"/"}]}]}`,
		want: true,
	}, {
		what: "a plain USB stick is not the system disk",
		json: `{"blockdevices":[{"name":"sdb","type":"disk","size":"61530439680","rm":true,"tran":"usb",
			"children":[{"name":"sdb1","type":"part","size":"61529391104","rm":true,
			  "mountpoint":"/run/media/dgb/Ubuntu 26.04.1 LTS amd64","mountpoints":["/run/media/dgb/Ubuntu 26.04.1 LTS amd64"]}]}]}`,
		want: false,
	}}

	for _, c := range cases {
		var out lsblkOut
		if err := json.Unmarshal([]byte(strings.ReplaceAll(c.json, "\n\t\t\t", "")), &out); err != nil {
			t.Fatalf("%s: %v", c.what, err)
		}
		if len(out.Blockdevices) != 1 {
			t.Fatalf("%s: test data has %d disks", c.what, len(out.Blockdevices))
		}
		var dev Device
		collectMounts(out.Blockdevices[0], &dev)
		if dev.System != c.want {
			t.Errorf("%s:\n  System = %v, want %v (saw mounts %v)", c.what, dev.System, c.want, dev.Mounts)
		}
	}
}

// Every mountpoint is recorded, not just the one that decided the question,
// because the listing shows them and a disk that says only "/mnt/data" reads
// like somebody's data drive.
func TestMountsOfReportsThemAll(t *testing.T) {
	var d lsblkDev
	if err := json.Unmarshal([]byte(`{"name":"root","type":"crypt",
		"mountpoint":"/mnt/data","mountpoints":["/mnt/data","/home","/"]}`), &d); err != nil {
		t.Fatal(err)
	}
	got := mountsOf(d)
	if len(got) != 3 || got[0] != "/mnt/data" || got[2] != "/" {
		t.Errorf("mountsOf = %v", got)
	}

	// A node mounted nowhere reports nothing rather than an empty string, so
	// the listing does not show a blank mount. lsblk really does write
	// "mountpoints":[null] for these, which is why the nulls are skipped
	// rather than counted. (A fresh value: Unmarshal leaves fields absent
	// from the document alone, so reusing the one above would carry its
	// singular mountpoint into this case and pass for the wrong reason.)
	var unmounted lsblkDev
	if err := json.Unmarshal([]byte(`{"name":"sdb","type":"disk","mountpoints":[null]}`), &unmounted); err != nil {
		t.Fatal(err)
	}
	if got := mountsOf(unmounted); len(got) != 0 {
		t.Errorf("an unmounted node reported %v", got)
	}
}

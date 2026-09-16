//go:build linux

package device

import "testing"

// Kernel constructs are not disks anybody flashes. Reading a virtual machine's
// disk loads the nbd module, which creates sixteen empty devices; they filled
// the device list on a machine whose real stick was not plugged in, and buried
// the line that said so.
func TestVirtualDisksAreNotOffered(t *testing.T) {
	for _, name := range []string{"nbd0", "nbd15", "loop0", "ram3", "zram0", "dm-0", "md127", "zd16"} {
		if !virtualDisk(name) {
			t.Errorf("%s is offered as a disk to write to", name)
		}
	}
	// Real hardware, whatever it is attached to.
	for _, name := range []string{"sda", "sdb", "nvme0n1", "mmcblk0", "vda", "hda", "sr0"} {
		if virtualDisk(name) {
			t.Errorf("%s is hidden, and it is a real disk", name)
		}
	}
}

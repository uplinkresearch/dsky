package compose

import (
	"bytes"
	"fmt"
)

// The kickstart reaches Anaconda as a second initramfs rather than as a
// filesystem to mount, and that is not a stylistic choice.
//
// The obvious way -- append a partition holding ks.cfg, label it, and point
// inst.ks= at the label -- cannot work on media made by appending to a hybrid
// ISO. The ISO9660 volume descriptor sits at offset 0 of the image, so the
// volume label is a property of the *whole disk* as well as of the partition
// holding the ISO. Anaconda resolves inst.stage2=hd:LABEL=<iso label> to
// /dev/sda and mounts it; with the whole disk mounted, no partition on that
// disk can be opened exclusively any more, and the kickstart fetch fails with
//
//	mount: /run/install/tmpmnt0: fsconfig() failed: /dev/sda3: Can't open blockdev
//	Warning: Can't get kickstart from /dev/sda3:/ks.cfg
//
// over a partition that is present, correctly labelled and holding a perfectly
// good kickstart. Two parts of the same installer, both right, over one disk.
//
// The kernel concatenates every initramfs it is given, so a second one holding
// /ks.cfg puts the file in the initramfs root before any disk is touched, and
// `inst.ks=file:/ks.cfg` reads it from there. Nothing is mounted, so nothing
// can be busy. This is the same mechanism driver updates and firmware blobs
// ride in on.

// cpioNewcFile builds a single-file initramfs in the newc format the kernel
// expects. Small enough to hand to GRUB as an extra initrd.
func cpioNewcFile(name string, data []byte) ([]byte, error) {
	if name == "" {
		return nil, fmt.Errorf("compose: cpio entry needs a name")
	}
	var b bytes.Buffer
	// Fields are eight hex digits each, in a fixed order. Mode 0100644 is a
	// regular file readable by all; mtime 0 keeps the build reproducible,
	// which is the same reason SOURCE_DATE_EPOCH exists elsewhere here.
	write := func(entryName string, content []byte, mode uint32) {
		hdr := fmt.Sprintf("070701%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X",
			0,                // c_ino
			mode,             // c_mode
			0,                // c_uid
			0,                // c_gid
			1,                // c_nlink
			0,                // c_mtime
			len(content),     // c_filesize
			0,                // c_devmajor
			0,                // c_devminor
			0,                // c_rdevmajor
			0,                // c_rdevminor
			len(entryName)+1, // c_namesize, including the NUL
			0,                // c_check, unused for newc
		)
		b.WriteString(hdr)
		b.WriteString(entryName)
		b.WriteByte(0)
		pad4(&b)
		b.Write(content)
		pad4(&b)
	}
	write(name, data, 0o100644)
	// Every cpio archive ends with this entry, and the kernel needs it: an
	// archive without it is read as truncated and dropped in silence.
	write("TRAILER!!!", nil, 0)
	return b.Bytes(), nil
}

// pad4 pads to the 4-byte boundary the newc format aligns on.
func pad4(b *bytes.Buffer) {
	for b.Len()%4 != 0 {
		b.WriteByte(0)
	}
}

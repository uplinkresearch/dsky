package diskutil

import (
	"fmt"
	"strings"
)

// Erase and prepare on Windows runs diskpart twice: once to empty the disk and
// make one partition, and once to format it. Both scripts were checked against
// diskpart on Windows 11 in the VM, command by command, before being written
// here.

// partitionScript empties the disk and makes one partition spanning it.
//
// diskpart's clean removes a disk's partitions but not its partition style: a
// GPT disk is still GPT afterwards. And convert refuses a disk that is already
// in the style asked for, stopping the script with "The disk you specified is
// not MBR formatted" -- reproduced in the VM on a GPT disk with no volumes,
// which is exactly how preparing a real Samsung stick failed. So convert is
// only asked for when the disk is in some other style or none.
//
// When the style could not be read, convert is still run but told to carry on
// if it fails (noerr): the disk is then either already in the style asked for,
// which is the common reason convert fails, or the format run after this one
// fails loudly on its own.
func partitionScript(index int, scheme, style string, styleKnown bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "select disk %d\r\n", index)
	// `detail disk` used to sit here, above `clean`, under a comment saying it
	// would refuse to proceed if diskpart disagreed about what this disk is.
	// It could not: both commands go into one script fed to `diskpart /s`, so
	// clean ran whatever detail printed, and nothing read the output for a
	// model or a size. A comment describing a check that does not exist is
	// worse than no comment, because the next person stops looking.
	//
	// The check it described is real and now happens before this script is
	// written at all -- see confirmDiskNumber, which asks the Storage module
	// rather than parsing diskpart's own words, since those are translated and
	// this must not depend on the language Windows is installed in.
	fmt.Fprintf(&b, "clean\r\n")
	switch {
	case !styleKnown:
		fmt.Fprintf(&b, "convert %s noerr\r\n", scheme)
	case style != scheme:
		fmt.Fprintf(&b, "convert %s\r\n", scheme)
	}
	fmt.Fprintf(&b, "create partition primary\r\n")
	fmt.Fprintf(&b, "exit\r\n")
	return b.String()
}

// formatScript formats the disk's one partition and gives it a letter.
//
// It selects the partition rather than relying on create partition to have
// left the new volume selected: on a removable drive the volume arrives after
// diskpart has moved on, and format then fails with "There is no volume
// selected". Selecting the partition once the volume exists brings the volume
// with it. assign is harmless on a volume Windows has already lettered.
func formatScript(index int, fsName, label string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "select disk %d\r\n", index)
	fmt.Fprintf(&b, "select partition 1\r\n")
	fmt.Fprintf(&b, "format fs=%s quick label=\"%s\"\r\n", fsName, label)
	fmt.Fprintf(&b, "assign\r\n")
	fmt.Fprintf(&b, "exit\r\n")
	return b.String()
}

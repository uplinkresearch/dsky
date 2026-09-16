package diskutil

import "fmt"

// Partition types Windows mounts. Every filesystem Erase and prepare offers --
// exFAT, FAT32, NTFS -- is one Windows reads, and a stick is prepared so it
// can be carried to a Windows machine, so its one partition says so.
const (
	gptBasicData = "EBD0A0A2-B9E5-4433-87C0-68B6B72699C7" // Microsoft basic data
	mbrNTFSexFAT = "7"                                    // NTFS and exFAT
	mbrFAT32LBA  = "c"                                    // FAT32, LBA addressed
)

// sfdiskScript is the partition table Erase and prepare writes on Linux: one
// partition spanning the disk, of a type Windows mounts.
//
// Without a type, sfdisk gives the partition its own default -- "Linux
// filesystem" on GPT, 83 on MBR -- and Windows assigns no drive letter to
// either. The filesystem inside was fine; Windows never looked at it. A Samsung
// stick prepared on Linux and filled with a payload showed up in diskpart on
// Windows 11 as a disk with "no volumes".
func sfdiskScript(opts Options) (label, script string) {
	label, typ := "gpt", gptBasicData
	if opts.Scheme == MBR {
		label, typ = "dos", mbrNTFSexFAT
		if opts.FS == FAT32 {
			typ = mbrFAT32LBA
		}
	}
	return label, fmt.Sprintf("label: %s\n,,%s\n", label, typ)
}

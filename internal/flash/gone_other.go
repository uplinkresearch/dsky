//go:build !windows

package flash

import "syscall"

// goneErrnos are the answers a Unix kernel gives for a disk that has left the
// bus. EIO is deliberately not among them: it is also what a bad sector
// says, and a bad sector is not something waiting for the stick fixes.
var goneErrnos = []syscall.Errno{syscall.ENODEV, syscall.ENXIO}

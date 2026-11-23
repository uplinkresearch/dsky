package flash

import "syscall"

// goneErrnos are the answers Windows gives for a disk that is no longer
// there. The first is the one a SanDisk stick produced when it reset itself
// partway through writing Windows 11 -- "A device which does not exist was
// specified" -- and then again during the readback of a later run. The event
// log showed the stick leaving the bus and arriving again within the same
// second, both times, with nobody near it.
var goneErrnos = []syscall.Errno{
	433,  // ERROR_NO_SUCH_DEVICE
	1167, // ERROR_DEVICE_NOT_CONNECTED
	55,   // ERROR_DEV_NOT_EXIST
	1617, // ERROR_DEVICE_REMOVED
	21,   // ERROR_NOT_READY: what a stick answers while it is resetting
}

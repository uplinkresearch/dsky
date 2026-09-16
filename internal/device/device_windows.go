package device

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"unsafe"

	"github.com/uplinkresearch/dsky/internal/winwmi"
	"golang.org/x/sys/windows"
)

// win32DiskDrive mirrors the WMI class (queried unelevated).
type win32DiskDrive struct {
	Index         uint32
	Model         string
	SerialNumber  string
	Size          uint64
	InterfaceType string
	MediaType     string
}

func list(_ context.Context) ([]Device, error) {
	var drives []win32DiskDrive
	if err := winwmi.Query("SELECT Index, Model, SerialNumber, Size, InterfaceType, MediaType FROM Win32_DiskDrive", &drives); err != nil {
		return nil, fmt.Errorf("device: WMI disk query: %w", err)
	}
	sysDisk, sysErr := systemDiskNumber()

	mounts := driveLettersByDisk()

	var out []Device
	for _, d := range drives {
		dev := Device{
			ID:        fmt.Sprintf(`\\.\PhysicalDrive%d`, d.Index),
			Index:     int(d.Index),
			Model:     strings.TrimSpace(d.Model),
			Serial:    strings.TrimSpace(d.SerialNumber),
			SizeBytes: int64(d.Size),
			Bus:       strings.ToLower(d.InterfaceType),
			Removable: strings.Contains(strings.ToLower(d.MediaType), "removable"),
			Mounts:    mounts[int(d.Index)],
		}
		if sysErr == nil && int(d.Index) == sysDisk {
			dev.System = true
		}
		out = append(out, dev)
	}
	// When the system disk could not be determined, fail safe: nothing
	// non-USB is flashable anyway, and USB system disks are marked below.
	sort.Slice(out, func(i, j int) bool {
		fi, fj := out[i].Flashable(), out[j].Flashable()
		if fi != fj {
			return fi
		}
		return out[i].Index < out[j].Index
	})
	return out, nil
}

// systemDiskNumber finds the physical disk backing the Windows volume.
func systemDiskNumber() (int, error) {
	drive := os.Getenv("SystemDrive") // e.g. "C:"
	if len(drive) != 2 || drive[1] != ':' {
		drive = "C:"
	}
	h, err := windows.CreateFile(
		windows.StringToUTF16Ptr(`\\.\`+drive),
		0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return -1, err
	}
	defer windows.CloseHandle(h)
	return diskNumberOfVolumeHandle(h)
}

const ioctlVolumeGetVolumeDiskExtents = 0x00560000

type diskExtent struct {
	DiskNumber   uint32
	_            uint32 // alignment
	StartingByte int64
	ExtentLength int64
}

type volumeDiskExtents struct {
	NumberOfDiskExtents uint32
	_                   uint32
	Extents             [4]diskExtent
}

func diskNumberOfVolumeHandle(h windows.Handle) (int, error) {
	var out volumeDiskExtents
	var ret uint32
	err := windows.DeviceIoControl(h, ioctlVolumeGetVolumeDiskExtents,
		nil, 0, (*byte)(unsafe.Pointer(&out)), uint32(unsafe.Sizeof(out)), &ret, nil)
	if err != nil {
		return -1, err
	}
	if out.NumberOfDiskExtents < 1 {
		return -1, fmt.Errorf("volume reports no disk extents")
	}
	return int(out.Extents[0].DiskNumber), nil
}

// driveLettersByDisk maps disk number -> mounted drive letters, via each
// letter's volume extents (works unelevated).
func driveLettersByDisk() map[int][]string {
	out := map[int][]string{}
	mask, err := windows.GetLogicalDrives()
	if err != nil {
		return out
	}
	for i := 0; i < 26; i++ {
		if mask&(1<<i) == 0 {
			continue
		}
		letter := string(rune('A'+i)) + ":"
		h, err := windows.CreateFile(
			windows.StringToUTF16Ptr(`\\.\`+letter),
			0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
			nil, windows.OPEN_EXISTING, 0, 0)
		if err != nil {
			continue
		}
		if n, err := diskNumberOfVolumeHandle(h); err == nil {
			out[n] = append(out[n], letter)
		}
		windows.CloseHandle(h)
	}
	return out
}

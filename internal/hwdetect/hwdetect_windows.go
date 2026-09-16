package hwdetect

import (
	"context"
	"sort"
	"strings"

	"github.com/uplinkresearch/dsky/internal/winwmi"
)

type win32System struct {
	Manufacturer string
	Model        string
}

type win32Processor struct {
	Name string
}

type win32PnPEntity struct {
	Name        string
	PNPClass    string
	HardwareID  []string
	PNPDeviceID string
}

func detect(_ context.Context) (*Hardware, error) {
	h := &Hardware{}

	var sys []win32System
	if err := winwmi.Query("SELECT Manufacturer, Model FROM Win32_ComputerSystem", &sys); err == nil && len(sys) > 0 {
		h.Vendor = strings.TrimSpace(sys[0].Manufacturer)
		h.Model = strings.TrimSpace(sys[0].Model)
	}
	var cpu []win32Processor
	if err := winwmi.Query("SELECT Name FROM Win32_Processor", &cpu); err == nil && len(cpu) > 0 {
		h.CPU = strings.TrimSpace(cpu[0].Name)
	}

	var ents []win32PnPEntity
	// Present devices only; a null PNPClass is fine (some devices lack it).
	_ = winwmi.Query("SELECT Name, PNPClass, HardwareID, PNPDeviceID FROM Win32_PnPEntity", &ents)

	seen := map[string]bool{}
	for _, e := range ents {
		hwid := firstPCIID(e.HardwareID)
		if hwid == "" {
			hwid = e.PNPDeviceID
		}
		if hwid == "" || seen[strings.ToUpper(hwid)] {
			continue
		}
		seen[strings.ToUpper(hwid)] = true
		d := Device{Name: strings.TrimSpace(e.Name), Class: e.PNPClass, HardwareID: strings.ToUpper(hwid)}
		switch strings.ToLower(e.PNPClass) {
		case "display":
			ven, _ := venDevFromHWID(hwid)
			d.GPUVendor = gpuVendor(ven)
			h.GPUs = append(h.GPUs, d)
		case "net":
			if strings.HasPrefix(strings.ToUpper(hwid), "PCI\\") || strings.HasPrefix(strings.ToUpper(hwid), "USB\\") {
				h.NICs = append(h.NICs, d)
			}
		}
		h.Devices = append(h.Devices, d)
	}
	sort.Slice(h.Devices, func(i, j int) bool { return h.Devices[i].HardwareID < h.Devices[j].HardwareID })
	return h, nil
}

// firstPCIID returns the first PCI/USB hardware ID from a list (the most
// specific one, which WMI lists first).
func firstPCIID(ids []string) string {
	for _, id := range ids {
		u := strings.ToUpper(id)
		if strings.HasPrefix(u, "PCI\\") || strings.HasPrefix(u, "USB\\") {
			return id
		}
	}
	if len(ids) > 0 {
		return ids[0]
	}
	return ""
}

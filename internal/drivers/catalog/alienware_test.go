package catalog

import (
	"compress/gzip"
	"io"
	"os"
	"strings"
	"testing"
)

// The fixtures are Dell's own files, trimmed: the consumer index with the m16
// R2, two Aurora R16 entries, one non-Alienware system and an M17x, still
// UTF-16 as Dell publishes it; and the m16 R2's full catalog.

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasSuffix(name, ".gz") {
		zr, err := gzip.NewReader(strings.NewReader(string(b)))
		if err != nil {
			t.Fatal(err)
		}
		if b, err = io.ReadAll(zr); err != nil {
			t.Fatal(err)
		}
	}
	return b
}

// The index is UTF-16. It is read as such, and only Alienware systems come out
// of it, each with the catalog path and the SHA-256 it is checked against.
func TestTheAlienwareIndexIsReadFromUTF16(t *testing.T) {
	b := readFixture(t, "CatalogIndexPC-sample.xml")
	if !looksXML(b[:8]) {
		t.Error("a UTF-16 catalog is not recognised as XML, so expanding its cab would find nothing")
	}
	systems, err := parseAlienwareIndex(b)
	if err != nil {
		t.Fatal(err)
	}
	// Alienware is known by brand, not by name: Dell calls one of them
	// "M17xR4", and an Inspiron sits in the same index.
	var m16 *alienwareSystem
	ids := map[string]bool{}
	for i, s := range systems {
		ids[s.SystemID] = true
		if s.SystemID == "0C91" {
			m16 = &systems[i]
		}
	}
	if ids["07E8"] {
		t.Error("the Inspiron 24-5488 came out of the index as Alienware")
	}
	if !ids["05AE"] || !ids["0CD2"] {
		t.Errorf("Alienware systems missing: %v", ids)
	}
	if m16 == nil {
		t.Fatalf("the m16 R2 (0C91) is not among %d systems", len(systems))
	}
	if m16.Model != "Alienware m16 R2" || !strings.HasSuffix(m16.Path, "Alienware_Notebook_0C91.cab") || len(m16.SHA256) != 64 {
		t.Errorf("m16 R2 read as %+v", m16)
	}
}

// A model's drivers are driver packages only -- not BIOS, firmware or
// applications -- for the OS, the newest release per set of devices, each
// pinned by Dell's SHA-256 and unpacked with Dell's switches.
func TestAnAlienwareModelIsItsNewestDriverPackages(t *testing.T) {
	cat := readFixture(t, "Alienware_Notebook_0C91.xml.gz")
	packs, err := alienwareComponents("Alienware m16 R2", [][]byte{cat}, "win11", "x64")
	if err != nil {
		t.Fatal(err)
	}
	// The catalog holds 60 driver packages for Windows 11 -- ten releases of
	// the Intel graphics driver alone, 16.6 GB -- and a model needs one of
	// each: about twenty, a couple of gigabytes.
	if len(packs) < 15 || len(packs) > 30 {
		t.Fatalf("%d packages for the m16 R2; one release per driver is about twenty", len(packs))
	}
	var total int64
	count := map[string]int{}
	for _, p := range packs {
		total += p.Size
		switch {
		case strings.Contains(p.Component, "Intel Graphics"):
			count["intel graphics"]++
		case strings.Contains(p.Component, "Bluetooth"):
			count["bluetooth"]++
		case strings.Contains(p.Component, "Wi-Fi"):
			count["wi-fi"]++
		case strings.Contains(p.Component, "Management Engine"):
			count["management engine"]++
		}
	}
	for driver, n := range count {
		if n != 1 {
			t.Errorf("%d releases of the %s driver; only the newest belongs on a stick", n, driver)
		}
	}
	if total > 4<<30 {
		t.Errorf("%d MB of drivers for one laptop", total>>20)
	}
	seenURL := map[string]bool{}
	seenID := map[string]bool{}
	var names []string
	for _, p := range packs {
		names = append(names, p.Component)
		low := strings.ToLower(p.Component + " " + p.URL)
		if strings.Contains(low, "bios") || strings.Contains(low, "command center") || strings.Contains(low, "firmware") && !strings.Contains(low, "driver") {
			t.Errorf("not a driver: %s", p.Component)
		}
		if len(p.SHA256) != 64 || !strings.HasPrefix(p.URL, "https://downloads.dell.com/") || p.Format != "exe" {
			t.Errorf("package not usable as staged: %+v", p)
		}
		if strings.Join(p.Extract, " ") != "/s /e={dir}" {
			t.Errorf("%s would not unpack: %v", p.Component, p.Extract)
		}
		if seenURL[p.URL] {
			t.Errorf("the same package twice: %s", p.URL)
		}
		seenURL[p.URL] = true
		if seenID[p.ID()] {
			t.Errorf("two packages share the id %s", p.ID())
		}
		seenID[p.ID()] = true
		if len(p.ID()) > 70 {
			t.Errorf("id too long: %s", p.ID())
		}
	}
	joined := strings.ToLower(strings.Join(names, "|"))
	for _, want := range []string{"intel platform monitoring technology", "nvidia", "realtek"} {
		if !strings.Contains(joined, want) {
			t.Errorf("no %s driver among: %s", want, strings.Join(names, "; "))
		}
	}

	// And nothing for an OS the catalog does not cover.
	none, err := alienwareComponents("Alienware m16 R2", [][]byte{cat}, "win11", "arm64")
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Errorf("%d ARM64 packages from an x64-only catalog", len(none))
	}
}

// Of two releases for the same devices, only the newer is kept -- even when the
// newer one's name and device list have both changed, as long as a device is
// shared. A driver for a device with no PCI ID, like a USB Bluetooth radio, is
// linked to its other releases by Dell's component ID instead.
func TestOnlyTheNewestReleaseForTheSameDevicesIsKept(t *testing.T) {
	catalog := `<?xml version="1.0" encoding="utf-8"?>
<Manifest xmlns="openmanage/cm/dm">
` + component("OLD", "2025-01-01T00:00:00", "Killer Wi-Fi Driver", "1", "2725") +
		component("NEW", "2026-03-01T00:00:00", "Intel Killer BE1750 Wi-Fi Driver", "2", "2725", "272B") +
		component("BTOLD", "2024-06-01T00:00:00", "Bluetooth UWD Driver", "77") +
		component("BTNEW", "2025-06-01T00:00:00", "Intel and Killer Bluetooth Driver", "77") +
		component("CARD", "2025-06-01T00:00:00", "Card Reader Driver", "9", "522A") + `
</Manifest>`
	packs, err := alienwareComponents("Alienware x", [][]byte{[]byte(catalog)}, "win11", "x64")
	if err != nil {
		t.Fatal(err)
	}
	var urls []string
	for _, p := range packs {
		urls = append(urls, p.URL)
	}
	got := strings.Join(urls, " ")
	if len(packs) != 3 || !strings.Contains(got, "/NEW.EXE") || !strings.Contains(got, "/BTNEW.EXE") || !strings.Contains(got, "/CARD.EXE") {
		t.Errorf("kept %s", got)
	}
}

func component(file, when, name, componentID string, devices ...string) string {
	pci := ""
	for _, d := range devices {
		pci += `<PCIInfo vendorID="8086" deviceID="` + d + `" subVendorID="1028" subDeviceID="` + file + `"/>`
	}
	return `<SoftwareComponent path="F/` + file + `.EXE" size="10" dateTime="` + when + `" vendorVersion="1" dellVersion="A00">
  <Name><Display lang="en">` + name + `</Display></Name>
  <ComponentType value="DRVR"/>
  <Category value="NI"/>
  <SupportedDCHDevices><Device componentID="` + componentID + `">` + pci + `</Device></SupportedDCHDevices>
  <SupportedOperatingSystems><OperatingSystem osCode="W21P4" osArch="x64"/></SupportedOperatingSystems>
  <Cryptography><Hash algorithm="SHA256">` + strings.Repeat("a", 64) + `</Hash></Cryptography>
</SoftwareComponent>
`
}

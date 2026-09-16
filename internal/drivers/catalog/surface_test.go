package catalog

import (
	"strings"
	"testing"
)

// The model table is read from Microsoft's own page: computers only, not the
// docks and Surface Hub that share the table.
func TestSurfaceModelsComeFromTheLearnPage(t *testing.T) {
	models := parseSurfaceModels(readFixture(t, "surface-learn.html"))
	if len(models) < 40 {
		t.Fatalf("%d Surface models; the page lists about fifty computers", len(models))
	}
	names := map[string]string{}
	for _, m := range models {
		names[m.Name] = m.ID
		low := strings.ToLower(m.Name)
		if strings.Contains(low, "dock") || strings.Contains(low, "hub") {
			t.Errorf("not a computer: %s", m.Name)
		}
	}
	if names["Surface Laptop 6"] != "105946" {
		t.Errorf("Surface Laptop 6 missing or mislinked: %v", names["Surface Laptop 6"])
	}
}

// A model's pack is its MSI for the OS, unpacked by an administrative install
// and swept -- not installed, and not claimed for the wrong architecture.
func TestASurfaceModelIsItsDriverMSI(t *testing.T) {
	page := readFixture(t, "surface-details-105946.html")
	p, ok := parseSurfaceDetails(page, "Surface Laptop 6 for Business", "win11", "x64")
	if !ok {
		t.Fatal("no Windows 11 MSI found on the page")
	}
	if p.URL != "https://download.microsoft.com/download/a53facb0-c939-4302-a0d3-53aa18217230/SurfaceLaptop6forBusiness_Win11_22631_26.072.19202.0.msi" {
		t.Errorf("url %s", p.URL)
	}
	if p.Size != 1034932224 || p.Format != "msi" || p.Install != "extract-then-sweep" || p.OSVersion != "22631" || p.Version != "26.072.19202.0" {
		t.Errorf("pack read as %+v", p)
	}
	if strings.Join(p.Extract, " ") != "/qn TARGETDIR={dir}" {
		t.Errorf("extract %v", p.Extract)
	}
	if _, ok := parseSurfaceDetails(page, "Surface Laptop 6 for Business", "win10", "x64"); ok {
		t.Error("a Windows 10 MSI was found on a page that has none")
	}
	if _, ok := parseSurfaceDetails(page, "Surface Laptop 6 for Business", "win11", "arm64"); ok {
		t.Error("an Intel Surface was offered as ARM64")
	}
}

// Arm Surfaces are known by name, including the two whose names do not say.
func TestSurfaceArmModelsAreRecognised(t *testing.T) {
	for name, want := range map[string]string{
		"Surface Pro 11th Edition (Snapdragon)":         "arm64",
		"Surface Pro 9 with 5G (SQ3)":                   "arm64",
		"Surface Pro X":                                 "arm64",
		"Surface Laptop 13-inch 1st Edition":            "arm64",
		"Surface Pro 12-inch 1st Edition":               "arm64",
		"Surface Pro for Business 11th Edition (Intel)": "x64",
		"Surface Laptop 6 for Business":                 "x64",
		"Surface Pro 7+ and Surface Pro 7+ (LTE)":       "x64",
	} {
		if got := surfaceArch(name, "x.msi"); got != want {
			t.Errorf("%s: %s, want %s", name, got, want)
		}
	}
}

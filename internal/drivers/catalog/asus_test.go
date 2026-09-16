package catalog

import (
	"encoding/json"
	"strings"
	"testing"
)

func asusFixture(t *testing.T, name string) *asusDriverList {
	t.Helper()
	var d asusDriverList
	if err := json.Unmarshal(readFixture(t, name), &d); err != nil {
		t.Fatal(err)
	}
	return &d
}

// An ASUS laptop's drivers are its driver installers -- not MyASUS, the
// recovery image, Store links or utilities -- each run with the switches ASUS
// publishes, pinned by ASUS's SHA-256, gated to the model code the laptop
// reports, and only the newest release of each.
func TestAnASUSLaptopIsItsDriverInstallers(t *testing.T) {
	packs := asusComponents("UX3405MA", asusFixture(t, "asus-drivers-UX3405MA.json.gz"), "win11", false)
	// ASUS lists 55 entries for this laptop, several releases of most drivers;
	// one of each is about twenty, a gigabyte.
	if len(packs) < 15 || len(packs) > 30 {
		t.Fatalf("%d drivers for the UX3405MA", len(packs))
	}
	graphics := 0
	for _, p := range packs {
		low := strings.ToLower(p.Component)
		// The recovery image and the Store-only apps name no hardware and are
		// not drivers. MyASUS Splendid does name hardware -- it is the panel's
		// colour profile -- and belongs.
		if strings.Contains(low, "winre") || strings.Contains(low, "codec console") || strings.Contains(p.URL, "microsoft.com") {
			t.Errorf("not a driver: %s", p.Component)
		}
		if !strings.HasPrefix(p.URL, "https://dlcdnets.asus.com/pub/") || len(p.SHA256) != 64 {
			t.Errorf("not pinned to ASUS's download: %+v", p)
		}
		if p.Format == "exe" {
			if p.Install != "exe" || strings.Join(p.Args, " ") != "/SUPPRESSMSGBOXES /VERYSILENT /NORESTART" || p.Gate != "UX3405MA" {
				t.Errorf("installer %s would not run unattended on its own model: %+v", p.Component, p)
			}
		}
		if strings.Contains(low, "graphics") {
			graphics++
		}
	}
	if graphics != 1 {
		t.Errorf("%d Intel graphics drivers; the list has two releases and only the newer belongs", graphics)
	}
	for _, p := range packs {
		if strings.Contains(p.Component, "Graphics") && !strings.Contains(p.URL, "8860") {
			t.Errorf("kept the older graphics driver: %s", p.URL)
		}
	}
}

// A NUC's drivers are its INF driver pack, swept -- not the per-device zips
// beside it, which the pack already holds.
func TestANUCIsItsINFDriverPack(t *testing.T) {
	packs := asusComponents("ASUS NUC 13 Pro Kit", asusFixture(t, "asus-drivers-NUC13Pro.json.gz"), "win11", true)
	if len(packs) == 0 {
		t.Fatal("no driver pack for the NUC 13 Pro")
	}
	for _, p := range packs {
		if !strings.Contains(strings.ToLower(p.Component), "inf driver pack") {
			t.Errorf("not the INF pack: %s", p.Component)
		}
		if p.Vendor != NUC || p.Format != "zip" || p.Install != "pnputil-sweep" || !strings.HasSuffix(p.URL, ".zip") {
			t.Errorf("pack would not be swept: %+v", p)
		}
		if p.Gate != "" {
			t.Errorf("a swept pack needs no model gate: %+v", p)
		}
	}
	if len(asusComponents("ASUS NUC 13 Pro Kit", asusFixture(t, "asus-drivers-NUC13Pro.json.gz"), "win10", true)) != 0 {
		t.Error("Windows 11 drivers offered for Windows 10")
	}
}

func TestASUSFileSizesAreRead(t *testing.T) {
	for in, want := range map[string]int64{"4.35 MB": 4561305, "55 KB": 56320, "1.08 GB": 1159641169, "": 0} {
		if got := asusBytes(in); got != want {
			t.Errorf("%q: %d, want %d", in, got, want)
		}
	}
}

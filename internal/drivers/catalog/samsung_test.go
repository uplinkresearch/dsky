package catalog

import (
	"encoding/json"
	"strings"
	"testing"
)

func samsungFixture(t *testing.T, code string) *samsungDetails {
	t.Helper()
	var d samsungDetails
	if err := json.Unmarshal(readFixture(t, "samsung-"+code+".json"), &d); err != nil {
		t.Fatal(err)
	}
	return &d
}

// A Galaxy Book with a driver pack is that pack, swept -- not the WinPE pack
// beside it, the BIOS, or the separate zips the pack already holds.
func TestAGalaxyBookIsItsDriverPack(t *testing.T) {
	packs := samsungComponents("Galaxy Book4 Pro (NP944XGK)", samsungFixture(t, "NP944XGK-KG1US"), "win11")
	if len(packs) != 1 {
		t.Fatalf("%d packages; a model with a driver pack is its pack", len(packs))
	}
	p := packs[0]
	if p.Component != "Windows11 DriverPack" || p.Size != 1078457635 || p.Format != "zip" || p.Install != "pnputil-sweep" {
		t.Errorf("pack read as %+v", p)
	}
	if p.URL != "https://downloadcenter.samsung.com/content/DR/202603/20260316092831370/BASW-A3973A0M_1063.zip" {
		t.Errorf("url %s", p.URL)
	}
	if len(samsungComponents("Galaxy Book4 Pro (NP944XGK)", samsungFixture(t, "NP944XGK-KG1US"), "win10")) != 0 {
		t.Error("Windows 11 drivers offered for Windows 10")
	}
}

// Model names are what a person calls the machine -- marketing name and base
// code -- with every regional code it is sold under.
func TestGalaxyBookModelsGroupTheirRegionalCodes(t *testing.T) {
	var raw map[string][]struct {
		Code string `json:"modelCode"`
	}
	if err := json.Unmarshal(readFixture(t, "samsung-models-us.json"), &raw); err != nil {
		t.Fatal(err)
	}
	codes := map[string][]string{}
	total := 0
	for name, l := range raw {
		for _, c := range l {
			codes[name] = append(codes[name], c.Code)
			total++
		}
	}
	models := groupSamsungModels(codes)
	byName := map[string]samsungModel{}
	for _, m := range models {
		byName[m.Name] = m
	}
	m, ok := byName["Galaxy Book4 Pro (NP944XGK)"]
	if !ok {
		t.Fatal("Galaxy Book4 Pro (NP944XGK) is not among the models")
	}
	if len(m.Codes) < 2 || !strings.HasPrefix(m.Codes[0], "NP944XGK-") {
		t.Errorf("its regional codes: %v", m.Codes)
	}
	if _, ok := byName["Galaxy Book4 Pro (NP964XGK)"]; !ok {
		t.Error("the 16-inch Book4 Pro was folded into the 14-inch one")
	}
	if len(models) >= total {
		t.Errorf("%d models for %d codes; regional codes were not grouped", len(models), total)
	}
}

// Without a pack -- the Galaxy Book2 has none -- its drivers are its separate
// zips, still never the WinPE pack or the BIOS.
func TestAGalaxyBookWithoutAPackIsItsDriverZips(t *testing.T) {
	d := samsungFixture(t, "NP944XGK-KG1US")
	drivers := d.Response.ResultData.Downloads.Drivers[:0]
	for _, it := range d.Response.ResultData.Downloads.Drivers {
		if it.Category != samsungDriverPack {
			drivers = append(drivers, it)
		}
	}
	d.Response.ResultData.Downloads.Drivers = drivers
	packs := samsungComponents("Galaxy Book4 Pro (NP944XGK)", d, "win11")
	if len(packs) < 10 {
		t.Fatalf("%d driver zips", len(packs))
	}
	for _, p := range packs {
		if strings.Contains(p.Component, "PE DriverPack") || strings.Contains(p.Component, "BIOS") || p.Format != "zip" {
			t.Errorf("not a driver zip: %s", p.Component)
		}
	}
}

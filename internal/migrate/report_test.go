package migrate

import (
	"strings"
	"testing"
)

// The report is read by people who will never see the manifest, so what it
// must never do is leave something out or promise something DSKY does not do.

func render(t *testing.T, name string) string {
	t.Helper()
	m := load(t, name)
	var b strings.Builder
	if err := Report(&b, m, "win11", "dsky vtest"); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return b.String()
}

func TestReportSaysEverythingTheManifestSays(t *testing.T) {
	out := render(t, "customized")
	for _, want := range []string{
		"WWFO-OPS-01",                       // the machine
		"HP EliteDesk 800 G4 SFF",           // what it is
		"wwfo.internal",                     // where it belongs
		"OU=Ops,OU=Computers",               // and in which OU
		"AutoVue 2D Professional",           // the blocker's subject
		"known-incompatible list",           // and why
		"WWFO Field Sync",                   // the application nobody has placed
		"ArcGIS Pro",                        // one that is placed
		"Microsoft.DotNet.DesktopRuntime.6", // and from where
		"Adobe Acrobat Pro DC",              // the one to do by hand
		"HP Support Assistant",              // and the one deliberately left
		"10.8.4.31",                         // the static IP, reported not applied
		"WWFO-Guest",                        // the Wi-Fi name, key not carried
		"Ops Plotter",                       // a printer
		"\\\\wwfo-fs01\\gis",                // a mapped drive
		"taskbar.search_mode",               // a setting
		"USMT",                              // the data plan
		"not ready to approve",              // and that it is not approvable yet
	} {
		if !strings.Contains(strings.ToLower(out), strings.ToLower(want)) {
			t.Errorf("the report never mentions %q", want)
		}
	}
	// It is one file: no stylesheet, script or image fetched from anywhere.
	for _, bad := range []string{"<script", "src=\"http", "href=\"http", "@import"} {
		if strings.Contains(out, bad) {
			t.Errorf("the report is not self-contained: found %q", bad)
		}
	}
	// And it states the limits, on every machine, whatever the plan says.
	for _, want := range []string{"Saved passwords", "Wi-Fi keys", "Licences tied to a machine"} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not say that %q stay behind", want)
		}
	}
}

// A setting captured with no way to apply it is the case most likely to be
// read as "migrated". It has to say otherwise in the row itself.
func TestReportMarksSettingsItCannotApply(t *testing.T) {
	out := render(t, "office")
	i := strings.Index(out, "defaults.browser")
	if i < 0 {
		t.Fatal("the captured default browser is missing from the report")
	}
	row := out[i:]
	if end := strings.Index(row, "</tr>"); end > 0 {
		row = row[:end]
	}
	if !strings.Contains(row, "not migrated") {
		t.Errorf("the default-browser row does not say it is not migrated:\n%s", row)
	}
}

func TestReportShowsApprovalAndTheKioskHasNothingToMigrate(t *testing.T) {
	m := load(t, "office")
	if err := m.Approve("dusty"); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	if err := Report(&b, m, "win11", "dsky vtest"); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if !strings.Contains(out, "dusty approved this plan") {
		t.Error("an approved report does not say who approved it")
	}
	if !strings.Contains(out, m.Approval.ContentHash[:16]) {
		t.Error("an approved report does not show the hash the build checks")
	}

	kiosk := render(t, "kiosk")
	for _, want := range []string{"not in a domain", "not migrated — this machine has no local data", "None found."} {
		if !strings.Contains(kiosk, want) {
			t.Errorf("the kiosk report does not say %q", want)
		}
	}
}

// HTML escaping, checked on the thing a customer's software is most likely to
// be called: a name with an ampersand or a bracket in it.
func TestReportEscapesNames(t *testing.T) {
	m := load(t, "kiosk")
	m.Apps[0].DisplayName = `Smith & Sons <Practice> "Manager"`
	var b strings.Builder
	if err := Report(&b, m, "win11", "dsky vtest"); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if strings.Contains(out, "<Practice>") {
		t.Error("a name was written into the page unescaped")
	}
	if !strings.Contains(out, "Smith &amp; Sons") {
		t.Errorf("the escaped name is missing")
	}
}

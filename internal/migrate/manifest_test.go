package migrate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The three fixtures are the machines this feature is built for: an ordinary
// office PC, a heavily customised one with line-of-business software and a
// blocker, and a kiosk with nothing to migrate. Every later stage is tested
// against them, so they have to stay readable and valid.
var fixtures = []string{"office", "customized", "kiosk"}

func load(t *testing.T, name string) *Manifest {
	t.Helper()
	m, err := Load(filepath.Join("testdata", name+".json"))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return m
}

func TestFixturesAreValid(t *testing.T) {
	for _, name := range fixtures {
		m := load(t, name)
		if m.SchemaVersion != SchemaVersion {
			t.Errorf("%s: schema_version %q, want %q", name, m.SchemaVersion, SchemaVersion)
		}
		if m.Source.Hostname == "" || m.Target.Hostname == "" {
			t.Errorf("%s: a fixture with no hostname teaches nothing", name)
		}
	}
}

// A manifest survives a trip through the file and back unchanged: the review
// reads, edits and writes the same file, and a field silently dropped there
// would be a machine silently missing something.
func TestSaveAndLoadKeepEverything(t *testing.T) {
	for _, name := range fixtures {
		m := load(t, name)
		path := filepath.Join(t.TempDir(), "manifest.json")
		if err := m.Save(path); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		again, err := Load(path)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		want, _ := json.Marshal(m)
		got, _ := json.Marshal(again)
		if string(want) != string(got) {
			t.Errorf("%s: changed by saving and loading\n before: %s\n  after: %s", name, want, got)
		}
	}
}

// An unknown field is refused rather than ignored. A manifest from a newer
// DSKY may say something this build would drop, and dropping half a migration
// plan without a word is worse than refusing to read it.
func TestUnknownFieldsAreRefused(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "kiosk.json"))
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(b), `"notes":`, `"restore_bitlocker_keys": true, "notes":`, 1)
	if _, err := Parse([]byte(edited)); err == nil || !strings.Contains(err.Error(), "restore_bitlocker_keys") {
		t.Errorf("unknown field accepted or unnamed: %v", err)
	}
	if _, err := Parse([]byte(strings.Replace(string(b), `"1.0"`, `"2.0"`, 1))); err == nil {
		t.Error("a manifest from a later schema was accepted")
	}
}

func TestValidateRefusesWhatCannotBeBuilt(t *testing.T) {
	cases := []struct {
		what string
		edit func(*Manifest)
		says string
	}{
		{"no schema version", func(m *Manifest) { m.SchemaVersion = "" }, "schema_version"},
		{"no generated_by", func(m *Manifest) { m.GeneratedBy = "" }, "generated_by"},
		{"two apps with one id", func(m *Manifest) { m.Apps = append(m.Apps, m.Apps[0]) }, "used twice"},
		{"app with no name", func(m *Manifest) { m.Apps[0].DisplayName = "" }, "display_name"},
		{"unknown source kind", func(m *Manifest) { m.Apps[0].SourceKind = "chocolatey" }, "source_kind"},
		{"unknown status", func(m *Manifest) { m.Apps[0].Resolution.Status = "maybe" }, "status"},
		{"resolved with no method", func(m *Manifest) { m.Apps[0].Resolution.Method = "" }, "method is required"},
		{"resolved with no ref", func(m *Manifest) { m.Apps[0].Resolution.Ref = "" }, "ref is required"},
		{"resolved by nobody", func(m *Manifest) { m.Apps[0].Resolution.ResolvedBy = "" }, "resolved_by"},
		{"confidence over one", func(m *Manifest) { m.Apps[0].Resolution.Confidence = 1.5 }, "confidence"},
		{"usmt with no store", func(m *Manifest) { m.Data = Data{Strategy: DataUSMT} }, "store_path"},
		{"usmt with no users", func(m *Manifest) { m.Data = Data{Strategy: DataUSMT, StorePath: `\\fs\store`} }, "users"},
		{"unknown data strategy", func(m *Manifest) { m.Data.Strategy = "robocopy" }, "strategy"},
		{"unknown severity", func(m *Manifest) { m.Compat = []Compat{{Severity: "scary", Reason: "x"}} }, "severity"},
		{"compat with no reason", func(m *Manifest) { m.Compat = []Compat{{Severity: Warning, Subject: "x"}} }, "reason"},
		{"an OU with no domain", func(m *Manifest) { m.Identity.DomainFQDN = "" }, "domain_fqdn is empty"},
		{"a drive that is not a letter", func(m *Manifest) {
			m.Peripherals.MappedDrives = []MappedDrive{{Letter: "S", UNC: `\\fs\s`}}
		}, "H:"},
		{"a drive that is not a share", func(m *Manifest) {
			m.Peripherals.MappedDrives = []MappedDrive{{Letter: "S:", UNC: "S:\\shared"}}
		}, "server"},
		{"a setting captured from nowhere", func(m *Manifest) {
			m.Settings = []Setting{{Key: "power.plan", Value: json.RawMessage(`"Balanced"`)}}
		}, "captured_from"},
		{"a setting applied by magic", func(m *Manifest) {
			m.Settings = []Setting{{Key: "power.plan", Value: json.RawMessage(`"x"`), CapturedFrom: "powercfg",
				Apply: map[string]ApplyMethod{"win11": {Method: "wishing", Ref: "x"}}}}
		}, "method"},
		{"a setting applied with no ref", func(m *Manifest) {
			m.Settings = []Setting{{Key: "power.plan", Value: json.RawMessage(`"x"`), CapturedFrom: "powercfg",
				Apply: map[string]ApplyMethod{"win11": {Method: ApplyRegistry}}}}
		}, "ref"},
		{"an empty config capture", func(m *Manifest) { m.Apps[0].ConfigCapture = &ConfigCapture{} }, "config_capture"},
		{"approved by nobody", func(m *Manifest) { m.Approval = Approval{Approved: true} }, "approved_by"},
	}
	for _, c := range cases {
		m := load(t, "office")
		c.edit(m)
		err := m.Validate()
		if err == nil {
			t.Errorf("%s: accepted", c.what)
			continue
		}
		if !strings.Contains(err.Error(), c.says) {
			t.Errorf("%s: error %q does not mention %q", c.what, err, c.says)
		}
	}
}

// Install order is what the runner walks: runtimes and database engines
// before the software that needs them, then by name so two runs of one
// manifest install in the same order and their logs can be compared.
func TestInstallOrderPutsPrerequisitesFirst(t *testing.T) {
	m := load(t, "customized")
	order := m.InstallOrder()
	if len(order) == 0 {
		t.Fatal("nothing to install in the customised fixture")
	}
	seenLater := false
	for _, a := range order {
		if a.Resolution.Order > OrderPrereq {
			seenLater = true
		} else if seenLater {
			t.Errorf("%s (order %d) comes after a later one", a.DisplayName, a.Resolution.Order)
		}
		if !a.Resolution.Installable() {
			t.Errorf("%s is in the install order but not installable", a.DisplayName)
		}
	}
	// Manual work is not an install; it is a line on the checklist.
	for _, a := range order {
		if a.Resolution.Method == MethodManual {
			t.Errorf("%s is manual and should not be installed", a.DisplayName)
		}
	}
	if man := m.Manual(); len(man) != 1 || man[0].ID != "adobeacrobatpro" {
		t.Errorf("manual list: %+v", man)
	}
}

func TestCountsMatchTheFixtures(t *testing.T) {
	office := load(t, "office").Count("win11")
	if office.Apps != 7 || office.Resolved != 5 || office.Dropped != 2 || office.Unmapped != 0 {
		t.Errorf("office: %+v", office)
	}
	// One setting (the default browser) is captured with no way to apply it on
	// Windows 11; it must still be counted as captured, and not as applied.
	if office.Settings != 4 || office.SettingsApplied != 3 {
		t.Errorf("office settings: %+v", office)
	}
	cust := load(t, "customized").Count("win11")
	if cust.Unmapped != 1 || cust.Blocked != 1 || cust.Blockers != 1 || cust.UnackedBlockers != 1 || cust.Manual != 1 {
		t.Errorf("customized: %+v", cust)
	}
	if cust.Warnings != 3 {
		t.Errorf("customized warnings: %d, want 3", cust.Warnings)
	}
	kiosk := load(t, "kiosk").Count("win11")
	if kiosk.Apps != 2 || kiosk.Resolved != 2 || kiosk.Printers != 0 || kiosk.Drives != 0 {
		t.Errorf("kiosk: %+v", kiosk)
	}
}

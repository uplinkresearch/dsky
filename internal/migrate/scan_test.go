package migrate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testdata/scan-win10.json is what a real Windows 10 office PC's readings
// look like, awkward parts included: the same product registered in both
// hives, a runtime component Windows marks as part of another product, an
// uninstall key with no name at all, an Appx framework and an inbox app, a
// mapped drive that is not a drive, and one section the machine refused to
// answer. Everything below is about what the scan makes of that.

func readings(t *testing.T) Collector {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "scan-win10.json"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := ParseScanJSON(b)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func scanned(t *testing.T) (*Manifest, []error) {
	t.Helper()
	now := time.Date(2026, 9, 17, 14, 0, 0, 0, time.UTC)
	m, errs := Scan(readings(t), ScanOptions{Now: func() time.Time { return now }})
	if err := m.Validate(); err != nil {
		t.Fatalf("a scan produced a manifest DSKY will not read: %v", err)
	}
	return m, errs
}

func TestScanProducesAValidPlan(t *testing.T) {
	m, errs := scanned(t)
	if m.Source.Hostname != "MEAD-FRONT-02" || m.Source.ScannerUser != `MEAD\dbaker` {
		t.Errorf("source: %+v", m.Source)
	}
	// The release goes into the name, because "Windows 10 Pro" alone does not
	// say whether this machine is on the last servicing build.
	if m.Source.OS.ProductName != "Windows 10 Pro 22H2" || m.Source.OS.Build != "19045.4651" {
		t.Errorf("os: %+v", m.Source.OS)
	}
	if m.Source.Hardware.Serial != "7XQ4L23" || m.Source.Hardware.RAMBytes == 0 || len(m.Source.Hardware.Disks) != 1 {
		t.Errorf("hardware: %+v", m.Source.Hardware)
	}
	// The target starts as the same machine: same name, the edition it had,
	// and DSKY's own local administrator.
	if m.Target.Hostname != "MEAD-FRONT-02" || m.Target.OSEdition != "Professional" || m.Target.LocalAdmin != "uplink" {
		t.Errorf("target: %+v", m.Target)
	}
	// This machine answered everything it was asked, so nothing is an error;
	// the one hive it would not hand over is a note (see the test below).
	if len(errs) != 0 {
		t.Errorf("errors: %v", errs)
	}
	// Nothing is resolved by a scan: that is the resolver's work, and an
	// empty status is what it looks for.
	for _, a := range m.Apps {
		if a.Resolution.Status != StatusUnset {
			t.Errorf("%s came back from the scan already resolved: %+v", a.DisplayName, a.Resolution)
		}
	}
	if m.Source.ScanDurationS < 0 {
		t.Errorf("scan duration %v", m.Source.ScanDurationS)
	}
}

func TestScanListsWhatAPersonWouldRecognise(t *testing.T) {
	m, _ := scanned(t)
	got := map[string]App{}
	for _, a := range m.Apps {
		got[a.DisplayName] = a
	}
	for _, want := range []string{"Google Chrome", "Dentrix G7.6", "Adobe Acrobat Pro DC", "Slack",
		"Microsoft Visual C++ 2015-2022 Redistributable (x64) - 14.40.33810", "SpotifyAB.SpotifyMusic"} {
		if _, ok := got[want]; !ok {
			t.Errorf("%q is missing from the scan", want)
		}
	}
	for _, unwanted := range []string{
		"Microsoft Visual C++ 2015-2022 Additional Runtime - 14.40.33810", // SystemComponent: part of the redistributable
		"Microsoft.VCLibs.140.00",     // an Appx framework other packages need
		"Microsoft.WindowsCalculator", // ships with Windows anyway
		"",                            // an uninstall key with no name
	} {
		if _, ok := got[unwanted]; ok {
			t.Errorf("%q should not be in the list", unwanted)
		}
	}
	// One product in two hives is one application to reinstall, and the
	// 64-bit registration is the one that counts.
	if n := len(m.Apps); n != 6 {
		names := []string{}
		for _, a := range m.Apps {
			names = append(names, a.DisplayName+" ["+a.Arch+"]")
		}
		t.Errorf("%d applications: %v", n, names)
	}
	if c := got["Google Chrome"]; c.Arch != "x64" {
		t.Errorf("Chrome kept the 32-bit registration: %+v", c)
	}
	// Ids are readable, unique and legal: they end up in mapping tables, in
	// compat entries and in the verify diff, where somebody has to know what
	// they refer to.
	seen := map[string]bool{}
	for _, a := range m.Apps {
		if !idRe.MatchString(a.ID) {
			t.Errorf("%q is not a usable id", a.ID)
		}
		if seen[a.ID] {
			t.Errorf("id %q used twice", a.ID)
		}
		seen[a.ID] = true
	}
	if got["Dentrix G7.6"].ID != "dentrixg76" {
		t.Errorf("Dentrix id: %q", got["Dentrix G7.6"].ID)
	}
	// A trailing backslash on an install location is Windows' habit, not part
	// of the path.
	if strings.HasSuffix(got["Google Chrome"].InstallLocation, `\`) {
		t.Errorf("install location: %q", got["Google Chrome"].InstallLocation)
	}
	// The per-user application is kept, because the person who has it will
	// expect it on the new machine.
	if got["Slack"].SourceKind != SourceWin32 {
		t.Errorf("Slack: %+v", got["Slack"])
	}
}

func TestScanCleansUpIdentityAndDrives(t *testing.T) {
	m, _ := scanned(t)
	if m.Identity.DomainFQDN != "mead.local" || m.Identity.DomainNetBIOS != "MEAD" {
		t.Errorf("identity: %+v", m.Identity)
	}
	if m.Identity.ComputerOUDN != "OU=Front Desk,OU=Workstations,DC=mead,DC=local" {
		t.Errorf("the OU is what djoin needs: %q", m.Identity.ComputerOUDN)
	}
	// Groups every Windows machine has say nothing about this one, and a
	// group left with no members worth naming is dropped entirely.
	for _, g := range m.Identity.LocalGroups {
		for _, mem := range g.Members {
			if strings.HasPrefix(mem, "NT AUTHORITY") || strings.HasPrefix(mem, "BUILTIN") {
				t.Errorf("%s kept a well-known member: %q", g.Name, mem)
			}
		}
		if g.Name == "Users" {
			t.Error("the Users group had nothing but well-known members and should have been dropped")
		}
	}
	if len(m.Identity.LocalGroups) != 2 {
		t.Errorf("local groups: %+v", m.Identity.LocalGroups)
	}
	// Windows' own defaults in a local group say nothing about this machine:
	// its Guest and DefaultAccount, the local Administrator, and the two
	// memberships a domain join makes by itself. What is left is what
	// somebody added.
	byName := map[string][]string{}
	for _, g := range m.Identity.LocalGroups {
		byName[g.Name] = g.Members
	}
	if got := byName["Administrators"]; len(got) != 1 || got[0] != `MEAD\practice-it` {
		t.Errorf("Administrators: %v", got)
	}
	if got := byName["Remote Desktop Users"]; len(got) != 1 || got[0] != `MEAD\reception` {
		t.Errorf("Remote Desktop Users: %v", got)
	}
	for _, empty := range []string{"Users", "Guests", "System Managed Accounts Group"} {
		if got, ok := byName[empty]; ok {
			t.Errorf("%s holds only Windows' defaults and should have been dropped: %v", empty, got)
		}
	}

	// A drive letter that is not a letter, and a "mapped drive" pointing at a
	// local folder, are readings to drop rather than put in a plan.
	if len(m.Peripherals.MappedDrives) != 1 || m.Peripherals.MappedDrives[0].Letter != "S:" {
		t.Errorf("mapped drives: %+v", m.Peripherals.MappedDrives)
	}
}

// A machine in a workgroup is not in a domain, whatever it calls itself.
func TestScanTreatsAWorkgroupAsNoDomain(t *testing.T) {
	id := IdentityFrom(RawIdentity{DomainFQDN: "WORKGROUP", DomainNetBIOS: "WORKGROUP",
		LocalGroups: []LocalGroup{{Name: "Administrators", Members: []string{`.\kiosk`}}}}, "KIOSK-01")
	if id.Domained() || id.DomainNetBIOS != "" {
		t.Errorf("workgroup: %+v", id)
	}
	if len(id.LocalGroups) != 1 {
		t.Errorf("local groups should survive: %+v", id.LocalGroups)
	}
}

func TestScanCapturesOnlyAllowlistedSettings(t *testing.T) {
	m, _ := scanned(t)
	got := map[string]Setting{}
	for _, s := range m.Settings {
		got[s.Key] = s
	}
	if _, ok := got["desktop.wallpaper"]; ok {
		t.Error("a setting outside the allowlist was kept")
	}
	if n := len(m.Settings); n != 7 {
		t.Errorf("%d settings: %+v", n, got)
	}
	// The power plan is applied by GUID: the name is translated and differs
	// by language, the GUID does not.
	if ref := got[KeyPowerPlan].Apply["win11"].Ref; !strings.Contains(ref, "381b4222-f694-41f0-9685-ff5bb260df2e") {
		t.Errorf("power plan: %q", ref)
	}
	// Minutes come back from PowerShell as 30.0 and should read as 30.
	if v := string(got[KeySleepAC].Value); v != "30" {
		t.Errorf("sleep timeout: %s", v)
	}
	// "Show extensions" is HideFileExt inverted, and getting that backwards
	// would turn extensions off on every machine DSKY builds.
	if ref := got[KeyShowExtensions].Apply["win11"].Ref; !strings.HasSuffix(ref, "HideFileExt=0") {
		t.Errorf("show extensions: %q", ref)
	}
	// Windows 10's search box (2) is Windows 11's icon (1): the same value
	// means a different thing on the new machine.
	if ref := got[KeyTaskbarSearch].Apply["win11"].Ref; !strings.HasSuffix(ref, "SearchboxTaskbarMode=1") {
		t.Errorf("taskbar search: %q", ref)
	}
	// The default browser cannot be set on Windows 11 without forging a hash,
	// so it is captured and reported, never applied.
	if got[KeyDefaultBrowser].Appliable("win11") {
		t.Error("the default browser is not something DSKY can set")
	}
	if got[KeyDefaultBrowser].CapturedFrom == "" {
		t.Error("a setting DSKY cannot apply still has to say where it was read")
	}
}

// Print queues Windows creates for itself are not peripherals to recreate.
// On the first real machine, they outnumbered the real printers three to one.
func TestScanDropsWindowsOwnPrinters(t *testing.T) {
	m, _ := scanned(t)
	if len(m.Peripherals.Printers) != 2 {
		t.Fatalf("printers: %+v", m.Peripherals.Printers)
	}
	names := m.Peripherals.Printers[0].Name + "," + m.Peripherals.Printers[1].Name
	if !strings.Contains(names, "Front Desk HP") || !strings.Contains(names, "Statements") {
		t.Errorf("printers: %s", names)
	}
	// A network queue needs its share path and no driver; an IP queue needs
	// the driver and the address.
	for _, p := range m.Peripherals.Printers {
		if p.SharedPath != "" && (p.Port != "" || p.IP != "") {
			t.Errorf("a shared queue kept its local port: %+v", p)
		}
	}
}

// A hive that would not load is a note in the plan, and the scan is not
// partial for it: the rest of the machine was read.
func TestScanNotesWhatItCouldNotReadWithoutFailing(t *testing.T) {
	m, errs := scanned(t)
	var hive bool
	for _, n := range m.Notes {
		if strings.Contains(n, "hygienist") && strings.Contains(n, "could not be read") {
			hive = true
		}
	}
	if !hive {
		t.Errorf("the unreadable hive is not in the notes: %v", m.Notes)
	}
	// The note does not make the scan partial: nothing of the machine was
	// lost by it, and a scan that exits 3 over one busy hive would have the
	// operator chasing a problem that is not there.
	if len(errs) != 0 {
		t.Errorf("errors: %v", errs)
	}
}

// A section the machine refuses is an error and a note both, and everything
// else still comes back.
func TestScanReportsASectionItCouldNotRead(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "scan-win10.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	json.Unmarshal(b, &doc)
	doc["problems"] = []string{"printers: The RPC server is unavailable."}
	edited, _ := json.Marshal(doc)
	c, err := ParseScanJSON(edited)
	if err != nil {
		t.Fatal(err)
	}
	m, errs := Scan(c, ScanOptions{})
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "printers") {
		t.Fatalf("errors: %v", errs)
	}
	var said bool
	for _, n := range m.Notes {
		if strings.Contains(n, "printers could not be read") {
			said = true
		}
	}
	if !said {
		t.Errorf("notes: %v", m.Notes)
	}
	if len(m.Peripherals.Printers) != 0 {
		t.Errorf("printers came back from a failed reading: %+v", m.Peripherals.Printers)
	}
	if len(m.Apps) == 0 || len(m.Settings) == 0 {
		t.Error("one failed section took the rest of the machine with it")
	}
}

// The replacement machine starts out as the old one: same time zone, same
// locale. A new PC in the installer's time zone is noticed within the hour.
func TestScanTargetInheritsTimeAndLanguage(t *testing.T) {
	m, _ := scanned(t)
	if m.Target.Timezone != "Central Standard Time" || m.Target.Locale != "en-US" {
		t.Errorf("target: %+v", m.Target)
	}
}

func TestScanDecidesHowFilesMove(t *testing.T) {
	m, _ := scanned(t)
	// Files are local, so USMT, and only the domain profiles somebody has
	// used lately: not the year-old seasonal profile, not the local
	// administrator (a new machine has new local accounts).
	if m.Data.Strategy != DataUSMT {
		t.Fatalf("data: %+v", m.Data)
	}
	if strings.Join(m.Data.Users, ",") != `MEAD\hygienist,MEAD\reception` {
		t.Errorf("users: %v", m.Data.Users)
	}

	now := time.Date(2026, 9, 17, 14, 0, 0, 0, time.UTC)
	opts := ScanOptions{Now: func() time.Time { return now }}
	users := []UserProf{{Account: `MEAD\a`, LastLogon: &now}}
	// OneDrive and folder redirection both mean the files are not on this
	// disk, so copying them would be slower and riskier than letting them
	// come back on their own.
	if d := DataPlan(RawHints{OneDriveKFM: true}, users, opts); d.Strategy != DataKFM {
		t.Errorf("KFM: %+v", d)
	}
	if d := DataPlan(RawHints{RedirectedFolders: []string{`Documents -> \\fs\home`}}, users, opts); d.Strategy != DataRedirect {
		t.Errorf("redirection: %+v", d)
	}
	// Nobody has signed in for months: a kiosk or a spare, nothing to carry.
	old := now.Add(-365 * 24 * time.Hour)
	if d := DataPlan(RawHints{}, []UserProf{{Account: `MEAD\gone`, LastLogon: &old}}, opts); d.Strategy != DataNoneStrat {
		t.Errorf("stale profiles: %+v", d)
	}
}

func TestScanSurfacesWhatSomebodyHasToDecide(t *testing.T) {
	m, _ := scanned(t)
	by := map[string]Compat{}
	for _, c := range m.Compat {
		by[c.Subject] = c
	}
	// A 32-bit application with no 64-bit sibling: the thing most likely to
	// have no Windows 11 version at all.
	if c, ok := by["dentrixg76"]; !ok || c.Severity != Warning || !strings.Contains(c.Reason, "32-bit") {
		t.Errorf("Dentrix: %+v", c)
	}
	// Chrome is in both hives, so it is not a 32-bit-only application and
	// must not be reported as one.
	if c, ok := by["googlechrome"]; ok && strings.Contains(c.Reason, "32-bit") {
		t.Errorf("Chrome reported as 32-bit only: %+v", c)
	}
	if c, ok := by["oem42.inf (Dell Inc., System)"]; !ok || c.Severity != Warning {
		t.Errorf("unsigned driver: %+v", c)
	}
	if c, ok := by["Intel(R) Ethernet Connection I219-LM"]; !ok || !strings.Contains(c.Reason, "10.20.1.31/24") {
		t.Errorf("static IP: %+v", c)
	}
	if c, ok := by["Mead-Staff"]; !ok || c.Severity != Info {
		t.Errorf("wifi: %+v", c)
	}
	// Local files and no USMT supplied is the one blocker here: without it
	// the rebuild would quietly leave the files behind.
	if c, ok := by["USMT"]; !ok || c.Severity != Blocker {
		t.Errorf("USMT: %+v", c)
	}
	// Adobe licences are bound to the machine they were activated on.
	if c, ok := by["adobeacrobatprodc"]; !ok || c.Severity != Info || !strings.Contains(c.Reason, "tied to the machine") {
		t.Errorf("Adobe: %+v", c)
	}
	// And a scanned machine is never approvable on the spot: the blocker and
	// the unresolved applications are exactly what the review is for.
	if err := m.ReadyToApprove(); err == nil {
		t.Error("a freshly scanned manifest should not be ready to approve")
	}
}

// Whatever a machine answers, a scan produces a manifest DSKY can read, or
// says why not in one sentence.
func TestScanSurvivesRubbish(t *testing.T) {
	if _, err := ParseScanJSON(nil); err == nil {
		t.Error("empty output accepted")
	}
	if _, err := ParseScanJSON([]byte("powershell : Get-WmiObject : Access denied\r\nAt line:1")); err == nil {
		t.Error("an error message was read as a scan")
	} else if !strings.Contains(err.Error(), "Access denied") {
		t.Errorf("the error should quote what PowerShell said: %v", err)
	}
	// A machine that answered nothing at all still produces a manifest, with
	// a note for every section.
	c, err := ParseScanJSON([]byte(`{"hostname":"PC01","problems":["os: nope","hardware: nope","apps: nope","identity: nope","printers: nope","mapped_drives: nope","network: nope","settings: nope","profiles: nope","hints: nope","unsigned_drivers: nope"]}`))
	if err != nil {
		t.Fatal(err)
	}
	m, errs := Scan(c, ScanOptions{})
	if len(errs) != 11 {
		t.Errorf("%d errors, want one per section: %v", len(errs), errs)
	}
	if err := m.Validate(); err != nil {
		t.Errorf("a manifest from a machine that answered nothing is still valid: %v", err)
	}
	if m.Data.Strategy != DataNoneStrat {
		t.Errorf("with no profiles read, there is nothing to carry: %+v", m.Data)
	}
	// And it renders, because that report is how somebody finds out the scan
	// went badly.
	var b strings.Builder
	if err := Report(&b, m, "win11", "dsky vtest"); err != nil {
		t.Errorf("report: %v", err)
	}
}

// The scan writes a manifest that round-trips and a report beside it, which
// is what the operator carries away from the old machine.
func TestScanOutputSavesAndReloads(t *testing.T) {
	m, _ := scanned(t)
	dir := t.TempDir()
	path := filepath.Join(dir, ManifestName)
	if err := m.Save(path); err != nil {
		t.Fatal(err)
	}
	again, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := json.Marshal(m)
	got, _ := json.Marshal(again)
	if string(want) != string(got) {
		t.Error("a scanned manifest changed on the way to disk and back")
	}
}

// Windows' own servicing entries are not applications, and one product
// installed 32-bit and 64-bit is one product -- both learned from the first
// real machine, which reported its 7-Zip as having no 64-bit version while
// the 64-bit one was two rows above it.
func TestScanKnowsWhatIsNotAnApplication(t *testing.T) {
	raw := []RawApp{
		{DisplayName: "Update for Windows 10 for x64-based Systems (KB5001716)", SourceKind: SourceWin32, Arch: "x64"},
		{DisplayName: "Security Update for Microsoft Excel 2016 (KB4484119) 64-Bit Edition", SourceKind: SourceWin32, Arch: "x64"},
		{DisplayName: "Microsoft Update Health Tools", SourceKind: SourceWin32, Arch: "x64"},
		{DisplayName: "7-Zip 24.08 (x64 edition)", SourceKind: SourceWin32, Arch: "x64"},
		{DisplayName: "7-Zip 24.08", SourceKind: SourceWin32, Arch: "x86"},
		{DisplayName: "Lab Dental Suite", SourceKind: SourceWin32, Arch: "x86"},
	}
	apps := AppsFrom(raw)
	names := []string{}
	for _, a := range apps {
		names = append(names, a.DisplayName)
	}
	if len(apps) != 3 {
		t.Fatalf("kept %d: %v", len(apps), names)
	}
	m := &Manifest{Apps: apps}
	var thirtyTwo []string
	for _, c := range CompatRules(m, nil, RawHints{}) {
		if strings.Contains(c.Reason, "32-bit") {
			thirtyTwo = append(thirtyTwo, c.Subject)
		}
	}
	// The dental suite is the only 32-bit application with no 64-bit twin.
	if len(thirtyTwo) != 1 || thirtyTwo[0] != "labdentalsuite" {
		t.Errorf("32-bit warnings: %v", thirtyTwo)
	}
}

func TestBaseNameStripsVersionsAndArchitectures(t *testing.T) {
	for name, want := range map[string]string{
		"7-Zip 24.08 (x64 edition)": "7-Zip",
		"7-Zip 24.08":               "7-Zip",
		"Notepad++ (64-bit x64)":    "Notepad++",
		"Google Chrome":             "Google Chrome",
		"Microsoft Edge":            "Microsoft Edge",
		"1Password 8.10.40":         "1Password",
		"Lab Dental Suite":          "Lab Dental Suite",
		"Adobe Acrobat Pro DC":      "Adobe Acrobat Pro DC",
		"Dentrix G7.6":              "Dentrix G7.6",
	} {
		if got := baseName(name); got != want {
			t.Errorf("%q -> %q, want %q", name, got, want)
		}
	}
}

// Every section that returns a list has to be wrapped in @(), not just the
// lists inside it.
//
// PowerShell 5.1 turns a one-element array into a scalar, so a machine with
// exactly one printer — or one user profile, which every freshly built PC has
// — produced an object where the parser wanted an array, and the whole scan
// failed with "cannot unmarshal object into Go struct field". It was missed
// because the machine it was written against had several of everything.
func TestEveryListSectionIsWrappedSoOneItemIsStillAList(t *testing.T) {
	b, err := os.ReadFile("scan.ps1")
	if err != nil {
		t.Fatal(err)
	}
	script := string(b)
	for _, section := range []string{"apps", "printers", "mapped_drives", "settings", "profiles", "unsigned_drivers"} {
		want := "$out." + section + " = @(Try-Read"
		if !strings.Contains(script, want) {
			t.Errorf("%s is a list section but is not wrapped: a machine with exactly one of them "+
				"would fail the whole scan. Expected %q", section, want)
		}
	}
	// The sections that are one object rather than a list must NOT be wrapped,
	// or they would arrive as an array of one and fail the same way.
	for _, section := range []string{"os", "hardware", "identity", "network", "hints"} {
		if strings.Contains(script, "$out."+section+" = @(Try-Read") {
			t.Errorf("%s is a single object and must not be wrapped in @()", section)
		}
	}
}

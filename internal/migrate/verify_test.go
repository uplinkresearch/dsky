package migrate

import (
	"strings"
	"testing"
)

func appOf(id, name string, r Resolution) App {
	return App{ID: id, DisplayName: name, SourceKind: SourceWin32, Resolution: r}
}

// The reading that settles it: what the plan said, against what the machine
// actually has.
func TestVerifyComparesThePlanWithTheMachine(t *testing.T) {
	plan := &Manifest{
		SchemaVersion: SchemaVersion,
		Target:        Target{Hostname: "NEWDESK01"},
		Identity:      Identity{DomainFQDN: "lab.dsky.local"},
		Apps: []App{
			appOf("firefox", "Mozilla Firefox", Resolution{Status: StatusResolved, Method: MethodWinget, Ref: "Mozilla.Firefox"}),
			appOf("acrobat", "Adobe Acrobat Reader DC", Resolution{Status: StatusResolved, Method: MethodWinget, Ref: "Adobe.Acrobat.Reader.64-bit"}),
			appOf("oldcad", "AutoVue 2D Professional", Resolution{Status: StatusDropped, ResolvedBy: ByOperator}),
			appOf("byhand", "Practice Suite", Resolution{Status: StatusResolved, Method: MethodManual}),
		},
	}
	after := &Manifest{
		SchemaVersion: SchemaVersion,
		Source:        Source{Hostname: "NEWDESK01"},
		Identity:      Identity{DomainFQDN: "lab.dsky.local"},
		Apps: []App{
			appOf("firefox", "Mozilla Firefox", Resolution{}),
			appOf("acrobat64", "Adobe Acrobat (64-bit)", Resolution{Status: StatusResolved, Method: MethodWinget, Ref: "Adobe.Acrobat.Reader.64-bit"}),
			appOf("teams", "Microsoft Teams", Resolution{}),
		},
	}
	v := Verify(plan, after)

	if v.Present != 1 {
		t.Errorf("present: %d", v.Present)
	}
	// A dropped application and one somebody installs by hand are not
	// failures: the plan already said they would not be installed.
	if v.Deferred != 2 {
		t.Errorf("the plan's own exclusions were counted as missing: deferred=%d missing=%+v", v.Deferred, v.Missing)
	}
	if len(v.Missing) != 1 || v.Missing[0].Wanted.ID != "acrobat" {
		t.Fatalf("missing: %+v", v.Missing)
	}
	// The near miss is the useful half: installed under a name nobody
	// expected looks exactly like failed to install.
	m := v.Missing[0]
	if m.Nearest == nil || m.Nearest.ID != "acrobat64" {
		t.Fatalf("no near miss offered: %+v", m)
	}
	if m.Confidence < AliasThreshold {
		t.Errorf("confidence %.2f is below the threshold it was offered at", m.Confidence)
	}
	if len(v.Aliases()) != 1 {
		t.Errorf("aliases: %+v", v.Aliases())
	}
	// Something on the machine that the plan never mentioned is worth
	// knowing, not a failure.
	if len(v.Extra) != 1 || v.Extra[0].ID != "teams" {
		t.Errorf("extra: %+v", v.Extra)
	}
	if v.OK() {
		t.Error("a machine missing a program it was supposed to have reported OK")
	}
	if !strings.Contains(v.Summary(), "1 of 2") {
		t.Errorf("summary: %s", v.Summary())
	}
}

// A machine that matches its plan says so, and says nothing else.
func TestAMachineThatMatchesItsPlanIsQuiet(t *testing.T) {
	plan := &Manifest{
		Target: Target{Hostname: "PC"},
		Apps:   []App{appOf("firefox", "Mozilla Firefox", Resolution{Status: StatusResolved, Method: MethodWinget, Ref: "Mozilla.Firefox"})},
	}
	after := &Manifest{
		Source: Source{Hostname: "PC"},
		Apps:   []App{appOf("firefox", "Mozilla Firefox", Resolution{})},
	}
	v := Verify(plan, after)
	if !v.OK() || len(v.Missing) != 0 {
		t.Errorf("%+v", v)
	}
	if !strings.Contains(v.Summary(), "nothing else to report") {
		t.Errorf("summary: %s", v.Summary())
	}
}

// The things a machine can be wrong about that are not programs.
func TestVerifyNoticesTheMachineIsNotWhoItShouldBe(t *testing.T) {
	plan := &Manifest{
		Target:   Target{Hostname: "NEWDESK01"},
		Identity: Identity{DomainFQDN: "lab.dsky.local"},
		Settings: []Setting{{
			Key: KeyTimezone, Value: []byte(`"Central Standard Time"`),
			Apply: map[string]ApplyMethod{"win11": {Method: ApplyPowerShell, Ref: "Set-TimeZone"}},
		}},
	}
	after := &Manifest{
		Source:   Source{Hostname: "DESKTOP-7F3K2"},
		Settings: []Setting{{Key: KeyTimezone, Value: []byte(`"Pacific Standard Time"`)}},
	}
	v := Verify(plan, after)
	if len(v.Identity) != 2 {
		t.Errorf("identity: %v", v.Identity)
	}
	if len(v.Settings) != 1 || v.Settings[0].Found != `"Pacific Standard Time"` {
		t.Errorf("settings: %+v", v.Settings)
	}
	joined := strings.Join(v.Identity, " | ")
	if !strings.Contains(joined, "DESKTOP-7F3K2") || !strings.Contains(joined, "workgroup") {
		t.Errorf("the differences are not in plain words: %s", joined)
	}
}

// A setting the plan never claimed it could apply is not a difference. The
// default browser cannot be set on Windows 11, and reporting it as wrong on
// every machine would train people to ignore the report.
func TestASettingThePlanCouldNotApplyIsNotAFailure(t *testing.T) {
	plan := &Manifest{Settings: []Setting{{Key: KeyDefaultBrowser, Value: []byte(`"Firefox"`)}}}
	after := &Manifest{Settings: []Setting{{Key: KeyDefaultBrowser, Value: []byte(`"Edge"`)}}}
	if v := Verify(plan, after); len(v.Settings) != 0 {
		t.Errorf("a setting nobody could apply was reported as wrong: %+v", v.Settings)
	}
}

// An alias has a direction, and the wrong one is useless in a way nobody
// notices: it writes an entry matching a name that already resolved, carrying
// whatever a fresh scan happened to say about a program it did not decide.
//
// The useful direction is the other one. The next machine to be scanned will
// show the program under its NEW name, and what it needs is the way the plan
// installed it.
func TestAnAliasMatchesTheNewNameAndCarriesThePlansInstaller(t *testing.T) {
	plan := &Manifest{Apps: []App{{
		ID: "fieldsync", DisplayName: "WWFO Field Sync", Publisher: "West Wildland Fire Ops",
		SourceKind: SourceWin32,
		Resolution: Resolution{Status: StatusResolved, Method: MethodEXE,
			Ref: `\\fs01\installers\fieldsync\setup.exe`, Args: []string{"/S"}},
	}}}
	// As a fresh scan of the new machine sees it: a new name, and nothing
	// decided about how to install it.
	after := &Manifest{Apps: []App{{
		ID: "fieldsyncclient", DisplayName: "WWFO Field Sync Client (64-bit)",
		Publisher: "West Wildland Fire Ops", SourceKind: SourceWin32,
	}}}

	v := Verify(plan, after)
	al := v.Aliases()
	if len(al) != 1 {
		t.Fatalf("no alias offered: %+v", v.Missing)
	}
	tbl := &Table{SchemaVersion: MappingVersion}
	tbl.Remember(*al[0].Nearest, al[0].Wanted.Resolution, al[0].Wanted.ConfigCapture, "verify")

	if len(tbl.Entries) != 1 {
		t.Fatalf("entries: %+v", tbl.Entries)
	}
	e := tbl.Entries[0]
	if !strings.Contains(e.Match.DisplayName, "Client") {
		t.Errorf("the alias matches the old name, which already resolved: %q", e.Match.DisplayName)
	}
	if e.Resolution.Ref != `\\fs01\installers\fieldsync\setup.exe` || e.Resolution.Method != MethodEXE {
		t.Errorf("the alias does not carry the plan's installer: %+v", e.Resolution)
	}
	// And it does what it was written for: the new name now resolves.
	got, _, ok := TableResolver{Table: tbl}.Find(after.Apps[0])
	if !ok || got.Ref != `\\fs01\installers\fieldsync\setup.exe` {
		t.Errorf("the next machine would not resolve it: %+v ok=%v", got, ok)
	}
}

// A setting that belongs to a person is applied at their first sign-in, so a
// machine read before that has not got it yet. Calling those faults is how a
// report earns a reputation for crying wolf — and it did exactly that on the
// first machine this was run against, listing two of them as wrong on a PC
// where nothing was.
func TestASettingWaitingForSomebodyToSignInIsNotAFault(t *testing.T) {
	perUser := func(key, val string) Setting {
		return Setting{Key: key, Value: []byte(val), Apply: map[string]ApplyMethod{
			"win11": {Method: ApplyRegistry, Ref: `HKCU\SOFTWARE\Microsoft\Windows\CurrentVersion\Explorer\Advanced\HideFileExt=0`},
		}}
	}
	machine := func(key, val string) Setting {
		return Setting{Key: key, Value: []byte(val), Apply: map[string]ApplyMethod{
			"win11": {Method: ApplyPowerShell, Ref: "Set-TimeZone -Id 'Central Standard Time'"},
		}}
	}
	plan := &Manifest{Settings: []Setting{
		perUser(KeyShowExtensions, "1"),
		machine(KeyTimezone, `"Central Standard Time"`),
	}}
	after := &Manifest{Settings: []Setting{
		{Key: KeyShowExtensions, Value: []byte("0")},                 // not applied yet: nobody has signed in
		{Key: KeyTimezone, Value: []byte(`"Pacific Standard Time"`)}, // genuinely wrong
	}}
	v := Verify(plan, after)

	if len(v.Settings) != 1 || v.Settings[0].Key != KeyTimezone {
		t.Errorf("the machine's own settings: %+v", v.Settings)
	}
	if len(v.Waiting) != 1 || v.Waiting[0].Key != KeyShowExtensions {
		t.Errorf("a per-user setting was not recognised as waiting: %+v", v.Waiting)
	}
	if !strings.Contains(v.Summary(), "waiting for the person") {
		t.Errorf("the summary does not separate them: %s", v.Summary())
	}
	// And a machine whose only outstanding settings are waiting ones is not
	// reported as needing a hand.
	ok := Verify(&Manifest{Settings: []Setting{perUser(KeyShowExtensions, "1")}},
		&Manifest{Settings: []Setting{{Key: KeyShowExtensions, Value: []byte("0")}}})
	if !ok.OK() {
		t.Errorf("a machine with nothing wrong was reported as wrong: %+v", ok.Settings)
	}
	if len(ok.Waiting) != 1 {
		t.Errorf("waiting: %+v", ok.Waiting)
	}
}

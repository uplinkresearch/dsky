package agent

import (
	"strings"
	"testing"
)

// The allowlist writes settings as "HIVE\path\Name=value", and a value that is
// not a number is a string. Getting this wrong writes the right setting with
// the wrong type, which Windows ignores in silence.
func TestARegistrySettingIsReadTheWayTheAllowlistWritesIt(t *testing.T) {
	p, err := parseRegistryRef(`HKCU\SOFTWARE\Microsoft\Windows\CurrentVersion\Explorer\Advanced\HideFileExt=0`)
	if err != nil {
		t.Fatal(err)
	}
	if p.Path != `HKCU\SOFTWARE\Microsoft\Windows\CurrentVersion\Explorer\Advanced` || p.Name != "HideFileExt" {
		t.Errorf("path/name: %+v", p)
	}
	if p.DWord != 0 || p.String != "" {
		t.Errorf("a number was not read as one: %+v", p)
	}
	// A GUID is a string, not a number.
	g, err := parseRegistryRef(`HKLM\SOFTWARE\Test\Scheme=8c5e7fda-e8bf-4a96-9a85-a6e23a8c635c`)
	if err != nil {
		t.Fatal(err)
	}
	if g.String == "" || g.DWord != 0 {
		t.Errorf("a GUID was not kept as text: %+v", g)
	}
	for _, bad := range []string{"no-equals", `=1`, `HKCU\OnlyAPath=`} {
		if _, err := parseRegistryRef(bad); err == nil && bad != `HKCU\OnlyAPath=` {
			t.Errorf("%q was accepted", bad)
		}
	}
}

// The per-user script is the same on every machine, and everything that
// differs between machines is data beside it. That is the whole reason the
// agent exists, so it is worth a test that notices if somebody starts
// assembling this text per build.
func TestThePerUserScriptIsFixedAndReadsItsDataFromAFile(t *testing.T) {
	for _, want := range []string{
		UserSettingsName,                // the data it reads
		"%LOCALAPPDATA%",                // the marker is in the person's own profile
		"if exist \"%MARK%\" exit /b 0", // and it stops there the second time
		"reg add",
		"explorer.exe", // the settings do not appear until Explorer restarts
	} {
		if !strings.Contains(userSetupScript, want) {
			t.Errorf("the per-user script has no %q:\n%s", want, userSetupScript)
		}
	}
	// It must not remove itself: the second person to sign in deserves the
	// same settings as the first.
	if strings.Contains(userSetupScript, "/v DSKYUserSetup /f") || strings.Contains(userSetupScript, "reg delete") {
		t.Error("the per-user script removes itself, so only the first person to sign in gets the settings")
	}
	if !strings.Contains(userSetupScript, "\r\n") {
		t.Error("the script has no Windows line endings, which cmd.exe reads as one long line")
	}
}

package recipe

import (
	"reflect"
	"strings"
	"testing"
)

func winRecipe(unattend map[string]string) *Recipe {
	return &Recipe{Windows: &WindowsSpec{Unattend: &UnattendSpec{Template: "t", Vars: unattend}}}
}

// The defect this is for: the scaffolded workspace.yaml ships
// `admin_password: ""`, so a recipe saved from the Install dialog with an
// administrator password -- which the recipe deliberately does not store --
// expanded to nothing and built a machine whose administrator had no password
// at all, silently. A missing Wi-Fi passphrase stopped the build; a missing
// administrator password did not.
func TestAnEmptyPasswordVarCountsAsMissing(t *testing.T) {
	r := winRecipe(map[string]string{"admin_password": "${var:admin_password}"})
	got := NeededVars(r, map[string]string{"admin_password": ""})
	if !reflect.DeepEqual(got, []string{"admin_password"}) {
		t.Errorf("got %q; a recipe that says the password lives elsewhere, and finds nothing there, "+
			"has lost the password rather than been told there is none", got)
	}
	// Whitespace is not a password either.
	if got := NeededVars(r, map[string]string{"admin_password": "   "}); len(got) != 1 {
		t.Errorf("a password of spaces was accepted: %q", got)
	}
	// And with a real one, nothing is missing.
	if got := NeededVars(r, map[string]string{"admin_password": "hunter2"}); len(got) != 0 {
		t.Errorf("got %q, want nothing", got)
	}
}

// The other half of the same distinction: a recipe that writes an empty string
// directly is saying there is no password, on purpose, and must still build.
// That is what the Install dialog writes when the field is left blank.
func TestAnEmptyPasswordWrittenDirectlyIsAChoiceAndNotMissing(t *testing.T) {
	r := winRecipe(map[string]string{"admin_password": ""})
	if got := NeededVars(r, map[string]string{}); len(got) != 0 {
		t.Errorf("got %q; a recipe saying \"no password\" was refused", got)
	}
}

// A value that is not a password is only missing when it has no value at all:
// an empty computer name or timezone is ordinary.
func TestAnEmptyValueThatIsNotAPasswordIsFine(t *testing.T) {
	r := winRecipe(map[string]string{"timezone": "${var:timezone}", "admin_user": "${var:admin_user}"})
	if got := NeededVars(r, map[string]string{"timezone": "", "admin_user": ""}); len(got) != 0 {
		t.Errorf("got %q, want nothing", got)
	}
	if got := NeededVars(r, map[string]string{"timezone": ""}); !reflect.DeepEqual(got, []string{"admin_user"}) {
		t.Errorf("got %q, want the one with no value at all", got)
	}
}

// A password hash is not a password: DSKY derives it, nobody types it, and the
// scaffolded vars.local.yaml ships one. Matching any name containing
// "password" would sweep it in.
func TestAPasswordHashIsNotAPassword(t *testing.T) {
	if IsPasswordVar("admin_password_hash") {
		t.Error("admin_password_hash was treated as a typed password")
	}
	for _, name := range []string{"admin_password", "domain_password", "wifi_password"} {
		if !IsPasswordVar(name) {
			t.Errorf("%s was not treated as a password", name)
		}
	}
}

// The passphrase and the join credential live in their own fields rather than
// in the unattend map, and getting those wrong is silent in the worst way: a
// wireless profile with an empty key joins nothing.
func TestTheWirelessPassphraseAndJoinCredentialAreChecked(t *testing.T) {
	r := &Recipe{Windows: &WindowsSpec{
		WiFi:   &WiFiSpec{SSID: "DeepCreekRanch4", Password: "${var:wifi_password}"},
		Domain: &DomainSpec{Join: "corp.example.com", Username: "svc", Password: "${var:domain_password}"},
	}}
	got := NeededVars(r, map[string]string{"domain_password": ""})
	want := []string{"domain_password", "wifi_password"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

// Everything at once, sorted and without repeats: expanding the values one at
// a time reported the first and stopped, so a recipe short of two values took
// two builds to find that out.
func TestEverythingMissingIsNamedTogether(t *testing.T) {
	r := &Recipe{Windows: &WindowsSpec{
		Unattend: &UnattendSpec{Template: "t", Vars: map[string]string{
			"admin_password": "${var:admin_password}",
			"computer_name":  "${var:site}-${var:site}",
		}},
		WiFi: &WiFiSpec{SSID: "x", Password: "${var:wifi_password}"},
	}}
	got := NeededVars(r, map[string]string{"admin_password": ""})
	want := []string{"admin_password", "site", "wifi_password"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	msg := NeededVarsError(got)
	for _, w := range []string{"admin_password, site, wifi_password", "vars.local.yaml", "--var"} {
		if !strings.Contains(msg, w) {
			t.Errorf("the message does not say %q:\n%s", w, msg)
		}
	}
	if NeededVarsError(nil) != "" {
		t.Error("a recipe with nothing missing produced a complaint")
	}
}

package recipe

import (
	"encoding/xml"
	"fmt"
	"strings"
	"testing"
)

// The profile is parsed by Windows and by nothing else until then, so the one
// thing this can check cheaply is that it is well-formed XML at all. A profile
// that is not costs a whole install to discover.
func TestWLANProfileIsWellFormed(t *testing.T) {
	for _, w := range []*WiFiSpec{
		{SSID: "Uplink", Password: "correct horse battery"},
		{SSID: "Uplink", Password: ""},
		{SSID: "Uplink", Password: "pass&word<>\"'", Hidden: true},
		{SSID: `Tom & Jerry's <net>`, Password: "12345678"},
	} {
		if err := xml.Unmarshal([]byte(GenerateWLANProfile(w)), new(any)); err != nil {
			t.Errorf("SSID %q password %q: %v", w.SSID, w.Password, err)
		}
	}
}

// An ampersand in a Wi-Fi password is ordinary, and pasting one in unescaped
// produces a document Windows rejects with "the profile is not valid" and no
// indication of why.
func TestWLANProfileEscapesTheKey(t *testing.T) {
	got := GenerateWLANProfile(&WiFiSpec{SSID: "net", Password: `a&b<c`})
	if strings.Contains(got, `<keyMaterial>a&b`) {
		t.Error("the passphrase went in unescaped")
	}
	if !strings.Contains(got, `<keyMaterial>a&amp;b&lt;c</keyMaterial>`) {
		t.Errorf("escaped passphrase not found in:\n%s", got)
	}
}

// Windows rejects a passphrase declared as a raw key and a raw key declared as
// a passphrase, and 64 characters is the only length where the two collide.
func TestWLANProfileKeyType(t *testing.T) {
	raw := strings.Repeat("ab", 32) // 64 hex digits
	if k := keyType(raw); k != "networkKey" {
		t.Errorf("64 hex digits should be a networkKey, got %s", k)
	}
	// 64 characters, but not hex: still something a person typed.
	if k := keyType(strings.Repeat("zz", 32)); k != "passPhrase" {
		t.Errorf("64 non-hex characters should be a passPhrase, got %s", k)
	}
	if k := keyType("hunter2hunter2"); k != "passPhrase" {
		t.Errorf("a typed password should be a passPhrase, got %s", k)
	}
}

// An open network has no sharedKey element at all; leaving an empty one in is
// another way to get "the profile is not valid".
func TestWLANProfileOpenNetworkHasNoKey(t *testing.T) {
	got := GenerateWLANProfile(&WiFiSpec{SSID: "guest"})
	if strings.Contains(got, "sharedKey") {
		t.Errorf("open network profile carries a sharedKey:\n%s", got)
	}
	if !strings.Contains(got, "<authentication>open</authentication>") {
		t.Errorf("open network is not declared open:\n%s", got)
	}
}

func TestWLANProfileHidden(t *testing.T) {
	if strings.Contains(GenerateWLANProfile(&WiFiSpec{SSID: "n", Password: "12345678"}), "nonBroadcast") {
		t.Error("a broadcast network should not be marked nonBroadcast")
	}
	if !strings.Contains(GenerateWLANProfile(&WiFiSpec{SSID: "n", Password: "12345678", Hidden: true}), "<nonBroadcast>true</nonBroadcast>") {
		t.Error("a hidden network is not marked nonBroadcast")
	}
}

// The SSID is carried as hex because it is 32 bytes rather than text.
func TestWLANProfileHexSSID(t *testing.T) {
	if !strings.Contains(GenerateWLANProfile(&WiFiSpec{SSID: "Uplink"}), "<hex>55706C696E6B</hex>") {
		t.Error("SSID hex is wrong or missing")
	}
}

// Catching these at build time is the whole point: the alternative is a silent
// failure discovered after an install, by noticing that no programs arrived.
func TestValidateWiFiRefusesWhatWindowsWould(t *testing.T) {
	fail := func(format string, args ...any) error { return fmt.Errorf(format, args...) }
	for _, tc := range []struct {
		name string
		spec *WiFiSpec
		bad  bool
	}{
		{"ok", &WiFiSpec{SSID: "net", Password: "12345678"}, false},
		{"open", &WiFiSpec{SSID: "net"}, false},
		{"raw key", &WiFiSpec{SSID: "net", Password: strings.Repeat("ab", 32)}, false},
		{"no ssid", &WiFiSpec{SSID: "  "}, true},
		{"ssid too long", &WiFiSpec{SSID: strings.Repeat("x", 33)}, true},
		{"password too short", &WiFiSpec{SSID: "net", Password: "short"}, true},
		{"password too long", &WiFiSpec{SSID: "net", Password: strings.Repeat("x", 65)}, true},
	} {
		err := ValidateWiFi(tc.spec, "ssid", "password", fail)
		if tc.bad && err == nil {
			t.Errorf("%s: accepted, should have been refused", tc.name)
		}
		if !tc.bad && err != nil {
			t.Errorf("%s: refused with %v", tc.name, err)
		}
	}
}

func TestWiFiEnabled(t *testing.T) {
	var nilSpec *WiFiSpec
	if nilSpec.Enabled() {
		t.Error("a nil spec is enabled")
	}
	if (&WiFiSpec{SSID: "   "}).Enabled() {
		t.Error("a blank SSID is enabled")
	}
	if !(&WiFiSpec{SSID: "net"}).Enabled() {
		t.Error("a named network is not enabled")
	}
}

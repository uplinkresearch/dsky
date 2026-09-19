package recipe

import (
	"encoding/hex"
	"fmt"
	"strings"
)

// The wireless profile.
//
// Windows imports a network as an XML document — `netsh wlan add profile` —
// and that document is the only way to hand a machine a network it has never
// seen. There is no command that takes an SSID and a passphrase directly.
//
// The schema is Microsoft's WLANProfile v1. Windows is unforgiving about it
// and unhelpful when it objects: a profile it dislikes produces "The profile
// is not valid" with nothing to say which element was wrong, so every field
// here is deliberate.

// WLANProfileName is what the staged profile is called on the media, and the
// name the first-boot step imports.
const WLANProfileName = "wifi.xml"

// wifiSettleSeconds is how long the generated script waits after asking to
// connect. netsh returns as soon as it has accepted the request, with the
// association and DHCP still to come, and the next thing the script does is
// download something. The agent watches for the network properly instead.
const wifiSettleSeconds = 20

// GenerateWLANProfile renders the profile Windows imports at first boot.
//
// The passphrase goes in as clear text with protected=false. That looks worse
// than it is possible to avoid: the encrypted form Windows writes when it
// saves a profile itself is sealed with a key belonging to that installation,
// so a profile encrypted here could never be read by the machine being built.
// Every provisioning tool that predates this one does the same, and the real
// mitigation is that the password lives in vars.local.yaml rather than the
// recipe, and that the media is a stick somebody keeps.
func GenerateWLANProfile(w *WiFiSpec) string {
	ssid := strings.TrimSpace(w.SSID)
	var b strings.Builder
	p := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }

	p(`<?xml version="1.0"?>`)
	p(`<WLANProfile xmlns="http://www.microsoft.com/networking/WLAN/profile/v1">`)
	p(`  <name>%s</name>`, xmlEscape(ssid))
	p(`  <SSIDConfig>`)
	p(`    <SSID>`)
	// The hex form is authoritative. An SSID is 32 bytes, not text, and a
	// name carrying anything outside ASCII survives the round trip through
	// <hex> when it may not survive <name>.
	p(`      <hex>%s</hex>`, strings.ToUpper(hex.EncodeToString([]byte(ssid))))
	p(`      <name>%s</name>`, xmlEscape(ssid))
	p(`    </SSID>`)
	if w.Hidden {
		p(`    <nonBroadcast>true</nonBroadcast>`)
	}
	p(`  </SSIDConfig>`)
	p(`  <connectionType>ESS</connectionType>`)
	// A machine being imaged should join the moment the radio is up, without
	// anybody signing in to ask for it.
	p(`  <connectionMode>auto</connectionMode>`)
	p(`  <MSM>`)
	p(`    <security>`)
	p(`      <authEncryption>`)
	if w.Password == "" {
		p(`        <authentication>open</authentication>`)
		p(`        <encryption>none</encryption>`)
		p(`        <useOneX>false</useOneX>`)
		p(`      </authEncryption>`)
	} else {
		// WPA2-Personal with AES. WPA3 access points that matter here run in
		// transition mode and accept a WPA2PSK client; a pure-WPA3 network
		// needs WPA3SAE, which Windows only honours on a machine whose
		// wireless driver supports it -- and the driver is the thing least
		// certain on a freshly imaged machine.
		p(`        <authentication>WPA2PSK</authentication>`)
		p(`        <encryption>AES</encryption>`)
		p(`        <useOneX>false</useOneX>`)
		p(`      </authEncryption>`)
		p(`      <sharedKey>`)
		p(`        <keyType>%s</keyType>`, keyType(w.Password))
		p(`        <protected>false</protected>`)
		p(`        <keyMaterial>%s</keyMaterial>`, xmlEscape(w.Password))
		p(`      </sharedKey>`)
	}
	p(`    </security>`)
	p(`  </MSM>`)
	p(`</WLANProfile>`)
	return b.String()
}

// keyType distinguishes a passphrase from a raw 256-bit key written as hex,
// which is the other thing 64 characters in this field can mean. Windows
// rejects a passphrase declared as networkKey and vice versa.
func keyType(password string) string {
	if isRawWLANKey(password) {
		return "networkKey"
	}
	return "passPhrase"
}

// isRawWLANKey reports whether the password is a 64-hex-digit pairwise master
// key rather than something a person typed.
func isRawWLANKey(password string) bool {
	if len(password) != 64 {
		return false
	}
	_, err := hex.DecodeString(password)
	return err == nil
}

// ValidateWiFi checks what Windows would otherwise reject at first boot, where
// nobody is watching and the only symptom is that no programs got installed.
//
// ssidField and passField are what to call the two fields, because the same
// check is made from two places that name them differently: a recipe has
// windows.wifi.ssid, and somebody in the install dialog has a box labelled
// "Wi-Fi network" and has never seen a recipe.
func ValidateWiFi(w *WiFiSpec, ssidField, passField string, fail func(string, ...any) error) error {
	ssid := strings.TrimSpace(w.SSID)
	switch {
	case ssid == "":
		return fail("%s is empty", ssidField)
	case len(ssid) > 32:
		// The field is 32 bytes, and it is bytes rather than characters.
		return fail("%s is %d bytes; a network name holds at most 32", ssidField, len(ssid))
	}
	if w.Password == "" || isRawWLANKey(w.Password) {
		return nil
	}
	if n := len(w.Password); n < 8 || n > 63 {
		return fail("%s is %d characters; a Wi-Fi password is 8 to 63 "+
			"(or 64 hex digits for a raw key). Leave it empty for an open network", passField, n)
	}
	return nil
}

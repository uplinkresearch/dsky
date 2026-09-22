package appcatalog

import "strings"

// Pinning the vendors' signing keys.
//
// A vendor apt or rpm repository is added at first boot by fetching the
// vendor's signing key over HTTPS and writing it where the package manager
// will trust it. That fetch is the whole of the trust: whatever comes back
// from that URL authorises everything from that repository, for the life of
// the machine, and nobody ever looks at it again.
//
// The window is narrow -- one request on a machine that has just come up --
// and everything else about the path is sound: the key is fetched over HTTPS,
// apt gets it as signed-by rather than trusted machine-wide, and the
// repository's own signatures are checked against it from then on. But a
// first boot happens on the customer's network, which is frequently the thing
// being replaced, occasionally behind a proxy that inspects TLS, and is the
// one moment DSKY has no control over. A wrong key installed then is a wrong
// key for years.
//
// So the fingerprint is pinned here and checked before the key is written.
// Ubuntu and Fedora fetch the same vendors' keys, mostly from the same URLs,
// so the pin belongs to the URL rather than to either table.
//
// The fingerprints are not written by hand. TestVendorKeysLive fetches each
// key, computes its fingerprint, and fails with the line to paste when the
// pin is missing or no longer matches -- which is also how a vendor rotating
// their key is noticed, rather than by every machine built that week
// failing to install a program.

// vendorKeyFingerprints is the OpenPGP fingerprint each vendor key must have:
// forty uppercase hex digits, as `gpg --show-keys --with-colons` reports it,
// with no spaces.
//
// An empty pin means the key is fetched and trusted as it always was, and the
// first-boot log says so. Empty is not a safe resting place -- it is where an
// entry sits between a vendor being added here and TestVendorKeysLive telling
// somebody what its fingerprint is.
var vendorKeyFingerprints = map[string]string{
	// Google's one signing key, used by both the Chrome apt and rpm repos.
	"https://dl.google.com/linux/linux_signing_key.pub": "",
	// AnyDesk publishes a separate key per package format.
	"https://keys.anydesk.com/repos/DEB-GPG-KEY": "",
	"https://keys.anydesk.com/repos/RPM-GPG-KEY": "",
	// TeamViewer's one key, used by both.
	"https://download.teamviewer.com/download/linux/signature/TeamViewer2017.asc": "",
}

// VendorKeyFingerprint is the fingerprint pinned for a key URL, or "" when
// none is pinned yet. Callers generating a first-boot script use the empty
// string to mean "fetch it as before, and say in the log that it was not
// checked" rather than to mean "no check needed".
func VendorKeyFingerprint(keyURL string) string {
	return vendorKeyFingerprints[strings.TrimSpace(keyURL)]
}

// VendorKeyURLs is every key URL with a pin recorded for it, for the live
// test that keeps them true.
func VendorKeyURLs() []string {
	out := make([]string, 0, len(vendorKeyFingerprints))
	for u := range vendorKeyFingerprints {
		out = append(out, u)
	}
	return out
}

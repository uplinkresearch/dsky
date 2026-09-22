package appcatalog

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Keeping the pinned fingerprints true.
//
// The pins in vendorkeys.go are what a machine checks a vendor's signing key
// against at first boot. They are not written by hand: this fetches each key
// from the URL the first-boot script will use, computes its fingerprint the
// same way the generated shell does, and reports what it found.
//
// It covers two jobs with one fetch. A vendor who rotates their key breaks
// every install of their program until the pin follows, and this is where
// that is noticed -- once, weekly, in CI -- rather than on a bench with a
// customer waiting. And a vendor added to the tables without a pin is named
// here with the line to paste, so an unpinned entry cannot sit quietly.
//
// catalog-health runs it weekly beside TestUbuntuVendorReposLive.
func TestVendorKeysLive(t *testing.T) {
	if testing.Short() {
		t.Skip("network")
	}
	if _, err := exec.LookPath("gpg"); err != nil {
		t.Skip("no gpg to read fingerprints with")
	}
	ctx := context.Background()
	urls := VendorKeyURLs()
	sort.Strings(urls)
	for _, u := range urls {
		code, body, err := liveGet(ctx, u, nil)
		if err != nil {
			t.Errorf("%s: %v", u, err)
			continue
		}
		if code != 200 {
			t.Errorf("%s: HTTP %d", u, code)
			continue
		}
		got, err := fingerprintOf(t, body)
		if err != nil {
			t.Errorf("%s: what it serves is not a signing key: %v", u, err)
			continue
		}
		switch pin := VendorKeyFingerprint(u); {
		case pin == "":
			// Deliberately a failure, not a log line. An unpinned vendor is
			// the gap this whole check exists to close, and the fix is one
			// paste away.
			t.Errorf("%s has no pinned fingerprint. It serves %s today — "+
				"verify that against the vendor's own published fingerprint, then put it in "+
				"vendorKeyFingerprints:\n\t%q: %q,", u, got, u, got)
		case pin != got:
			t.Errorf("%s now serves %s, but vendorKeyFingerprints pins %s. "+
				"If the vendor rotated their key, confirm the new one with them and update the pin; "+
				"until then every machine that installs their program will refuse it.", u, got, pin)
		default:
			t.Logf("%s: %s ok", u, got)
		}
	}
}

// fingerprintOf reads a key's primary fingerprint exactly as the generated
// first-boot script does, so this test and the machine cannot disagree about
// what a key's fingerprint is.
func fingerprintOf(t *testing.T, key string) (string, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "vendor.key")
	if err := os.WriteFile(p, []byte(key), 0o644); err != nil {
		return "", err
	}
	out, err := exec.Command("gpg", "--show-keys", "--with-colons", "--with-fingerprint", p).Output()
	if err != nil {
		return "", err
	}
	for _, ln := range strings.Split(string(out), "\n") {
		if !strings.HasPrefix(ln, "fpr:") {
			continue
		}
		f := strings.Split(ln, ":")
		if len(f) > 9 && f[9] != "" {
			return f[9], nil
		}
	}
	return "", errNoFingerprint
}

var errNoFingerprint = &fingerprintError{}

type fingerprintError struct{}

func (*fingerprintError) Error() string { return "gpg read no fingerprint from it" }

// Every key URL either table names must be in the pin table, or it is a key
// nothing will ever check. Adding a vendor is two edits, and this is the one
// that is easy to forget.
func TestEveryVendorKeyURLHasAnEntry(t *testing.T) {
	known := map[string]bool{}
	for _, u := range VendorKeyURLs() {
		known[u] = true
	}
	for _, r := range ubuntuRepos {
		if !known[r.KeyURL] {
			t.Errorf("%s: its signing key %s is not in vendorKeyFingerprints, so nothing will ever check it", r.ID, r.KeyURL)
		}
	}
	for _, r := range fedoraRepos {
		if !known[r.GPGKey] {
			t.Errorf("%s: its signing key %s is not in vendorKeyFingerprints, so nothing will ever check it", r.ID, r.GPGKey)
		}
	}
}

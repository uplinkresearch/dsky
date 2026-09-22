package oscatalog

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/uplinkresearch/dsky/internal/appcatalog"
)

// A throwaway key, generated once and pasted here, so the test needs neither
// the network nor a key generation of its own. Only its fingerprint matters.
const (
	testVendorKeyFingerprint = "9F259331030E40487C2580084E5038FCDE587CD3"
	testVendorKey            = `-----BEGIN PGP PUBLIC KEY BLOCK-----

mDMEarLdSxYJKwYBBAHaRw8BAQdA+KUAjlZJCCdftxzu6w/iysSXDweG+wGSWSHj
sDQ6gru0JERTS1kgVGVzdCBWZW5kb3IgPHRAZXhhbXBsZS5pbnZhbGlkPoiTBBMW
CgA7FiEEnyWTMQMOQEh8JYAITlA4/N5YfNMFAmqy3UsCGwMFCwkIBwICIgIGFQoJ
CAsCBBYCAwECHgcCF4AACgkQTlA4/N5YfNMRYwEAyE28hdaNoGN0mbLOF6EUVJDD
XnegVsTx0eXKmfF2bXUA/Av4iOAwTDCY3ziyShPzhxujz4o5BodYOY0lDLOpo9EJ
uDgEarLdSxIKKwYBBAGXVQEFAQEHQP+d4avNad0bIF+IM8c12Qo7BbU/4z5IyqvY
meZefk1BAwEIB4h4BBgWCgAgFiEEnyWTMQMOQEh8JYAITlA4/N5YfNMFAmqy3UsC
GwwACgkQTlA4/N5YfNNEpQD/UDZndXPV5R7JXrwrVh9OYQBARusrtgHwKKjU8Ctq
umMA/3wKyqPValUHYMKiILqf0zWFCau4c4FP5cpy9nZH3GIA
=Pn57
-----END PGP PUBLIC KEY BLOCK-----
`
)

// runKeyOK runs the generated keyok function against a key file, the way the
// first-boot script will, and reports whether it accepted the key.
//
// The shell is tested rather than read, because what this change is worth
// depends entirely on whether the function actually refuses -- and a script
// that is generated, written to a stick and first run on a customer's machine
// is the worst place to find out that a quoting mistake made it return 0.
func runKeyOK(t *testing.T, keyFile, pin string) (ok bool, out string) {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash to run the generated script with")
	}
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	writeKeyOK(w, "false")
	w(`note() { echo "$*"; }`)
	w(`keyok %q %q %q`, keyFile, pin, "Test Vendor")

	script := filepath.Join(t.TempDir(), "keyok.sh")
	if err := os.WriteFile(script, []byte(b.String()), 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := exec.Command(bash, script).CombinedOutput()
	return err == nil, string(res)
}

func writeTestKey(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "vendor.asc")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// The key the vendor publishes is the key the machine trusts.
func TestKeyOKAcceptsTheKeyItPins(t *testing.T) {
	if _, err := exec.LookPath("gpg"); err != nil {
		t.Skip("no gpg")
	}
	ok, out := runKeyOK(t, writeTestKey(t, testVendorKey), testVendorKeyFingerprint)
	if !ok {
		t.Fatalf("the vendor's own key was refused:\n%s", out)
	}
	if !strings.Contains(out, testVendorKeyFingerprint) {
		t.Errorf("the log does not record which key was accepted:\n%s", out)
	}
}

// The case the pin exists for: something other than the vendor's key comes
// back from that URL, on a customer network, at first boot.
func TestKeyOKRefusesAKeyThatIsNotThePinnedOne(t *testing.T) {
	if _, err := exec.LookPath("gpg"); err != nil {
		t.Skip("no gpg")
	}
	other := strings.Repeat("A", 40)
	ok, out := runKeyOK(t, writeTestKey(t, testVendorKey), other)
	if ok {
		t.Fatalf("a key that is not the pinned one was accepted:\n%s", out)
	}
	if !strings.Contains(out, "not trusting it") {
		t.Errorf("the log does not say the key was refused:\n%s", out)
	}
	// Both fingerprints, because a vendor who rotated their key and a machine
	// handed somebody else's key want opposite responses from whoever reads
	// this log.
	if !strings.Contains(out, testVendorKeyFingerprint) || !strings.Contains(out, other) {
		t.Errorf("the log does not name what was found and what was expected:\n%s", out)
	}
}

// A file that is not a key at all -- a captive portal's login page, which is
// what a coffee-shop network answers every request with.
func TestKeyOKRefusesSomethingThatIsNotAKey(t *testing.T) {
	if _, err := exec.LookPath("gpg"); err != nil {
		t.Skip("no gpg")
	}
	ok, out := runKeyOK(t, writeTestKey(t, "<html><body>Sign in to continue</body></html>\n"), testVendorKeyFingerprint)
	if ok {
		t.Fatalf("a web page was accepted as a signing key:\n%s", out)
	}
	if !strings.Contains(out, "unreadable") {
		t.Errorf("the log does not say the key could not be read:\n%s", out)
	}
}

// An unpinned vendor keeps working exactly as before, and says so. Silence
// here would let a vendor added without a pin look like one that was checked.
func TestKeyOKWithNoPinTrustsTheKeyAndSaysSo(t *testing.T) {
	ok, out := runKeyOK(t, writeTestKey(t, testVendorKey), "")
	if !ok {
		t.Fatalf("an unpinned vendor stopped working:\n%s", out)
	}
	if !strings.Contains(out, "no fingerprint is pinned") {
		t.Errorf("the log does not admit the key was not checked:\n%s", out)
	}
}

// Nothing may reach the path the package manager trusts before the check has
// passed. Fetching straight onto the keyring and checking afterwards would
// leave a window where a wrong key is already installed, and a script that
// dies between the two steps leaves it there for good.
func TestTheKeyringIsOnlyWrittenAfterTheCheck(t *testing.T) {
	ubuntu, err := appcatalog.ResolveUbuntu([]string{"chrome"})
	if err != nil {
		t.Skip("no chrome in the Ubuntu table")
	}
	fedora, err := appcatalog.ResolveFedora([]string{"chrome"})
	if err != nil {
		t.Skip("no chrome in the Fedora table")
	}
	for name, script := range map[string]string{
		"ubuntu": ubuntuFirstBootScript(ubuntu),
		"fedora": fedoraFirstBootScript(fedora),
	} {
		if !strings.Contains(script, "keyok ") {
			t.Errorf("%s: the vendor's key is never checked:\n%s", name, script)
			continue
		}
		// curl writes to the .new path; the trusted path is only reached by
		// the mv on the far side of keyok.
		for _, ln := range strings.Split(script, "\n") {
			if !strings.Contains(ln, "curl -fsSL") || !strings.Contains(ln, " -o ") {
				continue
			}
			if !strings.Contains(ln, ".new") {
				t.Errorf("%s: a key is fetched straight onto a trusted path:\n  %s", name, ln)
			}
			if !strings.Contains(ln, "keyok ") {
				t.Errorf("%s: a key is fetched without the check in the same condition:\n  %s", name, ln)
			}
		}
		if bash, err := exec.LookPath("bash"); err == nil {
			f := filepath.Join(t.TempDir(), "dsky-apps.sh")
			if err := os.WriteFile(f, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			if out, err := exec.Command(bash, "-n", f).CombinedOutput(); err != nil {
				t.Errorf("%s: the script does not parse: %v\n%s", name, err, out)
			}
		}
	}
}

// dnf must read the key DSKY checked, not fetch the vendor's URL again on its
// own terms -- which would be the very fetch the pin exists to stop.
func TestTheFedoraRepoFilePointsAtTheCheckedKey(t *testing.T) {
	plan, err := appcatalog.ResolveFedora([]string{"chrome"})
	if err != nil {
		t.Skip("no chrome in the Fedora table")
	}
	script := fedoraFirstBootScript(plan)
	if !strings.Contains(script, "gpgkey=file:///etc/pki/rpm-gpg/") {
		t.Errorf("the repo file does not point at the local key:\n%s", script)
	}
	if strings.Contains(script, "gpgkey=https://") || strings.Contains(script, "gpgkey=http://") {
		t.Errorf("the repo file still sends dnf to fetch the key itself:\n%s", script)
	}
}

package agent

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// What winget printed when Chrome failed in the VM, trimmed to the lines that
// matter.
const chromeHashMismatch = `Found Google Chrome [Google.Chrome] Version 153.0.8010.37
This application is licensed to you by its owner.
Downloading https://dl.google.com/dl/chrome/install/googlechromestandaloneenterprise64.msi
Installer hash does not match; this cannot be overridden when running as admin`

func TestTheInstallerLinkIsReadFromWinget(t *testing.T) {
	if got := installerURL(chromeHashMismatch); got != "https://dl.google.com/dl/chrome/install/googlechromestandaloneenterprise64.msi" {
		t.Errorf("read %q", got)
	}
	if got := installerURL("Found something\nInstaller hash does not match"); got != "" {
		t.Errorf("no Downloading line, but read %q", got)
	}
}

// The signer has to be the publisher the package is named for -- not merely
// somebody with a valid certificate.
func TestTheSignerMustBeThePublisher(t *testing.T) {
	google := "CN=Google LLC, O=Google LLC, L=Mountain View, S=California, C=US"
	for _, c := range []struct {
		subject, id string
		want        bool
	}{
		{google, "Google.Chrome", true},
		{"CN=Adobe Inc., O=Adobe Inc., L=San Jose, S=ca, C=US", "Adobe.Acrobat.Reader.64-bit", true},
		// A valid signature from somebody else is not Google's installer.
		{"CN=Totally Legit Software, O=Totally Legit Software", "Google.Chrome", false},
		// The word has to be the publisher, not a fragment of another name.
		{"CN=Googleplex Downloads Ltd, O=Googleplex Downloads Ltd", "Google.Chrome", false},
		// Only the name fields count: a locality or street is not a signer.
		{"CN=Evil Corp, O=Evil Corp, L=Google Street", "Google.Chrome", false},
		{"", "Google.Chrome", false},
		{google, "Chrome", false}, // no publisher in the id at all
	} {
		if got := signedBy(c.subject, publisherOf(c.id)); got != c.want {
			t.Errorf("signedBy(%q, %s) = %v, want %v", c.subject, c.id, got, c.want)
		}
	}
}

// The HP recipe's failure, handled: winget refuses on the stale hash, the
// agent fetches the same link, finds Google's valid signature, and installs
// it through msiexec -- once, without the pointless second and third attempts.
//
// The manifest names Google.Chrome as vouched because the build does: the
// fallback is only offered for ids DSKY's own list spells. See vouchedFor.
func TestChromeInstallsFromGooglesSignedInstallerWhenWingetsHashIsStale(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("an msi"))
	}))
	defer srv.Close()
	prevClient := httpClient
	httpClient = srv.Client()
	t.Cleanup(func() { httpClient = prevClient })

	prevVerify := verifySignatureFn
	verifySignatureFn = func(string) (string, string, error) {
		return "Valid", "CN=Google LLC, O=Google LLC, L=Mountain View, S=California, C=US", nil
	}
	t.Cleanup(func() { verifySignatureFn = prevVerify })

	out := strings.Replace(chromeHashMismatch, "https://dl.google.com/dl/chrome/install", srv.URL, 1)
	a, dir := newAgent(t, &Manifest{Version: ManifestVersion, Apps: &Apps{Vouched: []string{"Google.Chrome"}}})
	f := &fakeRun{}
	f.do = func(name string, args []string) (result, error) {
		if name == "winget" {
			return result{Code: wingetHashMismatch, Out: out}, nil
		}
		return result{}, nil
	}
	f.install(t)

	a.installPackage("winget", "Google.Chrome", "")

	wingets, msis := 0, 0
	for _, c := range f.calls {
		switch {
		case strings.HasPrefix(c, "winget "):
			wingets++
		case strings.HasPrefix(c, "msiexec /i ") && strings.Contains(c, "/qn"):
			msis++
		}
	}
	if wingets != 1 {
		t.Errorf("winget ran %d times; a stale hash is the same on every attempt, so once is enough", wingets)
	}
	if msis != 1 {
		t.Errorf("msiexec ran %d times, want the vendor's installer once: %v", msis, f.calls)
	}
	log := logText(t, dir)
	if !strings.Contains(log, "installed Google.Chrome from the vendor's signed installer") {
		t.Errorf("the log does not say how Chrome went in:\n%s", log)
	}
	if strings.Contains(log, "FAILED") {
		t.Errorf("a successful fallback was recorded as a failure:\n%s", log)
	}
}

// And anything not signed by the publisher is refused, just as winget refused
// it. The signature is the whole reason this is safe.
func TestAnInstallerNotSignedByThePublisherIsNeverRun(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("something"))
	}))
	defer srv.Close()
	prevClient := httpClient
	httpClient = srv.Client()
	t.Cleanup(func() { httpClient = prevClient })

	for _, c := range []struct {
		name, status, subject string
	}{
		{"unsigned", "NotSigned", ""},
		{"tampered", "HashMismatch", "CN=Google LLC, O=Google LLC"},
		{"someone else", "Valid", "CN=Somebody Else, O=Somebody Else"},
	} {
		t.Run(c.name, func(t *testing.T) {
			prevVerify := verifySignatureFn
			verifySignatureFn = func(string) (string, string, error) { return c.status, c.subject, nil }
			t.Cleanup(func() { verifySignatureFn = prevVerify })

			out := strings.Replace(chromeHashMismatch, "https://dl.google.com/dl/chrome/install", srv.URL, 1)
			a, dir := newAgent(t, &Manifest{Version: ManifestVersion, Apps: &Apps{Vouched: []string{"Google.Chrome"}}})
			f := &fakeRun{}
			f.do = func(name string, args []string) (result, error) {
				if name == "winget" {
					return result{Code: wingetHashMismatch, Out: out}, nil
				}
				return result{}, nil
			}
			f.install(t)

			a.installPackage("winget", "Google.Chrome", "")

			for _, call := range f.calls {
				if strings.HasPrefix(call, "msiexec") {
					t.Fatalf("ran an installer whose signature was %s by %q", c.status, c.subject)
				}
			}
			if !strings.Contains(logText(t, dir), "could not install Google.Chrome") {
				t.Error("the refusal is not recorded as a failure")
			}
		})
	}
}

// Plain HTTP and non-MSI installers are not attempted at all.
func TestOnlyHTTPSAndMSIAreFetched(t *testing.T) {
	a, dir := newAgent(t, &Manifest{Version: ManifestVersion, Apps: &Apps{Vouched: []string{"Google.Chrome"}}})
	if a.installFromVendor("Google.Chrome", "Downloading http://dl.google.com/chrome.msi") {
		t.Error("fetched an installer over plain HTTP")
	}
	if a.installFromVendor("Google.Chrome", "Downloading https://dl.google.com/ChromeSetup.exe") {
		t.Error("ran an .exe whose silent switches are unknown")
	}
	log := logText(t, dir)
	if !strings.Contains(log, "not HTTPS") || !strings.Contains(log, "not an MSI") {
		t.Errorf("the log does not say why:\n%s", log)
	}
}

// An id an operator typed into a recipe gets no vendor fallback, because the
// fallback works out which publisher to insist on from the id itself: an id
// chosen freely also chooses the name its own signature has to match.
// Acme.Thing would be satisfied by anyone whose certificate says "Acme", and
// registering a company is not a high bar. So winget's refusal stands.
func TestAPackageDSKYDoesNotVouchForGetsNoVendorFallback(t *testing.T) {
	// The signature check is made to pass, so that what stops the install is
	// unambiguously the gate and not the certificate.
	prevVerify := verifySignatureFn
	verifySignatureFn = func(string) (string, string, error) {
		return "Valid", "CN=Acme Holdings, O=Acme Holdings", nil
	}
	t.Cleanup(func() { verifySignatureFn = prevVerify })

	out := strings.Replace(chromeHashMismatch, "Google.Chrome", "Acme.Thing", -1)
	a, dir := newAgent(t, &Manifest{Version: ManifestVersion,
		Apps: &Apps{Winget: []string{"Acme.Thing"}, Vouched: []string{"Google.Chrome"}}})
	f := &fakeRun{}
	f.do = func(name string, args []string) (result, error) {
		if name == "winget" {
			return result{Code: wingetHashMismatch, Out: out}, nil
		}
		return result{}, nil
	}
	f.install(t)

	a.installPackage("winget", "Acme.Thing", "")

	for _, c := range f.calls {
		if strings.HasPrefix(c, "msiexec ") {
			t.Errorf("an installer was fetched and run for a package DSKY does not vouch for: %v", f.calls)
		}
	}
	log := logText(t, dir)
	if !strings.Contains(log, "not one DSKY's own list names") {
		t.Errorf("the log does not say why the fallback was not taken:\n%s", log)
	}
	// It still has to read as a failed program, or a machine comes off the
	// bench missing something with nothing saying so.
	if !strings.Contains(log, "FAILED Acme.Thing") {
		t.Errorf("the refusal is not recorded as a failure:\n%s", log)
	}
}

// Media built before the manifest recorded which ids were vouched for has no
// list, so nothing takes the fallback. Closed is the right way to fail here:
// the fallback is an extra route on a path winget has already refused, and
// doing without it costs a program, not a machine.
func TestWithNoVouchedListNothingTakesTheFallback(t *testing.T) {
	a, _ := newAgent(t, &Manifest{Version: ManifestVersion})
	if a.vouchedFor("Google.Chrome") {
		t.Error("a manifest that names no vouched packages vouched for one anyway")
	}
	b, _ := newAgent(t, &Manifest{Version: ManifestVersion, Apps: &Apps{}})
	if b.vouchedFor("Google.Chrome") {
		t.Error("an empty vouched list vouched for a package")
	}
}

// winget ids are case-insensitive, and a recipe that spells one differently
// from the catalog must not lose its fallback over the spelling.
func TestVouchingIsCaseInsensitive(t *testing.T) {
	a, _ := newAgent(t, &Manifest{Version: ManifestVersion,
		Apps: &Apps{Vouched: []string{"Google.Chrome"}}})
	if !a.vouchedFor("google.chrome") {
		t.Error("the same id spelled differently lost its fallback")
	}
}

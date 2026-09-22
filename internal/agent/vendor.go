package agent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// When winget's catalog is behind the vendor.
//
// Some vendors publish one download link that always serves their newest
// build, and winget's catalog records a hash of whatever that link served when
// the entry was last updated. The moment the vendor ships, the file and the
// hash disagree, and winget refuses: "Installer hash does not match; this
// cannot be overridden when running as admin". Watched in the VM, Google Chrome
// failed three identical attempts this way and the machine was handed over
// without a browser. Retrying cannot help -- the file will not change back.
//
// The hash was only ever standing in for a question: is this the vendor's own
// installer? The vendor answers that directly by signing it. So the agent
// fetches the same link winget did, and installs it only if Windows says the
// signature is valid and the signer is the publisher the package is named for.
// Anything else is refused, exactly as winget refused it. MSI only: every MSI
// takes the same silent switches, and an arbitrary installer's are anybody's
// guess.

// wingetHashMismatch is winget refusing because its catalog's hash for the
// installer no longer matches what the vendor's link serves.
const wingetHashMismatch = -1978335215 // 0x8A150011

// verifySignatureFn reports the Authenticode status and signer of a file. A
// variable so the tests never run PowerShell against a real file.
var verifySignatureFn = verifySignature

// httpClient fetches vendor installers. A variable so the tests can point it at
// a local server.
var httpClient = &http.Client{Timeout: 30 * time.Minute}

// installerURL is the link winget said it was downloading. The last one wins:
// a package can fetch more than one thing and the installer comes last.
func installerURL(out string) string {
	found := ""
	for _, ln := range strings.Split(out, "\n") {
		ln = strings.TrimSpace(ln)
		if i := strings.Index(ln, "Downloading "); i >= 0 {
			if u := strings.Fields(ln[i+len("Downloading "):]); len(u) > 0 {
				found = u[0]
			}
		}
	}
	return found
}

// publisherOf is the vendor a winget id is named for: Google.Chrome is Google's,
// Adobe.Acrobat.Reader.64-bit is Adobe's.
func publisherOf(id string) string {
	if i := strings.Index(id, "."); i > 0 {
		return id[:i]
	}
	return ""
}

// signedBy reports whether a certificate subject names the publisher, as its
// organisation or common name. "CN=Google LLC, O=Google LLC, ..." is Google's.
func signedBy(subject, publisher string) bool {
	if publisher == "" {
		return false
	}
	want := strings.ToLower(publisher)
	for _, part := range strings.Split(subject, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		k = strings.ToUpper(strings.TrimSpace(k))
		if k != "CN" && k != "O" {
			continue
		}
		for _, word := range strings.FieldsFunc(strings.ToLower(v), func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
		}) {
			if word == want {
				return true
			}
		}
	}
	return false
}

// vouchedFor reports whether this package may take the vendor fallback: it is
// one of the ids DSKY's own program list names, as recorded by the build.
//
// The gate is here rather than in the signature check because it is a
// different question. The signature check asks "did the publisher this id
// names sign this file"; this asks "is this an id whose name we chose". An
// operator can put any winget id in a recipe, and an id chosen freely also
// chooses which publisher name the signature has to match -- Acme.Thing is
// satisfied by anyone whose certificate says "Acme". Built-in ids are spelled
// by DSKY, so the publisher they imply is DSKY's to stand behind.
func (a *Agent) vouchedFor(id string) bool {
	if a.Manifest == nil || a.Manifest.Apps == nil {
		return false
	}
	for _, v := range a.Manifest.Apps.Vouched {
		if strings.EqualFold(v, id) {
			return true
		}
	}
	return false
}

// installFromVendor is the fallback for a package winget refused on its stale
// hash. It reports whether the package is now installed.
func (a *Agent) installFromVendor(id, wingetOut string) bool {
	link := installerURL(wingetOut)
	u, err := url.Parse(link)
	switch {
	case link == "" || err != nil:
		a.J.Info(stepApps, "%s: winget did not say where the installer is, so there is nothing to check", id)
		return false
	case u.Scheme != "https":
		a.J.Info(stepApps, "%s: the installer link is not HTTPS (%s); not fetching it", id, link)
		return false
	case !strings.EqualFold(path.Ext(u.Path), ".msi"):
		a.J.Info(stepApps, "%s: the installer is not an MSI, so its silent switches are unknown; not installing it", id)
		return false
	}
	publisher := publisherOf(id)
	a.J.Info(stepApps, "%s: fetching the installer from %s and checking %s signed it", id, u.Host, publisher)

	file := filepath.Join(os.TempDir(), "dsky-"+strings.NewReplacer(".", "-", "/", "-").Replace(id)+".msi")
	defer os.Remove(file)
	if err := download(link, file); err != nil {
		a.J.FailDetail(stepApps, id+": could not fetch the installer", err.Error())
		return false
	}

	status, subject, err := verifySignatureFn(file)
	if err != nil {
		a.J.FailDetail(stepApps, id+": could not check the installer's signature", err.Error())
		return false
	}
	if status != "Valid" {
		a.J.Fail(stepApps, "%s: the installer's signature is %q, not valid; not installing it", id, status)
		return false
	}
	if !signedBy(subject, publisher) {
		a.J.Fail(stepApps, "%s: the installer is signed by %q, not %s; not installing it", id, subject, publisher)
		return false
	}
	a.J.Info(stepApps, "%s: signature valid, signed by %s", id, subject)

	r := run(60*time.Minute, "msiexec", "/i", file, "/qn", "/norestart")
	a.J.Raw(r.Out)
	switch {
	case r.Err != nil:
		a.J.FailDetail(stepApps, id+": the vendor's installer did not finish", r.Err.Error())
		return false
	case r.Code == 0 || r.Code == 3010:
		a.J.Info(stepApps, "installed %s from the vendor's signed installer (exit %d)", id, r.Code)
		return true
	default:
		a.J.Fail(stepApps, "%s: the vendor's installer exited %d", id, r.Code)
		return false
	}
}

// download fetches link to file, refusing anything but a successful response.
func download(link, file string) error {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, link, nil)
	if err != nil {
		return err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", link, resp.Status)
	}
	f, err := os.Create(file)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

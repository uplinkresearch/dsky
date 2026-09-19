package agent

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const stepApps = "apps"

// msiPackageInvalid is msiexec's ERROR_INSTALL_PACKAGE_INVALID: the file is
// not a Windows Installer package, whatever it is named.
const msiPackageInvalid = 1620

// userInstallDeadline bounds one install run as the signed-in user.
const userInstallDeadline = 12 * time.Minute

// winget exit codes the agent reasons about. They arrive as signed 32-bit
// values; the rest are recorded as they come.
const (
	wingetAlreadyInstalled = -1978335189 // 0x8A15002B: nothing applicable to do
	wingetNoInstaller      = -1978335216 // 0x8A150010: no installer for that scope
	wingetProhibitsElev    = -1978335146 // 0x8A150056: installer refuses to run elevated
)

// findWinget waits for App Installer. winget arrives as a per-user MSIX that
// can still be registering at first sign-in, so it is waited for rather than
// assumed.
func (a *Agent) findWinget() string {
	candidates := []string{}
	if local := os.Getenv("LOCALAPPDATA"); local != "" {
		candidates = append(candidates, filepath.Join(local, `Microsoft\WindowsApps\winget.exe`))
	}
	deadline := time.Now().Add(5 * time.Minute)
	announced := false
	for {
		for _, c := range candidates {
			if _, err := os.Stat(c); err == nil {
				return c
			}
		}
		if p, err := lookPath("winget.exe"); err == nil {
			return p
		}
		if time.Now().After(deadline) {
			return ""
		}
		if !announced {
			a.J.Info(stepApps, "waiting for winget (App Installer) to become available")
			announced = true
		}
		time.Sleep(10 * time.Second)
	}
}

// connectTestURL is Microsoft's own connectivity endpoint: tiny, plain HTTP
// and unauthenticated, so it answers on a machine that has just been imaged
// and has no certificates or credentials of its own yet. A variable so tests
// can point it somewhere they control.
var connectTestURL = "http://www.msftconnecttest.com/connecttest.txt"

// onlineDeadline is how long a freshly imaged machine is given to reach the
// internet. Long enough for a wireless association and DHCP on a slow AP.
var onlineDeadline = 5 * time.Minute

// connectTestBody is what Microsoft's endpoint answers with. Checking it, and
// not merely that something answered, is the whole point of the endpoint.
//
// A captive portal -- hotel, airport, a guest network with a terms page --
// answers every request with its own login page, and answers it successfully.
// A machine behind one is not on the internet, but an HTTP client cannot tell
// the difference from the status code alone, and Go reports a 200 carrying a
// portal's HTML exactly as it reports a 200 carrying this. Asking for a known
// body is how Windows itself decides, and it is why this endpoint returns
// fourteen bytes of text rather than an empty 200.
const connectTestBody = "Microsoft Connect Test"

// reachedTheInternet reports whether the machine can actually fetch something
// from the internet, rather than whether something answered.
func reachedTheInternet(client *http.Client) bool {
	resp, err := client.Get(connectTestURL)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	// The real body is short; anything longer is somebody else's page.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1024))
	return err == nil && strings.Contains(string(body), connectTestBody)
}

// waitOnline waits for the machine to reach the network, because the packages
// come from the vendors. A machine that is simply not online yet should not
// be recorded as a pile of failures.
func (a *Agent) waitOnline() bool {
	client := &http.Client{Timeout: 10 * time.Second}
	deadline := time.Now().Add(onlineDeadline)
	announced := false
	for {
		if reachedTheInternet(client) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		if !announced {
			a.J.Info(stepApps, "waiting for the network")
			announced = true
		}
		time.Sleep(10 * time.Second)
	}
}

// appsStep installs every requested program, then the operator's own
// installers. One package failing never stops the others.
func (a *Agent) appsStep() {
	ap := a.Manifest.Apps
	if ap == nil {
		return
	}
	if len(ap.Winget) > 0 {
		wg := a.findWinget()
		if wg == "" {
			a.J.Fail(stepApps, "winget never appeared, no programs installed (a Windows 10 image may need App Installer from the Store first)")
		} else {
			a.J.Info(stepApps, "winget at %s", wg)
			a.UI.Detail("waiting for the network")
			if !a.waitOnline() {
				a.J.Info(stepApps, "no network yet; installs will be attempted anyway and can be re-run")
			}
			for i, id := range ap.Winget {
				a.UI.Detail(id + " (" + itoa(i+1) + " of " + itoa(len(ap.Winget)) + ")")
				a.installPackage(wg, id, ap.Scope)
			}
		}
	}
	for _, in := range ap.Installers {
		a.UI.Detail(in.File)
		a.runInstaller(in)
	}
	// Every installer above may have dropped an icon on the desktop.
	a.tidyDesktop()
}

// installPackage installs one winget package, handling the two ways Windows
// refuses: a package with no machine-wide installer, and a package whose
// installer refuses to run elevated at all.
func (a *Agent) installPackage(wg, id, scope string) {
	base := []string{"install", "--id", id, "--exact", "--silent",
		"--accept-package-agreements", "--accept-source-agreements", "--disable-interactivity"}

	attempts := [][]string{}
	if scope != "user" {
		attempts = append(attempts, append(append([]string{}, base...), "--scope", "machine"))
	}
	attempts = append(attempts, base, base)

	for i, args := range attempts {
		if i > 0 {
			time.Sleep(15 * time.Second)
		}
		a.J.Info(stepApps, "installing %s (attempt %d)", id, i+1)
		r := run(30*time.Minute, wg, args...)
		a.J.Raw(r.Out)
		switch r.Code {
		case 0:
			a.J.Info(stepApps, "installed %s", id)
			a.done(KindApp, id)
			return
		case wingetAlreadyInstalled:
			a.J.Info(stepApps, "%s is already present", id)
			a.done(KindApp, id)
			return
		case wingetProhibitsElev:
			// The agent runs elevated because pnputil requires it, and these
			// installers refuse an administrator outright. Run it as the
			// signed-in user with a standard token instead, which is the
			// context they expect.
			a.J.Info(stepApps, "%s refuses an elevated install; running it as the signed-in user", id)
			code, err := a.runAsSignedInUser(wg, base)
			switch {
			case err != nil:
				a.J.FailDetail(stepApps, id+" could not be installed as the signed-in user", err.Error())
				a.failed(KindApp, id, "it refuses an elevated install and could not be run as the signed-in user")
			case code == 0 || code == wingetAlreadyInstalled:
				a.J.Info(stepApps, "installed %s as the signed-in user", id)
				a.done(KindApp, id)
			default:
				a.J.Fail(stepApps, "%s as the signed-in user exited %d", id, code)
				a.failed(KindApp, id, "the installer exited "+itoa(code))
			}
			return
		case wingetHashMismatch:
			// The vendor has shipped since winget's catalog was updated. The
			// same file comes back on every attempt, so retrying is four
			// minutes of certain failure; check the vendor's own signature
			// instead, and stop either way.
			a.J.Info(stepApps, "%s: winget's catalog is behind the vendor's download (installer hash does not match)", id)
			if a.installFromVendor(id, r.Out) {
				a.done(KindApp, id)
			} else {
				a.J.Fail(stepApps, "could not install %s", id)
				a.failed(KindApp, id, "winget's catalog is behind the vendor's download, and the vendor's own installer did not run")
			}
			return
		case wingetNoInstaller:
			a.J.Info(stepApps, "%s has no installer for that scope, trying the next", id)
		default:
			a.J.Info(stepApps, "%s attempt %d exited %d", id, i+1, r.Code)
		}
		if r.Err != nil {
			a.J.FailDetail(stepApps, id+" did not finish", r.Err.Error())
			a.failed(KindApp, id, "the installer did not finish")
			return
		}
	}
	a.J.Fail(stepApps, "could not install %s", id)
	a.failed(KindApp, id, "every attempt to install it failed")
}

// runInstaller runs one of the operator's own installers from the stick.
// These need no network, so they are worth having even on a machine that
// never reached the internet.
func (a *Agent) runInstaller(in Installer) {
	src := filepath.Join(a.Dir, in.File)
	if _, err := os.Stat(src); err != nil {
		a.J.Fail(stepApps, "%s is not on the stick", in.File)
		a.failed(KindApp, installerName(in), "its installer is not on the stick")
		return
	}
	var r result
	if in.MSI {
		args := append([]string{"/i", src}, in.Args...)
		if len(in.Args) == 0 {
			args = append(args, "/qn", "/norestart")
		}
		r = run(60*time.Minute, "msiexec", args...)
	} else {
		r = run(60*time.Minute, src, in.Args...)
	}
	a.J.Raw(r.Out)
	switch {
	case r.Err != nil:
		a.J.FailDetail(stepApps, in.File+" did not finish", r.Err.Error())
	case r.Code == 0 || r.Code == 3010: // 3010: installed, wants a restart
		a.J.Info(stepApps, "installed %s (exit %d)", in.File, r.Code)
		a.done(KindApp, installerName(in))
	case r.Code == msiPackageInvalid:
		// Windows will not open the file as an installer package. Seen on a
		// bench: a ScreenConnect client saved as .msi that was really a
		// program, staged onto a stick and carried to a machine before
		// anybody found out.
		a.J.Fail(stepApps, "%s: Windows could not open this as an installer package (1620). %s",
			in.File, installerLooksLike(filepath.Join(a.Dir, in.File)))
		a.failed(KindApp, installerName(in), "Windows could not open the file as an installer package")
	default:
		a.J.FailDetail(stepApps, in.File+" exited "+itoa(r.Code), trimOut(r.Out))
		a.failed(KindApp, installerName(in), "its installer exited "+itoa(r.Code))
	}
}

// installerName is what to call one of the operator's own installers in the
// record. The staged file name is what the agent has; DSKY renders the report
// and holds the plan, so it is the thing that can turn migrate-arcgispro.msi
// back into "ArcGIS Pro" for somebody to read.
func installerName(in Installer) string { return in.File }

// itoa avoids pulling strconv in for one call site's sake.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}

// quoteArgs renders an argument list for a log line.
func quoteArgs(args []string) string { return strings.Join(args, " ") }

// installerLooksLike says what the file on the stick actually is, so the line
// in the log is something to act on rather than a number to look up.
func installerLooksLike(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return "The file could not be read on this machine."
	}
	defer f.Close()
	head := make([]byte, 8)
	n, _ := f.Read(head)
	head = head[:n]
	switch {
	case n >= 2 && head[0] == 'M' && head[1] == 'Z':
		return "It is a Windows program, not an MSI: add it to the recipe as the .exe installer."
	case n >= 4 && head[0] == 'P' && head[1] == 'K':
		return "It is a zip file: unpack it and add the installer inside."
	case n >= 4 && head[0] == '<':
		return "It is a web page: the download that produced it failed, so fetch the installer again."
	case n == 0:
		return "The file is empty."
	default:
		return "Whatever it is, it is not a Windows Installer package."
	}
}

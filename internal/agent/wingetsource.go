package agent

import (
	"fmt"
	"strings"
	"time"
)

// winget exit codes that say winget could not read its catalog at all. When
// one of these comes back, no package will install, whichever it is, so the
// cause is reported once instead of as a failure per program.
//
// Seen on an ASUS NUC15 with a fresh Windows 11: every program failed, every
// line said only "could not install", and nothing on the screen said why.
const (
	wingetDownloadFailed      = -1978335224 // 0x8A150008
	wingetSourcesInvalid      = -1978335221 // 0x8A15000B
	wingetSourceDataMissing   = -1978335217 // 0x8A15000F
	wingetNoPackageFound      = -1978335212 // 0x8A150014
	wingetNoSources           = -1978335211 // 0x8A150015
	wingetSourceOpenFailed    = -1978335163 // 0x8A150045
	wingetSourceAgreements    = -1978335162 // 0x8A150046
	wingetOpenAllSources      = -1978335157 // 0x8A15004B
	wingetServiceUnavailable  = -1978335123 // 0x8A15006D
	wingetInstallerExitFailed = -1978335159 // 0x8A150049
)

// wingetSource is the catalog every package id DSKY offers comes from. Naming
// it keeps a broken Microsoft Store source -- common on a machine whose Store
// has not updated itself yet -- from failing packages that never needed it.
const wingetSource = "winget"

// appInstallerURL is Microsoft's own link to the current App Installer, which
// is the package winget comes in.
const appInstallerURL = "https://aka.ms/getwinget"

// retryPause is the wait between install attempts. A variable so tests do not
// sleep.
var retryPause = 15 * time.Second

// isCatalogError reports whether a winget exit code means the catalog itself
// could not be read, rather than that one package's installer failed.
func isCatalogError(code int) bool {
	switch code {
	case wingetSourcesInvalid, wingetSourceDataMissing, wingetNoSources,
		wingetSourceOpenFailed, wingetSourceAgreements, wingetOpenAllSources,
		wingetServiceUnavailable:
		return true
	}
	return false
}

// hexCode writes a winget exit code the way Microsoft documents it.
func hexCode(code int) string { return fmt.Sprintf("0x%08X", uint32(int32(code))) }

// wingetMeaning says in words what a winget exit code means, so the line on
// the screen is something to act on rather than a number to look up.
func wingetMeaning(code int) string {
	var s string
	switch code {
	case wingetDownloadFailed:
		s = "the installer could not be downloaded"
	case wingetSourceDataMissing:
		s = "winget's catalog is missing or out of date on this PC (App Installer is probably too old)"
	case wingetSourcesInvalid, wingetNoSources:
		s = "winget has no usable catalog configured"
	case wingetSourceOpenFailed, wingetOpenAllSources:
		s = "winget could not open its catalog (usually no internet, or App Installer is too old)"
	case wingetSourceAgreements:
		s = "winget's catalog terms were not accepted"
	case wingetServiceUnavailable:
		s = "winget's catalog service is unavailable"
	case wingetNoPackageFound:
		s = "winget's catalog has no package with that id"
	case wingetNoInstaller:
		s = "the package has no installer for this PC"
	case wingetHashMismatch:
		s = "the vendor's download does not match winget's catalog"
	case wingetInstallerExitFailed:
		s = "the program's installer failed"
	default:
		return "winget exited " + itoa(code) + " (" + hexCode(code) + ")"
	}
	return s + " (" + hexCode(code) + ")"
}

// installArgs is the winget command line for one package. A package id with a
// dot in it (Google.Chrome) lives in the winget catalog; anything else is a
// Store product id and is left for winget to find.
func installArgs(id string) []string {
	args := []string{"install", "--id", id, "--exact"}
	if strings.Contains(id, ".") {
		args = append(args, "--source", wingetSource)
	}
	return append(args, "--silent",
		"--accept-package-agreements", "--accept-source-agreements", "--disable-interactivity")
}

// prepareWinget makes sure winget can read its catalog before any package is
// asked for. A freshly installed Windows carries the App Installer it shipped
// with, and that one can be too old to open today's catalog until the Store
// updates it, which can take an hour or more after first sign-in.
//
// In order: refresh the catalog; if that fails, reset it and refresh again;
// if that fails and the machine is online, install the current App Installer
// from Microsoft and refresh again. It reports whether the catalog is usable
// and, if not, why in words.
func (a *Agent) prepareWinget(wg string, online bool) (bool, string) {
	update := func() result {
		r := run(5*time.Minute, wg, "source", "update", "--name", wingetSource)
		a.J.Raw(r.Out)
		return r
	}
	r := update()
	if r.ok() {
		a.J.Info(stepApps, "winget's catalog is up to date")
		return true, ""
	}
	a.J.Info(stepApps, "winget could not refresh its catalog: %s", wingetMeaning(r.Code))
	if !online {
		return false, "this PC could not reach the internet, so winget could not fetch its catalog or any installer"
	}

	a.UI.Detail("repairing winget's catalog")
	reset := run(5*time.Minute, wg, "source", "reset", "--name", wingetSource, "--force")
	a.J.Raw(reset.Out)
	if r = update(); r.ok() {
		a.J.Info(stepApps, "winget's catalog works after a reset")
		return true, ""
	}

	a.UI.Detail("updating App Installer (winget)")
	if a.updateAppInstaller() {
		if r = update(); r.ok() {
			a.J.Info(stepApps, "winget's catalog works after updating App Installer")
			return true, ""
		}
	}
	return false, "winget could not read its catalog even after a reset and an App Installer update: " + wingetMeaning(r.Code)
}

// updateAppInstaller installs the current App Installer from Microsoft.
// Add-AppxPackage checks the package's signature itself and refuses anything
// Windows does not trust, so nothing unsigned can arrive this way.
func (a *Agent) updateAppInstaller() bool {
	ps := `$ErrorActionPreference = 'Stop'; $ProgressPreference = 'SilentlyContinue'; ` +
		`$p = Join-Path $env:TEMP 'dsky-appinstaller.msixbundle'; ` +
		`Invoke-WebRequest -UseBasicParsing -Uri '` + appInstallerURL + `' -OutFile $p; ` +
		`Add-AppxPackage -Path $p -ForceApplicationShutdown; Remove-Item $p -ErrorAction SilentlyContinue`
	r := run(15*time.Minute, "powershell", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", ps)
	a.J.Raw(r.Out)
	if !r.ok() {
		a.J.Info(stepApps, "could not update App Installer: %s", trimOut(r.Out))
		return false
	}
	a.J.Info(stepApps, "updated App Installer from %s", appInstallerURL)
	return true
}

// registerAppInstaller asks Windows to register App Installer for the signed-in
// user. On a fresh account the package is on disk but can take minutes to be
// registered on its own; this does it now.
func (a *Agent) registerAppInstaller() {
	r := run(5*time.Minute, "powershell", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command",
		"Add-AppxPackage -RegisterByFamilyName -MainPackage Microsoft.DesktopAppInstaller_8wekyb3d8bbwe")
	a.J.Raw(r.Out)
	if r.ok() {
		a.J.Info(stepApps, "asked Windows to register App Installer (winget)")
	} else {
		a.J.Info(stepApps, "could not register App Installer: %s", trimOut(r.Out))
	}
}

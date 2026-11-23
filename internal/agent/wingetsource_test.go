package agent

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// online points the connectivity check at a server that answers like
// Microsoft's, or like a dead network.
func online(t *testing.T, up bool) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !up {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte(connectTestBody))
	}))
	t.Cleanup(srv.Close)
	prevURL, prevDeadline, prevPause := connectTestURL, onlineDeadline, retryPause
	connectTestURL, onlineDeadline, retryPause = srv.URL, 0, time.Millisecond
	t.Cleanup(func() { connectTestURL, onlineDeadline, retryPause = prevURL, prevDeadline, prevPause })
}

func installCalls(calls []string, id string) int {
	n := 0
	for _, c := range calls {
		if strings.HasPrefix(c, "winget install --id "+id+" ") {
			n++
		}
	}
	return n
}

var freshPCApps = []string{"Google.Chrome", "7zip.7zip", "Zoom.Zoom", "Microsoft.Teams"}

// The NUC15 failure: winget is there, but cannot read its catalog. Every
// program used to fail three times each and say only "could not install".
// Now the catalog is repaired first, and if it cannot be, the cause is said
// once, in words, and the rest are not each tried three times for nothing.
func TestUnreadableCatalogIsReportedOnceWithItsCause(t *testing.T) {
	online(t, true)
	a, dir := newAgent(t, &Manifest{Version: ManifestVersion})
	f := &fakeRun{}
	f.do = func(name string, args []string) (result, error) {
		switch name {
		case "winget":
			return result{Code: wingetSourceDataMissing}, nil
		case "powershell":
			return result{Code: 1, Out: "Add-AppxPackage failed"}, nil
		}
		return result{}, nil
	}
	f.install(t)

	a.installAll("winget", freshPCApps, "")

	joined := strings.Join(f.calls, "\n")
	for _, want := range []string{"winget source update --name winget", "winget source reset --name winget --force", "powershell"} {
		if !strings.Contains(joined, want) {
			t.Errorf("never tried %q:\n%s", want, joined)
		}
	}
	if n := installCalls(f.calls, "7zip.7zip"); n != 0 {
		t.Errorf("tried 7zip %d times after the catalog was known to be unreadable", n)
	}
	log := logText(t, dir)
	if !strings.Contains(log, "4 programs were not installed because winget could not read its catalog") ||
		!strings.Contains(log, "0x8A15000F") ||
		!strings.Contains(log, "Google.Chrome, 7zip.7zip, Zoom.Zoom, Microsoft.Teams") {
		t.Errorf("the log does not say why, once:\n%s", log)
	}
	failed := 0
	for _, o := range a.outcomes {
		if o.State == StateFailed && o.Note != "" {
			failed++
		}
	}
	if failed != len(freshPCApps) {
		t.Errorf("%d of %d programs recorded as failed with a reason", failed, len(freshPCApps))
	}
}

// When updating App Installer is what fixes it, every program installs.
func TestUpdatingAppInstallerRepairsTheCatalog(t *testing.T) {
	online(t, true)
	a, dir := newAgent(t, &Manifest{Version: ManifestVersion})
	updated := false
	f := &fakeRun{}
	f.do = func(name string, args []string) (result, error) {
		if name == "powershell" {
			updated = true
			return result{}, nil
		}
		if name == "winget" && !updated {
			return result{Code: wingetSourceDataMissing}, nil
		}
		return result{}, nil
	}
	f.install(t)

	a.installAll("winget", freshPCApps, "")

	if !updated {
		t.Fatal("App Installer was never updated")
	}
	for _, o := range a.outcomes {
		if o.State != StateDone {
			t.Errorf("%s: %s %s", o.Name, o.State, o.Note)
		}
	}
	if strings.Contains(logText(t, dir), "FAILED") {
		t.Errorf("a repaired catalog still recorded failures:\n%s", logText(t, dir))
	}
}

// Offline, nothing is downloaded from Microsoft, and the screen says the
// machine never reached the internet.
func TestOfflineSaysSo(t *testing.T) {
	online(t, false)
	a, dir := newAgent(t, &Manifest{Version: ManifestVersion})
	f := &fakeRun{}
	f.do = func(name string, args []string) (result, error) {
		if name == "winget" {
			return result{Code: wingetDownloadFailed}, nil
		}
		return result{}, nil
	}
	f.install(t)

	a.installAll("winget", freshPCApps, "")

	for _, c := range f.calls {
		if strings.HasPrefix(c, "powershell") {
			t.Errorf("tried to fetch App Installer with no internet: %s", c)
		}
	}
	if n := installCalls(f.calls, "Zoom.Zoom"); n != 0 {
		t.Errorf("tried Zoom %d times with no internet", n)
	}
	if !strings.Contains(logText(t, dir), "could not reach the internet") {
		t.Errorf("the log does not say the PC was offline:\n%s", logText(t, dir))
	}
}

// One program failing on its own does not stop the others.
func TestOneBadInstallerDoesNotStopTheRest(t *testing.T) {
	online(t, true)
	a, dir := newAgent(t, &Manifest{Version: ManifestVersion})
	f := &fakeRun{}
	f.do = func(name string, args []string) (result, error) {
		if name == "winget" && len(args) > 2 && args[2] == "Google.Chrome" {
			return result{Code: wingetInstallerExitFailed}, nil
		}
		return result{}, nil
	}
	f.install(t)

	a.installAll("winget", freshPCApps, "")

	if n := installCalls(f.calls, "Microsoft.Teams"); n != 1 {
		t.Errorf("Teams tried %d times, want 1", n)
	}
	log := logText(t, dir)
	if !strings.Contains(log, "could not install Google.Chrome: the program's installer failed (0x8A150049)") {
		t.Errorf("the failure does not say what went wrong:\n%s", log)
	}
}

// Packages come from the winget catalog by name, so a broken Store source
// cannot fail them. A Store product id is left for winget to find.
func TestInstallArgsNameTheCatalog(t *testing.T) {
	got := strings.Join(installArgs("Google.Chrome"), " ")
	if !strings.Contains(got, "--source winget") {
		t.Errorf("Google.Chrome: %s", got)
	}
	got = strings.Join(installArgs("9WZDNCRFJ3PZ"), " ")
	if strings.Contains(got, "--source") {
		t.Errorf("a Store id was pinned to the winget catalog: %s", got)
	}
}

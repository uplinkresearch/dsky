package agent

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// onlineAt points the agent's connectivity check at a server the test controls,
// so waitOnline does not spend five minutes finding out that a build machine
// has no internet -- and does not pass by accident on one that has.
func onlineAt(t *testing.T, up bool) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !up {
			http.Error(w, "no", http.StatusInternalServerError)
			return
		}
		w.Write([]byte("Microsoft Connect Test"))
	}))
	t.Cleanup(srv.Close)
	prev := connectTestURL
	connectTestURL = srv.URL
	t.Cleanup(func() { connectTestURL = prev })
}

func TestWiFiStepJoinsTheNetwork(t *testing.T) {
	onlineAt(t, true)
	m := &Manifest{Version: ManifestVersion, Recipe: "w", Steps: []string{"wifi"},
		WiFi: &WiFi{SSID: "Uplink 5G", Profile: "wifi.xml"}}
	a, dir := newAgent(t, m)

	f := &fakeRun{}
	f.install(t)
	a.wifiStep()

	if len(f.calls) != 2 {
		t.Fatalf("expected an add and a connect, got %v", f.calls)
	}
	// user=all, so the profile belongs to the machine rather than to whoever
	// first boot happened to sign in as.
	if !strings.Contains(f.calls[0], "netsh wlan add profile") || !strings.Contains(f.calls[0], "user=all") {
		t.Errorf("profile not added for all users: %s", f.calls[0])
	}
	if !strings.Contains(f.calls[0], dir) {
		t.Errorf("the profile was not read from the agent's own directory: %s", f.calls[0])
	}
	if !strings.Contains(f.calls[1], `netsh wlan connect name=Uplink 5G`) {
		t.Errorf("did not connect to the network: %s", f.calls[1])
	}
	if got := logText(t, dir); !strings.Contains(got, "online") {
		t.Errorf("the log does not record reaching the network:\n%s", got)
	}
	if n := len(a.J.Failures()); n != 0 {
		t.Errorf("a successful join reported %d failure(s): %v", n, a.J.Failures())
	}
}

// A desktop with no wireless card is the ordinary case for this, and it must
// not be reported as though somebody typed the password wrong.
func TestWiFiStepSaysSoWhenThereIsNoRadio(t *testing.T) {
	onlineAt(t, true)
	m := &Manifest{Version: ManifestVersion, Recipe: "w", Steps: []string{"wifi"},
		WiFi: &WiFi{SSID: "Uplink", Profile: "wifi.xml"}}
	a, dir := newAgent(t, m)

	f := &fakeRun{do: func(name string, args []string) (result, error) {
		return result{Code: 1, Out: "There is no wireless interface on the system."}, nil
	}}
	f.install(t)
	a.wifiStep()

	if len(f.calls) != 1 {
		t.Errorf("it kept going after the profile could not be added: %v", f.calls)
	}
	if got := logText(t, dir); !strings.Contains(got, "no wireless interface") {
		t.Errorf("the log does not say what netsh said:\n%s", got)
	}
	if len(a.J.Failures()) != 1 {
		t.Errorf("expected one failure, got %v", a.J.Failures())
	}
}

// The failure that matters: the profile went on, Windows was happy, and the
// machine still cannot reach the vendors -- usually a wrong passphrase. Left
// unsaid, this becomes "it installed fine but there are no programs".
func TestWiFiStepReportsJoiningButNotReachingTheInternet(t *testing.T) {
	onlineAt(t, false)
	m := &Manifest{Version: ManifestVersion, Recipe: "w", Steps: []string{"wifi"},
		WiFi: &WiFi{SSID: "Uplink", Profile: "wifi.xml"}}
	a, dir := newAgent(t, m)
	// One attempt: the point here is the verdict, not the waiting.
	prev := onlineDeadline
	onlineDeadline = 0
	t.Cleanup(func() { onlineDeadline = prev })

	f := &fakeRun{}
	f.install(t)
	a.wifiStep()

	if len(a.J.Failures()) != 1 {
		t.Fatalf("expected one failure, got %v", a.J.Failures())
	}
	if got := logText(t, dir); !strings.Contains(got, "did not reach the internet") {
		t.Errorf("the log does not say the machine never got online:\n%s", got)
	}
}

// A connect that netsh refuses is not the end of it: the profile is on the
// machine and set to connect automatically, so the radio may associate a
// moment later. What decides the outcome is whether the machine got online.
func TestWiFiStepKeepsGoingWhenConnectIsRefused(t *testing.T) {
	onlineAt(t, true)
	m := &Manifest{Version: ManifestVersion, Recipe: "w", Steps: []string{"wifi"},
		WiFi: &WiFi{SSID: "Uplink", Profile: "wifi.xml"}}
	a, dir := newAgent(t, m)

	f := &fakeRun{do: func(name string, args []string) (result, error) {
		if strings.Contains(strings.Join(args, " "), "connect") {
			return result{Code: 1, Out: "The network is not available."}, nil
		}
		return result{}, nil
	}}
	f.install(t)
	a.wifiStep()

	if n := len(a.J.Failures()); n != 0 {
		t.Errorf("a refused connect that still came online was reported as failure: %v", a.J.Failures())
	}
	if got := logText(t, dir); !strings.Contains(got, "online") {
		t.Errorf("the log does not record reaching the network:\n%s", got)
	}
}

// A captive portal answers everything with its own login page, successfully.
// Believing it means declaring the machine online and then failing every
// install -- which is the same silent outcome as having no network at all,
// with the added confusion of a log that says the network came up.
func TestCaptivePortalIsNotTheInternet(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		online bool
	}{
		{"the real thing", 200, connectTestBody, true},
		{"a hotel login page", 200, "<html><body>Please accept our terms</body></html>", false},
		{"a portal redirect", 302, "", false},
		{"a broken proxy", 500, "upstream error", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			prev := connectTestURL
			connectTestURL = srv.URL
			defer func() { connectTestURL = prev }()

			if got := reachedTheInternet(srv.Client()); got != tc.online {
				t.Errorf("online=%v, want %v", got, tc.online)
			}
		})
	}
}

// Nothing configured, nothing done -- and in particular no netsh on a machine
// that was never asked to join anything.
func TestWiFiStepDoesNothingWithoutANetwork(t *testing.T) {
	m := &Manifest{Version: ManifestVersion, Recipe: "w", Steps: []string{"wifi"}}
	a, _ := newAgent(t, m)
	f := &fakeRun{}
	f.install(t)
	a.wifiStep()
	if len(f.calls) != 0 {
		t.Errorf("it ran something with no network configured: %v", f.calls)
	}
}

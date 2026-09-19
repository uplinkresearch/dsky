package agent

import (
	"path/filepath"
	"time"
)

const stepWiFi = "wifi"

// KindNetwork is the outcome kind for joining a network.
const KindNetwork = "network"

// netshTimeout bounds one netsh call. Both of these return promptly or not at
// all: the association itself is waited for separately, by looking for the
// network rather than by asking netsh to block.
const netshTimeout = 90 * time.Second

// wifiStep joins the machine to the wireless network the build was given.
//
// It runs after the drivers step, because on a fresh image the wireless card
// may have had no working driver until that step put one on, and before the
// programs, because the programs are what needs the network.
//
// Failing to join is recorded but never fatal. A machine that also has an
// ethernet cable in it is already online and does not care, and a machine
// that is not gains nothing from the agent stopping.
func (a *Agent) wifiStep() {
	w := a.Manifest.WiFi
	if w == nil || w.Profile == "" {
		return
	}
	profile := filepath.Join(a.Dir, w.Profile)

	// user=all puts the profile on the machine rather than in the profile of
	// whoever happens to be signed in, so it survives the account that first
	// boot created and applies to whoever the machine is handed to.
	a.UI.Detail("joining " + w.SSID)
	if r := run(netshTimeout, "netsh", "wlan", "add", "profile",
		"filename="+profile, "user=all"); !r.ok() {
		// The usual cause is a machine with no wireless card at all, which is
		// worth saying plainly: it is not a mistake in the password, and on a
		// desktop it is not a mistake at all.
		a.J.Fail(stepWiFi, "could not add the wireless profile for %q: %s", w.SSID, trimOut(r.Out))
		a.failed(KindNetwork, w.SSID, "the wireless profile could not be added: "+trimOut(r.Out))
		return
	}
	a.J.Info(stepWiFi, "added the wireless profile for %q", w.SSID)

	// Connecting explicitly rather than trusting the profile's auto mode: the
	// radio may already have been scanning before the profile existed, and
	// nothing makes it look again.
	if r := run(netshTimeout, "netsh", "wlan", "connect", "name="+w.SSID); !r.ok() {
		a.J.Info(stepWiFi, "connect to %q was refused (%s); waiting to see whether it joins by itself",
			w.SSID, trimOut(r.Out))
	}

	// What matters is reaching the internet, not what netsh reported: it
	// returns success as soon as it has accepted the request, long before the
	// association and DHCP have finished, and returns failure in cases that
	// then connect anyway.
	a.UI.Detail("waiting for " + w.SSID)
	if !a.waitOnline() {
		a.J.Fail(stepWiFi, "joined %q but the machine did not reach the internet", w.SSID)
		a.failed(KindNetwork, w.SSID, "the machine did not reach the internet after joining "+
			"(wrong password, or the network has no route out)")
		return
	}
	a.J.Info(stepWiFi, "on %q and online", w.SSID)
	a.done(KindNetwork, w.SSID)
}

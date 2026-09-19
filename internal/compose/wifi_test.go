package compose

import (
	"strings"
	"testing"

	"github.com/uplinkresearch/dsky/internal/recipe"
)

// Where the wireless step goes is not cosmetic. Before the drivers it may be
// talking to a card with no working driver; after the programs it is useless,
// because the programs are what needed the network.
func TestWiFiStepGoesAfterTheDrivers(t *testing.T) {
	for _, tc := range []struct {
		name  string
		steps []string
		want  []string
	}{
		{"the usual build", []string{"drivers", "debloat", "apps"}, []string{"drivers", "wifi", "debloat", "apps"}},
		{"no debloat", []string{"drivers", "apps"}, []string{"drivers", "wifi", "apps"}},
		{"no drivers at all", []string{"debloat", "apps"}, []string{"wifi", "debloat", "apps"}},
		{"a domain member", []string{"drivers", "debloat", "apps"}, []string{"drivers", "wifi", "debloat", "apps"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := insertWiFi(append([]string{}, tc.steps...), true)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// No network configured means no step, rather than a step that does nothing.
func TestNoWiFiStepWithoutANetwork(t *testing.T) {
	got := insertWiFi([]string{"drivers", "apps"}, false)
	if strings.Join(got, ",") != "drivers,apps" {
		t.Errorf("a build with no wireless got %v", got)
	}
}

// The bug a built image caught and no unit test had: the recipe holds
// "${var:wifi_password}", and rendering the profile straight from the spec put
// that text on the stick as the network's key. Windows accepts such a profile
// -- it is a valid document with a valid passphrase in it -- so there is no
// build failure and no error on the machine. It simply never authenticates,
// downloads nothing, and installs no programs.
func TestTheProfileCarriesThePassphraseAndNotItsName(t *testing.T) {
	spec := &recipe.WiFiSpec{SSID: "Uplink 5G", Password: "${var:wifi_password}"}
	got, err := wlanProfile(spec, map[string]string{"wifi_password": "correct-horse-battery"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "${var:") {
		t.Errorf("the profile carries an unexpanded variable:\n%s", got)
	}
	if !strings.Contains(got, "<keyMaterial>correct-horse-battery</keyMaterial>") {
		t.Errorf("the passphrase is not in the profile:\n%s", got)
	}
	// The spec itself is left alone: it belongs to the recipe, which is built
	// from more than once.
	if spec.Password != "${var:wifi_password}" {
		t.Errorf("the recipe's own spec was rewritten to %q", spec.Password)
	}
}

// A passphrase nobody supplied stops the build. Media that installs with a
// placeholder for a password is the failure this rule exists to prevent.
func TestAMissingPassphraseStopsTheBuild(t *testing.T) {
	_, err := wlanProfile(&recipe.WiFiSpec{SSID: "Uplink", Password: "${var:wifi_password}"}, nil)
	if err == nil {
		t.Fatal("a build with no passphrase was allowed")
	}
	if !strings.Contains(err.Error(), "wifi_password") {
		t.Errorf("the error does not name what is missing: %v", err)
	}
}

// The manifest tells the agent which network to join, and carries the name of
// the staged profile rather than the passphrase: the manifest is logged and
// read freely on the imaged machine.
func TestManifestCarriesTheNetworkButNotTheKey(t *testing.T) {
	r := &recipe.Recipe{ID: "w", Windows: &recipe.WindowsSpec{
		WiFi: &recipe.WiFiSpec{SSID: "Uplink 5G", Password: "correct-horse-battery"},
		Apps: &recipe.AppsSpec{Winget: []string{"7zip.7zip"}},
		Firstboot: recipe.FirstbootSpec{Mode: "generate", Steps: []recipe.Step{
			{Drivers: true}, {Apps: true},
		}},
	}}
	m := buildManifest(r, recipe.ResolvedDrivers{}, nil, nil, "", migrateParts{})

	if m.WiFi == nil {
		t.Fatal("the manifest carries no network")
	}
	if m.WiFi.SSID != "Uplink 5G" {
		t.Errorf("SSID is %q", m.WiFi.SSID)
	}
	if m.WiFi.Profile != recipe.WLANProfileName {
		t.Errorf("profile is %q, want %q", m.WiFi.Profile, recipe.WLANProfileName)
	}
	if strings.Join(m.Steps, ",") != "drivers,wifi,apps" {
		t.Errorf("steps are %v", m.Steps)
	}
}

func TestManifestHasNoNetworkWhenNoneWasAskedFor(t *testing.T) {
	r := &recipe.Recipe{ID: "w", Windows: &recipe.WindowsSpec{
		Apps:      &recipe.AppsSpec{Winget: []string{"7zip.7zip"}},
		Firstboot: recipe.FirstbootSpec{Mode: "generate", Steps: []recipe.Step{{Drivers: true}, {Apps: true}}},
	}}
	m := buildManifest(r, recipe.ResolvedDrivers{}, nil, nil, "", migrateParts{})
	if m.WiFi != nil {
		t.Errorf("a build with no wireless got %+v", m.WiFi)
	}
	for _, s := range m.Steps {
		if s == "wifi" {
			t.Errorf("steps include wifi: %v", m.Steps)
		}
	}
}

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
	m := buildManifest(r, recipe.ResolvedDrivers{}, nil, "", migrateParts{})

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
	m := buildManifest(r, recipe.ResolvedDrivers{}, nil, "", migrateParts{})
	if m.WiFi != nil {
		t.Errorf("a build with no wireless got %+v", m.WiFi)
	}
	for _, s := range m.Steps {
		if s == "wifi" {
			t.Errorf("steps include wifi: %v", m.Steps)
		}
	}
}

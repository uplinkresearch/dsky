package oscatalog

import (
	"context"
	"strings"
	"testing"
)

// A recipe outlives the build and is the copy somebody finds in the library,
// so the Wi-Fi passphrase goes in as a variable and travels to the answer file
// in memory -- the same discipline the administrator's password already uses.
func TestQuickRecipeKeepsTheWiFiPassphraseOut(t *testing.T) {
	e, ok := Get("windows-11")
	if !ok {
		t.Skip("no windows-11 in the catalog")
	}
	const pass = "correct-horse-battery"
	opts := Options{WiFiSSID: "Uplink 5G", WiFiPassword: pass}
	opts.defaults(e)
	y := recipeYAML(recipeMeta{ID: e.ID, Name: e.Name, Template: quickTemplate}, e, opts, nil)

	if strings.Contains(y, pass) {
		t.Errorf("the passphrase is written into the recipe:\n%s", y)
	}
	for _, want := range []string{`ssid: "Uplink 5G"`, `password: "${var:wifi_password}"`} {
		if !strings.Contains(y, want) {
			t.Errorf("recipe is missing %s:\n%s", want, y)
		}
	}
	vars, err := quickVars(opts)
	if err != nil {
		t.Fatal(err)
	}
	if vars["wifi_password"] != pass {
		t.Errorf("the passphrase does not reach the build: %q", vars["wifi_password"])
	}
}

// An open network has no passphrase to hide, and a password variable that
// resolves to nothing would make the profile unreadable to Windows.
func TestQuickRecipeOpenNetwork(t *testing.T) {
	e, ok := Get("windows-11")
	if !ok {
		t.Skip("no windows-11 in the catalog")
	}
	opts := Options{WiFiSSID: "Guest"}
	opts.defaults(e)
	y := recipeYAML(recipeMeta{ID: e.ID, Name: e.Name, Template: quickTemplate}, e, opts, nil)
	if strings.Contains(y, "wifi_password") {
		t.Errorf("an open network asked for a password variable:\n%s", y)
	}
	if !strings.Contains(y, `ssid: "Guest"`) {
		t.Errorf("the network is missing:\n%s", y)
	}
}

// No wireless means no wifi block at all, rather than an empty one.
func TestQuickRecipeWithoutWiFi(t *testing.T) {
	e, ok := Get("windows-11")
	if !ok {
		t.Skip("no windows-11 in the catalog")
	}
	opts := Options{}
	opts.defaults(e)
	if y := recipeYAML(recipeMeta{ID: e.ID, Name: e.Name, Template: quickTemplate}, e, opts, nil); strings.Contains(y, "wifi") {
		t.Errorf("a build with no wireless still mentions it:\n%s", y)
	}
}

// A saved recipe with a wireless network must come back as editable, with the
// network still in it. If FormFromRecipe cannot account for a field, the
// recipe becomes "not editable in the install dialog" -- so a build with Wi-Fi
// would have been a recipe Dusty could no longer change from the GUI.
func TestWiFiRecipeRoundTrip(t *testing.T) {
	lib, wsDir := editWorkspace(t)
	win, _ := Get("windows-11")
	opts := Options{Edition: "Pro", AccountMode: "local", Debloat: "standard",
		Apps: []string{"brave"}, WiFiSSID: "Uplink 5G", WiFiPassword: "hunter2hunter2"}
	if _, err := SaveRecipe(context.Background(), lib, wsDir, "desk", "Desk", win, opts, nil); err != nil {
		t.Fatal(err)
	}
	f, err := loadForm(t, wsDir, "desk")
	if err != nil {
		t.Fatalf("a recipe with a network came back not editable: %v", err)
	}
	if f.WiFiSSID != "Uplink 5G" {
		t.Errorf("the network did not survive the round trip: %q", f.WiFiSSID)
	}
	if !f.WiFiPassword {
		t.Error("the form does not know the network needs a passphrase")
	}
}

// An open network round-trips too, and must not come back claiming to need a
// passphrase -- that would make the dialog ask for one that does not exist.
func TestOpenWiFiRecipeRoundTrip(t *testing.T) {
	lib, wsDir := editWorkspace(t)
	win, _ := Get("windows-11")
	opts := Options{Edition: "Pro", AccountMode: "local", Debloat: "standard", WiFiSSID: "Guest"}
	if _, err := SaveRecipe(context.Background(), lib, wsDir, "g", "Guest", win, opts, nil); err != nil {
		t.Fatal(err)
	}
	f, err := loadForm(t, wsDir, "g")
	if err != nil {
		t.Fatalf("not editable: %v", err)
	}
	if f.WiFiSSID != "Guest" || f.WiFiPassword {
		t.Errorf("open network came back as %q needsPassword=%v", f.WiFiSSID, f.WiFiPassword)
	}
}

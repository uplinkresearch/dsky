package oscatalog

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/uplinkresearch/dsky/internal/recipe"
)

const neverExpiresCmd = "Set-LocalUser -Name $env:USERNAME -PasswordNeverExpires $true"

// The tick reaches the answer file as a first sign-in command that runs before
// firstboot.cmd, only for the local account DSKY creates, and only when asked
// for. A recipe saved before the option existed has no such var at all, and
// must still render: the template uses missingkey=error.
func TestPasswordNeverExpiresReachesTheAnswerFile(t *testing.T) {
	tmpl, err := templatesFS.ReadFile("templates/autounattend.xml.tmpl")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "autounattend.xml.tmpl")
	if err := os.WriteFile(path, tmpl, 0o644); err != nil {
		t.Fatal(err)
	}
	render := func(account string, extra map[string]string) string {
		t.Helper()
		vars := map[string]string{
			"locale": "en-US", "computer_name": "*", "edition_key": "KEY",
			"admin_user": "user", "admin_display_name": "User", "admin_password": "",
			"account_mode": account, "bypass_requirements": "0", "domain_mode": "",
		}
		for k, v := range extra {
			vars[k] = v
		}
		out, err := recipe.RenderTemplate(path, recipe.Context{Vars: vars})
		if err != nil {
			t.Fatalf("account=%s %v: %v", account, extra, err)
		}
		if p := undeclaredPrefix(t, out); p != "" {
			t.Errorf("undeclared namespace prefix on %s", p)
		}
		return out
	}

	on := render("local", map[string]string{"password_never_expires": "1"})
	i, j := strings.Index(on, neverExpiresCmd), strings.Index(on, "firstboot.cmd</CommandLine>")
	if i < 0 {
		t.Fatalf("ticked, but the answer file does not set it:\n%s", on)
	}
	if j < 0 || i > j {
		t.Errorf("the setting does not run before firstboot.cmd")
	}
	for name, out := range map[string]string{
		"older recipe, no var": render("local", nil),
		"not ticked":           render("local", map[string]string{"password_never_expires": "0"}),
		"normal Windows setup": render("oobe", map[string]string{"password_never_expires": "1"}),
	} {
		if strings.Contains(out, neverExpiresCmd) {
			t.Errorf("%s: the answer file sets it anyway", name)
		}
		if !strings.Contains(out, "firstboot.cmd</CommandLine>") {
			t.Errorf("%s: firstboot.cmd is missing", name)
		}
	}
}

// The recipe carries the line only when ticked, so a recipe saved before the
// option existed is still exactly what the dialog would save, and stays
// editable.
func TestPasswordNeverExpiresInTheRecipe(t *testing.T) {
	meta := recipeMeta{ID: "q", Name: "Quick", Template: "autounattend.xml.tmpl"}
	win := Entry{ID: "windows-11", Family: Windows}
	if y := recipeYAML(meta, win, Options{Edition: "Pro", AccountMode: "local"}, nil); strings.Contains(y, "password_never_expires") {
		t.Errorf("not ticked, but the recipe mentions it:\n%s", y)
	}
	if y := recipeYAML(meta, win, Options{Edition: "Pro", AccountMode: "oobe", PasswordNeverExpires: true}, nil); strings.Contains(y, "password_never_expires") {
		t.Errorf("normal Windows setup has no account of DSKY's, but the recipe asks for it:\n%s", y)
	}
	if y := recipeYAML(meta, win, Options{Edition: "Pro", AccountMode: "local", PasswordNeverExpires: true}, nil); !strings.Contains(y, `password_never_expires: "1"`) {
		t.Errorf("ticked, but the recipe does not say so:\n%s", y)
	}

	lib, wsDir := editWorkspace(t)
	w11, _ := Get("windows-11")
	opts := Options{Edition: "Pro", AccountMode: "local", PasswordNeverExpires: true}
	if _, err := SaveRecipe(context.Background(), lib, wsDir, "bench", "Bench", w11, opts, nil); err != nil {
		t.Fatal(err)
	}
	f, err := loadForm(t, wsDir, "bench")
	if err != nil {
		t.Fatal(err)
	}
	if !f.PasswordNeverExpires {
		t.Errorf("the tick did not come back when the recipe was opened: %+v", f)
	}
}

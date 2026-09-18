package oscatalog

import (
	"encoding/xml"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/uplinkresearch/dsky/internal/agent"
	"github.com/uplinkresearch/dsky/internal/recipe"
)

// TestQuickTemplateIsValidXMLInEveryMode renders the Install dialog's answer
// file for each combination that changes its shape, and checks every
// namespace prefix is declared. Setup rejects an answer file with an undeclared
// one, and Go's own decoder does not complain: the requirement-check bypass
// used wcm:action with no wcm declaration in scope, so every stick built with
// "Skip TPM / Secure Boot / RAM checks" carried an answer file Setup refuses.
func TestQuickTemplateIsValidXMLInEveryMode(t *testing.T) {
	tmpl, err := templatesFS.ReadFile("templates/autounattend.xml.tmpl")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "autounattend.xml.tmpl")
	if err := os.WriteFile(path, tmpl, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, bypass := range []string{"0", "1"} {
		for _, mode := range []string{"", "offline", "credentialed", "agent"} {
			for _, account := range []string{"local", "oobe"} {
				vars := map[string]string{
					"locale": "en-US", "computer_name": "*", "edition_key": "KEY",
					"admin_user": "user", "admin_display_name": "User", "admin_password": "",
					"account_mode": account, "bypass_requirements": bypass, "domain_mode": mode,
					"domain_odj_blob": "QUJD", "domain_join": "corp.example.com", "domain_ou": "",
					"domain_user": "u", "domain_account_domain": "CORP", "domain_password": "p",
				}
				out, err := recipe.RenderTemplate(path, recipe.Context{Vars: vars})
				if err != nil {
					t.Fatalf("bypass=%s mode=%q account=%s: %v", bypass, mode, account, err)
				}
				if p := undeclaredPrefix(t, out); p != "" {
					t.Errorf("bypass=%s mode=%q account=%s: undeclared namespace prefix on %s", bypass, mode, account, p)
				}
			}
		}
	}
}

func undeclaredPrefix(t *testing.T, doc string) string {
	t.Helper()
	d := xml.NewDecoder(strings.NewReader(doc))
	bound := func(space string) bool { return space == "" || strings.Contains(space, ":") || space == "xmlns" }
	for {
		tok, err := d.Token()
		if err == io.EOF {
			return ""
		}
		if err != nil {
			t.Fatalf("not XML: %v\n%s", err, doc)
		}
		if se, ok := tok.(xml.StartElement); ok {
			if !bound(se.Name.Space) {
				return se.Name.Space + ":" + se.Name.Local
			}
			for _, a := range se.Attr {
				if !bound(a.Name.Space) {
					return se.Name.Local + " @" + a.Name.Space + ":" + a.Name.Local
				}
			}
		}
	}
}

// The answer file's automatic sign-ins and the agent's restart cap are two
// numbers in two files that have to agree. One automatic sign-in was spent on
// the first boot, so the restart the agent takes after a driver sweep brought
// the machine back to a login prompt, half provisioned, with nothing to say
// so. Raise the cap without raising this and it happens again.
func TestTheAnswerFileCoversEveryRestartTheAgentMayTake(t *testing.T) {
	tmpl, err := templatesFS.ReadFile("templates/autounattend.xml.tmpl")
	if err != nil {
		t.Fatal(err)
	}
	want := agent.MaxRestarts + 1 // the first boot, plus one sign-in per restart
	need := "<LogonCount>" + strconv.Itoa(want) + "</LogonCount>"
	if !strings.Contains(string(tmpl), need) {
		t.Errorf("the answer file does not ask for %d automatic sign-ins (%s).\n"+
			"The agent may restart the machine %d time(s), and each restart needs one to come back.",
			want, need, agent.MaxRestarts)
	}
}

// A domain-joined machine signs itself in to run its first boot, so the
// administrator it signs in as needs a password. Two things have to be true at
// once: the password must reach the answer file, and it must not be written
// into the recipe the build leaves behind in the library -- a file somebody
// can read later is not where the password to a domain member belongs.
func TestAnAdminPasswordReachesTheAnswerFileAndNotTheRecipe(t *testing.T) {
	const pass = "Sup3rSecret!pw"
	opts := Options{Edition: "Pro", AccountMode: "local", AdminUser: "uplink", AdminPassword: pass}
	yaml := recipeYAML(recipeMeta{ID: "q", Name: "Quick", Template: "autounattend.xml.tmpl"},
		Entry{ID: "windows-11", Family: Windows}, opts, nil)
	if strings.Contains(yaml, pass) {
		t.Errorf("the password is written into the recipe on disk:\n%s", yaml)
	}
	if !strings.Contains(yaml, `admin_password: "${var:admin_password}"`) {
		t.Errorf("the recipe does not say where to find the password:\n%s", yaml)
	}
	v, err := quickVars(opts)
	if err != nil || v["admin_password"] != pass {
		t.Errorf("the build does not carry the password in memory: %v %v", v, err)
	}
	// Linux answers take a hash of the same password, never the password.
	if h := v["admin_password_hash"]; !strings.HasPrefix(h, "$6$") || strings.Contains(h, pass) {
		t.Errorf("the hash for Linux answers is wrong: %q", h)
	}
	// And with no password the recipe is exactly what it has always been, so
	// a standalone quick install is unchanged.
	plain := recipeYAML(recipeMeta{ID: "q", Name: "Quick", Template: "autounattend.xml.tmpl"},
		Entry{ID: "windows-11", Family: Windows}, Options{Edition: "Pro", AccountMode: "local"}, nil)
	if !strings.Contains(plain, `admin_password: ""`) {
		t.Errorf("a build with no password changed shape:\n%s", plain)
	}
	if v, err := quickVars(Options{}); v != nil || err != nil {
		t.Errorf("a build with no password carries a password var: %v %v", v, err)
	}

	// Rendered: the account gets it, the automatic sign-in gets it, and so
	// does the registry command that stands in for the sign-in OOBE refuses
	// to set on a domain-joined machine.
	tmpl, err := templatesFS.ReadFile("templates/autounattend.xml.tmpl")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "autounattend.xml.tmpl")
	if err := os.WriteFile(path, tmpl, 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := recipe.RenderTemplate(path, recipe.Context{Vars: map[string]string{
		"locale": "en-US", "computer_name": "*", "edition_key": "KEY",
		"admin_user": "uplink", "admin_display_name": "Uplink", "admin_password": pass,
		"account_mode": "local", "bypass_requirements": "1", "domain_mode": "offline",
		"domain_odj_blob": "QUJD",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(out, "<Value>"+pass+"</Value>"); n != 2 {
		t.Errorf("the password reached %d of the 2 places that need it", n)
	}
	if !strings.Contains(out, `/v DefaultPassword /t REG_SZ /d "`+pass+`"`) {
		t.Errorf("the automatic sign-in has no password to use:\n%s", out)
	}
	if strings.Contains(out, "<Value></Value>") {
		t.Errorf("an empty password element survived alongside a real password:\n%s", out)
	}
	if p := undeclaredPrefix(t, out); p != "" {
		t.Errorf("undeclared namespace prefix on %s", p)
	}
}

package compose

import (
	"encoding/xml"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/uplinkresearch/dsky/internal/recipe"
)

type runSync struct {
	Passes []struct {
		Pass       string `xml:"pass,attr"`
		Components []struct {
			Name     string `xml:"name,attr"`
			Commands []struct {
				Order       int    `xml:"Order"`
				Description string `xml:"Description"`
				Path        string `xml:"Path"`
			} `xml:"RunSynchronous>RunSynchronousCommand"`
		} `xml:"component"`
	} `xml:"settings"`
}

func specializeCommands(t *testing.T, doc string) []string {
	t.Helper()
	var u runSync
	if err := xml.Unmarshal([]byte(doc), &u); err != nil {
		t.Fatalf("does not parse: %v\n%s", err, doc)
	}
	var out []string
	deployments := 0
	for _, p := range u.Passes {
		if p.Pass != "specialize" {
			continue
		}
		for _, c := range p.Components {
			if c.Name != "Microsoft-Windows-Deployment" {
				continue
			}
			deployments++
			for _, cmd := range c.Commands {
				out = append(out, strings.Repeat("#", cmd.Order)+" "+cmd.Description)
				if cmd.Description == oemCopyDescription && cmd.Path != oemCopyCommand {
					t.Errorf("path came back as %q", cmd.Path)
				}
			}
		}
	}
	if deployments > 1 {
		t.Errorf("%d Deployment components in specialize; Setup allows one", deployments)
	}
	return out
}

func TestOEMCopyCommandFits(t *testing.T) {
	// Setup's limit for a RunSynchronous path.
	if n := len(oemCopyCommand); n > 259 {
		t.Errorf("command is %d characters, over 259", n)
	}
}

// The copy goes first in every shape of answer file: DSKY's own template in
// each domain mode (by serial number already has a specialize command), and
// files with no specialize pass or no Deployment component.
func TestWithOEMCopy(t *testing.T) {
	tmpl, err := os.ReadFile(filepath.Join("..", "oscatalog", "templates", "autounattend.xml.tmpl"))
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "u.tmpl")
	os.WriteFile(p, tmpl, 0o644)
	base := map[string]string{"locale": "en-US", "account_mode": "local", "admin_user": "user", "admin_display_name": "User", "admin_password": "", "computer_name": "*", "edition_key": "X", "bypass_requirements": "1", "domain_account_domain": "", "domain_join": "", "domain_ou": "", "domain_password": "", "domain_user": ""}
	for _, mode := range []string{"", "offline", "agent"} {
		vars := map[string]string{}
		for k, v := range base {
			vars[k] = v
		}
		vars["domain_mode"] = mode
		vars["domain_odj_blob"] = "QUJD"
		rendered, err := recipe.RenderTemplate(p, recipe.Context{Vars: vars, Recipe: &recipe.Recipe{ID: "t"}})
		if err != nil {
			t.Fatalf("%q: %v", mode, err)
		}
		got, err := withOEMCopy(rendered)
		if err != nil {
			t.Fatalf("%q: %v", mode, err)
		}
		cmds := specializeCommands(t, got)
		if len(cmds) == 0 || cmds[0] != "# "+oemCopyDescription {
			t.Errorf("%q: specialize commands %v, want the copy first", mode, cmds)
		}
		// An agent-applied join puts nothing in the answer file at all, so the
		// copy is the only specialize command and there is nothing after it to
		// renumber.
		if mode == "agent" && len(cmds) != 1 {
			t.Errorf("agent: the answer file should carry no join: %v", cmds)
		}
		if mode == "offline" && len(cmds) < 2 {
			t.Errorf("an offline join should still sign the machine in automatically: %v", cmds)
		}
		again, _ := withOEMCopy(got)
		if again != got {
			t.Errorf("%q: adding twice changed the file", mode)
		}
	}

	bare := `<?xml version="1.0"?><unattend xmlns="urn:schemas-microsoft-com:unattend"><settings pass="oobeSystem"></settings></unattend>`
	got, err := withOEMCopy(bare)
	if err != nil || len(specializeCommands(t, got)) != 1 {
		t.Errorf("no specialize pass: %v\n%s", err, got)
	}
	noDeploy := `<unattend xmlns="urn:schemas-microsoft-com:unattend"><settings pass="specialize"><component name="Microsoft-Windows-Shell-Setup"></component></settings></unattend>`
	got, err = withOEMCopy(noDeploy)
	if err != nil || len(specializeCommands(t, got)) != 1 {
		t.Errorf("no Deployment component: %v\n%s", err, got)
	}
	if _, err := withOEMCopy("<unattend>"); err == nil {
		t.Error("broken answer file accepted")
	}
	if !regexp.MustCompile(`xcopy %d:\\sources\\\$OEM\$\\\$\$ C:\\Windows`).MatchString(oemCopyCommand) {
		t.Error("command does not copy $OEM$\\$$ to C:\\Windows")
	}
}

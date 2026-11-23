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

// The copy goes first in every shape of answer file: DSKY's own template, on
// its own and with the serial-naming command, and files with no specialize
// pass or no Deployment component.
func TestWithOEMCopy(t *testing.T) {
	rendered := renderBuiltInTemplate(t)
	got, err := withOEMCopy(rendered)
	if err != nil {
		t.Fatal(err)
	}
	if cmds := specializeCommands(t, got); len(cmds) != 1 || cmds[0] != "# "+oemCopyDescription {
		t.Errorf("specialize commands %v, want only the copy", cmds)
	}
	if again, _ := withOEMCopy(got); again != got {
		t.Error("adding twice changed the file")
	}

	// Naming by serial runs a script the copy puts on the machine, so the
	// copy must still come first and the naming straight after it.
	named, err := withSerialName(rendered)
	if err != nil {
		t.Fatal(err)
	}
	if named, err = withOEMCopy(named); err != nil {
		t.Fatal(err)
	}
	want := []string{"# " + oemCopyDescription, "## " + serialNameDescription} // # count is the Order
	if cmds := specializeCommands(t, named); strings.Join(cmds, "|") != strings.Join(want, "|") {
		t.Errorf("specialize commands %v, want %v", cmds, want)
	}
	if !strings.Contains(named, `<Order>2</Order>`) || !strings.Contains(named, recipe.SerialNameScriptName) {
		t.Errorf("the naming command is not second, or does not run the script:\n%s", named)
	}
	if n := len(serialNameCommand); n > 259 {
		t.Errorf("naming command is %d characters, over 259", n)
	}

	bare := `<?xml version="1.0"?><unattend xmlns="urn:schemas-microsoft-com:unattend"><settings pass="oobeSystem"></settings></unattend>`
	got, err = withOEMCopy(bare)
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

// renderBuiltInTemplate renders DSKY's own answer file template with the
// values a quick install gives it.
func renderBuiltInTemplate(t *testing.T) string {
	t.Helper()
	tmpl, err := os.ReadFile(filepath.Join("..", "oscatalog", "templates", "autounattend.xml.tmpl"))
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "u.tmpl")
	if err := os.WriteFile(p, tmpl, 0o644); err != nil {
		t.Fatal(err)
	}
	vars := map[string]string{"locale": "en-US", "account_mode": "local", "admin_user": "user",
		"admin_display_name": "User", "admin_password": "", "computer_name": "*", "edition_key": "X",
		"bypass_requirements": "1"}
	rendered, err := recipe.RenderTemplate(p, recipe.Context{Vars: vars, Recipe: &recipe.Recipe{ID: "t"}})
	if err != nil {
		t.Fatal(err)
	}
	return rendered
}

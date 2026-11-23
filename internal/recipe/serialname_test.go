package recipe

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/uplinkresearch/dsky/internal/testpwsh"
)

func TestSerialName(t *testing.T) {
	for _, c := range SerialNameCases {
		got, _ := SerialName(c.Serial, c.MAC)
		if got != c.Want {
			t.Errorf("serial %q, mac %q: got %q, want %q", c.Serial, c.MAC, got, c.Want)
		}
		if len(got) > 15 {
			t.Errorf("%q is over Windows' 15-character limit", got)
		}
	}
}

// The script is what runs on the machine and SerialName is what the tests
// read, so they must agree on every case.
func TestSerialNameScriptAgreesWithGo(t *testing.T) {
	pwsh := testpwsh.Find()
	if pwsh == "" {
		t.Skip("no PowerShell that runs here")
	}
	script := filepath.Join(t.TempDir(), SerialNameScriptName)
	if err := os.WriteFile(script, []byte(SerialNameScriptFile()), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, c := range SerialNameCases {
		out, err := exec.Command(pwsh, "-NoProfile", "-NonInteractive", "-File", script,
			"-Serial", c.Serial, "-Mac", c.MAC, "-CheckOnly").CombinedOutput()
		if err != nil {
			t.Fatalf("script failed: %v\n%s", err, out)
		}
		want := c.Want
		if want == "" {
			want = "none"
		}
		if got := strings.TrimSpace(string(out)); got != want {
			t.Errorf("serial %q, mac %q: script says %q, Go says %q", c.Serial, c.MAC, got, want)
		}
	}
}

func TestNameBySerialNeedsAnAnswerFileAndIsChecked(t *testing.T) {
	r := appsRecipe("machine", "Google.Chrome")
	r.Windows.NameBySerial = true
	r.Windows.Unattend = &UnattendSpec{Template: "t.tmpl"}
	if err := r.Validate(); err != nil {
		t.Fatalf("a recipe that names by serial was refused: %v", err)
	}
	if got := renderVerify(r, ResolvedDrivers{}); !strings.Contains(got, "$env:COMPUTERNAME -like 'PC-*'") {
		t.Errorf("the post-install check never looks at the name:\n%s", got)
	}
	r.Windows.Unattend = nil
	if err := r.Validate(); err == nil || !strings.Contains(err.Error(), "name_by_serial") {
		t.Errorf("name_by_serial with no answer file: %v", err)
	}
	plain := appsRecipe("machine", "Google.Chrome")
	if got := renderVerify(plain, ResolvedDrivers{}); strings.Contains(got, "COMPUTERNAME -like") {
		t.Error("a recipe that does not name by serial still checks the name")
	}
}

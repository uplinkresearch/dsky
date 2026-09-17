package oscatalog

import (
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/uplinkresearch/dsky/internal/appcatalog"
)

func TestFedoraKickstart(t *testing.T) {
	plan, err := appcatalog.ResolveFedora([]string{"vlc", "obsidian", "chrome", "dsky"})
	if err != nil {
		t.Fatal(err)
	}
	ks := fedoraKickstart(plan)
	if strings.Contains(ks, "{{") {
		t.Errorf("the kickstart is rendered as a Go template, so it may not contain {{:\n%s", ks)
	}
	// --ignoremissing is load-bearing: install media carries a subset of the
	// archive, and one name it happens not to have ends the whole install
	// with "No match for argument" after the disk is already erased.
	if !strings.Contains(ks, "%packages --ignoremissing") {
		t.Errorf("%%packages is not tolerant of names this medium lacks:\n%s", ks)
	}
	// No account in DSKY's answers, the same decision as Ubuntu Server's.
	for _, forbidden := range []string{"rootpw", "user --name"} {
		if strings.Contains(ks, forbidden) {
			t.Errorf("the kickstart carries %q; accounts are asked for on screen", forbidden)
		}
	}
	if !strings.Contains(ks, "clearpart --all") || !strings.Contains(ks, "reboot") {
		t.Errorf("the kickstart does not erase and reboot:\n%s", ks)
	}

	// The first-boot script travels base64-encoded in %post; decode it and
	// check the shell that will run it can parse it.
	var script string
	for _, line := range strings.Split(ks, "\n") {
		if strings.HasPrefix(line, "echo ") && strings.Contains(line, "dsky-apps.sh") {
			raw, err := base64.StdEncoding.DecodeString(strings.Fields(line)[1])
			if err != nil {
				t.Fatal(err)
			}
			script = string(raw)
		}
	}
	if script == "" {
		t.Fatalf("no first-boot script in the kickstart:\n%s", ks)
	}
	for _, want := range []string{
		"dnf install -y -q vlc",               // Fedora's own repositories
		"flathub md.obsidian.Obsidian",        // Flathub
		"/etc/yum.repos.d/google-chrome.repo", // a vendor's rpm repository
		"uplinkresearch/dsky/releases",        // a published release
		"/var/lib/dsky/apps-done",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("first-boot script lacks %q", want)
		}
	}
	// gpgcheck stays on, so a package that is not the vendor's cannot install
	// quietly.
	if !strings.Contains(script, "gpgcheck=1") || !strings.Contains(script, "rpm --import") {
		t.Errorf("the vendor repository is added without checking signatures:\n%s", script)
	}
	if bash, err := exec.LookPath("bash"); err == nil {
		f := filepath.Join(t.TempDir(), "dsky-apps.sh")
		os.WriteFile(f, []byte(script), 0o755)
		if out, err := exec.Command(bash, "-n", f).CombinedOutput(); err != nil {
			t.Errorf("first-boot script does not parse: %v\n%s", err, out)
		}
	}

	// Nothing picked: nothing to run.
	empty, _ := appcatalog.ResolveFedora(nil)
	if !empty.Empty() || strings.Contains(fedoraKickstart(empty), "dsky-apps.sh") {
		t.Error("a first-boot pass was written for an empty program list")
	}
}

// Only the entries whose installer has actually been watched taking a
// kickstart offer programs, and they get a kickstart rather than Ubuntu's
// answers.
func TestFedoraProgramsReachTheRecipe(t *testing.T) {
	e, ok := Get("fedora-44-server")
	if !ok {
		t.Fatal("fedora-44-server missing")
	}
	if !e.ProgramsSupported() || e.AppTarget() != appcatalog.TargetFedora {
		t.Fatalf("fedora-44-server: programs %v target %v", e.ProgramsSupported(), e.AppTarget())
	}
	// "Drivers for this computer" is Ubuntu's question; Anaconda has no
	// equivalent, so the dialog must not offer a control that does nothing.
	if e.ThirdPartyDriversSupported() {
		t.Error("fedora-44-server offers third-party drivers, which it cannot act on")
	}
	dir := t.TempDir()
	rel, err := writeLinuxAnswers(dir, e.ID, e, Options{Apps: []string{"vlc", "chrome"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(rel, ".cfg.tmpl") {
		t.Errorf("Fedora was given %q, which is not a kickstart", rel)
	}
	body, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), programsMarker+"vlc chrome") {
		t.Errorf("the picks are not recorded for reopening the dialog:\n%s", body)
	}

	// Fedora Workstation is a live image: %packages is not what installs it,
	// so it is not offered until that is a job somebody has done.
	ws, _ := Get("fedora-44-workstation")
	if ws.ProgramsSupported() {
		t.Error("Fedora Workstation offers programs; its live installer does not take them this way")
	}
	if err := CheckPrograms(e, []string{"steam"}); err == nil {
		t.Error("steam accepted for Fedora, which has no acceptable source for it")
	}
}

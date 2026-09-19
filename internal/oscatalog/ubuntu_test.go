package oscatalog

import (
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/uplinkresearch/dsky/internal/appcatalog"
	"github.com/uplinkresearch/dsky/internal/library"
)

type autoinstallDoc struct {
	Autoinstall struct {
		Version     int      `yaml:"version"`
		Interactive []string `yaml:"interactive-sections"`
		Identity    any      `yaml:"identity"`
		Storage     struct {
			Layout struct {
				Name string `yaml:"name"`
			} `yaml:"layout"`
		} `yaml:"storage"`
		Drivers *struct {
			Install bool `yaml:"install"`
		} `yaml:"drivers"`
		Packages []string `yaml:"packages"`
		Snaps    []struct {
			Name    string `yaml:"name"`
			Classic bool   `yaml:"classic"`
		} `yaml:"snaps"`
		LateCommands []string `yaml:"late-commands"`
		Shutdown     string   `yaml:"shutdown"`
	} `yaml:"autoinstall"`
}

func TestUbuntuUserData(t *testing.T) {
	plan, err := appcatalog.ResolveUbuntu([]string{"vlc", "vscode", "obsidian", "chrome"})
	if err != nil {
		t.Fatal(err)
	}
	for _, desktop := range []bool{false, true} {
		out := ubuntuUserData(plan, desktop, false, linuxAccount{})
		if !strings.HasPrefix(out, "#cloud-config\n") || strings.Contains(out, "{{") {
			t.Fatalf("desktop=%v: not a plain cloud-config:\n%s", desktop, out)
		}
		var doc autoinstallDoc
		if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
			t.Fatalf("desktop=%v: not YAML: %v\n%s", desktop, err, out)
		}
		a := doc.Autoinstall
		if a.Version != 1 || a.Identity != nil {
			t.Errorf("desktop=%v: %+v (no account may be in DSKY's answers)", desktop, a)
		}
		// Each edition gets the layout its own installer would choose: Ubuntu
		// Server's guided install uses LVM, Desktop's a plain partition. DSKY
		// said "direct" for both, which matched Desktop by accident and gave
		// every server a disk that cannot be grown or snapshotted later.
		wantLayout := "lvm"
		if desktop {
			wantLayout = "direct"
		}
		if a.Storage.Layout.Name != wantLayout {
			t.Errorf("desktop=%v: storage layout %q, want %q", desktop, a.Storage.Layout.Name, wantLayout)
		}
		// Server asks for the account on screen; Desktop's installer asks itself.
		if server := !desktop; server != (len(a.Interactive) == 1 && a.Interactive[0] == "identity") {
			t.Errorf("desktop=%v: interactive sections %v", desktop, a.Interactive)
		}
		if strings.Join(a.Packages, ",") != "vlc" || len(a.Snaps) != 1 || !a.Snaps[0].Classic {
			t.Errorf("desktop=%v: packages %v snaps %+v", desktop, a.Packages, a.Snaps)
		}
		// The first-boot script travels base64-encoded in a late-command;
		// decode it and check bash can parse it.
		var script string
		for _, c := range a.LateCommands {
			if strings.Contains(c, "dsky-apps.sh") && strings.HasPrefix(c, "echo ") {
				b64 := strings.Fields(c)[1]
				raw, err := base64.StdEncoding.DecodeString(b64)
				if err != nil {
					t.Fatal(err)
				}
				script = string(raw)
			}
		}
		for _, want := range []string{"flatpak install --system -y --noninteractive flathub md.obsidian.Obsidian", "google-chrome-stable", "/var/lib/dsky/apps-done"} {
			if !strings.Contains(script, want) {
				t.Errorf("first-boot script lacks %q", want)
			}
		}
		if bash, err := exec.LookPath("bash"); err == nil {
			f := filepath.Join(t.TempDir(), "dsky-apps.sh")
			os.WriteFile(f, []byte(script), 0o755)
			if out, err := exec.Command(bash, "-n", f).CombinedOutput(); err != nil {
				t.Errorf("first-boot script does not parse: %v\n%s", err, out)
			}
		}
	}

	// Even a plan the installer can carry out entirely by itself gets a
	// first-boot pass, because that is what confirms the programs actually
	// arrived on a machine whose network was late.
	plain, _ := appcatalog.ResolveUbuntu([]string{"vlc"})
	out := ubuntuUserData(plain, true, false, linuxAccount{})
	if !strings.Contains(out, "late-commands") || !strings.Contains(out, "dsky-apps.sh") {
		t.Error("no first-boot pass for a plan that still needs confirming")
	}

	// Nothing picked at all: nothing to run.
	empty, _ := appcatalog.ResolveUbuntu(nil)
	if !empty.Empty() || strings.Contains(ubuntuUserData(empty, true, false, linuxAccount{}), "dsky-apps.sh") {
		t.Error("a first-boot pass was written for an empty program list")
	}

	// The first-boot service must neither be ordered after cloud-init nor
	// wait for it, however much its race with cloud-init invites both.
	// cloud-final.service is itself ordered after multi-user.target, which
	// this unit is wanted by, so either one closes an ordering cycle:
	// After= makes systemd break it by deleting this job, and the service
	// silently never runs at all; waiting inside the script deadlocks the
	// boot instead. Both were watched happening on Ubuntu Server 26.04.
	script := firstBootScript(t, ubuntuUserData(plan, false, false, linuxAccount{}))
	for _, forbidden := range []string{"cloud-final", "cloud-init status"} {
		if strings.Contains(ubuntuFirstBootUnit, forbidden) {
			t.Errorf("the first-boot unit mentions %q, which closes an ordering cycle", forbidden)
		}
		if strings.Contains(script, forbidden) {
			t.Errorf("the first-boot script waits on %q, which deadlocks the boot", forbidden)
		}
	}
	// What replaces them: apt waits for the lock cloud-init is holding, and a
	// snap is waited for rather than declared missing the moment snapd, which
	// has not been asked for it yet, looks idle.
	if !strings.Contains(script, "DPkg::Lock::Timeout") {
		t.Error("apt does not wait for dpkg's lock, which cloud-init is still holding")
	}
	if !strings.Contains(script, `[ "$i" -le "$grace" ]`) {
		t.Error("the snap waiter has no grace period before it races cloud-init")
	}

	// Drivers for this computer, Ubuntu's way: its installer is told to put on
	// the proprietary ones it finds, and nothing is staged on the media.
	drivers := ubuntuUserData(empty, true, true, linuxAccount{})
	var doc autoinstallDoc
	if err := yaml.Unmarshal([]byte(drivers), &doc); err != nil {
		t.Fatalf("drivers answers are not YAML: %v\n%s", err, drivers)
	}
	if doc.Autoinstall.Drivers == nil || !doc.Autoinstall.Drivers.Install {
		t.Errorf("the answers do not ask for third-party drivers:\n%s", drivers)
	}
	if strings.Contains(ubuntuUserData(empty, true, false, linuxAccount{}), "drivers:") {
		t.Error("third-party drivers were asked for when nobody asked")
	}
}

func TestUbuntuProgramsReachTheRecipe(t *testing.T) {
	lib, err := library.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for id, wantPatch := range map[string]bool{"ubuntu-26.04-server": true, "ubuntu-24.04-server": true, "ubuntu-26.04-desktop": false} {
		e, ok := Get(id)
		if !ok {
			t.Fatalf("%s missing", id)
		}
		if !e.ProgramsSupported() {
			t.Fatalf("%s: programs not offered", id)
		}
		dir, err := scaffoldQuickWorkspace(lib, e, Options{Apps: []string{"vlc", "brave"}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		r := assertLoads(t, dir, e.ID)
		a := r.Linux
		if a == nil || a.Autoinstall == nil || a.Autoinstall.PatchKernel() != wantPatch {
			t.Fatalf("%s: autoinstall %+v, want kernel_patch %v", id, a, wantPatch)
		}
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(a.Autoinstall.UserData))); err != nil {
			t.Fatalf("%s: user data not written: %v", id, err)
		}
		// Without programs, Ubuntu is written as it is.
		dir, _ = scaffoldQuickWorkspace(lib, e, Options{}, nil)
		if r := assertLoads(t, dir, e.ID); r.Linux != nil && r.Linux.Autoinstall != nil {
			t.Fatalf("%s: autoinstall without programs", id)
		}
	}
	// A distro whose installer does not read Ubuntu's answers must never be
	// given them, whatever the options say. Asking for drivers and then
	// changing the operating system is the way this happened: the answers are
	// Ubuntu autoinstall, and appending them to another distro's ISO leaves a
	// CIDATA partition nothing will ever read — on a stick that looks built.
	fedora, _ := Get("fedora-44-workstation")
	dir, err := scaffoldQuickWorkspace(lib, fedora, Options{ThirdPartyDrivers: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r := assertLoads(t, dir, fedora.ID); r.Linux != nil && r.Linux.Autoinstall != nil {
		t.Error("Fedora was given Ubuntu's autoinstall answers")
	}
	if err := CheckPrograms(fedora, []string{"vlc"}); err == nil {
		t.Fatal("programs accepted for Fedora")
	}
	win, _ := Get("windows-11")
	if err := CheckPrograms(win, []string{"nosuchprogram"}); err == nil {
		t.Fatal("an unknown program accepted for Windows")
	}
}

// firstBootScript pulls DSKY's first-boot script back out of the answers,
// where it travels base64-encoded inside a late-command.
func firstBootScript(t *testing.T, userData string) string {
	t.Helper()
	var doc autoinstallDoc
	if err := yaml.Unmarshal([]byte(userData), &doc); err != nil {
		t.Fatalf("answers are not YAML: %v", err)
	}
	for _, c := range doc.Autoinstall.LateCommands {
		if !strings.Contains(c, "dsky-apps.sh") || !strings.HasPrefix(c, "echo ") {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(strings.Fields(c)[1])
		if err != nil {
			t.Fatalf("the first-boot script is not base64: %v", err)
		}
		return string(raw)
	}
	t.Fatal("no first-boot script in the answers")
	return ""
}

// The release installer runs on both families, so it must not reach for a
// tool only one of them has. dpkg --print-architecture left $arch empty on
// Fedora, which made the asset name "dsky-v0.7.41-linux-" and the download a
// 404 — reported as "no v0.7.41 build for ", with nothing after "for".
func TestReleaseInstallIsPortable(t *testing.T) {
	ubuntu, _ := appcatalog.ResolveUbuntu([]string{"dsky"})
	fedora, _ := appcatalog.ResolveFedora([]string{"dsky"})
	for name, script := range map[string]string{
		"ubuntu": ubuntuFirstBootScript(ubuntu),
		"fedora": fedoraFirstBootScript(fedora),
	} {
		if strings.Contains(script, "dpkg --print-architecture") {
			t.Errorf("%s: the release installer asks dpkg for the architecture; Fedora has no dpkg", name)
		}
		if !strings.Contains(script, `x86_64) arch=amd64 ;;`) {
			t.Errorf("%s: no portable architecture mapping:\n%s", name, script)
		}
		// Both scripts have to parse with the shell that will run them.
		if bash, err := exec.LookPath("bash"); err == nil {
			f := filepath.Join(t.TempDir(), "dsky-apps.sh")
			os.WriteFile(f, []byte(script), 0o755)
			if out, err := exec.Command(bash, "-n", f).CombinedOutput(); err != nil {
				t.Errorf("%s: first-boot script does not parse: %v\n%s", name, err, out)
			}
		}
	}
}

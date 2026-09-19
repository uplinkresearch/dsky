package compose

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/uplinkresearch/dsky/internal/fsimg"
	"github.com/uplinkresearch/dsky/internal/library"
	"github.com/uplinkresearch/dsky/internal/manifest"
	"github.com/uplinkresearch/dsky/internal/recipe"
	"github.com/uplinkresearch/dsky/internal/workspace"
)

// TestComposeWindowsTreeMode drives the full Windows pipeline against a
// synthetic captured-master tree: scaffolded workspace templates, an INF
// driver pack from the workspace, a cab + agent MSI from the library, and a
// generated firstboot — then reads every product back out of the image.
func TestComposeWindowsTreeMode(t *testing.T) {
	t.Setenv("SOURCE_DATE_EPOCH", "1756800000")
	root := t.TempDir()

	// ── Workspace ────────────────────────────────────────────────────────
	wsDir := filepath.Join(root, "ws")
	if err := workspace.Scaffold(wsDir, "Test Org"); err != nil {
		t.Fatal(err)
	}

	// ── Synthetic captured master (the "working stick") ──────────────────
	tree := filepath.Join(root, "master")
	writeFile(t, filepath.Join(tree, "autounattend.xml"), []byte("<stale/>"))
	writeFile(t, filepath.Join(tree, "efi", "boot", "bootx64.efi"), make([]byte, 1<<20))
	writeFile(t, filepath.Join(tree, "sources", "boot.wim"), make([]byte, 2<<20))
	writeFile(t, filepath.Join(tree, "sources", "install.swm"), make([]byte, 4<<20))
	writeFile(t, filepath.Join(tree, "sources", "ei.cfg"), []byte("stale"))

	// ── Workspace-local INF driver pack ──────────────────────────────────
	writeFile(t, filepath.Join(wsDir, "drivers", "intel-lan", "e1d.inf"), []byte("[Version]\r\n"))
	writeFile(t, filepath.Join(wsDir, "drivers", "intel-lan", "e1d.sys"), make([]byte, 4096))

	// ── Library blobs: an SST-style cab and an agent MSI ─────────────────
	lib, err := library.Open(filepath.Join(root, "lib"))
	if err != nil {
		t.Fatal(err)
	}
	cabHost := filepath.Join(root, "intel-sst.cab")
	writeFile(t, cabHost, []byte("MSCFfake-cab-bytes"))
	if _, err := lib.Import(&manifest.Source{ID: "intel-sst-cab", Kind: manifest.KindDriverPack, Format: manifest.FormatCab, Filename: "intel-sst.cab"}, cabHost); err != nil {
		t.Fatal(err)
	}
	msiHost := filepath.Join(root, "Agent.ClientSetup.msi")
	writeFile(t, msiHost, []byte("fake-msi-bytes"))
	if _, err := lib.Import(&manifest.Source{ID: "test-agent-msi", Kind: manifest.KindPayload, Format: manifest.FormatMsi}, msiHost); err != nil {
		t.Fatal(err)
	}

	// ── Recipe ───────────────────────────────────────────────────────────
	recipeYAML := `version: 1
id: sff-test
name: "Test SFF PC stick"
os:
  type: windows
  source_mode: tree
  tree_path: ` + filepath.ToSlash(tree) + `
target:
  volume_label: ESD-USB
  min_stick: 1GiB
windows:
  ei_cfg: { edition: Professional, channel: Retail, vl: false }
  unattend:
    template: templates/autounattend.xml.tmpl
    vars:
      edition_key: VK7JG-NPHTM-C97JM-9MPGT-3V66T
      locale: en-US
  driver_packs:
    - { path: drivers/intel-lan, install: pnputil-sweep }
    - { ref: intel-sst-cab, install: expand-then-sweep }
  debloat:
    preset: standard
    remove_apps: [Contoso.Bloatware]
    keep_apps: [Microsoft.BingWeather]
  firstboot:
    mode: generate
    steps:
      - drivers
      - wait: 10s
      - msi: { ref: test-agent-msi, args: ["/qn"] }
`
	writeFile(t, filepath.Join(wsDir, "recipes", "sff-test.yaml"), []byte(recipeYAML))

	ws, err := workspace.Load(wsDir)
	if err != nil {
		t.Fatal(err)
	}
	r, err := ws.Recipe("sff-test")
	if err != nil {
		t.Fatal(err)
	}
	if findings := r.Lint(); len(findings) != 0 {
		t.Fatalf("unexpected lint findings: %v", findings)
	}

	art, err := Build(context.Background(), Request{Workspace: ws, Library: lib, Recipe: r})
	if err != nil {
		t.Fatal(err)
	}
	if art.Kind != "image" || art.SHA256 == "" {
		t.Fatalf("unexpected artifact: %+v", art)
	}

	// ── Verify the composed image contents ───────────────────────────────
	sizes, err := fsimg.ReadTreeSizes(art.Path)
	if err != nil {
		t.Fatal(err)
	}
	scripts := "/sources/$OEM$/$$/Setup/Scripts"
	for _, want := range []string{
		"/autounattend.xml",
		"/sources/ei.cfg",
		"/sources/boot.wim",
		"/sources/install.swm",
		"/efi/boot/bootx64.efi",
		scripts + "/firstboot.cmd",
		scripts + "/intel-sst.cab",
		scripts + "/Agent.ClientSetup.msi",
		scripts + "/Drivers/intel-lan/e1d.inf",
		scripts + "/Drivers/intel-lan/e1d.sys",
	} {
		if _, ok := sizes[want]; !ok {
			t.Errorf("missing from image: %s", want)
		}
	}

	unattend := readImageFile(t, art.Path, "/autounattend.xml")
	for _, want := range []string{
		"VK7JG-NPHTM-C97JM-9MPGT-3V66T",
		"<UILanguage>en-US</UILanguage>",
		`cmd /c C:\Windows\Setup\Scripts\firstboot.cmd`,
		"<Name>user</Name>",
	} {
		if !strings.Contains(unattend, want) {
			t.Errorf("unattend missing %q", want)
		}
	}
	if strings.Contains(unattend, "{{") {
		t.Error("unattend has unrendered template markers")
	}

	eiCfg := readImageFile(t, art.Path, "/sources/ei.cfg")
	if want := "[EditionID]\r\nProfessional\r\n[Channel]\r\nRetail\r\n[VL]\r\n0\r\n"; eiCfg != want {
		t.Errorf("ei.cfg = %q, want %q", eiCfg, want)
	}

	fb := readImageFile(t, art.Path, scripts+"/firstboot.cmd")
	checksInOrder := []string{
		`expand "%SCRIPTS%\intel-sst.cab" -F:* "%SCRIPTS%\Drivers\intel-sst-cab"`,
		`pnputil /add-driver "%SCRIPTS%\Drivers\*.inf" /subdirs /install`,
		`powershell -NoProfile -ExecutionPolicy Bypass -File "%SCRIPTS%\debloat.ps1"`,
		`ping -n 11 127.0.0.1 > nul`,
		`msiexec /i "%SCRIPTS%\Agent.ClientSetup.msi" /qn /l*v`,
	}
	pos := -1
	for _, want := range checksInOrder {
		idx := strings.Index(fb, want)
		if idx < 0 {
			t.Errorf("firstboot missing %q\n---\n%s", want, fb)
			continue
		}
		if idx < pos {
			t.Errorf("firstboot step out of order: %q", want)
		}
		pos = idx
	}
	if !strings.Contains(fb, "\r\n") {
		t.Error("firstboot.cmd is not CRLF")
	}

	// The debloat pass is staged, honors keep/remove overrides, and sets
	// the policy keys.
	if _, ok := sizes[scripts+"/debloat.ps1"]; !ok {
		t.Fatal("debloat.ps1 missing from image")
	}
	db := readImageFile(t, art.Path, scripts+"/debloat.ps1")
	for _, want := range []string{
		"'Microsoft.BingNews',",
		"'Contoso.Bloatware',", // remove_apps extension
		"Remove-AppxProvisionedPackage",
		"TurnOffWindowsCopilot",
		"DisableWindowsConsumerFeatures",
		"AllowNewsAndInterests",
	} {
		if !strings.Contains(db, want) {
			t.Errorf("debloat.ps1 missing %q", want)
		}
	}
	if strings.Contains(db, "Microsoft.BingWeather") {
		t.Error("keep_apps exception ignored — BingWeather still in the removal list")
	}
	if !strings.Contains(db, "\r\n") {
		t.Error("debloat.ps1 is not CRLF")
	}

	// The stale master files must have been replaced by overlays.
	if strings.Contains(unattend, "stale") {
		t.Error("master autounattend.xml leaked through the overlay")
	}
	if eiCfg == "stale" {
		t.Error("master ei.cfg leaked through the overlay")
	}
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func readImageFile(t *testing.T, imgPath, filePath string) string {
	t.Helper()
	rc, closeImg, err := fsimg.OpenImageFile(imgPath, filePath)
	if err != nil {
		t.Fatalf("open %s in image: %v", filePath, err)
	}
	defer closeImg()
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The stick check exists so nobody writes an image to a device that cannot
// hold it. It was reading the recipe's guess, made before the image existed,
// and Windows 11 25H2 composes to about 8.9 GiB against a recipe that says 8
// GiB -- so the check passed exactly the stick it is there to refuse, and the
// write failed part way through instead.
func TestTheStickMinimumIsNeverSmallerThanTheImage(t *testing.T) {
	r := &recipe.Recipe{Target: recipe.TargetSpec{MinStick: "8GiB"}}
	const eightGiB = 8 << 30
	if got := minStickBytes(r, 0); got != eightGiB {
		t.Errorf("with no image size, min = %d, want the recipe's %d", got, int64(eightGiB))
	}
	if got := minStickBytes(r, 4<<30); got != eightGiB {
		t.Errorf("a small image lowered the recipe's minimum to %d", got)
	}
	const nineGiB = 9 << 30
	if got := minStickBytes(r, nineGiB); got != nineGiB {
		t.Errorf("min = %d for a %d-byte image; an 8 GB stick would be accepted and then run out",
			got, int64(nineGiB))
	}
	// A recipe with no figure at all still refuses a stick too small for the
	// image, which it never did before.
	if got := minStickBytes(&recipe.Recipe{}, nineGiB); got != nineGiB {
		t.Errorf("min = %d with no recipe figure", got)
	}
}

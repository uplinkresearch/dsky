package compose

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	diskfs "github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/disk"
	"github.com/diskfs/go-diskfs/filesystem"
	"github.com/diskfs/go-diskfs/filesystem/iso9660"
	"github.com/diskfs/go-diskfs/partition/gpt"

	"github.com/uplinkresearch/dsky/internal/library"
	"github.com/uplinkresearch/dsky/internal/manifest"
	"github.com/uplinkresearch/dsky/internal/workspace"
)

// ubuntuGrubCfg is the byte-exact /boot/grub/grub.cfg from the Ubuntu
// 24.04.5 live-server ISO (573 bytes).
const ubuntuGrubCfg = "set timeout=30\n\nloadfont unicode\n\nset menu_color_normal=white/black\nset menu_color_highlight=black/light-gray\n\nmenuentry \"Try or Install Ubuntu Server\" {\n\tset gfxpayload=keep\n\tlinux\t/casper/vmlinuz  ---\n\tinitrd\t/casper/initrd\n}\nmenuentry \"Ubuntu Server with the HWE kernel\" {\n\tset gfxpayload=keep\n\tlinux\t/casper/hwe-vmlinuz  ---\n\tinitrd\t/casper/hwe-initrd\n}\ngrub_platform\nif [ \"$grub_platform\" = \"efi\" ]; then\nmenuentry 'Boot from next volume' {\n\texit 1\n}\nmenuentry 'UEFI Firmware Settings' {\n\tfwsetup\n}\nelse\nmenuentry 'Test memory' {\n\tlinux16 /boot/memtest86+x64.bin\n}\nfi\n"

func TestGrubAutoinstallMenu(t *testing.T) {
	if len(ubuntuGrubCfg) != 573 {
		t.Fatalf("fixture drifted: %d bytes", len(ubuntuGrubCfg))
	}
	out, err := grubAutoinstallMenu([]byte(ubuntuGrubCfg))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 573 {
		t.Errorf("replacement is %d bytes, must equal original 573", len(out))
	}
	for _, want := range []string{"linux\t/casper/vmlinuz autoinstall ---", "initrd\t/casper/initrd", "set timeout=2"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("menu missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(string(out), "hwe") {
		t.Error("HWE entry should not survive the rewrite")
	}
}

// makeHybridISO builds a small ISO9660 image holding /boot/grub/grub.cfg
// and stamps a GPT into its system area, mimicking an isohybrid ISO.
func makeHybridISO(t *testing.T, path string) {
	t.Helper()
	const size = 8 << 20
	d, err := diskfs.Create(path, size, diskfs.SectorSize(2048))
	if err != nil {
		t.Fatal(err)
	}
	fsys, err := d.CreateFilesystem(disk.FilesystemSpec{Partition: 0, FSType: filesystem.TypeISO9660, WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := fsys.Mkdir("/boot/grub"); err != nil {
		t.Fatal(err)
	}
	f, err := fsys.OpenFile("/boot/grub/grub.cfg", os.O_CREATE|os.O_RDWR)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte(ubuntuGrubCfg)); err != nil {
		t.Fatal(err)
	}
	if c, ok := f.(io.Closer); ok {
		c.Close() // iso9660 records the size on close, before Finalize
	}
	if f2, err := fsys.OpenFile("/casper/vmlinuz", os.O_CREATE|os.O_RDWR); err == nil {
		f2.Write(make([]byte, 4096))
		if c, ok := f2.(io.Closer); ok {
			c.Close()
		}
	}
	iso, ok := fsys.(*iso9660.FileSystem)
	if !ok {
		t.Fatalf("unexpected filesystem type %T", fsys)
	}
	// Rock Ridge, like real Ubuntu media, so lowercase paths resolve.
	if err := iso.Finalize(iso9660.FinalizeOptions{VolumeIdentifier: "TESTISO", RockRidge: true}); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	// GPT in the ISO system area (LBA 1–33 sit inside the first 32 KiB).
	d, err = diskfs.Open(path, diskfs.WithOpenMode(diskfs.ReadWrite))
	if err != nil {
		t.Fatal(err)
	}
	const isoSectors = (2 << 20) / 512
	err = d.Partition(&gpt.Table{
		ProtectiveMBR: false,
		Partitions: []*gpt.Partition{{
			Index: 1, Start: 64, End: isoSectors - 1, Size: (isoSectors - 64) * 512,
			Type: gpt.EFISystemPartition, Name: "ISO9660",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestComposeLinuxAutoinstall drives the full linux-iso pipeline against a
// synthetic hybrid ISO: GRUB rewritten in place for zero-touch boot, CIDATA
// partition appended with rendered cloud-init data, artifact hashed.
func TestComposeLinuxAutoinstall(t *testing.T) {
	root := t.TempDir()
	wsDir := filepath.Join(root, "ws")
	if err := workspace.Scaffold(wsDir, "Test Org"); err != nil {
		t.Fatal(err)
	}
	isoPath := filepath.Join(root, "test.iso")
	makeHybridISO(t, isoPath)

	lib, err := library.Open(filepath.Join(root, "lib"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lib.Import(&manifest.Source{ID: "test-iso", Kind: manifest.KindOSImage, Format: manifest.FormatISO}, isoPath); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(wsDir, "manifests", "test-iso.yaml"), []byte("id: test-iso\nkind: os-image\nformat: iso\n"))
	writeFile(t, filepath.Join(wsDir, "recipes", "ubuntu-auto.yaml"), []byte(`version: 1
id: ubuntu-auto
os:
  type: linux-iso
  source: test-iso
linux:
  autoinstall:
    user_data: templates/autoinstall.yaml.tmpl
    vars:
      hostname: testbox
      admin_password_hash: "${var:admin_password_hash}"
`))

	ws, err := workspace.Load(wsDir)
	if err != nil {
		t.Fatal(err)
	}
	r, err := ws.Recipe("ubuntu-auto")
	if err != nil {
		t.Fatal(err)
	}
	art, err := Build(context.Background(), Request{Workspace: ws, Library: lib, Recipe: r})
	if err != nil {
		t.Fatal(err)
	}
	if art.Kind != "image" || art.SHA256 == "" {
		t.Fatalf("unexpected artifact: %+v", art)
	}

	// GRUB is rewritten, same length, visible through ISO9660.
	cfg, err := readISOFile(art.Path, grubCfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg) != 573 || !strings.Contains(string(cfg), "/casper/vmlinuz autoinstall ---") {
		t.Errorf("grub.cfg not rewritten in place:\n%s", cfg)
	}

	// CIDATA partition exists, is FAT with the right label, and carries
	// the rendered cloud-init data.
	d, err := diskfs.Open(art.Path, diskfs.WithOpenMode(diskfs.ReadOnly))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	tbl, err := d.GetPartitionTable()
	if err != nil {
		t.Fatal(err)
	}
	parts := tbl.GetPartitions()
	if len(parts) != 2 {
		t.Fatalf("expected 2 partitions, got %d", len(parts))
	}
	gt := tbl.(*gpt.Table)
	if gt.Partitions[1].Name != "CIDATA" {
		t.Errorf("second partition named %q, want CIDATA", gt.Partitions[1].Name)
	}
	cid, err := d.GetFilesystem(2)
	if err != nil {
		t.Fatalf("CIDATA filesystem: %v", err)
	}
	if lbl := cid.Label(); !strings.EqualFold(strings.TrimSpace(lbl), "CIDATA") {
		t.Errorf("CIDATA label = %q", lbl)
	}
	ud, err := cid.OpenFile("user-data", os.O_RDONLY)
	if err != nil {
		t.Fatalf("user-data: %v", err)
	}
	udb, _ := io.ReadAll(ud)
	for _, want := range []string{"#cloud-config", "hostname: testbox", "username: user", "$6$dsky$", "autoinstall:"} {
		if !bytes.Contains(udb, []byte(want)) {
			t.Errorf("user-data missing %q:\n%s", want, udb)
		}
	}
	md, err := cid.OpenFile("meta-data", os.O_RDONLY)
	if err != nil {
		t.Fatalf("meta-data: %v", err)
	}
	mdb, _ := io.ReadAll(md)
	if !bytes.HasPrefix(mdb, []byte("instance-id: dsky-ubuntu-auto-")) {
		t.Errorf("meta-data = %q", mdb)
	}

	// The isohybrid MBR sector must be untouched by the GPT rewrite.
	orig, err := os.ReadFile(isoPath)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(art.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(orig[:512], got[:512]) {
		t.Error("sector 0 changed — the isohybrid MBR must be preserved")
	}
}

// The Fedora Server 44 menu, as published. Two things about it decide whether
// a kickstart stick works at all: it ships with default="1", and entry 1 is
// the media check — which fails on any media with answers appended to it,
// because it hashes the whole device.
const fedoraGrubCfg = `set default="1"

function load_video {
  insmod efi_gop
  insmod all_video
}

load_video
set gfxpayload=keep
insmod gzio
insmod part_gpt
insmod ext2

set timeout=60
### END /etc/grub.d/00_header ###

search --no-floppy --set=root -l 'Fedora-S-dvd-x86_64-44'

### BEGIN /etc/grub.d/10_linux ###
menuentry 'Install Fedora 44' --class fedora --class gnu-linux --class gnu --class os {
	linux /images/pxeboot/vmlinuz inst.stage2=hd:LABEL=Fedora-S-dvd-x86_64-44 quiet
	initrd /images/pxeboot/initrd.img
}
menuentry 'Test this media & install Fedora 44' --class fedora --class gnu-linux --class gnu --class os {
	linux /images/pxeboot/vmlinuz inst.stage2=hd:LABEL=Fedora-S-dvd-x86_64-44 rd.live.check quiet
	initrd /images/pxeboot/initrd.img
}
`

func TestGrubKickstartMenu(t *testing.T) {
	out, err := grubKickstartMenu([]byte(fedoraGrubCfg))
	if err != nil {
		t.Fatal(err)
	}
	// Same length, so it can be written back into the ISO in place.
	if len(out) != len(fedoraGrubCfg) {
		t.Fatalf("menu is %d bytes, the original is %d", len(out), len(fedoraGrubCfg))
	}
	got := string(out)
	// The installer must start: no media check, and the entry that runs is
	// the only one there.
	if strings.Contains(got, "rd.live.check") {
		t.Error("the media check survived; it fails on media with answers appended and halts the install")
	}
	if !strings.Contains(got, "set default=0") {
		t.Error("the menu does not select its own only entry")
	}
	// Not a short countdown: Fedora's GRUB drew the menu, ignored one, and sat
	// there with the installer unstarted. Nothing to wait for, nothing to stop.
	if !strings.Contains(got, "set timeout=0") || !strings.Contains(got, "timeout_style=hidden") {
		t.Errorf("the menu waits instead of booting:\n%s", got)
	}
	// inst.stage2 is not optional: without it Anaconda cannot find its own
	// installer image and stops at a shell.
	if !strings.Contains(got, "inst.stage2=hd:LABEL=Fedora-S-dvd-x86_64-44") {
		t.Errorf("the kernel line lost the arguments Anaconda needs:\n%s", got)
	}
	// The kickstart rides in as a second initramfs, which is the whole point:
	// nothing is mounted, so nothing can be busy. GRUB finds it by label.
	if !strings.Contains(got, "initrd\t/images/pxeboot/initrd.img ($ksdev)/ks.img") {
		t.Errorf("the kickstart is not loaded as a second initramfs:\n%s", got)
	}
	if !strings.Contains(got, "search --no-floppy --set=ksdev -l DSKYKS") {
		t.Errorf("nothing looks for the answers partition:\n%s", got)
	}
	// The kickstart is named, not left to be discovered: Fedora Server 44 came
	// up asking for a language with a labelled partition sitting on the same
	// stick. The label is not OEMDRV, because Anaconda's driver-disk stage
	// claims an OEMDRV volume and holds it against the kickstart fetch.
	if !strings.Contains(got, "inst.ks=file:/ks.cfg") {
		t.Errorf("the kernel line does not say where the kickstart is:\n%s", got)
	}
	// Without the ISO's own search line, $root is not the install medium and
	// every path in the entry misses. GRUB then drops back to the menu, which
	// is indistinguishable on screen from a timeout that never fired.
	if !strings.Contains(got, "search --no-floppy --set=root -l 'Fedora-S-dvd-x86_64-44'") {
		t.Errorf("the search line that sets $root was dropped:\n%s", got)
	}

	// A config with no entries at all is a mistake worth refusing rather than
	// writing an empty menu into somebody's media.
	if _, err := grubKickstartMenu([]byte("set default=0\n")); err == nil {
		t.Error("a grub.cfg with no linux line was accepted")
	}
}

// An Anaconda ISO carries its UEFI menu twice -- in the ISO9660 tree and
// inside the El Torito EFI boot image embedded in the same file -- so patching
// the first copy and reading back the second is a rewrite that silently did
// not take. Both have to be written.
func TestFindAllBytes(t *testing.T) {
	needle := []byte("MENUENTRY-NEEDLE")
	var buf []byte
	var want []int64
	for i := 0; i < 3; i++ {
		want = append(want, int64(len(buf)))
		buf = append(buf, needle...)
		buf = append(buf, bytes.Repeat([]byte{0x41}, 9<<20)...) // across chunks
	}
	p := filepath.Join(t.TempDir(), "img")
	if err := os.WriteFile(p, buf, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := findAllBytes(p, needle)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("found %d copies at %v, want %d at %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("copy %d at %d, want %d", i, got[i], want[i])
		}
	}
	// A needle that is not there is not an error, just no offsets.
	none, err := findAllBytes(p, []byte("NOT-PRESENT-ANYWHERE"))
	if err != nil || len(none) != 0 {
		t.Errorf("absent needle: %v %v", none, err)
	}
}

// AlmaLinux 10.2's menu, as published. The RHEL rebuilds write linuxefi and
// initrdefi where Fedora writes linux and initrd, and a rewrite that assumes
// Fedora's spelling finds nothing here — the build stops with "could not find
// linux/initrd lines in the ISO's grub.cfg" before anything is written.
const almaGrubCfg = `set default="1"

load_video
set gfxpayload=keep
insmod gzio

set timeout=60

search --no-floppy --set=root -l 'AlmaLinux-10-2-x86_64-dvd'

menuentry 'Install AlmaLinux 10.2' --class fedora --class gnu-linux --class gnu --class os {
	linuxefi /images/pxeboot/vmlinuz inst.stage2=hd:LABEL=AlmaLinux-10-2-x86_64-dvd quiet
	initrdefi /images/pxeboot/initrd.img
}
menuentry 'Test this media & install AlmaLinux 10.2' --class fedora --class gnu-linux --class gnu --class os {
	linuxefi /images/pxeboot/vmlinuz inst.stage2=hd:LABEL=AlmaLinux-10-2-x86_64-dvd rd.live.check quiet
	initrdefi /images/pxeboot/initrd.img
}
`

func TestGrubKickstartMenuRHELFamily(t *testing.T) {
	out, err := grubKickstartMenu([]byte(almaGrubCfg))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != len(almaGrubCfg) {
		t.Fatalf("menu is %d bytes, the original is %d", len(out), len(almaGrubCfg))
	}
	got := string(out)
	// Written back with the commands this ISO uses, not Fedora's: a GRUB that
	// spells them linuxefi may not have plain linux at all.
	for _, want := range []string{
		"linuxefi\t/images/pxeboot/vmlinuz",
		"initrdefi\t/images/pxeboot/initrd.img ($ksdev)/ks.img",
		"inst.stage2=hd:LABEL=AlmaLinux-10-2-x86_64-dvd",
		"search --no-floppy --set=root -l 'AlmaLinux-10-2-x86_64-dvd'",
		"inst.ks=file:/ks.cfg",
		"set timeout=0",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("menu lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "rd.live.check") {
		t.Error("the media check survived; it fails on media with answers appended")
	}
	// Fedora's spelling must not be written onto a RHEL-family ISO.
	if strings.Contains(got, "\tlinux\t") || strings.Contains(got, "\tinitrd\t") {
		t.Errorf("Fedora's command names were written onto an AlmaLinux menu:\n%s", got)
	}
}

package compose

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	diskfs "github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/disk"
	"github.com/diskfs/go-diskfs/filesystem"
	"github.com/diskfs/go-diskfs/partition/gpt"

	"github.com/uplinkresearch/dsky/internal/library"
	"github.com/uplinkresearch/dsky/internal/recipe"
	"github.com/uplinkresearch/dsky/internal/stream"
)

const (
	// cidataSize is the appended NoCloud partition: FAT16 needs ≥ 4085
	// clusters, and 8 MiB leaves room for user-data that embeds scripts
	// or certificates.
	cidataSize = 8 << 20
	// gptLinuxFilesystem is the GPT type GUID for a Linux data partition.
	gptLinuxFilesystem = gpt.Type("0FC63DAF-8483-4772-8E79-3D69D8477DE4")
	grubCfgPath        = "/boot/grub/grub.cfg"
	// Anaconda ISOs keep the menu their UEFI GRUB reads here. DSKY is
	// UEFI-only, so this is the only one that decides anything.
	efiGrubCfgPath = "/EFI/BOOT/grub.cfg"
	// kickstartLabel is the volume label of the appended answers partition.
	//
	// Deliberately not OEMDRV, which is the obvious choice and the wrong one.
	// OEMDRV is how Anaconda recognises a *driver disk*, and its driver-updates
	// stage claims such a volume early in the initramfs and holds it open. The
	// kickstart fetch then cannot mount the same partition, and the install
	// stops with "Can't get kickstart from /dev/sda3:/ks.cfg" over a partition
	// that is present, correctly labelled, and holding a perfectly good
	// ks.cfg. Two stages of the same installer fighting over one volume.
	//
	// Since the kernel line names the kickstart anyway, the label only has to
	// be something to point at, and something nothing else wants.
	kickstartLabel = "DSKYKS"
	sectorSize     = 512
)

// buildLinuxAutoinstall turns a hybrid Ubuntu ISO into zero-touch install
// media: the ISO bytes copied into an image, GRUB rewritten in place (same
// byte length — no ISO rebuild) to boot with `autoinstall`, and a CIDATA
// partition appended with the rendered cloud-init user-data/meta-data.
func buildLinuxAutoinstall(ctx context.Context, req Request, entry library.Entry, blob, comp string) (*Artifact, error) {
	r := req.Recipe
	ws := req.Workspace
	lib := req.Library
	a := r.Linux.Autoinstall

	key, err := inputsKey(req, entry.SHA256)
	if err != nil {
		return nil, err
	}
	imgPath := filepath.Join(lib.ArtifactsDir(), fmt.Sprintf("%s-%s.img", r.ID, key))
	if !req.Rebuild {
		if art, err := LoadArtifact(MetaPath(imgPath)); err == nil {
			if _, err := os.Stat(art.Path); err == nil {
				req.progress("cached", 1, 1)
				return art, nil
			}
		}
	}

	// ── Render cloud-init data first: cheap, and it fails fast on template
	// mistakes before the multi-GB copy. ──────────────────────────────────
	vars, err := overlayVars(ws.MergedVars(r, req.CLIVars), a.Vars)
	if err != nil {
		return nil, fmt.Errorf("compose: autoinstall vars: %w", err)
	}
	tctx := recipe.Context{Org: ws.Org(), Vars: vars, Recipe: r}
	userData, err := recipe.RenderTemplate(filepath.Join(ws.Dir, filepath.FromSlash(a.UserData)), tctx)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(strings.TrimSpace(userData), "#cloud-config") {
		return nil, fmt.Errorf("compose: %s must start with #cloud-config (cloud-init ignores it otherwise)", a.UserData)
	}
	metaData := fmt.Sprintf("instance-id: dsky-%s-%s\n", r.ID, key)
	if a.MetaData != "" {
		if metaData, err = recipe.RenderTemplate(filepath.Join(ws.Dir, filepath.FromSlash(a.MetaData)), tctx); err != nil {
			return nil, err
		}
	}

	// ── Materialize the ISO bytes ────────────────────────────────────────
	if err := copyBlobToImage(ctx, req, blob, comp, imgPath, entry.Size); err != nil {
		return nil, err
	}
	cleanup := func() { os.Remove(imgPath) }

	// ── GRUB rewrite for zero-touch boot ─────────────────────────────────
	if a.PatchKernel() {
		req.progress("patch grub", 0, -1)
		if err := patchGrubForAutoinstall(imgPath); err != nil {
			cleanup()
			return nil, err
		}
	}

	// ── CIDATA partition ─────────────────────────────────────────────────
	req.progress("cidata", 0, -1)
	if err := appendCIDATA(imgPath, userData, metaData); err != nil {
		cleanup()
		return nil, err
	}

	req.progress("hash", 0, -1)
	sum, size, err := hashFileWithProgress(imgPath, func(done, total int64) { req.progress("hash", done, total) })
	if err != nil {
		cleanup()
		return nil, err
	}
	art := &Artifact{
		RecipeID: r.ID, Kind: "image", Path: imgPath, Size: size, SHA256: sum,
		InputsKey: key, Verify: r.Flash.Verify, MinStick: minStickBytes(r),
		CreatedAt: nowUTC(), Tool: toolVersion(),
	}
	return art, art.save()
}

// copyBlobToImage streams (decompressing as needed) the ISO into imgPath,
// padded to a whole sector.
func copyBlobToImage(ctx context.Context, req Request, blob, comp, imgPath string, hint int64) error {
	in, err := stream.Open(blob, comp)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(imgPath)
	if err != nil {
		return err
	}
	total := hint
	if comp != "" && comp != "none" {
		total = -1
	}
	buf := make([]byte, 4<<20)
	var done int64
	for {
		if err := ctx.Err(); err != nil {
			out.Close()
			os.Remove(imgPath)
			return err
		}
		n, rerr := in.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				out.Close()
				os.Remove(imgPath)
				return werr
			}
			done += int64(n)
			req.progress("copy iso", done, total)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			out.Close()
			os.Remove(imgPath)
			return rerr
		}
	}
	if pad := (sectorSize - done%sectorSize) % sectorSize; pad != 0 {
		if _, err := out.Write(make([]byte, pad)); err != nil {
			out.Close()
			return err
		}
	}
	return out.Close()
}

var (
	grubKernelRe = regexp.MustCompile(`(?m)^\s*linux\s+(\S+)`)
	grubInitrdRe = regexp.MustCompile(`(?m)^\s*initrd\s+(\S+)`)
	// Whole lines, arguments and all: an Anaconda installer will not start
	// without the inst.stage2= its own menu entry carries.
	//
	// The command is captured as well as its arguments because the family does
	// not agree on what it is called. Fedora writes linux/initrd; AlmaLinux
	// and the other RHEL rebuilds write linuxefi/initrdefi. A replacement menu
	// that assumes Fedora's spelling finds nothing on an AlmaLinux ISO, and
	// the build stops with "could not find linux/initrd lines". Whichever the
	// ISO used is what the replacement writes back.
	grubKernelLineRe = regexp.MustCompile(`(?m)^\s*(linuxefi|linux16|linux)\s+(.+)$`)
	grubInitrdLineRe = regexp.MustCompile(`(?m)^\s*(initrdefi|initrd16|initrd)\s+(.+)$`)
	// The line that points $root at the install medium, which every path in
	// the menu entry is relative to.
	grubSearchLineRe = regexp.MustCompile(`(?m)^\s*search\s+.*--set=root.*$`)
)

// grubKickstartMenu builds a replacement grub.cfg of exactly len(orig) bytes
// that boots the installer the distribution configured, with one thing taken
// out: the media check.
//
// Fedora's menu has "Install" and "Test this media & install", and ships with
// the second as the default. The media check hashes the whole device against
// the sum implanted in the ISO, and the answers partition appended after it is
// part of that device -- so on any media DSKY builds the check fails, says "It
// is not recommended to use this media", and halts before the installer runs
// at all. Watched happening on Fedora Server 44.
//
// So the appealing part of this approach -- an unmodified ISO, no boot option,
// answers found by volume label alone -- did not survive contact with the
// media. The installer has to be allowed to start, which means rewriting the
// menu; and once the menu is being rewritten, the kickstart is named on the
// kernel line rather than left to be discovered (see below).
func grubKickstartMenu(orig []byte, extraArgs ...string) ([]byte, error) {
	k := grubKernelLineRe.FindSubmatch(orig)
	i := grubInitrdLineRe.FindSubmatch(orig)
	if k == nil || i == nil {
		return nil, fmt.Errorf("compose: could not find linux/initrd lines in the ISO's grub.cfg")
	}
	// The first entry is the plain install on every Anaconda ISO seen so far,
	// but the check is stripped by name rather than by position, because that
	// holds whichever order a distribution lists them in.
	kernelCmd, initrdCmd := string(k[1]), string(i[1])
	kernel := strings.Join(strings.Fields(strings.ReplaceAll(string(k[2]), "rd.live.check", "")), " ")
	initrd := strings.TrimSpace(string(i[2]))

	// Name the kickstart rather than relying on Anaconda finding it.
	//
	// Anaconda is documented to look for a filesystem labelled OEMDRV and use
	// the ks.cfg on it with nothing on the command line, and that is why this
	// path appends an OEMDRV partition at all. On Fedora Server 44 it did not:
	// the partition is there, labelled, third in the GPT, and the installer
	// came up asking which language to install in. Since the menu has to be
	// rewritten anyway -- the shipped default media-checks itself to a halt --
	// there is nothing left to gain by being implicit about it. Saying where
	// the answers are also fails loudly if they are unreadable, instead of
	// quietly presenting a language list to nobody.
	if !strings.Contains(kernel, "inst.ks=") {
		kernel += " inst.ks=file:/ks.cfg"
	}
	for _, a := range extraArgs {
		if a = strings.TrimSpace(a); a != "" {
			kernel += " " + a
		}
	}

	// The ISO's own `search --set=root` line has to be kept, and this is the
	// part that is easy to throw away. Every path in the entry below is
	// relative to $root, and on an Anaconda ISO $root is not the boot device
	// by default — the config sets it by searching for the volume label. A
	// replacement without that line parses, shows its menu, fails to find the
	// kernel, and drops straight back to the menu, which looks exactly like a
	// timeout that did not fire. That cost two runs to tell apart.
	search := ""
	if m := grubSearchLineRe.FindSubmatch(orig); m != nil {
		search = strings.TrimSpace(string(m[0])) + "\n"
	}

	// timeout=0 with the menu hidden, rather than a countdown: unattended
	// media has nothing to ask, and a menu is something a passer-by can stop.
	// GRUB finds the answers partition by its label and hands its ks.img to
	// the kernel as a second initramfs. GRUB reading a FAT partition is a far
	// smaller ask than the installer mounting one mid-boot.
	menu := fmt.Sprintf("set default=0\nset timeout=0\nset timeout_style=hidden\n%ssearch --no-floppy --set=ksdev -l %s\nmenuentry \"Install\" {\n\t%s\t%s\n\t%s\t%s ($ksdev)/ks.img\n}\n",
		search, kickstartLabel, kernelCmd, kernel, initrdCmd, initrd)
	if len(menu) > len(orig) {
		return nil, fmt.Errorf("compose: replacement grub.cfg (%d bytes) exceeds the original (%d) — cannot patch in place", len(menu), len(orig))
	}
	out := make([]byte, len(orig))
	copy(out, menu)
	for j := len(menu); j < len(out); j++ {
		out[j] = '\n'
	}
	return out, nil
}

// grubAutoinstallMenu builds a replacement grub.cfg of exactly len(orig)
// bytes: one entry booting the ISO's own kernel/initrd with `autoinstall`.
func grubAutoinstallMenu(orig []byte) ([]byte, error) {
	k := grubKernelRe.FindSubmatch(orig)
	i := grubInitrdRe.FindSubmatch(orig)
	if k == nil || i == nil {
		return nil, fmt.Errorf("compose: could not find linux/initrd lines in the ISO's grub.cfg")
	}
	menu := fmt.Sprintf("set timeout=2\nmenuentry \"Automated install\" {\n\tset gfxpayload=keep\n\tlinux\t%s autoinstall ---\n\tinitrd\t%s\n}\n", k[1], i[1])
	if len(menu) > len(orig) {
		return nil, fmt.Errorf("compose: replacement grub.cfg (%d bytes) exceeds the original (%d) — cannot patch in place", len(menu), len(orig))
	}
	out := make([]byte, len(orig))
	copy(out, menu)
	for j := len(menu); j < len(out); j++ {
		out[j] = '\n'
	}
	return out, nil
}

// patchGrubForAutoinstall locates the ISO's /boot/grub/grub.cfg bytes inside
// the image and overwrites them with a same-length autoinstall menu.
func patchGrubForAutoinstall(imgPath string) error {
	return patchGrub(imgPath, grubCfgPath, grubAutoinstallMenu)
}

// patchGrubForKickstart does the same for an Anaconda ISO's UEFI menu, so the
// installer starts rather than media-checking itself to a halt.
func patchGrubForKickstart(imgPath string, extraArgs []string) error {
	return patchGrub(imgPath, efiGrubCfgPath, func(orig []byte) ([]byte, error) {
		return grubKickstartMenu(orig, extraArgs...)
	})
}

func patchGrub(imgPath, cfgPath string, build func([]byte) ([]byte, error)) error {
	orig, err := readISOFile(imgPath, cfgPath)
	if err != nil {
		return fmt.Errorf("compose: reading %s from the ISO: %w", cfgPath, err)
	}
	if len(orig) < 32 {
		return fmt.Errorf("compose: %s is implausibly small (%d bytes)", cfgPath, len(orig))
	}
	repl, err := build(orig)
	if err != nil {
		return err
	}
	// Every copy, not the first. An Anaconda ISO carries its UEFI menu twice:
	// once in the ISO9660 tree and once inside the El Torito EFI boot image
	// embedded in the same file. Patching one and reading back the other is
	// how this was found -- and patching only the one the readback happens to
	// check would leave the firmware booting the old menu.
	offs, err := findAllBytes(imgPath, orig)
	if err != nil {
		return err
	}
	if len(offs) == 0 {
		return fmt.Errorf("compose: %s content not found in the image", cfgPath)
	}
	f, err := os.OpenFile(imgPath, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	for _, off := range offs {
		if _, err := f.WriteAt(repl, off); err != nil {
			return err
		}
	}
	if err := f.Sync(); err != nil {
		return err
	}
	// Prove the rewrite is visible through the filesystem, not just the raw bytes.
	check, err := readISOFile(imgPath, cfgPath)
	if err != nil {
		return err
	}
	if !bytes.Equal(check, repl) {
		return fmt.Errorf("compose: grub.cfg rewrite did not read back through ISO9660")
	}
	return nil
}

// readISOFile reads one file from the ISO9660 filesystem at image offset 0
// (the hybrid layout keeps the ISO at the start regardless of GPT).
func readISOFile(imgPath, p string) ([]byte, error) {
	// ISO9660 is addressed in 2048-byte blocks; the GPT (512-byte sectors)
	// is irrelevant for this read and its parse failure is ignored.
	d, err := diskfs.Open(imgPath, diskfs.WithOpenMode(diskfs.ReadOnly), diskfs.WithSectorSize(2048))
	if err != nil {
		return nil, err
	}
	defer d.Close()
	fsys, err := d.GetFilesystem(0)
	if err != nil {
		return nil, err
	}
	var f filesystem.File
	for _, candidate := range []string{p, strings.TrimPrefix(p, "/")} {
		if f, err = fsys.OpenFile(candidate, os.O_RDONLY); err == nil {
			break
		}
	}
	if err != nil {
		return nil, err
	}
	b, err := io.ReadAll(f)
	if c, ok := f.(io.Closer); ok {
		c.Close()
	}
	return b, err
}

// findBytes returns the offset of needle in the file, or -1.
func findBytes(path string, needle []byte) (int64, error) {
	offs, err := findAllBytes(path, needle)
	if err != nil || len(offs) == 0 {
		return -1, err
	}
	return offs[0], nil
}

// findAllBytes returns every offset at which needle appears in the file, in
// order.
func findAllBytes(path string, needle []byte) ([]int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	const chunk = 8 << 20
	buf := make([]byte, chunk+len(needle))
	var out []int64
	var base int64
	carry := 0
	for {
		n, err := f.Read(buf[carry:])
		if n == 0 && err == io.EOF {
			return out, nil
		}
		if err != nil && err != io.EOF {
			return nil, err
		}
		view := buf[:carry+n]
		// Every match in this window, not just the first.
		for at := 0; ; {
			i := bytes.Index(view[at:], needle)
			if i < 0 {
				break
			}
			out = append(out, base+int64(at+i))
			at += i + 1
		}
		// Keep a needle-length tail so a match spanning two windows is found
		// -- and not counted twice, since the tail is what the next window
		// starts from.
		keep := len(needle) - 1
		if keep > len(view) {
			keep = len(view)
		}
		copy(buf, view[len(view)-keep:])
		base += int64(len(view) - keep)
		carry = keep
		if err == io.EOF {
			return out, nil
		}
	}
}

// appendCIDATA grows the image by a FAT16 partition labeled CIDATA holding
// user-data and meta-data — cloud-init's NoCloud datasource.
func appendCIDATA(imgPath, userData, metaData string) error {
	return appendAnswers(imgPath, "CIDATA", map[string]string{
		"user-data": userData, "meta-data": metaData,
	})
}

// appendAnswers grows the image by a small FAT16 partition with the given
// label, holding the files an installer looks for by that label: CIDATA for
// cloud-init's NoCloud datasource, OEMDRV for Anaconda's kickstart. Both
// installers find their answers by volume label alone, from the same stick
// they booted, which is what makes appending one to an unmodified ISO work at
// all. The isohybrid MBR (sector 0) is never touched; the GPT is resized so
// its backup header lands at the new end of the image.
func appendAnswers(imgPath, label string, files map[string]string) error {
	st, err := os.Stat(imgPath)
	if err != nil {
		return err
	}
	const align = 1 << 20
	start := (st.Size() + align - 1) / align * align
	newSize := (start + cidataSize + 34*sectorSize + align - 1) / align * align
	if err := os.Truncate(imgPath, newSize); err != nil {
		return err
	}

	d, err := diskfs.Open(imgPath, diskfs.WithOpenMode(diskfs.ReadWrite))
	if err != nil {
		return err
	}
	defer d.Close()
	tbl, err := d.GetPartitionTable()
	if err != nil {
		return fmt.Errorf("compose: the ISO has no partition table — autoinstall needs a hybrid (GPT) ISO: %w", err)
	}
	gt, ok := tbl.(*gpt.Table)
	if !ok {
		return fmt.Errorf("compose: the ISO's partition table is not GPT — autoinstall needs a hybrid (GPT) ISO")
	}
	gt.Resize(uint64(newSize))
	gt.ProtectiveMBR = false
	startLBA := uint64(start / sectorSize)
	endLBA := startLBA + cidataSize/sectorSize - 1
	idx := len(gt.Partitions) + 1
	gt.Partitions = append(gt.Partitions, &gpt.Partition{
		Index: idx,
		Start: startLBA,
		End:   endLBA,
		Size:  cidataSize,
		Type:  gptLinuxFilesystem,
		Name:  label,
	})
	if err := d.Partition(gt); err != nil {
		return fmt.Errorf("compose: appending %s partition: %w", label, err)
	}
	fsys, err := d.CreateFilesystem(disk.FilesystemSpec{
		Partition:   idx,
		FSType:      filesystem.TypeFat16,
		VolumeLabel: label,
	})
	if err != nil {
		return fmt.Errorf("compose: formatting %s: %w", label, err)
	}
	for name, content := range files {
		f, err := fsys.OpenFile(name, os.O_CREATE|os.O_RDWR|os.O_TRUNC)
		if err != nil {
			return fmt.Errorf("compose: writing %s: %w", name, err)
		}
		if _, err := f.Write([]byte(content)); err != nil {
			return err
		}
		if c, ok := f.(io.Closer); ok {
			c.Close()
		}
	}
	return nil
}

// buildLinuxKickstart turns an Anaconda installer ISO -- Fedora Server, RHEL,
// AlmaLinux, Rocky -- into unattended install media: the ISO bytes copied into
// an image unmodified, and a small partition labelled OEMDRV appended holding
// ks.cfg.
//
// Unlike the Ubuntu path there is no GRUB rewrite, because there is nothing to
// pass on the kernel command line: Anaconda looks for an OEMDRV filesystem by
// itself, on every install, and uses the ks.cfg it finds there. That is also
// what makes this safe to do to an unmodified ISO -- the media stays exactly
// what the distribution published, signed shim and all, and the answers ride
// beside it.
func buildLinuxKickstart(ctx context.Context, req Request, entry library.Entry, blob, comp string) (*Artifact, error) {
	r := req.Recipe
	ws := req.Workspace
	lib := req.Library
	k := r.Linux.Kickstart

	key, err := inputsKey(req, entry.SHA256)
	if err != nil {
		return nil, err
	}
	imgPath := filepath.Join(lib.ArtifactsDir(), fmt.Sprintf("%s-%s.img", r.ID, key))
	if !req.Rebuild {
		if art, err := LoadArtifact(MetaPath(imgPath)); err == nil {
			if _, err := os.Stat(art.Path); err == nil {
				req.progress("cached", 1, 1)
				return art, nil
			}
		}
	}

	// Rendered first: cheap, and it fails on a template mistake before the
	// multi-gigabyte copy rather than after it.
	vars, err := overlayVars(ws.MergedVars(r, req.CLIVars), k.Vars)
	if err != nil {
		return nil, fmt.Errorf("compose: kickstart vars: %w", err)
	}
	tctx := recipe.Context{Org: ws.Org(), Vars: vars, Recipe: r}
	ks, err := recipe.RenderTemplate(filepath.Join(ws.Dir, filepath.FromSlash(k.File)), tctx)
	if err != nil {
		return nil, err
	}
	// Anaconda reads a kickstart that says nothing as "ask me everything",
	// which on unattended media is a machine sitting at a prompt forever.
	if strings.TrimSpace(ks) == "" {
		return nil, fmt.Errorf("compose: %s is empty", k.File)
	}

	if err := copyBlobToImage(ctx, req, blob, comp, imgPath, entry.Size); err != nil {
		return nil, err
	}
	cleanup := func() { os.Remove(imgPath) }

	req.progress("patch grub", 0, -1)
	if err := patchGrubForKickstart(imgPath, k.KernelArgs); err != nil {
		cleanup()
		return nil, err
	}

	// ks.img is what the installer actually reads (see cpio.go); ks.cfg is
	// written beside it so the answers on a stick can be read, and edited, by
	// anyone who plugs it in.
	ksimg, err := cpioNewcFile("ks.cfg", []byte(ks))
	if err != nil {
		return nil, err
	}
	req.progress("answers", 0, -1)
	if err := appendAnswers(imgPath, kickstartLabel, map[string]string{
		"ks.cfg": ks,
		"ks.img": string(ksimg),
	}); err != nil {
		cleanup()
		return nil, err
	}

	req.progress("hash", 0, -1)
	sum, size, err := hashFileWithProgress(imgPath, func(done, total int64) { req.progress("hash", done, total) })
	if err != nil {
		cleanup()
		return nil, err
	}
	art := &Artifact{
		RecipeID: r.ID, Kind: "image", Path: imgPath, Size: size, SHA256: sum,
		InputsKey: key, Verify: r.Flash.Verify, MinStick: minStickBytes(r),
		CreatedAt: nowUTC(), Tool: toolVersion(),
	}
	return art, art.save()
}

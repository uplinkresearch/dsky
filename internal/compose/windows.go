package compose

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/uplinkresearch/dsky/internal/fsimg"
	"github.com/uplinkresearch/dsky/internal/helpers"
	"github.com/uplinkresearch/dsky/internal/manifest"
	"github.com/uplinkresearch/dsky/internal/recipe"
	"github.com/uplinkresearch/dsky/internal/workspace"
)

// scriptsImg is where $OEM$ payload lands on the stick; Windows Setup copies
// it to C:\Windows\Setup\Scripts during install.
const scriptsImg = "/sources/$OEM$/$$/Setup/Scripts"

// buildWindows composes Windows install media: base (extracted ISO or
// captured master tree) + overlays (unattend, ei.cfg, drivers, payload,
// generated firstboot) → FAT32 image.
func buildWindows(ctx context.Context, req Request) (*Artifact, error) {
	r := req.Recipe
	w := r.Windows
	ws := req.Workspace
	lib := req.Library

	// ── Resolve the base media ───────────────────────────────────────────
	mode := r.OS.SourceMode
	treePath := r.OS.TreePath
	if treePath != "" && !filepath.IsAbs(treePath) {
		treePath = filepath.Join(ws.Dir, filepath.FromSlash(treePath))
	}
	if mode == recipe.SourceAuto {
		// Prefer the captured master when it is present on THIS machine;
		// fall back to the ISO source otherwise (recipes are shared across
		// machines and the master usually lives on just one).
		treeExists := false
		if treePath != "" {
			if st, err := os.Stat(treePath); err == nil && st.IsDir() {
				treeExists = true
			}
		}
		switch {
		case treeExists:
			mode = recipe.SourceTree
		case r.OS.Source != "":
			mode = recipe.SourceISO
		default:
			return nil, fmt.Errorf("compose: os.tree_path %s does not exist on this machine and no os.source is set", treePath)
		}
	}

	stage := fsimg.StageMap{}
	var sourceFingerprint string
	switch mode {
	case recipe.SourceTree:
		if st, err := os.Stat(treePath); err != nil || !st.IsDir() {
			return nil, fmt.Errorf("compose: os.tree_path %s is not a directory", treePath)
		}
		req.progress("stage base tree", 0, -1)
		if err := stage.AddTree(treePath, "/"); err != nil {
			return nil, err
		}
		// Trees are mutable directories; fingerprint cheaply by path+mtime.
		sourceFingerprint = "tree:" + treePath
	case recipe.SourceISO:
		src, err := ws.Source(r.OS.Source)
		if err != nil {
			return nil, err
		}
		entry, err := lib.Resolve(src.ID)
		if err != nil {
			return nil, err
		}
		extractDir, err := extractISOCached(ctx, req, lib.BlobPath(entry.SHA256), entry.SHA256)
		if err != nil {
			return nil, err
		}
		if err := stage.AddTree(extractDir, "/"); err != nil {
			return nil, err
		}
		sourceFingerprint = "iso:" + entry.SHA256
	default:
		return nil, fmt.Errorf("compose: unsupported source_mode %q", mode)
	}

	// ── Split oversized WIMs (FAT32 4 GiB limit) ─────────────────────────
	if err := splitOversizeWIM(ctx, req, stage); err != nil {
		return nil, err
	}

	// ── Cache check ──────────────────────────────────────────────────────
	key, err := inputsKey(req, sourceFingerprint)
	if err != nil {
		return nil, err
	}
	imgPath := filepath.Join(lib.ArtifactsDir(), fmt.Sprintf("%s-%s.img", r.ID, key))
	if !req.Rebuild && mode != recipe.SourceTree { // tree contents aren't fingerprinted; always rebuild
		if a, err := LoadArtifact(MetaPath(imgPath)); err == nil {
			if _, err := os.Stat(a.Path); err == nil {
				req.progress("cached", 1, 1)
				return a, nil
			}
		}
	}

	// ── Overlays ─────────────────────────────────────────────────────────
	buildTmp, err := os.MkdirTemp(lib.TmpDir(), "build-"+r.ID+"-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(buildTmp)

	vars := ws.MergedVars(r, req.CLIVars)
	refFiles := map[string]string{} // source ref -> staged filename under Scripts/

	// Unattend.
	if w.Unattend != nil {
		uvars, err := overlayVars(vars, w.Unattend.Vars)
		if err != nil {
			return nil, fmt.Errorf("compose: unattend vars: %w", err)
		}
		if err := domainVars(uvars, ws.Dir, w.Domain, vars, func(ref string) (string, error) {
			f, err := materializeFile(ctx, req, ref)
			return f.host, err
		}); err != nil {
			return nil, fmt.Errorf("compose: %w", err)
		}
		rendered, err := recipe.RenderTemplate(
			filepath.Join(ws.Dir, filepath.FromSlash(w.Unattend.Template)),
			recipe.Context{Org: ws.Org(), Vars: uvars, Recipe: r})
		if err != nil {
			return nil, err
		}
		if err := checkDomainRendered(rendered, w.Domain, w.Unattend.Template); err != nil {
			return nil, err
		}
		if rendered, err = withOEMCopy(rendered); err != nil {
			return nil, err
		}
		p := filepath.Join(buildTmp, "autounattend.xml")
		if err := os.WriteFile(p, []byte(rendered), 0o644); err != nil {
			return nil, err
		}
		stage.AddFile(p, "/autounattend.xml")
	}

	// Domain join by serial number: every computer's file, and the script
	// that picks this computer's during Setup.
	if w.Domain.BySerial() {
		blobs, err := SerialBlobs(serialBlobDir(ws.Dir, w.Domain))
		if err != nil {
			return nil, fmt.Errorf("compose: windows.domain.blobs_by_serial: %w", err)
		}
		dir := filepath.Join(buildTmp, recipe.DomainSerialDir)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		for _, b := range blobs {
			p := filepath.Join(dir, b.Serial+".txt")
			if err := os.WriteFile(p, odjFileBytes(b.Base64), 0o600); err != nil {
				return nil, err
			}
			stage.AddFile(p, path.Join(scriptsImg, recipe.DomainSerialDir, b.Serial+".txt"))
		}
		sp := filepath.Join(buildTmp, recipe.DomainSerialScriptName)
		if err := writePS(sp, recipe.DomainSerialScriptFile()); err != nil {
			return nil, err
		}
		stage.AddFile(sp, path.Join(scriptsImg, recipe.DomainSerialScriptName))
		req.progress(fmt.Sprintf("domain join files for %d computers", len(blobs)), 0, -1)
	}

	// ei.cfg.
	if w.EICfg != nil {
		p := filepath.Join(buildTmp, "ei.cfg")
		if err := os.WriteFile(p, []byte(recipe.GenerateEICfg(w.EICfg)), 0o644); err != nil {
			return nil, err
		}
		stage.AddFile(p, "/sources/ei.cfg")
	}

	// Driver packs and payload files, as the standalone payload stages them.
	drivers, stagedRefs, err := stageDriversAndPayload(ctx, req, stage, buildTmp)
	if err != nil {
		return nil, err
	}
	for k, v := range stagedRefs {
		refFiles[k] = v
	}

	// Boot-critical WinPE drivers: Setup loads $WinpeDriver$ from removable
	// media root during windowsPE (VMD/RST storage, NICs).
	for _, ref := range w.WinPEDrivers {
		dir, err := materializeDir(ctx, req, buildTmp, recipe.DriverPack{Ref: ref, Install: recipe.InstallSweep})
		if err != nil {
			return nil, err
		}
		if err := stage.AddTree(dir, path.Join("/$WinpeDriver$", ref)); err != nil {
			return nil, err
		}
	}

	// First-boot script.
	resolveRef := refResolver(ctx, req, stage, refFiles)
	// First boot: the agent where it covers the recipe, the generated scripts
	// otherwise. The agent is a compiled program with one manifest, which is
	// why it exists: the scripts were assembled per build and so could only
	// be tested after they had already been written onto somebody's stick.
	useAgent, why := agentCovers(r)
	var firstboot string
	if useAgent {
		firstboot = agentFirstboot
	} else {
		switch w.Firstboot.Mode {
		case "generate":
			firstboot, err = recipe.GenerateFirstboot(r, drivers, resolveRef)
		case "template":
			firstboot, err = recipe.RenderTemplate(
				filepath.Join(ws.Dir, filepath.FromSlash(w.Firstboot.Template)),
				recipe.Context{Org: ws.Org(), Vars: vars, Recipe: r})
		}
		if err != nil {
			return nil, err
		}
		req.progress("first boot uses the generated scripts: "+why, 0, -1)
	}
	fbPath := filepath.Join(buildTmp, "firstboot.cmd")
	if err := os.WriteFile(fbPath, []byte(firstboot), 0o644); err != nil {
		return nil, err
	}
	stage.AddFile(fbPath, path.Join(scriptsImg, "firstboot.cmd"))

	if !useAgent {
		// Debloat pass (invoked from firstboot; also available to
		// template-mode scripts that call it themselves).
		if w.Debloat.Enabled() {
			dbPath := filepath.Join(buildTmp, "debloat.ps1")
			if err := writePS(dbPath, recipe.GenerateDebloatPS(r)); err != nil {
				return nil, err
			}
			stage.AddFile(dbPath, path.Join(scriptsImg, "debloat.ps1"))
		}

		// Program installs (winget at first boot; nothing large staged here).
		if w.Apps.Enabled() {
			apPath := filepath.Join(buildTmp, "apps.ps1")
			if err := writePS(apPath, recipe.GenerateAppsPS(r)); err != nil {
				return nil, err
			}
			stage.AddFile(apPath, path.Join(scriptsImg, "apps.ps1"))
		}
	}

	// The operator's success artwork, staged under the name the generated
	// script looks for.
	if s := w.StatusScreen; s.Enabled() {
		src, ref := s.SuccessImage()
		switch {
		case ref != "":
			file, err := materializeFile(ctx, req, ref)
			if err != nil {
				return nil, fmt.Errorf("compose: windows.status_screen.success_ref: %w", err)
			}
			stage.AddFile(file.host, path.Join(scriptsImg, file.name))
		case src != "":
			host := filepath.Join(ws.Dir, filepath.FromSlash(src))
			if _, err := os.Stat(host); err != nil {
				return nil, fmt.Errorf("compose: windows.status_screen.success %s: %w", src, err)
			}
			stage.AddFile(host, path.Join(scriptsImg, filepath.Base(host)))
		}
		// A way back to the Windows default, since the screens are the
		// imaging bench's signal and not the customer's wallpaper.
		clPath := filepath.Join(buildTmp, "clear-status-screen.cmd")
		if err := os.WriteFile(clPath, []byte(recipe.GenerateClearStatusScreen()), 0o644); err != nil {
			return nil, err
		}
		stage.AddFile(clPath, path.Join(scriptsImg, "clear-status-screen.cmd"))
	}

	// The post-install check. Generated because only this build knows what it
	// promised, and staged beside the scripts that deliver it so the imaged
	// machine can be checked without installing anything on it.
	vfPath := filepath.Join(buildTmp, "verify.ps1")
	if err := writePS(vfPath, recipe.GenerateVerifyPS(r, drivers, resolveRef)); err != nil {
		return nil, err
	}
	stage.AddFile(vfPath, path.Join(scriptsImg, "verify.ps1"))

	// The agent's instructions, and the agent itself. Staged last so it
	// describes everything above it.
	if useAgent {
		verifyScript := ""
		if w.StatusScreen.Enabled() {
			// The check paints the lock screen, so it only runs by itself
			// where the recipe asked for that; otherwise it stays on the
			// machine to be run by hand.
			verifyScript = "verify.ps1"
		}
		if err := stageAgent(stage, buildTmp, buildManifest(r, drivers, agentInstallers(w, refFiles), verifyScript)); err != nil {
			return nil, err
		}
	}

	// ── Size and build ───────────────────────────────────────────────────
	contentBytes, entries, err := stage.Stats()
	if err != nil {
		return nil, err
	}
	var sizeBytes int64
	if r.Target.Size == "auto" {
		sizeBytes = fsimg.SizeForContent(contentBytes, entries)
	} else {
		sizeBytes, _ = recipe.ParseSize(r.Target.Size)
		if need := fsimg.SizeForContent(contentBytes, entries); sizeBytes < need {
			return nil, fmt.Errorf("compose: target.size %s is too small for %d MiB of content (need about %d MiB)",
				r.Target.Size, contentBytes>>20, need>>20)
		}
	}

	if os.Getenv("SOURCE_DATE_EPOCH") == "" {
		os.Setenv("SOURCE_DATE_EPOCH", defaultSourceDateEpoch)
	}
	req.progress("image", 0, contentBytes)
	err = fsimg.BuildStaged(imgPath, fsimg.Options{
		Scheme:       fsimg.Scheme(r.Target.Scheme),
		Label:        r.Target.VolumeLabel,
		SizeBytes:    sizeBytes,
		Reproducible: true,
	}, stage, func(done, total int64) {
		req.progress("image", done, total)
	})
	if err != nil {
		return nil, err
	}

	req.progress("hash", 0, sizeBytes)
	sum, size, err := hashFileWithProgress(imgPath, func(done, total int64) {
		req.progress("hash", done, total)
	})
	if err != nil {
		return nil, err
	}

	a := &Artifact{
		RecipeID: r.ID, Kind: "image", Path: imgPath, Size: size, SHA256: sum,
		InputsKey: key, Verify: r.Flash.Verify, MinStick: minStickBytes(r),
		CreatedAt: nowUTC(), Tool: toolVersion(),
	}
	return a, a.save()
}

// hardwarePacks turns windows.hardware entries into driver packs by finding
// workspace manifests whose `hardware:` block matches (written by
// `dsky drivers resolve`). Each entry must have at least one pack.
func hardwarePacks(ws *workspace.Workspace, hw []recipe.HardwareSpec) ([]recipe.DriverPack, error) {
	if len(hw) == 0 {
		return nil, nil
	}
	sources, err := ws.Sources()
	if err != nil {
		return nil, err
	}
	var out []recipe.DriverPack
	seen := map[string]bool{}
	for _, h := range hw {
		targets := []struct{ vendor, model, hwid string }{}
		if h.Vendor != "" {
			targets = append(targets, struct{ vendor, model, hwid string }{h.Vendor, h.Model, ""})
		}
		for _, id := range h.HWIDs {
			targets = append(targets, struct{ vendor, model, hwid string }{"", "", id})
		}
		for _, t := range targets {
			found := 0
			for _, src := range sources {
				if !src.Hardware.Matches(t.vendor, t.model, t.hwid) || seen[src.ID] {
					continue
				}
				seen[src.ID] = true
				found++
				install := recipe.InstallMethod(src.Install)
				if install == "" {
					switch src.Format {
					case manifest.FormatCab:
						install = recipe.InstallExpandSweep
					case manifest.FormatExe:
						install = recipe.InstallExtractSweep
					default:
						install = recipe.InstallSweep
					}
				}
				pack := recipe.DriverPack{Ref: src.ID, Install: install, Extract: src.Extract, Args: src.Args}
				// An installer found for a model runs only on that model. A sweep
				// needs no such care — pnputil installs only what matches — but an
				// installer does whatever it does, and Framework's stops at a
				// prompt on the wrong mainboard.
				if install == recipe.InstallExe && src.Hardware != nil && src.Hardware.Model != "" {
					pack.OnlyVendor, pack.OnlyModel = src.Hardware.Vendor, src.Hardware.Model
				}
				out = append(out, pack)
			}
			if found == 0 {
				what := t.hwid
				if what == "" {
					what = t.vendor + " " + t.model
				}
				return nil, fmt.Errorf("compose: no driver-pack manifest for hardware %q — run `dsky drivers resolve %s`", what, ws.Dir)
			}
		}
	}
	return out, nil
}

// overlayVars expands ${var:...} in overlay values against base, then merges
// overlay over base.
func overlayVars(base, overlay map[string]string) (map[string]string, error) {
	out := map[string]string{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range overlay {
		expanded, err := recipe.ExpandVars(v, base)
		if err != nil {
			return nil, fmt.Errorf("var %s: %w", k, err)
		}
		out[k] = expanded
	}
	return out, nil
}

type materialized struct {
	host string // host path of the file
	name string // filename to stage as
}

// ensureRef makes sure a source's bytes are in the library, fetching them if
// they are not.
//
// A recipe records which driver packs it needs; the bytes are fetched when a
// stick is built. Saving a recipe used to download them instead, so naming a
// machine meant waiting on a gigabyte before the recipe file existed. A build
// is the moment the bytes are actually needed, and the moment somebody is
// already waiting on a progress bar.
func ensureRef(ctx context.Context, req Request, ref string) error {
	if _, err := req.Library.Resolve(ref); err == nil {
		return nil
	}
	src, err := req.Workspace.Source(ref)
	if err != nil {
		return fmt.Errorf("compose: %s is not in the library and no manifest says where to get it: %w", ref, err)
	}
	req.progress("downloading "+ref, 0, -1)
	if _, err := req.Library.Pull(ctx, src, true, nil, func(done, total int64) {
		req.progress("downloading "+ref, done, total)
	}); err != nil {
		return fmt.Errorf("compose: could not fetch %s: %w", ref, err)
	}
	return nil
}

// materializeFile resolves a source ref to its library blob.
func materializeFile(ctx context.Context, req Request, ref string) (materialized, error) {
	if err := ensureRef(ctx, req, ref); err != nil {
		return materialized{}, err
	}
	entry, err := req.Library.Resolve(ref)
	if err != nil {
		return materialized{}, err
	}
	return materialized{host: req.Library.BlobPath(entry.SHA256), name: entry.Filename}, nil
}

// materializeDir turns an INF driver pack (workspace dir, or zip blob) into
// a directory of files.
func materializeDir(ctx context.Context, req Request, buildTmp string, pack recipe.DriverPack) (string, error) {
	if pack.Path != "" {
		dir := filepath.Join(req.Workspace.Dir, filepath.FromSlash(pack.Path))
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			return "", fmt.Errorf("compose: driver pack path %s is not a directory", pack.Path)
		}
		return dir, nil
	}
	if err := ensureRef(ctx, req, pack.Ref); err != nil {
		return "", err
	}
	entry, err := req.Library.Resolve(pack.Ref)
	if err != nil {
		return "", err
	}
	blob := req.Library.BlobPath(entry.SHA256)
	if !strings.EqualFold(filepath.Ext(entry.Filename), ".zip") {
		return "", fmt.Errorf("compose: driver pack %s (install: %s) must be a .zip of INFs or a workspace path; got %s",
			pack.Ref, pack.Install, entry.Filename)
	}
	dest := filepath.Join(buildTmp, "packs", pack.Ref)
	if err := helpers.ExpandZip(blob, dest); err != nil {
		return "", err
	}
	return dest, nil
}

// extractISOCached extracts a Windows ISO once per content hash, reusing the
// extraction across builds (a .done marker gates reuse).
func extractISOCached(ctx context.Context, req Request, isoPath, sha string) (string, error) {
	dir := filepath.Join(req.Library.TmpDir(), "extract", sha[:16])
	marker := filepath.Join(dir, ".dsky-extracted")
	if _, err := os.Stat(marker); err == nil {
		return dir, nil
	}
	if err := os.RemoveAll(dir); err != nil {
		return "", err
	}
	// Both tools are needed from here (7-Zip to read the ISO, wimlib to split
	// install.wim for FAT32), so say what to install before starting either.
	if err := helpers.WindowsMediaToolsError(req.Library.HelpersDir()); err != nil {
		return "", err
	}
	req.progress("extract iso", 0, -1)
	if err := helpers.ExtractISO(ctx, req.Library.HelpersDir(), isoPath, dir); err != nil {
		return "", err
	}
	if err := os.WriteFile(marker, []byte(sha), 0o644); err != nil {
		return "", err
	}
	return dir, nil
}

// splitOversizeWIM replaces sources/install.wim in the stage with split .swm
// parts when it exceeds the FAT32 file limit. install.esd cannot be split
// (solid archive) — that's a hard error pointing at the fix.
func splitOversizeWIM(ctx context.Context, req Request, stage fsimg.StageMap) error {
	const wimImg = "/sources/install.wim"
	const esdImg = "/sources/install.esd"
	if host, ok := stage[esdImg]; ok {
		st, err := os.Stat(host)
		if err != nil {
			return err
		}
		if st.Size() > fsimg.MaxFileSize {
			return fmt.Errorf("compose: %s is %d MiB — install.esd cannot be split for FAT32; use media with install.wim (ISO download) instead", esdImg, st.Size()>>20)
		}
	}
	host, ok := stage[wimImg]
	if !ok {
		return nil
	}
	st, err := os.Stat(host)
	if err != nil {
		return err
	}
	if st.Size() <= fsimg.MaxFileSize {
		return nil
	}
	// Split lives next to the extraction so it caches with it.
	outDir := filepath.Join(filepath.Dir(host), "..", "dsky-swm")
	outDir, err = filepath.Abs(outDir)
	if err != nil {
		return err
	}
	swm := filepath.Join(outDir, "install.swm")
	if _, err := os.Stat(swm); err != nil {
		req.progress("split wim", 0, -1)
		if err := os.MkdirAll(outDir, 0o755); err != nil {
			return err
		}
		if err := helpers.SplitWIM(ctx, req.Library.HelpersDir(), host, swm, 3800); err != nil {
			return err
		}
	}
	delete(stage, wimImg)
	// The split is cached inside the extraction, so staging the extraction
	// on a later build picks the parts up a second time, at /dsky-swm/. That
	// put 7 GiB of duplicate parts on every rebuilt Windows image (17 GiB
	// instead of 9, too big for an 8 GB stick); the first build of an ISO
	// split after staging and never showed it. They belong only in /sources.
	for imgPath, hostPath := range stage {
		if rel, err := filepath.Rel(outDir, hostPath); err == nil && !strings.HasPrefix(rel, "..") {
			delete(stage, imgPath)
		}
	}
	matches, err := filepath.Glob(filepath.Join(outDir, "install*.swm"))
	if err != nil || len(matches) == 0 {
		return fmt.Errorf("compose: WIM split produced no .swm files in %s", outDir)
	}
	for _, m := range matches {
		stage.AddFile(m, "/sources/"+filepath.Base(m))
	}
	return nil
}

// stageDriversAndPayload stages what the agent works with on the machine: the
// driver packs, and the recipe's payload files. Shared by the install image
// and the standalone payload, so the two cannot drift apart in how a pack is
// staged or what it is called.
func stageDriversAndPayload(ctx context.Context, req Request, stage fsimg.StageMap, buildTmp string) (drivers recipe.ResolvedDrivers, refFiles map[string]string, err error) {
	r := req.Recipe
	w := r.Windows
	ws := req.Workspace
	refFiles = map[string]string{} // source ref -> staged filename under Scripts/

	// Driver packs: explicit ones, then packs resolved for windows.hardware
	// entries (manifests carrying a matching `hardware:` block).
	packs := append([]recipe.DriverPack(nil), w.DriverPacks...)
	hwPacks, err := hardwarePacks(ws, w.Hardware)
	if err != nil {
		return drivers, nil, err
	}
	packs = append(packs, hwPacks...)
	gateStaged := false
	for _, pack := range packs {
		switch pack.Install {
		case recipe.InstallSweep:
			dir, err := materializeDir(ctx, req, buildTmp, pack)
			if err != nil {
				return drivers, nil, err
			}
			if err := stage.AddTree(dir, path.Join(scriptsImg, "Drivers", pack.Name())); err != nil {
				return drivers, nil, err
			}
			drivers.HasSweepable = true
		case recipe.InstallExpandSweep:
			file, err := materializeFile(ctx, req, pack.Ref)
			if err != nil {
				return drivers, nil, err
			}
			stage.AddFile(file.host, path.Join(scriptsImg, file.name))
			refFiles[pack.Ref] = file.name
			drivers.Cabs = append(drivers.Cabs, struct {
				File string
				Dir  string
			}{File: file.name, Dir: pack.Name()})
			drivers.HasSweepable = true
		case recipe.InstallExtractSweep:
			file, err := materializeFile(ctx, req, pack.Ref)
			if err != nil {
				return drivers, nil, err
			}
			if len(pack.Extract) == 0 {
				return drivers, nil, fmt.Errorf("compose: driver pack %s uses extract-then-sweep but has no extract args (e.g. Dell: /s /e={dir})", pack.Ref)
			}
			stage.AddFile(file.host, path.Join(scriptsImg, file.name))
			refFiles[pack.Ref] = file.name
			drivers.Extracts = append(drivers.Extracts, struct {
				File string
				Dir  string
				Args []string
			}{File: file.name, Dir: pack.Name(), Args: pack.Extract})
			drivers.HasSweepable = true
		case recipe.InstallExe:
			file, err := materializeFile(ctx, req, pack.Ref)
			if err != nil {
				return drivers, nil, err
			}
			stage.AddFile(file.host, path.Join(scriptsImg, file.name))
			refFiles[pack.Ref] = file.name
			drivers.Exes = append(drivers.Exes, struct {
				File       string
				Args       []string
				Log        string
				OnlyVendor string
				OnlyModel  string
			}{File: file.name, Args: pack.Args, Log: pack.Log, OnlyVendor: pack.OnlyVendor, OnlyModel: pack.OnlyModel})
			if pack.OnlyModel != "" && !gateStaged {
				gatePath := filepath.Join(buildTmp, recipe.ModelInstallerScriptName)
				if err := writePS(gatePath, recipe.ModelInstallerScriptFile()); err != nil {
					return drivers, nil, err
				}
				stage.AddFile(gatePath, path.Join(scriptsImg, recipe.ModelInstallerScriptName))
				gateStaged = true
			}
		}
	}

	// Payload.
	for _, item := range w.Payload {
		if item.Ref != "" {
			file, err := materializeFile(ctx, req, item.Ref)
			if err != nil {
				return drivers, nil, err
			}
			stage.AddFile(file.host, path.Join(scriptsImg, file.name))
			refFiles[item.Ref] = file.name
			continue
		}
		host := filepath.Join(ws.Dir, filepath.FromSlash(item.Path))
		if _, err := os.Stat(host); err != nil {
			return drivers, nil, fmt.Errorf("compose: payload path %s: %w", item.Path, err)
		}
		stage.AddFile(host, path.Join(scriptsImg, filepath.Base(host)))
	}

	return drivers, refFiles, nil
}

// refResolver finds the staged name of a source a first-boot step names,
// staging it if nothing has yet.
func refResolver(ctx context.Context, req Request, stage fsimg.StageMap, refFiles map[string]string) func(string) (string, error) {
	return func(ref string) (string, error) {
		if name, ok := refFiles[ref]; ok {
			return name, nil
		}
		file, err := materializeFile(ctx, req, ref)
		if err != nil {
			return "", fmt.Errorf("compose: firstboot step references %q: %w", ref, err)
		}
		stage.AddFile(file.host, path.Join(scriptsImg, file.name))
		refFiles[ref] = file.name
		return file.name, nil
	}
}

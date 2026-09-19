package compose

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/uplinkresearch/dsky/internal/agent"
	"github.com/uplinkresearch/dsky/internal/agentbin"
	"github.com/uplinkresearch/dsky/internal/fsimg"
	"github.com/uplinkresearch/dsky/internal/recipe"
)

// A payload is a recipe without the operating system: the agent, its
// manifest, and the driver packs and installers the recipe stages -- zipped,
// to be carried to a machine that already runs Windows and run there. The
// same recipe builds both; a stick installs Windows and then does these
// things, a payload does these things to the Windows that is already there.
//
// It stages through the same code as the install image, so the two cannot
// drift: a pack that unpacks on a stick unpacks in a payload, under the same
// name, judged the same way. What a payload leaves out is everything that is
// about installing an OS -- the answer file, ei.cfg, WinPE boot drivers, and
// the verify.ps1 whose paths assume first boot. Its check is the agent's own:
// `dsky-agent.exe verify`, run beside the manifest, wherever that is.

// launcherName is the double-click way in, named as a sentence because the
// person double-clicking it may know nothing else about DSKY.
const launcherName = "Run DSKY.cmd"

// BuildPayload builds a standalone payload for a Windows recipe: everything
// the recipe does after Windows exists, for a machine where Windows already
// does.
func BuildPayload(ctx context.Context, req Request) (*Artifact, error) {
	r := req.Recipe
	if r.Windows == nil {
		return nil, fmt.Errorf("compose: %s is not a Windows recipe; payloads deploy onto machines that run Windows", r.ID)
	}
	// No generated-script fallback here. The generated scripts assume a
	// first boot -- one run, at first sign-in, on a machine with no owner --
	// and a payload is the opposite of that, so the agent is the only thing
	// allowed to carry one out.
	if covered, why := agentCovers(r); !covered {
		return nil, fmt.Errorf("compose: %s cannot be built as a payload — %s", r.ID, why)
	}

	key, err := inputsKey(req, "standalone-payload")
	if err != nil {
		return nil, err
	}
	lib := req.Library
	outPath := filepath.Join(lib.ArtifactsDir(), fmt.Sprintf("%s-payload-%s.exe", r.ID, key))
	if !req.Rebuild {
		if a, err := LoadArtifact(MetaPath(outPath)); err == nil {
			if _, err := os.Stat(a.Path); err == nil {
				req.progress("cached", 1, 1)
				return a, nil
			}
		}
	}

	buildTmp, err := os.MkdirTemp(lib.TmpDir(), "payload-"+r.ID+"-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(buildTmp)

	stage := fsimg.StageMap{}
	drivers, refFiles, err := stageDriversAndPayload(ctx, req, stage, buildTmp)
	if err != nil {
		return nil, err
	}
	if len(r.Windows.WinPEDrivers) > 0 {
		req.progress("winpe drivers are for booting Setup, so a payload does not carry them", 0, -1)
	}

	// The installers named by first-boot steps, staged explicitly. An install
	// image stages these as a side effect of generating verify.ps1, which a
	// payload does not carry -- and an installer that is not staged is
	// silently absent from the manifest, which is a payload of programs
	// without its programs.
	resolveRef := refResolver(ctx, req, stage, refFiles)
	for _, st := range r.Windows.Firstboot.Steps {
		for _, item := range []*recipe.RunItem{st.MSI, st.Exe} {
			if item != nil && item.Ref != "" {
				if _, err := resolveRef(item.Ref); err != nil {
					return nil, err
				}
			}
		}
	}

	// The manifest: the same one an install image writes, in standalone
	// mode, named by this build so a machine can tell a re-run of the same
	// payload from a newer one. No verify script -- its paths assume first
	// boot -- and the agent's own `verify` is the check instead.
	m := buildManifest(r, drivers, agentInstallers(r.Windows, refFiles),
		agentOfflineApps(r.Windows, refFiles), "", req.migrateParts())
	m.Mode = agent.ModeStandalone
	m.Build = key
	if err := stageAgent(stage, buildTmp, m); err != nil {
		return nil, err
	}

	// The way in for a person: elevate and run. And a way to say so for a
	// person who found this zip with no instructions at all.
	launcher := filepath.Join(buildTmp, "launcher.cmd")
	if err := os.WriteFile(launcher, []byte(payloadLauncher()), 0o755); err != nil {
		return nil, err
	}
	stage.AddFile(launcher, path.Join(scriptsImg, launcherName))
	readme := filepath.Join(buildTmp, "README.txt")
	if err := os.WriteFile(readme, []byte(payloadReadme(r, m)), 0o644); err != nil {
		return nil, err
	}
	stage.AddFile(readme, path.Join(scriptsImg, "README.txt"))

	req.progress("payload", 0, -1)
	if err := writePayloadExe(outPath, stage); err != nil {
		return nil, err
	}
	sum, size, err := hashFileWithProgress(outPath, func(done, total int64) {
		req.progress("hash", done, total)
	})
	if err != nil {
		return nil, err
	}
	a := &Artifact{
		RecipeID: r.ID, Kind: "payload", Path: outPath, Size: size, SHA256: sum,
		InputsKey: key, CreatedAt: nowUTC(), Tool: toolVersion(),
	}
	return a, a.save()
}

// writePayloadExe writes the payload as one file that is both a program and a
// zip: the agent, then the staged files as a zip appended to it. Windows runs
// the program and ignores what follows it; a zip reader finds the archive from
// its own end and ignores what comes before it. Double-click it and it runs;
// open it with anything that opens zips and out comes the folder.
//
// One file because the folder form has a wrong way to start it. Explorer will
// run "Run DSKY.cmd" from inside the un-extracted zip, in a temporary folder
// beside none of the files it needs, and the launcher has to catch that and
// tell somebody to go back and extract it properly. There is no equivalent
// mistake to make with one file.
//
// The zip is deterministic -- sorted entries, one fixed timestamp -- and the
// agent ahead of it is the same embedded bytes every time, so the same inputs
// are the same file and the build caches by its inputs.
func writePayloadExe(outPath string, stage fsimg.StageMap) error {
	exe, err := agentbin.Binary(agentbin.AMD64)
	if err != nil {
		return err
	}
	imgPaths := make([]string, 0, len(stage))
	for p := range stage {
		if !strings.HasPrefix(p, scriptsImg+"/") {
			// Everything a payload stages lives under the scripts dir; a
			// path outside it means a new kind of staging this function has
			// not been taught to place, and guessing would scatter files.
			return fmt.Errorf("compose: staged path %s is outside the payload root", p)
		}
		imgPaths = append(imgPaths, p)
	}
	sort.Strings(imgPaths)

	f, err := os.Create(outPath + ".partial")
	if err != nil {
		return err
	}
	if _, err := f.Write(exe); err != nil {
		f.Close()
		return err
	}
	zw := zip.NewWriter(f)
	// The archive begins after the agent, and every offset inside it is
	// written relative to that, which is what makes both halves readable.
	zw.SetOffset(int64(len(exe)))
	stamp := time.Unix(defaultSourceDateEpochUnix, 0).UTC()
	for _, p := range imgPaths {
		rel := strings.TrimPrefix(p, scriptsImg+"/")
		hdr := &zip.FileHeader{Name: rel, Method: zip.Deflate, Modified: stamp}
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			return err
		}
		src, err := os.Open(stage[p])
		if err != nil {
			return err
		}
		_, err = io.Copy(w, src)
		src.Close()
		if err != nil {
			return err
		}
	}
	if err := zw.Close(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(outPath+".partial", 0o755); err != nil {
		return err
	}
	return os.Rename(outPath+".partial", outPath)
}

// payloadLauncher is the double-click way to run a payload: elevate the agent
// and let it take it from there. ASCII with CRLF line endings, like every
// batch file DSKY writes, because cmd reads anything else wrongly.
//
// It guards against the one way Explorer sabotages this: double-clicking the
// .cmd inside the un-extracted zip, which extracts the .cmd alone to a
// temporary folder and runs it there, beside nothing.
func payloadLauncher() string {
	lines := []string{
		"@echo off",
		"rem Generated by DSKY. Runs this payload's agent as an administrator.",
		`cd /d "%~dp0"`,
		`if not exist ".\dsky-agent.exe" (`,
		"  echo This file is still inside the zip. Extract the whole folder first,",
		"  echo then run it from there.",
		"  pause",
		"  exit /b 1",
		")",
		`powershell -NoProfile -Command "Start-Process -Verb RunAs -FilePath '.\dsky-agent.exe' -ArgumentList 'apply'"`,
		"",
	}
	return strings.Join(lines, "\r\n")
}

// payloadReadme says what this is to whoever is holding it, and how a remote
// tool runs it with nobody at the machine. It is read by somebody who opened
// the file with a zip program instead of running it, so it starts by saying
// that running it is all there is to do.
func payloadReadme(r *recipe.Recipe, m *agent.Manifest) string {
	var b strings.Builder
	p := func(s string) { b.WriteString(s); b.WriteString("\r\n") }
	p("DSKY payload: " + r.ID + " (build " + m.Build + ")")
	p("")
	p("Sets up programs and drivers on a machine that already runs Windows.")
	p("It does not install an operating system and does not erase anything.")
	p("")
	p("To run it: copy the .exe this came out of onto the machine and")
	p("double-click it, then approve the administrator prompt. A window shows")
	p("what is being done. Nothing has to be extracted first -- that file is")
	p("both a program and this zip.")
	p("")
	p("The payload is copied to C:\\ProgramData\\DSKY\\payloads as it runs, and")
	p("the log is firstboot.log there.")
	p("")
	p("From a remote tool or a script, as an administrator, with no window:")
	p("")
	p("  <payload>.exe apply --quiet --unattended")
	p("")
	p("or, from these files extracted into a folder:")
	p("")
	p("  dsky-agent.exe apply --quiet --unattended")
	p("")
	p("The exit code is the number of problems; 0 is a clean run. To check a")
	p("machine afterwards: dsky-agent.exe verify")
	if m.Debloat != nil {
		p("")
		p("Note: this recipe also removes Windows consumer apps (preset " + m.Debloat.Preset + "),")
		p("as it would on a machine it had just installed.")
	}
	return b.String()
}

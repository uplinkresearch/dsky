// Package compose turns a recipe into a flashable artifact: for Windows
// media, a raw MBR/GPT+FAT32 image built in userspace; for Linux ISOs and
// appliance images, a reference to the (possibly compressed) blob that the
// flash engine raw-writes.
package compose

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/uplinkresearch/dsky/internal/agent"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/uplinkresearch/dsky/internal/agentbin"
	"github.com/uplinkresearch/dsky/internal/buildinfo"
	"github.com/uplinkresearch/dsky/internal/library"
	"github.com/uplinkresearch/dsky/internal/recipe"
	"github.com/uplinkresearch/dsky/internal/stream"
	"github.com/uplinkresearch/dsky/internal/workspace"
)

// defaultSourceDateEpoch fixes FAT timestamps for reproducible images when
// the caller has not set SOURCE_DATE_EPOCH (an arbitrary fixed date).
const defaultSourceDateEpoch = "1756800000"

// defaultSourceDateEpochUnix is the same moment as a number, for the zip
// writer, which takes a time rather than an environment variable.
const defaultSourceDateEpochUnix = 1756800000

// Request is one build.
type Request struct {
	Workspace *workspace.Workspace
	Library   *library.Library
	Recipe    *recipe.Recipe
	CLIVars   map[string]string
	// Settings are what a migration asks the machine to be set to. They are
	// handed in rather than read from the recipe on purpose: they belong to
	// one machine's approved plan, not to a recipe somebody reuses.
	Settings *agent.Settings
	// Printers and Drives likewise: the queues and drive letters the old
	// machine had.
	Printers []agent.Printer
	Drives   []agent.MappedDrive
	// Rebuild forces composing even when a cached artifact matches.
	Rebuild bool
	// Progress receives coarse stage updates; total may be -1.
	Progress func(stage string, done, total int64)
}

func (r *Request) progress(stage string, done, total int64) {
	if r.Progress != nil {
		r.Progress(stage, done, total)
	}
}

// Artifact is a flashable build product, persisted as a JSON sidecar so
// `dsky flash` can run in a later invocation.
type Artifact struct {
	RecipeID  string    `json:"recipe_id"`
	Kind      string    `json:"kind"` // "image" (composed) | "raw" (blob passthrough)
	Path      string    `json:"path"`
	Size      int64     `json:"size"`
	SHA256    string    `json:"sha256,omitempty"`   // of Path as stored
	Compress  string    `json:"compress,omitempty"` // raw kind: none|xz|zstd|gz
	InputsKey string    `json:"inputs_key"`
	Verify    string    `json:"verify"` // readback-sha256 | none
	MinStick  int64     `json:"min_stick,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	Tool      string    `json:"tool"`

	// Name and Choices belong to whoever asked for the build: a name somebody
	// gave it, and the options it was built from, so it can be shown by that
	// name and built again with those options changed. Compose neither reads
	// nor needs them, and keeps them only because the description beside the
	// file is the one place that goes wherever the file goes.
	Name    string          `json:"name,omitempty"`
	Choices json.RawMessage `json:"choices,omitempty"`
}

// MetaPath is the sidecar location for an artifact image path.
func MetaPath(imgPath string) string { return imgPath + ".json" }

// ArtifactForImage describes an image file this tool did not build, so it can
// be written like anything else: a distro the catalog has never heard of, a
// vendor's firmware updater, an in-house WinPE stick, a .img from a colleague.
//
// Nothing is copied and nothing is hashed up front. The flash engine hashes
// what it writes as it writes and compares the readback against that, so
// readback verification is as strong here as for a composed artifact — what
// is missing is only a pinned hash saying the file was the right one to begin
// with. That is the operator's business: they chose the file.
//
// Compression is sniffed from the magic bytes rather than the extension,
// because a .img.xz renamed to .img is common and guessing from the name
// would write the compressed bytes to the stick.
func ArtifactForImage(path string) (*Artifact, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if st.IsDir() {
		return nil, fmt.Errorf("%s is a directory, not an image file", path)
	}
	if st.Size() == 0 {
		return nil, fmt.Errorf("%s is empty", path)
	}
	comp, err := stream.Detect(path)
	if err != nil {
		return nil, err
	}
	// A compressed image expands to an unknown size, so no useful minimum can
	// be stated; the write fails cleanly on a too-small stick either way,
	// because it checks as it goes rather than only up front.
	var minStick int64
	if comp == "" || comp == "none" {
		minStick = st.Size()
	}
	return &Artifact{
		RecipeID: filepath.Base(path), Kind: "raw", Path: path, Size: st.Size(),
		Compress: comp, Verify: "readback-sha256", MinStick: minStick,
		InputsKey: "", CreatedAt: nowUTC(), Tool: toolVersion(),
	}, nil
}

// ResolveImage turns a path on disk into something flashable, and reports
// whether it came from a sidecar this tool wrote.
//
// A sidecar is preferred when present: it carries the recipe's minimum stick
// size and verification choice, which a bare file cannot state. Otherwise the
// file is described on its own terms, which is what lets any image be written.
//
// One implementation for every caller on purpose — the CLI, the one-shot and
// the portal must agree about what a given path means, and about the size
// interlock that comes with it.
func ResolveImage(path string) (a *Artifact, composed bool, err error) {
	if a, err := LoadArtifact(MetaPath(path)); err == nil {
		return a, true, nil
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil, false, fmt.Errorf("no such file: %s", path)
		}
		return nil, false, err
	}
	a, err = ArtifactForImage(path)
	return a, false, err
}

// LoadArtifact reads a sidecar.
func LoadArtifact(metaPath string) (*Artifact, error) {
	b, err := os.ReadFile(metaPath)
	if err != nil {
		return nil, err
	}
	var a Artifact
	if err := json.Unmarshal(b, &a); err != nil {
		return nil, fmt.Errorf("%s: %w", metaPath, err)
	}
	return &a, nil
}

// Save rewrites the description beside the file, for a caller that has
// changed its Name or Choices.
func (a *Artifact) Save() error { return a.save() }

func (a *Artifact) save() error {
	b, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(MetaPath(a.Path), b, 0o644)
}

// Build dispatches on recipe OS type.
func Build(ctx context.Context, req Request) (*Artifact, error) {
	switch req.Recipe.OS.Type {
	case recipe.OSWindows:
		return buildWindows(ctx, req)
	case recipe.OSLinuxISO, recipe.OSRawImg:
		return buildRaw(ctx, req)
	default:
		return nil, fmt.Errorf("compose: unsupported os.type %q", req.Recipe.OS.Type)
	}
}

// inputsKey fingerprints everything that shapes the output: the recipe file,
// the workspace config, resolved source hashes, and the tool version.
func inputsKey(req Request, sourceHashes ...string) (string, error) {
	h := sha256.New()
	for _, p := range []string{req.Recipe.Path, filepath.Join(req.Workspace.Dir, "workspace.yaml")} {
		b, err := os.ReadFile(p)
		if err != nil {
			return "", err
		}
		h.Write(b)
		h.Write([]byte{0})
	}
	// Template files referenced by the recipe.
	if w := req.Recipe.Windows; w != nil {
		var tmpls []string
		if w.Unattend != nil && w.Unattend.Template != "" {
			tmpls = append(tmpls, w.Unattend.Template)
		}
		if w.Firstboot.Template != "" {
			tmpls = append(tmpls, w.Firstboot.Template)
		}
		for _, t := range tmpls {
			b, err := os.ReadFile(filepath.Join(req.Workspace.Dir, filepath.FromSlash(t)))
			if err != nil {
				return "", err
			}
			h.Write(b)
			h.Write([]byte{0})
		}
	}
	if l := req.Recipe.Linux; l != nil && l.Kickstart != nil && l.Kickstart.File != "" {
		b, err := os.ReadFile(filepath.Join(req.Workspace.Dir, filepath.FromSlash(l.Kickstart.File)))
		if err != nil {
			return "", err
		}
		h.Write(b)
		h.Write([]byte{0})
	}
	if l := req.Recipe.Linux; l != nil && l.Autoinstall != nil {
		for _, t := range []string{l.Autoinstall.UserData, l.Autoinstall.MetaData} {
			if t == "" {
				continue
			}
			b, err := os.ReadFile(filepath.Join(req.Workspace.Dir, filepath.FromSlash(t)))
			if err != nil {
				return "", err
			}
			h.Write(b)
			h.Write([]byte{0})
		}
	}
	for _, s := range sourceHashes {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	// Vars affect rendering (vars.local.yaml and --var included).
	vars := req.Workspace.MergedVars(req.Recipe, req.CLIVars)
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(h, "%s=%s\x00", k, vars[k])
	}
	h.Write([]byte(buildinfo.Version))
	// The agent is staged onto the media and does the work at first boot, so
	// a different agent is a different build -- whatever the version says.
	// Development builds all carry one version string, so without this a
	// fixed agent came back from the cache as the media that had the broken
	// one.
	h.Write([]byte(agentbin.Digest(agentbin.AMD64)))
	h.Write([]byte(agentbin.Digest(agentbin.ARM64)))
	return hex.EncodeToString(h.Sum(nil))[:16], nil
}

// hashFileWithProgress hashes a file reporting progress.
func hashFileWithProgress(path string, report func(done, total int64)) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	h := sha256.New()
	buf := make([]byte, 4<<20)
	var done int64
	for {
		n, rerr := f.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
			done += int64(n)
			if report != nil {
				report(done, st.Size())
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return "", 0, rerr
		}
	}
	return hex.EncodeToString(h.Sum(nil)), st.Size(), nil
}

// minStickBytes is the smallest stick this build may be written to: what the
// recipe asks for, and never less than the image itself.
//
// The recipe's figure is a guess made before the image exists, and it has been
// wrong in the direction that matters. Windows 11 25H2 composes to about 8.9
// GiB on its own, against a recipe that says 8 GiB -- so the check that exists
// to stop somebody writing to a stick that cannot hold the image was passing
// exactly that stick, and the write failed part way through instead. Staging
// programs for an offline install makes the image bigger again.
//
// size is the composed image; zero where the caller does not have it yet.
func minStickBytes(r *recipe.Recipe, size int64) int64 {
	var n int64
	if r.Target.MinStick != "" {
		if v, err := recipe.ParseSize(r.Target.MinStick); err == nil {
			n = v
		}
	}
	if size > n {
		return size
	}
	return n
}

// migrateParts gathers what a migration asked for, for the agent's manifest.
func (r Request) migrateParts() migrateParts {
	return migrateParts{Settings: r.Settings, Printers: r.Printers, Drives: r.Drives}
}

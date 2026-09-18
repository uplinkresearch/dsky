// Package library is the machine-local content store: a content-addressed
// blob store plus a catalog mapping source IDs to blobs. Multi-gigabyte
// binaries live here (never in git); workspaces reference them by manifest.
package library

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/uplinkresearch/dsky/internal/fetch"
	"github.com/uplinkresearch/dsky/internal/manifest"
)

// Entry is one catalog record.
type Entry struct {
	ID         string          `json:"id"`
	SHA256     string          `json:"sha256"`
	Filename   string          `json:"filename"`
	Kind       manifest.Kind   `json:"kind"`
	Format     manifest.Format `json:"format"`
	Size       int64           `json:"size"`
	SourceURL  string          `json:"source_url,omitempty"`
	ImportedAt time.Time       `json:"imported_at"`
}

// Library is a store rooted at one directory.
type Library struct {
	Root string
}

// DefaultRoot picks the per-user store location: never a roaming or
// cloud-synced path (multi-GB blobs).
func DefaultRoot() string {
	if env := os.Getenv("DSKY_LIBRARY"); env != "" {
		return env
	}
	switch runtime.GOOS {
	case "windows":
		if la := os.Getenv("LOCALAPPDATA"); la != "" {
			return filepath.Join(la, "dsky")
		}
	case "darwin":
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, "Library", "Application Support", "dsky")
		}
	default:
		if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
			return filepath.Join(xdg, "dsky")
		}
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, ".local", "share", "dsky")
		}
	}
	return filepath.Join(".", "dsky-library")
}

// formerNames are the directory names this library has lived under, newest
// first. The tool has been renamed twice and the library is the one thing that
// must not be lost to it: a full cache is tens of gigabytes of ISOs, and losing
// it looks like the tool forgetting everything and silently re-downloading.
var formerNames = []string{"bootwright", "uplink-composer", "the-composer"}

// Open ensures the directory layout exists and returns the library. A library
// left under one of the tool's former names is migrated once, so an existing
// cache of ISOs and driver packs survives the rename.
func Open(root string) (*Library, error) {
	if filepath.Base(root) == "dsky" {
		if _, err := os.Stat(root); os.IsNotExist(err) {
			for _, name := range formerNames {
				old := filepath.Join(filepath.Dir(root), name)
				if st, err := os.Stat(old); err == nil && st.IsDir() {
					// Best-effort: a rename within one directory is atomic and
					// instant whatever the size, and if it fails we fall
					// through to a fresh directory rather than refusing to run.
					if os.Rename(old, root) == nil {
						break
					}
				}
			}
		}
	}
	l := &Library{Root: root}
	for _, d := range []string{l.blobDir(), l.TmpDir(), l.ArtifactsDir(), l.HelpersDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	return l, nil
}

func (l *Library) blobDir() string     { return filepath.Join(l.Root, "blobs", "sha256") }
func (l *Library) catalogPath() string { return filepath.Join(l.Root, "catalog.json") }

// TmpDir holds in-progress downloads and staging trees; same volume as blobs
// so finalizing is a rename.
func (l *Library) TmpDir() string { return filepath.Join(l.Root, "tmp") }

// ArtifactsDir holds composed images.
func (l *Library) ArtifactsDir() string { return filepath.Join(l.Root, "artifacts") }

// HelpersDir holds pinned helper binaries (7zz on Linux, wimlib, ...).
func (l *Library) HelpersDir() string { return filepath.Join(l.Root, "helpers") }

// BlobPath is where content with this hash lives.
func (l *Library) BlobPath(sha256 string) string {
	return filepath.Join(l.blobDir(), sha256[:2], sha256)
}

func (l *Library) loadCatalog() (map[string]Entry, error) {
	b, err := os.ReadFile(l.catalogPath())
	if os.IsNotExist(err) {
		return map[string]Entry{}, nil
	}
	if err != nil {
		return nil, err
	}
	var m map[string]Entry
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("library catalog %s is corrupt: %w", l.catalogPath(), err)
	}
	return m, nil
}

func (l *Library) saveCatalog(m map[string]Entry) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := l.catalogPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	if err := os.Remove(l.catalogPath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.Rename(tmp, l.catalogPath())
}

// storeBlob moves a fully-verified file into the blob store.
func (l *Library) storeBlob(path, sha string) error {
	dst := l.BlobPath(sha)
	if _, err := os.Stat(dst); err == nil {
		return os.Remove(path) // already have it
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.Rename(path, dst); err != nil {
		// Cross-volume import: fall back to copy.
		if err := copyFile(path, dst); err != nil {
			return err
		}
		return os.Remove(path)
	}
	return nil
}

// Import copies a local file (e.g. a manually downloaded Windows ISO) into
// the store under the given source metadata and returns its entry.
func (l *Library) Import(src *manifest.Source, filePath string) (Entry, error) {
	return l.ImportWithProgress(src, filePath, nil)
}

// ImportWithProgress is Import with something to watch.
//
// Importing reads the file twice — once to hash it, once to copy it — and for
// a Windows ISO that is ten gigabytes of work. Without progress the tool sits
// silent for minutes on a step the operator just asked for, which is
// indistinguishable from being hung. progress may be nil.
func (l *Library) ImportWithProgress(src *manifest.Source, filePath string, progress func(stage string, done, total int64)) (Entry, error) {
	st, err := os.Stat(filePath)
	if err != nil {
		return Entry{}, err
	}
	name := filepath.Base(filePath)
	sum, err := hashFile(filePath, st.Size(), "checking "+name, progress)
	if err != nil {
		return Entry{}, err
	}
	if src.SHA256 != "" && src.SHA256 != sum {
		return Entry{}, fmt.Errorf("library: %s has sha256 %s, manifest %s pins %s — wrong file or corrupted download",
			filePath, sum, src.ID, src.SHA256)
	}
	// Copy into tmp first so the original file is never consumed.
	tmp := filepath.Join(l.TmpDir(), "import-"+sum)
	if err := copyFileProgress(filePath, tmp, st.Size(), "filing "+name, progress); err != nil {
		return Entry{}, err
	}
	if err := l.storeBlob(tmp, sum); err != nil {
		return Entry{}, err
	}
	e := Entry{
		ID: src.ID, SHA256: sum, Filename: filepath.Base(filePath),
		Kind: src.Kind, Format: src.Format, Size: st.Size(),
		ImportedAt: time.Now().UTC(),
	}
	if src.Filename != "" {
		e.Filename = src.Filename
	}
	return e, l.record(e)
}

// ErrUnpinned is returned by Pull for a manifest without a sha256 pin when
// trust-on-first-use was not explicitly requested.
type ErrUnpinned struct {
	ID     string
	SHA256 string // computed hash of what was downloaded
}

func (e *ErrUnpinned) Error() string {
	return fmt.Sprintf("source %s has no sha256 pin; downloaded content hashes to %s — verify it, add `sha256: %s` to the manifest, or re-run with --pin-tofu",
		e.ID, e.SHA256, e.SHA256)
}

// URLResolver resolves a provider-based source (e.g. fido) to a concrete
// download URL at pull time.
type URLResolver func(ctx context.Context, src *manifest.Source) (string, error)

// Pull downloads a manifest source into the store. Unpinned sources download
// but return *ErrUnpinned unless pinTOFU is set; either way the computed hash
// is preserved (the blob stays cached), so pinning then re-pulling is free.
// resolve is consulted for provider-based sources; nil restricts Pull to
// plain-URL manifests.
//
// note hears which server the bytes are coming from and why that changed: a
// mirror picked for being faster, a server tried again after it stopped
// sending, the move on to the next one. It may be nil — and it was nil at
// every call site for as long as there was anything to say, so DSKY has been
// quietly downloading Ubuntu from kernel.org, and quietly retrying, while the
// screen said "downloading" and nothing else. A progress bar that stops for
// seven seconds and explains nothing is how a working program looks broken.
func (l *Library) Pull(ctx context.Context, src *manifest.Source, pinTOFU bool, resolve URLResolver, progress fetch.Progress, note func(string)) (Entry, error) {
	if e, err := l.Resolve(src.ID); err == nil && src.SHA256 != "" && e.SHA256 == src.SHA256 {
		return e, nil // already present and matching
	}
	url := src.URL
	if src.Provider != "" {
		if resolve == nil {
			return Entry{}, fmt.Errorf("library: source %s uses provider %s, which this caller cannot resolve", src.ID, src.Provider)
		}
		var err error
		if url, err = resolve(ctx, src); err != nil {
			return Entry{}, err
		}
	}
	if url == "" {
		return Entry{}, fmt.Errorf("library: source %s has no url; use `dsky sources import %s <file>`", src.ID, src.ID)
	}
	dest := filepath.Join(l.TmpDir(), src.ID+"-"+src.DownloadFilename())
	// A pinned file may come from a mirror (Ubuntu's release server is slow
	// from some networks), because the hash below decides whether what arrived
	// is the file. An unpinned one only ever comes from its own URL: there is
	// nothing to catch a mirror serving something else.
	sum, used, err := fetch.DownloadAny(ctx, pullURLs(src, url), dest, progress, note)
	if err != nil {
		return Entry{}, err
	}
	url = used
	if src.SHA256 != "" && sum != src.SHA256 {
		os.Remove(dest)
		return Entry{}, fmt.Errorf("library: %s downloaded from %s hashes to %s, manifest pins %s — refusing (upstream changed or download corrupted)",
			src.ID, url, sum, src.SHA256)
	}
	st, _ := os.Stat(dest)
	var size int64
	if st != nil {
		size = st.Size()
	}
	if err := l.storeBlob(dest, sum); err != nil {
		return Entry{}, err
	}
	filename := src.DownloadFilename()
	if src.Filename == "" && src.Provider != "" {
		// Providers resolve the real name at pull time (e.g. the ISO name
		// Microsoft serves); prefer it over the manifest id.
		if base := urlBasename(url); base != "" {
			filename = base
		}
	}
	e := Entry{
		ID: src.ID, SHA256: sum, Filename: filename,
		Kind: src.Kind, Format: src.Format, Size: size,
		SourceURL: url, ImportedAt: time.Now().UTC(),
	}
	if src.SHA256 == "" && !pinTOFU {
		// Blob is stored (cache) but not cataloged as trusted.
		return Entry{}, &ErrUnpinned{ID: src.ID, SHA256: sum}
	}
	return e, l.record(e)
}

// pullURLs is where a source may be downloaded from: its own URL, plus known
// mirrors when the file is pinned by hash and not resolved by a provider.
func pullURLs(src *manifest.Source, url string) []string {
	urls := []string{url}
	if src.SHA256 != "" && src.Provider == "" {
		urls = append(urls, fetch.Alternates(url)...)
	}
	return urls
}

func (l *Library) record(e Entry) error {
	cat, err := l.loadCatalog()
	if err != nil {
		return err
	}
	cat[e.ID] = e
	return l.saveCatalog(cat)
}

// Resolve returns the catalog entry for id; the blob is verified to exist.
func (l *Library) Resolve(id string) (Entry, error) {
	cat, err := l.loadCatalog()
	if err != nil {
		return Entry{}, err
	}
	e, ok := cat[id]
	if !ok {
		return Entry{}, fmt.Errorf("library: %s is not in the local library — run `dsky sources pull %s` or `dsky sources import %s <file>`", id, id, id)
	}
	if _, err := os.Stat(l.BlobPath(e.SHA256)); err != nil {
		return Entry{}, fmt.Errorf("library: %s is cataloged but its blob is missing (%s) — re-pull or re-import", id, l.BlobPath(e.SHA256))
	}
	return e, nil
}

// Remove takes id out of the catalog and deletes its blob, unless another
// entry shares it. Returns bytes freed.
func (l *Library) Remove(id string) (int64, error) {
	cat, err := l.loadCatalog()
	if err != nil {
		return 0, err
	}
	e, ok := cat[id]
	if !ok {
		return 0, fmt.Errorf("library: %s is not in the local library", id)
	}
	delete(cat, id)
	if err := l.saveCatalog(cat); err != nil {
		return 0, err
	}
	for _, other := range cat {
		if strings.EqualFold(other.SHA256, e.SHA256) {
			return 0, nil
		}
	}
	p := l.BlobPath(e.SHA256)
	st, err := os.Stat(p)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err := os.Remove(p); err != nil {
		return 0, err
	}
	return st.Size(), nil
}

// List returns catalog entries sorted by ID.
func (l *Library) List() ([]Entry, error) {
	cat, err := l.loadCatalog()
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(cat))
	for _, e := range cat {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// GC removes blobs no catalog entry references and clears tmp. Returns bytes
// freed.
func (l *Library) GC() (int64, error) {
	cat, err := l.loadCatalog()
	if err != nil {
		return 0, err
	}
	wanted := map[string]bool{}
	for _, e := range cat {
		wanted[strings.ToLower(e.SHA256)] = true
	}
	var freed int64
	err = filepath.WalkDir(l.blobDir(), func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if !wanted[strings.ToLower(d.Name())] {
			if st, err := os.Stat(p); err == nil {
				freed += st.Size()
			}
			return os.Remove(p)
		}
		return nil
	})
	if err != nil {
		return freed, err
	}
	entries, err := os.ReadDir(l.TmpDir())
	if err != nil {
		return freed, err
	}
	for _, e := range entries {
		p := filepath.Join(l.TmpDir(), e.Name())
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			freed += st.Size()
		}
		os.RemoveAll(p)
	}
	return freed, nil
}

// urlBasename extracts the final path segment of a URL, without query.
func urlBasename(u string) string {
	base := u[strings.LastIndex(u, "/")+1:]
	if i := strings.IndexAny(base, "?#"); i >= 0 {
		base = base[:i]
	}
	return base
}

func copyFile(src, dst string) error {
	return copyFileProgress(src, dst, 0, "", nil)
}

// counter reports bytes as they pass, so hashing and copying can be watched
// without reading the file a third time.
type counter struct {
	w        io.Writer
	done     int64
	total    int64
	stage    string
	progress func(string, int64, int64)
	// Reported on a byte interval rather than per write: a 4 MiB copy loop
	// would otherwise call back thousands of times a second for a display
	// that changes ten times a second.
	next int64
}

func (c *counter) Write(b []byte) (int, error) {
	n, err := c.w.Write(b)
	c.done += int64(n)
	if c.progress != nil && (c.done >= c.next || c.done == c.total) {
		c.next = c.done + 4<<20
		c.progress(c.stage, c.done, c.total)
	}
	return n, err
}

func newCounter(w io.Writer, total int64, stage string, progress func(string, int64, int64)) *counter {
	if progress != nil {
		progress(stage, 0, total) // draw the stage before the first chunk lands
	}
	return &counter{w: w, total: total, stage: stage, progress: progress}
}

// hashFile is fetch.SHA256File with progress.
func hashFile(path string, total int64, stage string, progress func(string, int64, int64)) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(newCounter(h, total, stage, progress), f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func copyFileProgress(src, dst string, total int64, stage string, progress func(string, int64, int64)) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(newCounter(out, total, stage, progress), in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	return out.Close()
}

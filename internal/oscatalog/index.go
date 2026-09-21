package oscatalog

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/uplinkresearch/dsky/internal/buildinfo"
)

// The catalog index: the OS list, published separately from the program.
//
// The two change at completely different rates. A binary is cut every few
// weeks; Ubuntu, Fedora, Proxmox and the rest move whenever they like, and
// a pinned URL dies with them. Compiled into the binary, fixing one dead
// link costs a release and reaches only the people who then upgrade. Served
// as a file, it costs a push.
//
// The index is signed, and that is not optional. Its entire job is to say
// "fetch this URL and expect this hash", so whoever controls it controls
// what lands on someone's disk — and an attacker serving a modified index
// would supply a matching hash too, sailing straight through the existing
// verification. An Ed25519 public key compiled in here makes the hosting
// untrusted infrastructure: a tampered index is refused no matter who
// served it.
//
// Everything degrades toward working: a fetched index is preferred, then
// the last good cached one, then the list compiled into this binary. Being
// offline is normal on a bench and is not an error.

// indexSchema is the format this build understands. Additive changes keep
// the number; it moves only for a change that would mislead an older
// reader, and a mismatch makes the whole index be ignored rather than
// half-read.
const indexSchema = 1

// catalogPublicKey verifies the published index. The matching private key
// never goes near CI or this repository — it signs on the maintainer's
// machine, so a compromise of the hosting or the build cannot forge a
// catalog.
const catalogPublicKey = "b0bff8849496955ac361ef5461013035aeefad64dc433185f74ea75850d9a3f7"

// signingKey is the key actually consulted. It is a variable only so tests
// can substitute a throwaway keypair — it is unexported, so nothing outside
// this package can reach it, and it is never assigned in normal operation.
var signingKey = catalogPublicKey

// IndexURL is where the published index and its detached signature live.
// Raw file hosting on the repository's default branch: updating the catalog
// is a push, and no release is involved.
const (
	IndexURL    = "https://raw.githubusercontent.com/uplinkresearch/dsky/main/catalog/index.json"
	IndexSigURL = IndexURL + ".sig"
)

// knownFeatures are the capabilities this build can honour. An entry asking
// for anything outside this set is skipped — see Entry.Requires.
var knownFeatures = map[string]bool{
	"iso":                true, // hybrid installer ISO, written whole
	"raw-image":          true, // compressed raw disk image (Raspberry Pi, Steam Deck)
	"fido":               true, // Windows media resolved at pull time
	"checksums-url":      true, // hash resolved from a vendor checksum file
	FeatureImportOnly:    true, // no fetchable URL; the operator supplies the ISO
	FeatureWindowsServer: true, // Server media: image chosen by index, no ei.cfg
}

// Index is the published catalog document.
type Index struct {
	Schema int `json:"schema"`
	// Serial only ever increases. A client refuses an index older than the
	// one it already trusts, so a signed-but-stale copy cannot be replayed
	// to walk someone back onto a withdrawn entry.
	Serial    int64     `json:"serial"`
	Generated time.Time `json:"generated"`
	Entries   []Entry   `json:"entries"`
}

// active is the catalog in force: the published index once one has been
// accepted, otherwise the list compiled into this build.
var (
	activeMu      sync.RWMutex
	activeEntries []Entry
	activeSerial  int64
	activeSource  = "built in"
)

// Catalog returns the OS list in force.
func Catalog() []Entry {
	activeMu.RLock()
	defer activeMu.RUnlock()
	if activeEntries != nil {
		return activeEntries
	}
	return builtin
}

// Source describes where the current list came from, for `dsky catalog`
// and the portal to show. Someone debugging a wrong entry needs to know
// whether they are looking at a published list or a compiled-in one.
func Source() string {
	activeMu.RLock()
	defer activeMu.RUnlock()
	return activeSource
}

// Get returns the entry with id, or false.
func Get(id string) (Entry, bool) {
	for _, e := range Catalog() {
		if e.ID == id {
			return e, true
		}
	}
	return Entry{}, false
}

// cachePaths are where a verified index is kept between runs.
func cachePaths(root string) (payload, sig string) {
	dir := filepath.Join(root, "catalog")
	return filepath.Join(dir, "index.json"), filepath.Join(dir, "index.json.sig")
}

// LoadCached adopts the last index this machine verified, without touching
// the network. Called at startup so an offline run still has a current list.
func LoadCached(root string) {
	payloadPath, sigPath := cachePaths(root)
	payload, err := os.ReadFile(payloadPath)
	if err != nil {
		return
	}
	sig, err := os.ReadFile(sigPath)
	if err != nil {
		return
	}
	// Re-verified on every load rather than trusted because we wrote it:
	// the cache is a file on disk that anything could have edited.
	idx, err := parseIndex(payload, sig, 0)
	if err != nil {
		return
	}
	adopt(idx, "cached index")
}

// Refresh fetches the published index, verifies it, and adopts it if it is
// newer than what is already trusted. Every failure is returned for logging
// but none is fatal: the caller keeps whatever list it had.
func Refresh(ctx context.Context, root string) error {
	payload, err := fetch(ctx, IndexURL, 1<<20)
	if err != nil {
		return err
	}
	sig, err := fetch(ctx, IndexSigURL, 4096)
	if err != nil {
		return err
	}
	activeMu.RLock()
	haveSerial := activeSerial
	activeMu.RUnlock()

	idx, err := parseIndex(payload, sig, haveSerial)
	if err != nil {
		return err
	}
	payloadPath, sigPath := cachePaths(root)
	if err := os.MkdirAll(filepath.Dir(payloadPath), 0o755); err == nil {
		_ = os.WriteFile(payloadPath, payload, 0o644)
		_ = os.WriteFile(sigPath, sig, 0o644)
	}
	adopt(idx, fmt.Sprintf("published index #%d", idx.Serial))
	return nil
}

// adopt installs an index as the catalog in force.
func adopt(idx *Index, source string) {
	usable := make([]Entry, 0, len(idx.Entries))
	for _, e := range idx.Entries {
		if supported(e) {
			usable = append(usable, e)
		}
	}
	// An index that says nothing this build can use is not an improvement
	// on the compiled-in list, so it is ignored rather than adopted empty.
	if len(usable) == 0 {
		return
	}
	activeMu.Lock()
	defer activeMu.Unlock()
	if idx.Serial < activeSerial {
		return
	}
	activeEntries, activeSerial, activeSource = usable, idx.Serial, source
}

// supported reports whether this build can honour everything an entry asks
// for. Skipping one unknown entry keeps the rest of a newer catalog usable.
func supported(e Entry) bool {
	for _, f := range e.Requires {
		if !knownFeatures[f] {
			return false
		}
	}
	return true
}

// parseIndex verifies the signature, then the document. minSerial rejects a
// replayed older index; pass 0 to accept any.
func parseIndex(payload, sig []byte, minSerial int64) (*Index, error) {
	key, err := hex.DecodeString(signingKey)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("catalog: the built-in public key is unusable")
	}
	raw, err := hex.DecodeString(strings.TrimSpace(string(sig)))
	if err != nil {
		return nil, fmt.Errorf("catalog: signature is not hex")
	}
	if !ed25519.Verify(key, payload, raw) {
		return nil, fmt.Errorf("catalog: signature does not match — refusing the index")
	}
	var idx Index
	if err := json.Unmarshal(payload, &idx); err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	if idx.Schema != indexSchema {
		return nil, fmt.Errorf("catalog: index is schema %d, this build reads %d — ignoring it",
			idx.Schema, indexSchema)
	}
	if idx.Serial < minSerial {
		return nil, fmt.Errorf("catalog: index #%d is older than the one already trusted (#%d) — refusing it",
			idx.Serial, minSerial)
	}
	if len(idx.Entries) == 0 {
		return nil, fmt.Errorf("catalog: index lists no operating systems")
	}
	return &idx, nil
}

func fetch(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", buildinfo.UserAgent())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("catalog: fetching %s: %w", filepath.Base(url), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("catalog: fetching %s: HTTP %d", filepath.Base(url), resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

// BuildIndex serialises the compiled-in list for publication. The generator
// uses it so the published index is produced from the same data the program
// ships with, never hand-maintained beside it.
func BuildIndex(serial int64, now time.Time) *Index {
	return &Index{
		Schema:    indexSchema,
		Serial:    serial,
		Generated: now.UTC().Truncate(time.Second),
		Entries:   builtin,
	}
}

// Marshal renders an index exactly as it should be published and signed.
// Signing covers these literal bytes, so this is the only place that may
// decide them.
func Marshal(idx *Index) ([]byte, error) {
	b, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// VerifyIndexBytes is the check the generator and the tests run: does this
// payload and signature satisfy the key compiled into this build?
func VerifyIndexBytes(payload, sig []byte) (*Index, error) {
	return parseIndex(payload, sig, 0)
}

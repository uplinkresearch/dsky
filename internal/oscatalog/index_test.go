package oscatalog

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// signWith produces a payload and signature the way the generator does.
func signWith(t *testing.T, priv ed25519.PrivateKey, idx *Index) (payload, sig []byte) {
	t.Helper()
	p, err := Marshal(idx)
	if err != nil {
		t.Fatal(err)
	}
	return p, []byte(hex.EncodeToString(ed25519.Sign(priv, p)))
}

// withKey swaps in a throwaway public key for the duration of a test, since
// the real private key is not in the repository.
func withKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	orig := signingKey
	signingKey = hex.EncodeToString(pub)
	t.Cleanup(func() { signingKey = orig })
	return priv
}

func sampleIndex(serial int64, entries ...Entry) *Index {
	if len(entries) == 0 {
		entries = []Entry{{ID: "test-os", Name: "Test OS", Family: Linux, Notes: "n", URL: "https://x/y.iso", SHA256: strings.Repeat("a", 64), Filename: "y.iso"}}
	}
	return &Index{Schema: indexSchema, Serial: serial, Generated: time.Now(), Entries: entries}
}

// TestRejectsForgedSignature is the property the whole design rests on: an
// index is only as trustworthy as its signature, because its contents decide
// what gets downloaded and what hash is expected. A tampered index that
// verification accepted would defeat every other check in the program.
func TestRejectsForgedSignature(t *testing.T) {
	priv := withKey(t)
	payload, sig := signWith(t, priv, sampleIndex(1))

	if _, err := parseIndex(payload, sig, 0); err != nil {
		t.Fatalf("a correctly signed index was refused: %v", err)
	}

	// Content changed after signing — the exact attack signing exists to stop.
	tampered := []byte(strings.Replace(string(payload), "https://x/y.iso", "https://evil/y.iso", 1))
	if _, err := parseIndex(tampered, sig, 0); err == nil {
		t.Error("ACCEPTED an index whose contents were changed after signing")
	}

	// Signed by someone else entirely.
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	_, otherSig := signWith(t, other, sampleIndex(1))
	if _, err := parseIndex(payload, otherSig, 0); err == nil {
		t.Error("ACCEPTED an index signed by a key that is not ours")
	}

	// Garbage where a signature should be.
	if _, err := parseIndex(payload, []byte("not-hex"), 0); err == nil {
		t.Error("ACCEPTED a malformed signature")
	}
}

// TestRejectsRollback: a signed index stays valid forever, so an old one
// could be replayed to walk somebody back onto an entry that was withdrawn.
// The serial is what stops that.
func TestRejectsRollback(t *testing.T) {
	priv := withKey(t)
	payload, sig := signWith(t, priv, sampleIndex(5))
	if _, err := parseIndex(payload, sig, 7); err == nil {
		t.Error("ACCEPTED index #5 when #7 was already trusted")
	}
	if _, err := parseIndex(payload, sig, 5); err != nil {
		t.Errorf("refused an index equal to the trusted serial: %v", err)
	}
}

// TestRejectsWrongSchema: a format this build does not understand must be
// ignored whole, not read in part and half-understood.
func TestRejectsWrongSchema(t *testing.T) {
	priv := withKey(t)
	idx := sampleIndex(1)
	idx.Schema = indexSchema + 1
	payload, sig := signWith(t, priv, idx)
	if _, err := parseIndex(payload, sig, 0); err == nil {
		t.Error("ACCEPTED an index from a newer schema")
	}
}

// TestSkipsEntriesNeedingUnknownFeatures is what makes a newer catalog safe
// on an older binary: one entry it cannot build is skipped, the rest still
// work.
func TestSkipsEntriesNeedingUnknownFeatures(t *testing.T) {
	resetActive(t)
	known := Entry{ID: "ok", Name: "OK", Family: Linux, Notes: "n", URL: "https://x/a.iso", Filename: "a.iso", SHA256: strings.Repeat("a", 64)}
	future := Entry{ID: "future", Name: "Future", Family: Linux, Notes: "n", URL: "https://x/b.iso", Filename: "b.iso",
		SHA256: strings.Repeat("b", 64), Requires: []string{"time-travel"}}

	adopt(sampleIndex(1, known, future), "test")
	got := Catalog()
	if len(got) != 1 || got[0].ID != "ok" {
		t.Fatalf("expected only the supported entry, got %+v", got)
	}
	if _, ok := Get("future"); ok {
		t.Error("an entry requiring an unknown feature was offered anyway")
	}
}

// TestIgnoresIndexWithNothingUsable: if a build understands none of a
// published index, the compiled-in list is better than an empty one.
func TestIgnoresIndexWithNothingUsable(t *testing.T) {
	resetActive(t)
	useless := Entry{ID: "x", Name: "X", Family: Linux, Notes: "n", Requires: []string{"nonsense"}}
	adopt(sampleIndex(1, useless), "test")
	if len(Catalog()) != len(builtin) {
		t.Error("adopted an index that offered nothing this build can use")
	}
}

// TestCacheRoundTrip: a verified index survives a restart, so an offline
// machine still has a current list.
func TestCacheRoundTrip(t *testing.T) {
	priv := withKey(t)
	resetActive(t)
	root := t.TempDir()
	payload, sig := signWith(t, priv, sampleIndex(42,
		Entry{ID: "cached-os", Name: "Cached", Family: Linux, Notes: "n", URL: "https://x/c.iso", Filename: "c.iso", SHA256: strings.Repeat("c", 64)}))

	p, s := cachePaths(root)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(p, payload, 0o644)
	os.WriteFile(s, sig, 0o644)

	LoadCached(root)
	if _, ok := Get("cached-os"); !ok {
		t.Fatal("the cached index was not adopted")
	}

	// A cache edited on disk must be refused, not trusted because we wrote it.
	resetActive(t)
	os.WriteFile(p, []byte(strings.Replace(string(payload), "cached-os", "evil-os", 1)), 0o644)
	LoadCached(root)
	if _, ok := Get("evil-os"); ok {
		t.Error("ACCEPTED a cached index that had been edited on disk")
	}
}

// TestBuiltinIsAlwaysAValidIndex: the generator publishes exactly what is
// compiled in, so the compiled-in list must itself satisfy every rule the
// reader applies.
func TestBuiltinIsAlwaysAValidIndex(t *testing.T) {
	idx := BuildIndex(1, time.Now())
	payload, err := Marshal(idx)
	if err != nil {
		t.Fatal(err)
	}
	var back Index
	if err := json.Unmarshal(payload, &back); err != nil {
		t.Fatalf("the built-in list does not round-trip through the index format: %v", err)
	}
	if len(back.Entries) != len(builtin) {
		t.Fatalf("round-trip lost entries: %d of %d", len(back.Entries), len(builtin))
	}
	for i, e := range back.Entries {
		if e.ID != builtin[i].ID || e.URL != builtin[i].URL || e.SHA256 != builtin[i].SHA256 {
			t.Errorf("entry %d changed across the index format: %+v vs %+v", i, e, builtin[i])
		}
		if !supported(e) {
			t.Errorf("%s requires a feature this build does not have", e.ID)
		}
	}
}

// TestCommittedIndexMatchesBuiltin stops the published list drifting from
// the compiled-in one. Editing builtin.go and forgetting to regenerate would
// otherwise publish a stale catalog that looks fine — and since the index is
// what users actually get, the mistake would be invisible here and visible
// to everyone else.
func TestCommittedIndexMatchesBuiltin(t *testing.T) {
	payload, err := os.ReadFile(filepath.Join("..", "..", "catalog", "index.json"))
	if err != nil {
		t.Skipf("no committed index: %v", err)
	}
	var idx Index
	if err := json.Unmarshal(payload, &idx); err != nil {
		t.Fatalf("committed index does not parse: %v", err)
	}
	if len(idx.Entries) != len(builtin) {
		t.Fatalf("committed index has %d entries, this build has %d — run: go run ./test/catalogen",
			len(idx.Entries), len(builtin))
	}
	// Whole entries, not the download fields alone. Editions, notes and the
	// required features are just as much what users get — the pickers read
	// them straight from this index — and comparing only where the bytes come
	// from let a renamed edition ship as a stale label in every portal while
	// this test stayed green.
	for i, e := range idx.Entries {
		b := builtin[i]
		got, _ := json.Marshal(e)
		want, _ := json.Marshal(b)
		if string(got) != string(want) {
			t.Errorf("entry %d (%s) differs from the built-in list — run: go run ./test/catalogen\n index: %s\n built: %s",
				i, b.ID, got, want)
		}
	}
	// And it must still satisfy the verifier, so a bad signature is caught
	// here rather than by every user at once.
	sig, err := os.ReadFile(filepath.Join("..", "..", "catalog", "index.json.sig"))
	if err != nil {
		t.Fatalf("committed index has no signature: %v", err)
	}
	if _, err := VerifyIndexBytes(payload, sig); err != nil {
		t.Errorf("the committed index does not verify against the built-in key: %v", err)
	}
}

// TestImportOnlyEntriesAreFeatureGated: an entry with no fetchable URL is
// only safe to publish because older builds skip it. If the feature name ever
// came off an import-only entry, those builds would offer it and then fail
// somewhere inside the downloader with nothing useful to say.
func TestImportOnlyEntriesAreFeatureGated(t *testing.T) {
	for _, e := range builtin {
		hasSource := e.URL != "" || e.Provider != "" || e.ChecksumsURL != ""
		switch {
		case !hasSource && !e.ImportOnly():
			t.Errorf("%s has nothing to download and is not marked %q — an older build would offer it and fail",
				e.ID, FeatureImportOnly)
		case e.ImportOnly() && hasSource:
			t.Errorf("%s is marked import-only but has a source; one of the two is wrong", e.ID)
		case e.ImportOnly() && e.ImportFrom == "":
			t.Errorf("%s cannot be downloaded and does not say where to get it", e.ID)
		}
	}
}

// TestImportOnlyRefusalSaysWhatToDo: this is the error most likely to be
// someone's first surprise, so it has to name the OS, the place to get it and
// the command that takes it.
func TestImportOnlyRefusalSaysWhatToDo(t *testing.T) {
	e := Entry{ID: "rhel-10", Name: "Red Hat Enterprise Linux 10",
		Requires: []string{FeatureImportOnly}, ImportFrom: "https://access.redhat.com/downloads"}
	msg := e.ImportOnlyError().Error()
	for _, want := range []string{e.Name, e.ImportFrom, "--iso", "dsky install rhel-10"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not mention %q: %s", want, msg)
		}
	}
}

// resetActive puts the package back to "nothing adopted" between tests.
func resetActive(t *testing.T) {
	t.Helper()
	activeMu.Lock()
	activeEntries, activeSerial, activeSource = nil, 0, "built in"
	activeMu.Unlock()
	t.Cleanup(func() {
		activeMu.Lock()
		activeEntries, activeSerial, activeSource = nil, 0, "built in"
		activeMu.Unlock()
	})
}

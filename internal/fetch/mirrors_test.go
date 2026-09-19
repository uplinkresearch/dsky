package fetch

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAlternates(t *testing.T) {
	got := Alternates("https://releases.ubuntu.com/26.04/ubuntu-26.04.1-desktop-amd64.iso")
	if len(got) == 0 || got[0] != "https://mirrors.edge.kernel.org/ubuntu-releases/26.04/ubuntu-26.04.1-desktop-amd64.iso" {
		t.Fatalf("alternates = %v", got)
	}
	if Alternates("https://example.com/releases.ubuntu.com/x.iso") != nil {
		t.Fatal("matched a URL that only mentions the host")
	}
	if Alternates("http://releases.ubuntu.com/24.04/a.iso") == nil {
		t.Fatal("http origin not matched")
	}
}

// The VM harness downloads the same ISOs a user would, and for a while it
// named the kernel.org mirror directly -- which made it fast and also made it
// a single source, because Alternates only knows the origin. One i/o timeout
// there failed desktop-26.04 twice with five working mirrors untried. The
// harness must name a URL this package can widen, or CI is one flaky host away
// from red for a reason that has nothing to do with the change under test.
func TestVMHarnessDownloadsFromAURLWithMirrors(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "test", "autoinstall", "vm.sh"))
	if err != nil {
		t.Skipf("harness not present: %v", err)
	}
	const key = "MIRROR=${UBUNTU_MIRROR:-"
	i := bytes.Index(b, []byte(key))
	if i < 0 {
		t.Fatalf("vm.sh no longer sets %s -- find where it gets its Ubuntu URL and check that here", key)
	}
	rest := b[i+len(key):]
	root := string(rest[:bytes.IndexByte(rest, '}')])

	// The default must be a root Alternates widens, and widen to more than one
	// place -- a fallback list of length one is not a fallback.
	got := Alternates(root + "/26.04/ubuntu-26.04.1-desktop-amd64.iso")
	if len(got) < 2 {
		t.Fatalf("vm.sh downloads from %s, which yields %d alternates; DSKY will use it as a single source", root, len(got))
	}
}

func testFile(n int) ([]byte, string) {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i/4096)
	}
	s := sha256.Sum256(b)
	return b, hex.EncodeToString(s[:])
}

// server serves data with Range support. chunkDelay throttles it; failAt, if
// positive, drops the connection once that many bytes of the file have gone
// out; hangAt stops sending without closing.
type server struct {
	data       []byte
	chunkDelay time.Duration
	failAt     int
	hangAt     int
	size       int // reported size, if it should differ from len(data)
}

func (s server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := 0
	if rg := r.Header.Get("Range"); strings.HasPrefix(rg, "bytes=") {
		spec := strings.TrimPrefix(rg, "bytes=")
		start, _ = strconv.Atoi(strings.SplitN(spec, "-", 2)[0])
	}
	size := len(s.data)
	if s.size > 0 {
		size = s.size
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(s.data)-start))
	if r.Header.Get("Range") != "" {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, size-1, size))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	fl, _ := w.(http.Flusher)
	for off := start; off < len(s.data); off += 16 << 10 {
		end := min(off+16<<10, len(s.data))
		if s.failAt > 0 && end > s.failAt {
			if hj, ok := w.(http.Hijacker); ok {
				w.Write(s.data[off:s.failAt])
				fl.Flush()
				c, _, _ := hj.Hijack()
				c.Close()
			}
			return
		}
		if s.hangAt > 0 && end > s.hangAt {
			select {
			case <-r.Context().Done():
			case <-time.After(time.Minute):
			}
			return
		}
		if _, err := w.Write(s.data[off:end]); err != nil {
			return
		}
		if fl != nil {
			fl.Flush()
		}
		if s.chunkDelay > 0 {
			time.Sleep(s.chunkDelay)
		}
	}
}

func shorten(t *testing.T) {
	t.Helper()
	pb, pt, st := probeBytes, probeTimeout, stallTimeout
	// The stall limit is ten times Download's 200 ms progress interval. At
	// 600 ms a loaded Windows runner once went that long between reports on a
	// healthy mirror and gave up on it too.
	probeBytes, probeTimeout, stallTimeout = 256<<10, 3*time.Second, 2*time.Second
	ap, rb := attemptsPerSource, retryBackoff
	retryBackoff = []time.Duration{10 * time.Millisecond}
	t.Cleanup(func() {
		probeBytes, probeTimeout, stallTimeout = pb, pt, st
		attemptsPerSource, retryBackoff = ap, rb
	})
}

func run(t *testing.T, urls []string) (sum, used string, got []byte, notes []string, err error) {
	t.Helper()
	dest := filepath.Join(t.TempDir(), "file.iso")
	sum, used, err = DownloadAny(context.Background(), urls, dest, nil, func(s string) { notes = append(notes, s) })
	if err == nil {
		got, _ = os.ReadFile(dest)
	}
	return
}

func TestDownloadAnyPicksTheFastest(t *testing.T) {
	shorten(t)
	data, want := testFile(2 << 20)
	slow := httptest.NewServer(server{data: data, chunkDelay: 40 * time.Millisecond})
	defer slow.Close()
	fast := httptest.NewServer(server{data: data})
	defer fast.Close()

	sum, used, got, notes, err := run(t, []string{slow.URL + "/a.iso", fast.URL + "/a.iso"})
	if err != nil {
		t.Fatal(err)
	}
	if used != fast.URL+"/a.iso" || sum != want || !bytes.Equal(got, data) {
		t.Fatalf("used %s, sum ok=%v, bytes ok=%v", used, sum == want, bytes.Equal(got, data))
	}
	if len(notes) == 0 || !strings.Contains(notes[0], "faster than") {
		t.Errorf("no note saying a mirror was chosen: %v", notes)
	}
}

func TestDownloadAnyIgnoresAMirrorWithAnotherFile(t *testing.T) {
	shorten(t)
	data, want := testFile(1 << 20)
	origin := httptest.NewServer(server{data: data, chunkDelay: 5 * time.Millisecond})
	defer origin.Close()
	other, _ := testFile(1<<20 + 512)
	wrong := httptest.NewServer(server{data: other})
	defer wrong.Close()

	sum, used, _, _, err := run(t, []string{origin.URL + "/a.iso", wrong.URL + "/a.iso"})
	if err != nil || used != origin.URL+"/a.iso" || sum != want {
		t.Fatalf("used %s, err %v, sum ok=%v", used, err, sum == want)
	}
}

func TestDownloadAnyResumesFromAnotherServer(t *testing.T) {
	shorten(t)
	data, want := testFile(3 << 20)
	// The origin is fastest at first and drops the connection a third of the
	// way in; the mirror, slower, carries on from there.
	origin := httptest.NewServer(server{data: data, failAt: 1 << 20})
	defer origin.Close()
	mirror := httptest.NewServer(server{data: data, chunkDelay: 2 * time.Millisecond})
	defer mirror.Close()

	sum, used, got, notes, err := run(t, []string{origin.URL + "/a.iso", mirror.URL + "/a.iso"})
	if err != nil {
		t.Fatal(err)
	}
	if used != mirror.URL+"/a.iso" || sum != want || !bytes.Equal(got, data) {
		t.Fatalf("used %s, sum ok=%v, bytes ok=%v, notes %v", used, sum == want, bytes.Equal(got, data), notes)
	}
}

func TestDownloadAnyLeavesAServerThatStopsSending(t *testing.T) {
	shorten(t)
	data, want := testFile(2 << 20)
	origin := httptest.NewServer(server{data: data, hangAt: 768 << 10})
	defer origin.Close()
	mirror := httptest.NewServer(server{data: data, chunkDelay: 3 * time.Millisecond})
	defer mirror.Close()

	start := time.Now()
	sum, used, _, _, err := run(t, []string{origin.URL + "/a.iso", mirror.URL + "/a.iso"})
	if err != nil || used != mirror.URL+"/a.iso" || sum != want {
		t.Fatalf("used %s, err %v, sum ok=%v", used, err, sum == want)
	}
	if time.Since(start) > 20*time.Second {
		t.Fatalf("took %s to give up on a silent server", time.Since(start))
	}
}

// Every Linux ISO the catalog downloads should have somewhere else to go. The
// Ubuntu entry existed for a year before anyone noticed the others had none,
// and it took a truncated read from dl.fedoraproject.org to show it.
func TestEveryCatalogISORootHasMirrors(t *testing.T) {
	// One real URL per family, as they appear in internal/oscatalog/builtin.go.
	for _, u := range []string{
		"https://releases.ubuntu.com/26.04/ubuntu-26.04.1-live-server-amd64.iso",
		"https://dl.fedoraproject.org/pub/fedora/linux/releases/44/Server/x86_64/iso/Fedora-Server-dvd-x86_64-44-1.7.iso",
		"https://repo.almalinux.org/almalinux/10/isos/x86_64/AlmaLinux-10.2-x86_64-minimal.iso",
		"https://download.rockylinux.org/pub/rocky/10/isos/x86_64/Rocky-10.2-x86_64-minimal.iso",
		"https://cdimage.debian.org/debian-cd/current/amd64/iso-cd/debian-13.7.0-amd64-netinst.iso",
		"https://mirrors.edge.kernel.org/linuxmint/stable/22.1/linuxmint-22.1-cinnamon-64bit.iso",
	} {
		alts := Alternates(u)
		if len(alts) < 2 {
			t.Errorf("%s has %d alternates; one bad minute at its origin fails the build", u, len(alts))
			continue
		}
		// A mirror must carry the same path under a different root, not point
		// back at the origin or repeat itself.
		seen := map[string]bool{u: true}
		for _, a := range alts {
			if seen[a] {
				t.Errorf("%s: duplicate or self-referential alternate %s", u, a)
			}
			seen[a] = true
			if !strings.HasSuffix(a, u[strings.LastIndex(u, "/"):]) {
				t.Errorf("%s: alternate %s does not end in the same file name", u, a)
			}
		}
	}
}

// flaky drops the connection on its first n requests and serves normally after
// that, counting what it was asked for.
type flaky struct {
	data    []byte
	failFor int
	mu      sync.Mutex
	hits    int
}

func (f *flaky) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.hits++
	fail := f.hits <= f.failFor
	f.mu.Unlock()
	if fail {
		// Send some of it, then hang up: a truncated read, which is what
		// dl.fedoraproject.org did at byte 131563792.
		start := 0
		if rg := r.Header.Get("Range"); strings.HasPrefix(rg, "bytes=") {
			start, _ = strconv.Atoi(strings.SplitN(strings.TrimPrefix(rg, "bytes="), "-", 2)[0])
		}
		server{data: f.data, failAt: start + len(f.data)/4}.ServeHTTP(w, r)
		return
	}
	server{data: f.data}.ServeHTTP(w, r)
}

func (f *flaky) count() int { f.mu.Lock(); defer f.mu.Unlock(); return f.hits }

// A server having a moment is not a server that is down. This used to abandon
// it on the first truncated read, and with no mirror behind it -- which was
// every Linux ISO but Ubuntu's -- that ended the build.
func TestDownloadAnyRetriesTheSameServer(t *testing.T) {
	shorten(t)
	data, want := testFile(2 << 20)
	f := &flaky{data: data, failFor: 2}
	srv := httptest.NewServer(f)
	defer srv.Close()

	sum, used, got, notes, err := run(t, []string{srv.URL + "/a.iso"})
	if err != nil {
		t.Fatalf("gave up on a server that works: %v (notes %v)", err, notes)
	}
	if used != srv.URL+"/a.iso" || sum != want || !bytes.Equal(got, data) {
		t.Fatalf("used %s, sum ok=%v, bytes ok=%v", used, sum == want, bytes.Equal(got, data))
	}
	// Each try resumes rather than starting again: the probe is skipped for a
	// single URL, so the hits are the two failures plus the one that finished.
	if f.count() != 3 {
		t.Errorf("%d requests, want 3 (two that broke, one that finished)", f.count())
	}
	if len(notes) == 0 || !strings.Contains(strings.Join(notes, " "), "trying it again") {
		t.Errorf("nothing said about the retry: %v", notes)
	}
}

// A 404 is an answer. Asking again three times is just slower.
func TestDownloadAnyDoesNotRetryAMissingFile(t *testing.T) {
	shorten(t)
	var hits int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		http.NotFound(w, r)
	}))
	defer srv.Close()

	_, _, _, _, err := run(t, []string{srv.URL + "/gone.iso"})
	if err == nil {
		t.Fatal("a 404 succeeded")
	}
	mu.Lock()
	defer mu.Unlock()
	if hits != 1 {
		t.Errorf("%d requests for a 404, want 1", hits)
	}
	// And the message says how many places were tried, because "every source
	// failed" of a one-item list read as "the internet is down".
	if !strings.Contains(err.Error(), "all 1 source(s) failed") {
		t.Errorf("message hides the size of the list: %v", err)
	}
}

// Every source failing used to flatten each cause to a string, so a caller
// could not tell a name that did not resolve from a server that answered and
// refused — and the advice printed on the end of a failed Windows download was
// written for one of those and shown for both. The sentence is unchanged; what
// is new is that errors.As still finds what happened.
func TestWhyEverySourceFailedSurvivesTheMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer srv.Close()

	// A host that cannot resolve, and one that answers and refuses: the two
	// cases the caller has to tell apart.
	_, _, err := DownloadAny(context.Background(),
		[]string{"https://no-such-host.invalid/x.iso", srv.URL + "/x.iso"},
		filepath.Join(t.TempDir(), "out"), nil, nil)
	if err == nil {
		t.Fatal("both sources failed and the download reported success")
	}
	if !strings.Contains(err.Error(), "all 2 source(s) failed") {
		t.Errorf("the sentence changed: %v", err)
	}
	var dns *net.DNSError
	if !errors.As(err, &dns) {
		t.Fatalf("errors.As found no DNS failure in %v", err)
	}
	if dns.Name != "no-such-host.invalid" {
		t.Errorf("the host that did not resolve came back as %q", dns.Name)
	}
}

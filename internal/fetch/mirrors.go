package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/uplinkresearch/dsky/internal/buildinfo"
)

// Mirrors for downloads whose origin is known to be slow from some networks.
//
// releases.ubuntu.com served GitHub's runners at 0.2-1.4 MiB/s, so a 6 GiB
// Ubuntu Desktop ISO took over an hour and a half; the same file came from the
// kernel.org mirror at full speed. These are official Ubuntu release mirrors
// that carry the same paths under a different root. Using one is only ever
// safe because the file is pinned: the library checks the SHA-256 of what
// arrived, whichever server sent it, so a mirror that serves a different file
// fails the pin rather than being trusted.
// Ubuntu was the only entry here for a while, which meant every other Linux
// ISO in the catalog had exactly one place to come from. A partial read from
// dl.fedoraproject.org at byte 131563792 then failed a Fedora build outright,
// reporting "every source failed" of a list with one source in it. The same
// download from any of three mirrors would have carried on from that byte.
var mirrorRoots = map[string][]string{
	"https://releases.ubuntu.com/": {
		"https://mirrors.edge.kernel.org/ubuntu-releases/",
		"https://mirror.csclub.uwaterloo.ca/ubuntu-releases/",
		"https://mirror.us.leaseweb.net/ubuntu-releases/",
		"https://ftp.halifax.rwth-aachen.de/ubuntu-releases/",
		"https://mirror.aarnet.edu.au/pub/ubuntu/releases/",
	},
	"https://dl.fedoraproject.org/pub/fedora/linux/": {
		"https://mirrors.kernel.org/fedora/",
		"https://ftp.lysator.liu.se/pub/fedora/linux/",
		"https://mirror.math.princeton.edu/pub/fedora/linux/",
	},
	"https://repo.almalinux.org/almalinux/": {
		"https://mirrors.kernel.org/almalinux/",
		"https://mirror.rackspace.com/almalinux/",
	},
	// download.rockylinux.org is a redirector; dl.rockylinux.org is the
	// server it hands out, and is listed here as an ordinary mirror.
	"https://download.rockylinux.org/pub/rocky/": {
		"https://dl.rockylinux.org/pub/rocky/",
		"https://mirror.rackspace.com/rocky/",
		"https://ftp.lysator.liu.se/pub/rocky/",
	},
}

// Alternates returns the mirror URLs for u, or nil if none are known.
func Alternates(u string) []string {
	for _, scheme := range []string{"https://", "http://"} {
		for root, mirrors := range mirrorRoots {
			origin := scheme + strings.TrimPrefix(root, "https://")
			if !strings.HasPrefix(u, origin) {
				continue
			}
			rest := strings.TrimPrefix(u, origin)
			out := make([]string, 0, len(mirrors))
			for _, m := range mirrors {
				out = append(out, m+rest)
			}
			return out
		}
	}
	return nil
}

// Probe and stall limits. Variables so tests can shorten them.
var (
	probeBytes   int64 = 4 << 20
	probeTimeout       = 8 * time.Second
	stallTimeout       = 60 * time.Second
)

// DownloadAny fetches the same file from the fastest of urls, falling back to
// the others if one fails or stalls part way. The first URL is the origin.
//
// Every URL must serve byte-for-byte the same file, and the caller must verify
// the returned SHA-256 against a pin: a partial download from one server is
// resumed from another.
//
// note, if not nil, hears which server is being used and why it changed.
func DownloadAny(ctx context.Context, urls []string, dest string, progress Progress, note func(string)) (sum, used string, err error) {
	if len(urls) == 0 {
		return "", "", errors.New("fetch: no URL to download from")
	}
	if note == nil {
		note = func(string) {}
	}
	order := urls
	if len(urls) > 1 {
		order = rankBySpeed(ctx, urls, note)
	}
	var errs []string
	for i, u := range order {
		if i > 0 {
			note("continuing from " + host(u))
		}
		sum, err := downloadWatched(ctx, u, dest, progress)
		if err == nil {
			return sum, u, nil
		}
		if ctx.Err() != nil {
			return "", "", ctx.Err()
		}
		errs = append(errs, host(u)+": "+err.Error())
	}
	return "", "", fmt.Errorf("fetch: every source failed: %s", strings.Join(errs, "; "))
}

// downloadWatched is Download that gives up on a server that stops sending, so
// the next one can carry on from the same .part file.
func downloadWatched(ctx context.Context, u, dest string, progress Progress) (string, error) {
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var mu sync.Mutex
	last := time.Now()
	stalled := false
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		t := time.NewTicker(stallTimeout / 6)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				mu.Lock()
				if time.Since(last) > stallTimeout {
					stalled = true
					mu.Unlock()
					cancel()
					return
				}
				mu.Unlock()
			}
		}
	}()
	sum, err := Download(cctx, u, dest, func(done, total int64) {
		mu.Lock()
		last = time.Now()
		mu.Unlock()
		if progress != nil {
			progress(done, total)
		}
	})
	mu.Lock()
	defer mu.Unlock()
	if err != nil && stalled && ctx.Err() == nil {
		return "", fmt.Errorf("no data for %s", stallTimeout)
	}
	return sum, err
}

type probeResult struct {
	url   string
	speed float64 // bytes per second
	total int64   // file size, -1 if unknown
	err   error
}

// rankBySpeed reads the first few MiB from every server at once and orders
// them fastest first. A server that fails, or reports a different file size
// from the origin, is dropped: the size is a cheap check that a mirror has the
// same release before gigabytes are spent finding out from the hash.
func rankBySpeed(ctx context.Context, urls []string, note func(string)) []string {
	results := make([]probeResult, len(urls))
	var wg sync.WaitGroup
	for i, u := range urls {
		wg.Add(1)
		go func(i int, u string) {
			defer wg.Done()
			results[i] = probe(ctx, u)
		}(i, u)
	}
	wg.Wait()

	want := int64(-1)
	if results[0].err == nil {
		want = results[0].total
	}
	var ok []probeResult
	for _, r := range results {
		if r.err != nil {
			continue
		}
		if want > 0 && r.total > 0 && r.total != want {
			continue
		}
		ok = append(ok, r)
	}
	if len(ok) == 0 {
		return urls // nothing answered the probe; try them in order anyway
	}
	sort.SliceStable(ok, func(i, j int) bool { return ok[i].speed > ok[j].speed })
	order := make([]string, 0, len(urls))
	seen := map[string]bool{}
	for _, r := range ok {
		order = append(order, r.url)
		seen[r.url] = true
	}
	// Servers that failed the probe stay as last resorts, except a mirror whose
	// size disagreed, which is serving something else.
	for _, r := range results {
		if !seen[r.url] && r.err != nil {
			order = append(order, r.url)
		}
	}
	if order[0] != urls[0] {
		note(fmt.Sprintf("downloading from %s (%s, faster than %s)", host(order[0]), rate(ok[0].speed), host(urls[0])))
	}
	return order
}

func probe(ctx context.Context, u string) probeResult {
	r := probeResult{url: u, total: -1}
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(pctx, http.MethodGet, u, nil)
	if err != nil {
		r.err = err
		return r
	}
	req.Header.Set("User-Agent", buildinfo.UserAgent())
	req.Header.Set("Range", "bytes=0-"+strconv.FormatInt(probeBytes-1, 10))
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		r.err = err
		return r
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusPartialContent:
		// Content-Range: bytes 0-4194303/6482409472
		if cr := resp.Header.Get("Content-Range"); cr != "" {
			if i := strings.LastIndex(cr, "/"); i >= 0 {
				if n, err := strconv.ParseInt(cr[i+1:], 10, 64); err == nil {
					r.total = n
				}
			}
		}
	case http.StatusOK:
		r.total = resp.ContentLength
	default:
		r.err = fmt.Errorf("%s", resp.Status)
		return r
	}
	n, _ := io.Copy(io.Discard, io.LimitReader(resp.Body, probeBytes))
	elapsed := time.Since(start).Seconds()
	if n == 0 {
		r.err = errors.New("sent nothing")
		return r
	}
	r.speed = float64(n) / elapsed
	return r
}

func host(u string) string {
	if p, err := url.Parse(u); err == nil && p.Host != "" {
		return p.Host
	}
	return u
}

func rate(bps float64) string {
	if bps >= 1<<20 {
		return fmt.Sprintf("%.1f MiB/s", bps/(1<<20))
	}
	return fmt.Sprintf("%.0f KiB/s", bps/(1<<10))
}

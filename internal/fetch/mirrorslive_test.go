package fetch

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// One real URL per mirrored root, as it appears in internal/oscatalog.
var liveISOs = []string{
	"https://releases.ubuntu.com/26.04/ubuntu-26.04.1-live-server-amd64.iso",
	"https://dl.fedoraproject.org/pub/fedora/linux/releases/44/Server/x86_64/iso/Fedora-Server-dvd-x86_64-44-1.7.iso",
	"https://repo.almalinux.org/almalinux/10/isos/x86_64/AlmaLinux-10.2-x86_64-minimal.iso",
	"https://download.rockylinux.org/pub/rocky/10/isos/x86_64/Rocky-10.2-x86_64-minimal.iso",
	"https://cdimage.debian.org/debian-cd/current/amd64/iso-cd/debian-13.7.0-amd64-netinst.iso",
	"https://mirrors.edge.kernel.org/linuxmint/stable/22.1/linuxmint-22.1-cinnamon-64bit.iso",
}

// sizeOf asks for the first byte and reads the total out of Content-Range,
// which costs nothing next to a 4 GiB ISO.
func sizeOf(ctx context.Context, u string) (int64, error) {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Range", "bytes=0-0")
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		return 0, errStatus(resp.Status)
	}
	cr := resp.Header.Get("Content-Range")
	i := strings.LastIndex(cr, "/")
	if i < 0 {
		return 0, errStatus("no Content-Range: " + cr)
	}
	return strconv.ParseInt(cr[i+1:], 10, 64)
}

type errStatus string

func (e errStatus) Error() string { return string(e) }

// TestMirrorsLive checks that each catalog ISO still has somewhere else to come
// from. A mirror that has dropped a release, or serves a different file under
// the same name, is worth hearing about from the scheduled run rather than
// from a build that failed at 3 GiB.
//
// The bar is "the fallback still exists", not "every mirror is up": one mirror
// down is normal and must not cry wolf, since DownloadAny simply moves on. It
// fails when a root is down to a single working source, which is the state
// that turned one truncated read into a failed Fedora build.
func TestMirrorsLive(t *testing.T) {
	if testing.Short() {
		t.Skip("live")
	}
	ctx := context.Background()
	for _, origin := range liveISOs {
		alts := Alternates(origin)
		if len(alts) == 0 {
			t.Errorf("%s: no mirrors at all", origin)
			continue
		}
		urls := append([]string{origin}, alts...)
		sizes := make([]int64, len(urls))
		errs := make([]error, len(urls))
		var wg sync.WaitGroup
		for i, u := range urls {
			wg.Add(1)
			go func(i int, u string) {
				defer wg.Done()
				sizes[i], errs[i] = sizeOf(ctx, u)
			}(i, u)
		}
		wg.Wait()

		want := sizes[0]
		if errs[0] != nil {
			// The origin being down is not itself a failure — that is what the
			// mirrors are for — so take the most common size as the truth.
			counts := map[int64]int{}
			for i := range urls {
				if errs[i] == nil {
					counts[sizes[i]]++
				}
			}
			for s, n := range counts {
				if n > counts[want] {
					want = s
				}
			}
		}
		good := 0
		for i, u := range urls {
			switch {
			case errs[i] != nil:
				t.Logf("%s: %v", host(u), errs[i])
			case want > 0 && sizes[i] != want:
				t.Errorf("%s serves %d bytes, not %d — a different file under the same name", host(u), sizes[i], want)
			default:
				good++
			}
		}
		if good < 2 {
			t.Errorf("%s: %d of %d sources working — no fallback left", host(origin), good, len(urls))
		}
	}
}

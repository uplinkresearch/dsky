package library

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uplinkresearch/dsky/internal/manifest"
)

// Pull's note is how anything downstream learns that the bytes changed server,
// or that one was tried again. It was nil at every call site, so DSKY switched
// mirrors and retried in silence and the screen said "downloading" throughout.
// A progress bar that stops for seven seconds and explains nothing is how a
// working program looks broken.
func TestPullNoteReachesTheCaller(t *testing.T) {
	body := make([]byte, 512<<10)
	for i := range body {
		body[i] = byte(i)
	}
	sum := sha256.Sum256(body)

	// Truncates its first response, then serves properly: fetch retries the
	// same server rather than giving up, and should say so.
	var mu sync.Mutex
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		first := hits == 1
		mu.Unlock()
		if first {
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			w.WriteHeader(http.StatusOK)
			w.Write(body[:len(body)/4])
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			if hj, ok := w.(http.Hijacker); ok {
				c, _, _ := hj.Hijack()
				c.Close()
			}
			return
		}
		http.ServeContent(w, r, "a.bin", time.Time{}, bytes.NewReader(body))
	}))
	defer srv.Close()

	l, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	src := &manifest.Source{
		ID:       "thing",
		Kind:     manifest.KindOSImage,
		Format:   manifest.FormatISO,
		URL:      srv.URL + "/a.bin",
		SHA256:   hex.EncodeToString(sum[:]),
		Filename: "a.bin",
	}

	var notes []string
	if _, err := l.Pull(context.Background(), src, false, nil, nil, func(s string) {
		notes = append(notes, s)
	}); err != nil {
		t.Fatalf("pull: %v (notes %v)", err, notes)
	}
	if len(notes) == 0 {
		t.Fatal("the retry was never mentioned to the caller")
	}
	if !strings.Contains(strings.Join(notes, " "), "trying it again") {
		t.Errorf("notes say nothing about a retry: %v", notes)
	}
}

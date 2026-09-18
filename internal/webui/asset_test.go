package webui

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// get builds a request the host guard will accept: the portal only answers
// loopback, test or not.
func get(path, ifNoneMatch string) *http.Request {
	r := httptest.NewRequest("GET", path, nil)
	r.Host = "127.0.0.1:8931"
	if ifNoneMatch != "" {
		r.Header.Set("If-None-Match", ifNoneMatch)
	}
	return r
}

// An updated build must not keep drawing the previous build's logo.
//
// The page, the logo and the icon are served from URLs that never change, so a
// lifetime alone tells the browser nothing: v0.8.0 replaced the logo and people
// went on seeing the old one, because the copy already in the cache had a day
// left to run and no reason to be checked. They carry a tag of their own
// contents now, which is the thing that differs between builds.
func TestAssetsAreCheckedRatherThanTrusted(t *testing.T) {
	for _, path := range []string{"/", "/logo.webp", "/favicon.png"} {
		t.Run(path, func(t *testing.T) {
			h := (&Server{}).handler()

			first := httptest.NewRecorder()
			h.ServeHTTP(first, get(path, ""))
			if first.Code != http.StatusOK {
				t.Fatalf("got %d, want 200", first.Code)
			}
			etag := first.Header().Get("ETag")
			if etag == "" {
				t.Fatal("no ETag: nothing for the browser to check the copy it has against")
			}
			if cc := first.Header().Get("Cache-Control"); cc != "no-cache" {
				t.Errorf("Cache-Control is %q, want no-cache — a lifetime without a tag is what left the old logo on screen", cc)
			}
			if first.Body.Len() == 0 {
				t.Error("served nothing")
			}

			// Unchanged: answered without sending the bytes again.
			second := httptest.NewRecorder()
			h.ServeHTTP(second, get(path, etag))
			if second.Code != http.StatusNotModified {
				t.Errorf("got %d for a copy the browser already has, want 304", second.Code)
			}

			// Changed: a stale tag has to be answered with the file itself.
			third := httptest.NewRecorder()
			h.ServeHTTP(third, get(path, `"0000000000000000"`))
			if third.Code != http.StatusOK || third.Body.Len() == 0 {
				t.Errorf("got %d with %d bytes for a stale tag, want 200 and the file",
					third.Code, third.Body.Len())
			}
		})
	}
}

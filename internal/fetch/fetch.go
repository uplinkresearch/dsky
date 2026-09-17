// Package fetch downloads pinned sources: HTTP with resume, SHA-256 computed
// on the fly, and progress reporting.
package fetch

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/uplinkresearch/dsky/internal/buildinfo"
)

// Progress receives byte counts as a download advances. total is -1 when the
// server does not report a length.
type Progress func(done, total int64)

var client = &http.Client{
	// No overall timeout: multi-GB ISOs on slow links are legitimate.
	// Dial/TLS phases get bounded by the transport defaults.
	Timeout: 0,
}

// StatusError is a server answering with something other than the file.
type StatusError struct {
	URL    string
	Code   int
	Status string
}

func (e *StatusError) Error() string { return fmt.Sprintf("fetch: GET %s: %s", e.URL, e.Status) }

// Download fetches url into dest, resuming a dest+".part" file when the
// server supports byte ranges, and returns the SHA-256 of the complete file.
// The caller verifies the hash against the pin and deletes dest on mismatch.
func Download(ctx context.Context, url, dest string, progress Progress) (string, error) {
	part := dest + ".part"

	var offset int64
	if st, err := os.Stat(part); err == nil {
		offset = st.Size()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", buildinfo.UserAgent())
	if offset > 0 {
		req.Header.Set("Range", "bytes="+strconv.FormatInt(offset, 10)+"-")
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var f *os.File
	total := int64(-1)
	switch resp.StatusCode {
	case http.StatusPartialContent:
		// O_CREATE: a server may answer 206 to a request with no Range at all,
		// when there is no partial file yet.
		f, err = os.OpenFile(part, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return "", err
		}
		if resp.ContentLength >= 0 {
			total = offset + resp.ContentLength
		}
	case http.StatusOK:
		offset = 0 // server ignored the range; start over
		f, err = os.Create(part)
		if err != nil {
			return "", err
		}
		total = resp.ContentLength
	default:
		return "", &StatusError{URL: url, Code: resp.StatusCode, Status: resp.Status}
	}

	done := offset
	lastReport := time.Now()
	buf := make([]byte, 1<<20)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				f.Close()
				return "", werr
			}
			done += int64(n)
			if progress != nil && (time.Since(lastReport) > 200*time.Millisecond || rerr == io.EOF) {
				progress(done, total)
				lastReport = time.Now()
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			f.Close()
			return "", fmt.Errorf("fetch: reading %s at byte %d: %w (re-run to resume)", url, done, rerr)
		}
	}
	if err := f.Close(); err != nil {
		return "", err
	}

	sum, err := SHA256File(part)
	if err != nil {
		return "", err
	}
	if err := os.Remove(dest); err != nil && !os.IsNotExist(err) {
		return "", err
	}
	if err := os.Rename(part, dest); err != nil {
		return "", err
	}
	return sum, nil
}

// SHA1File hashes a file on disk with SHA-1 (vendor catalogs still publish
// it; never the only pin).
func SHA1File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha1.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// SHA256File hashes a file on disk.
func SHA256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

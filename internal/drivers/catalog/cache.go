package catalog

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/uplinkresearch/dsky/internal/fetch"
	"github.com/uplinkresearch/dsky/internal/helpers"
)

// Cache keeps vendor catalogs on disk (they are hundreds of KB to a few MB
// and change weekly), refreshed after TTL.
type Cache struct {
	Dir        string // e.g. <library>/helpers/catalogs
	HelpersDir string // for cab expansion on non-Windows hosts
	TTL        time.Duration
}

// NewCache returns a cache under helpersDir with a 24h TTL.
func NewCache(helpersDir string) *Cache {
	return &Cache{Dir: filepath.Join(helpersDir, "catalogs"), HelpersDir: helpersDir, TTL: 24 * time.Hour}
}

// file fetches url into the cache as name unless a fresh copy exists.
func (c *Cache) file(ctx context.Context, name, url string) (string, error) {
	if err := os.MkdirAll(c.Dir, 0o755); err != nil {
		return "", err
	}
	p := filepath.Join(c.Dir, name)
	if st, err := os.Stat(p); err == nil && time.Since(st.ModTime()) < c.TTL && st.Size() > 0 {
		return p, nil
	}
	if _, err := fetch.Download(ctx, url, p, nil); err != nil {
		if st, serr := os.Stat(p); serr == nil && st.Size() > 0 {
			return p, nil // offline: use the stale copy
		}
		return "", fmt.Errorf("fetching %s: %w", name, err)
	}
	return p, nil
}

// xmlFromCab fetches a .cab catalog, extracts it into a per-cab subdir, and
// returns the XML inside — located by content, since expand.exe and 7-Zip
// don't agree on the extracted filename's case or extension.
func (c *Cache) xmlFromCab(ctx context.Context, cabName, url, xmlName string) (string, error) {
	cab, err := c.file(ctx, cabName, url)
	if err != nil {
		return "", err
	}
	subdir := filepath.Join(c.Dir, strings.TrimSuffix(cabName, filepath.Ext(cabName))+"-x")
	cabSt, _ := os.Stat(cab)
	if found := findXML(subdir); found != "" {
		if st, err := os.Stat(found); err == nil && cabSt != nil && !st.ModTime().Before(cabSt.ModTime()) {
			return found, nil
		}
	}
	if err := os.RemoveAll(subdir); err != nil {
		return "", err
	}
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		return "", err
	}
	if err := helpers.ExpandCab(ctx, c.HelpersDir, cab, subdir); err != nil {
		return "", err
	}
	if found := findXML(subdir); found != "" {
		now := time.Now()
		_ = os.Chtimes(found, now, now)
		return found, nil
	}
	return "", fmt.Errorf("%s did not expand to a catalog XML (looked in %s)", cabName, subdir)
}

// findXML returns the largest file under dir whose content begins with an
// XML declaration or element — robust to whatever name the extractor used.
func findXML(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var best string
	var bestSize int64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		p := filepath.Join(dir, e.Name())
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		head := make([]byte, 64)
		n, _ := f.Read(head)
		f.Close()
		if !looksXML(head[:n]) {
			continue
		}
		if info, err := e.Info(); err == nil && info.Size() > bestSize {
			best, bestSize = p, info.Size()
		}
	}
	return best
}

func looksXML(b []byte) bool {
	// Dell's consumer catalogs are UTF-16, which starts with a byte-order
	// mark and then has a zero after every ASCII character.
	if len(b) >= 4 && (b[0] == 0xFF && b[1] == 0xFE && b[2] == '<' || b[0] == 0xFE && b[1] == 0xFF && b[3] == '<') {
		return true
	}
	s := strings.TrimSpace(strings.TrimPrefix(string(b), "\ufeff"))
	return strings.HasPrefix(s, "<?xml") || strings.HasPrefix(s, "<")
}

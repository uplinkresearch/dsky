package oscatalog

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFindDownloadedISO(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	dl := filepath.Join(home, "Downloads")
	if err := os.MkdirAll(dl, 0o755); err != nil {
		t.Fatal(err)
	}
	mk := func(name string, size int64, age time.Duration) string {
		p := filepath.Join(dl, name)
		f, err := os.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Truncate(size); err != nil {
			t.Fatal(err)
		}
		f.Close()
		when := time.Now().Add(-age)
		os.Chtimes(p, when, when)
		return p
	}
	win11, _ := Get("windows-11")
	if got := FindDownloadedISO(win11); got != "" {
		t.Fatalf("empty Downloads found %q", got)
	}
	mk("Win11_24H2_English_x64.iso", 2<<30, 48*time.Hour)
	newer := mk("Win11_25H2_English_x64.iso", 2<<30, time.Hour)
	mk("Win11_small.iso", 1<<20, 0)                       // too small to be real
	partial := mk("Win11_26H1_English_x64.iso", 2<<30, 0) // still downloading
	os.WriteFile(partial+".part", nil, 0o644)
	mk("ubuntu-26.04.1-desktop-amd64.iso", 2<<30, 0)
	if got := FindDownloadedISO(win11); got != newer {
		t.Fatalf("found %q, want the newest complete Windows 11 ISO %q", got, newer)
	}
	// Saved to the Desktop instead, and newer: found there too.
	desk := filepath.Join(home, "Desktop")
	os.MkdirAll(desk, 0o755)
	onDesk := filepath.Join(desk, "Win11_25H2_English_x64v2.iso")
	if f, err := os.Create(onDesk); err == nil {
		f.Truncate(2 << 30)
		f.Close()
	}
	if got := FindDownloadedISO(win11); got != onDesk {
		t.Fatalf("found %q, want the newer one on the Desktop %q", got, onDesk)
	}
	ubuntu, _ := Get("ubuntu-26.04-desktop")
	if got := FindDownloadedISO(ubuntu); got != "" {
		t.Fatalf("a hash-pinned distro used an unverified file: %q", got)
	}
}

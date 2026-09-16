package appcatalog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An installer is checked for what it is, not what it is called. A
// ScreenConnect client saved as .msi reached a real machine and came back from
// msiexec as 1620, "this installation package could not be opened", after
// being staged onto a stick and carried to a bench.
func TestAnInstallerIsCheckedForWhatItIs(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, head []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, append(head, make([]byte, 64)...), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	ole := []byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1}

	if got, err := FormatForFile(write("real.msi", ole)); err != nil || got != "msi" {
		t.Errorf("a real MSI: %q %v", got, err)
	}
	if got, err := FormatForFile(write("real.exe", []byte("MZ\x90\x00"))); err != nil || got != "exe" {
		t.Errorf("a real program: %q %v", got, err)
	}

	// The failure that cost a bench visit: a program named .msi.
	_, err := FormatForFile(write("ScreenConnect.ClientSetup.msi", []byte("MZ\x90\x00")))
	if err == nil {
		t.Fatal("a Windows program named .msi was accepted as an installer package")
	}
	for _, want := range []string{"1620", "Windows program", "ScreenConnect.ClientSetup.msi"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}

	// A download that fetched an error page instead of a file.
	_, err = FormatForFile(write("client.msi", []byte("<!DOCTYPE html>")))
	if err == nil || !strings.Contains(err.Error(), "web page") {
		t.Errorf("an HTML error page named .msi: %v", err)
	}

	// A zip somebody renamed.
	_, err = FormatForFile(write("tool.exe", []byte("PK\x03\x04")))
	if err == nil {
		t.Error("a zip named .exe was accepted")
	}

	// Still refused on the name alone, before anything is read.
	if _, err := FormatForFile(filepath.Join(dir, "notes.txt")); err == nil {
		t.Error("a .txt was accepted")
	}
	// A file that cannot be read is somebody else's error: the extension is
	// all this promised, and refusing here would block adding an installer
	// from a path only the build will see.
	if got, err := FormatForFile(filepath.Join(dir, "missing.msi")); err != nil || got != "msi" {
		t.Errorf("an unreadable path: %q %v", got, err)
	}
}

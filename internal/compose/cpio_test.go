package compose

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A malformed cpio is read as truncated and dropped in silence: the installer
// then comes up asking questions, with nothing anywhere saying why. So the
// archive is checked by a real cpio, and the padding and trailer it needs are
// checked by hand for the machines that have no cpio to check with.
func TestCpioNewcFile(t *testing.T) {
	const body = "# kickstart\ntext\nreboot\n"
	out, err := cpioNewcFile("ks.cfg", []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(out), "070701") {
		t.Errorf("not a newc archive: %q", out[:min(len(out), 16)])
	}
	if !strings.Contains(string(out), "TRAILER!!!") {
		t.Error("no trailer entry; the kernel drops an archive without one")
	}
	if len(out)%4 != 0 {
		t.Errorf("archive is %d bytes, not padded to 4", len(out))
	}

	cpio, err := exec.LookPath("cpio")
	if err != nil {
		t.Skip("no cpio to check against")
	}
	dir := t.TempDir()
	cmd := exec.Command(cpio, "-idm")
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(string(out))
	if msg, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cpio refused the archive: %v\n%s", err, msg)
	}
	got, err := os.ReadFile(filepath.Join(dir, "ks.cfg"))
	if err != nil {
		t.Fatalf("ks.cfg not extracted: %v", err)
	}
	if string(got) != body {
		t.Errorf("extracted %q, want %q", got, body)
	}
}

package diskutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The partition Erase and prepare makes on Linux is of a type Windows mounts.
// Checked against sfdisk itself, on a file, so it is what sfdisk actually
// writes and not what the script was believed to mean.
func TestAPreparedPartitionIsOneWindowsMounts(t *testing.T) {
	if _, err := exec.LookPath("sfdisk"); err != nil {
		t.Skip("sfdisk is not installed")
	}
	for _, c := range []struct {
		scheme Scheme
		fs     FS
		want   string
	}{
		{GPT, ExFAT, "type=EBD0A0A2-B9E5-4433-87C0-68B6B72699C7"},
		{GPT, FAT32, "type=EBD0A0A2-B9E5-4433-87C0-68B6B72699C7"},
		{GPT, NTFS, "type=EBD0A0A2-B9E5-4433-87C0-68B6B72699C7"},
		{MBR, ExFAT, "type=7"},
		{MBR, NTFS, "type=7"},
		{MBR, FAT32, "type=c"},
	} {
		t.Run(string(c.scheme)+"-"+string(c.fs), func(t *testing.T) {
			img := filepath.Join(t.TempDir(), "disk.img")
			if err := os.WriteFile(img, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Truncate(img, 64<<20); err != nil {
				t.Fatal(err)
			}
			_, script := sfdiskScript(Options{Scheme: c.scheme, FS: c.fs})
			cmd := exec.Command("sfdisk", img)
			cmd.Stdin = strings.NewReader(script)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("sfdisk: %v\n%s", err, out)
			}
			out, err := exec.Command("sfdisk", "-d", img).Output()
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(out), c.want) {
				t.Errorf("the partition is not %s:\n%s", c.want, out)
			}
		})
	}
}

package diskutil

import (
	"strings"
	"testing"
)

// A disk already in the style asked for is not converted -- diskpart refuses,
// and the refusal failed Erase and prepare on a real GPT stick. Any other style
// is converted, and an unreadable style converts without stopping the script.
func TestDiskpartConvertsOnlyWhatNeedsConverting(t *testing.T) {
	for _, c := range []struct {
		name, scheme, style string
		known               bool
		want                string // the convert line, or "" for none
	}{
		{"already GPT", "gpt", "gpt", true, ""},
		{"already MBR", "mbr", "mbr", true, ""},
		{"MBR to GPT", "gpt", "mbr", true, "convert gpt"},
		{"GPT to MBR", "mbr", "gpt", true, "convert mbr"},
		{"no style yet", "gpt", "", true, "convert gpt"},
		{"style unreadable", "gpt", "", false, "convert gpt noerr"},
	} {
		t.Run(c.name, func(t *testing.T) {
			script := partitionScript(3, c.scheme, c.style, c.known)
			var got string
			for _, line := range strings.Split(script, "\r\n") {
				if strings.HasPrefix(line, "convert") {
					got = line
				}
			}
			if got != c.want {
				t.Errorf("convert line %q, want %q\n%s", got, c.want, script)
			}
			i := strings.Index
			if !(i(script, "select disk 3") < i(script, "clean") && i(script, "clean") < i(script, "create partition primary")) {
				t.Errorf("steps out of order:\n%s", script)
			}
			if strings.Contains(script, "format") {
				t.Errorf("the partition run formats; the volume is not there yet:\n%s", script)
			}
		})
	}
}

// The format run selects the partition before formatting, which is what made
// format find the volume on Windows 11.
func TestTheFormatRunSelectsThePartition(t *testing.T) {
	script := formatScript(3, "exfat", "DSKY")
	want := "select disk 3\r\nselect partition 1\r\nformat fs=exfat quick label=\"DSKY\"\r\nassign\r\nexit\r\n"
	if script != want {
		t.Errorf("format script:\n%q\nwant\n%q", script, want)
	}
}

package jobs

import (
	"errors"
	"testing"
)

// Every event a disk-bound job publishes has to name the disk, not just the
// first one. The page clears a finished run when its disk is unplugged, and it
// learns which disk that was from whichever event happened to arrive -- a
// progress update on reconnect as readily as the terminal one. An event that
// dropped the field would leave that run on screen for a stick nobody can find.
func TestEveryEventNamesTheDisk(t *testing.T) {
	r := NewRegistry()
	events, cancel := r.Subscribe()
	defer cancel()

	j := r.NewOn("flash", "Ubuntu → /dev/sdb", "/dev/sdb")
	j.Progress("writing", 1, 2)
	j.Finish("installed")

	k := r.NewOn("flash", "Fedora → /dev/sdc", "/dev/sdc")
	k.Fail(errors.New("write failed"))

	want := map[string]string{"Ubuntu → /dev/sdb": "/dev/sdb", "Fedora → /dev/sdc": "/dev/sdc"}
	seen := 0
	for seen < 5 {
		ev := <-events
		seen++
		if got := ev.Device; got != want[ev.Title] {
			t.Errorf("%s (%s) carried device %q, want %q", ev.Title, ev.Stage, got, want[ev.Title])
		}
	}
}

// A job that touches no disk says so with an empty field rather than borrowing
// one, or every download would clear itself the next time a stick came out.
func TestWorkWithoutADiskNamesNone(t *testing.T) {
	r := NewRegistry()
	events, cancel := r.Subscribe()
	defer cancel()

	j := r.New("download", "download Ubuntu")
	j.Finish("downloaded")

	for i := 0; i < 2; i++ {
		if ev := <-events; ev.Device != "" {
			t.Errorf("a download named disk %q", ev.Device)
		}
	}
}

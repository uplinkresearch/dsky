package recipe

import (
	"fmt"
	"strings"
	"testing"
)

// Joining a batch of computers by serial number is withdrawn, and a recipe
// that still asks for it is refused by name rather than ignored.
//
// It joined each computer during Windows Setup, and a PC that joins a domain
// during Setup does not finish setting itself up: Windows will not sign a
// local account in automatically on a machine that has just joined a domain,
// so the first boot never runs, and Windows resets itself back into setup
// waiting for somebody to type. A single offline join was fixed by joining
// afterwards instead; this path has not been, and nothing has run it since.
//
// Refused rather than dropped, because a recipe that quietly lost its domain
// join would erase a batch of disks and hand back workgroup machines that look
// perfectly finished -- which is the failure this file exists to prevent.
func TestABatchJoinBySerialNumberIsRefused(t *testing.T) {
	fail := func(f string, a ...any) error { return fmt.Errorf(f, a...) }
	d := &DomainSpec{BlobsBySerial: "join-files"}
	err := d.validate(fail)
	if err == nil {
		t.Fatal("a withdrawn batch join was accepted")
	}
	for _, want := range []string{"blobs_by_serial", "withdrawn", "does not finish", "windows.domain.blob"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal never mentions %q:\n%v", want, err)
		}
	}
	// It still counts as a join, so it reaches validate at all rather than
	// looking like a recipe that never wanted one.
	if !d.Enabled() {
		t.Error("a recipe with a batch join reads as having no domain join")
	}
	// And the paths that remain are unaffected.
	if err := (&DomainSpec{Blob: "pc.txt"}).validate(fail); err != nil {
		t.Errorf("an offline join was refused: %v", err)
	}
}

package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/uplinkresearch/dsky/internal/compose"
	"github.com/uplinkresearch/dsky/internal/device"
)

// A payload is a zip to be run on a machine, not a disk image. Raw-written to
// a stick it would wipe the stick and boot nothing, so flashing one is
// refused before any confirmation is offered -- and the refusal says what to
// do with a payload instead.
func TestAPayloadIsNeverFlashed(t *testing.T) {
	art := &compose.Artifact{RecipeID: "office-apps", Kind: "payload", Path: "/x/office-apps-payload-abc.zip"}
	err := armAndFlashMany(context.Background(), art, []device.Device{{ID: "/dev/sdz"}}, true)
	if err == nil {
		t.Fatal("a payload was offered to the flash path")
	}
	for _, want := range []string{"payload", "not bootable", "README.txt"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}
}

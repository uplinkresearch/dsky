package agent

import "testing"

// The wordmark is flattened onto the window's background before GDI ever sees
// it, so the colour it is flattened onto has to be the colour the window is
// actually painted. GDI stores a COLORREF as 0x00BBGGRR -- the bytes in the
// opposite order to the way the constant reads -- and getting that backwards
// gives a mark that looks right in isolation and sits in a rectangle of the
// wrong dark on the machine.
//
// Which is the worst place for it: nobody on this side of the build ever sees
// that window. It is drawn during a first boot, on a customer's machine.
func TestTheLogoIsFlattenedOntoTheColourTheWindowIsPainted(t *testing.T) {
	got := bgColour()
	want := struct{ r, g, b uint8 }{0x19, 0x1e, 0x20} // #191e20
	if got.R != want.r || got.G != want.g || got.B != want.b {
		t.Errorf("background is #%02x%02x%02x, want #%02x%02x%02x — colBG read in the wrong byte order",
			got.R, got.G, got.B, want.r, want.g, want.b)
	}
	if got.A != 0xff {
		t.Errorf("background alpha is %d, want 255: a transparent ground leaves the mark composited onto nothing", got.A)
	}
}

package cli

import (
	"io"
	"strings"
	"testing"

	"github.com/uplinkresearch/dsky/internal/oscatalog"
)

// Windows has had the whole mechanism since the migration work -- a local
// administrator, an automatic sign-in, and a password kept out of the recipe
// -- reachable only from `dsky migrate build`. Quick Install now reaches it
// too, and says the one thing that is different about it.
func TestAdminPasswordFromStdin(t *testing.T) {
	win, ok := oscatalog.Get("windows-11")
	if !ok {
		t.Skip("no windows-11 in the catalog")
	}
	ubu, ok := oscatalog.Get("ubuntu-26.04-server")
	if !ok {
		t.Skip("no ubuntu-26.04-server in the catalog")
	}

	// Windows: taken, and the stick's plain text said out loud.
	var out strings.Builder
	got, err := readAdminPassword(strings.NewReader("hunter2\n"), &out, true, win)
	if err != nil || got != "hunter2" {
		t.Fatalf("windows: %q %v", got, err)
	}
	said := out.String()
	for _, want := range []string{"clear text", "reads the", "stick keeps it"} {
		if !strings.Contains(said, want) {
			t.Errorf("windows: nothing said about %q:\n%s", want, said)
		}
	}
	if strings.Contains(said, "hunter2") {
		t.Errorf("the password is in the warning:\n%s", said)
	}

	// Linux: taken, and nothing said, because the answers get a hash.
	out.Reset()
	if got, err = readAdminPassword(strings.NewReader("hunter2"), &out, true, ubu); err != nil || got != "hunter2" {
		t.Fatalf("linux: %q %v", got, err)
	}
	if out.String() != "" {
		t.Errorf("linux: warned about plain text it does not write:\n%s", out.String())
	}

	// Not asked for: stdin is not read at all, so a build without the flag
	// never blocks waiting for input that is not coming.
	out.Reset()
	if got, err = readAdminPassword(neverRead{t}, &out, false, win); err != nil || got != "" {
		t.Fatalf("without the flag: %q %v", got, err)
	}

	// Asked for and not given: refused before anything is built.
	out.Reset()
	if _, err = readAdminPassword(strings.NewReader("\n"), &out, true, ubu); err == nil {
		t.Error("an empty stdin was accepted as a password")
	}
}

// neverRead fails the test if anything reads it.
type neverRead struct{ t *testing.T }

func (n neverRead) Read([]byte) (int, error) {
	n.t.Error("stdin was read without --admin-password-stdin")
	return 0, io.EOF
}

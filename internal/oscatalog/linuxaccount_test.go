package oscatalog

import (
	"strings"
	"testing"

	"github.com/uplinkresearch/dsky/internal/appcatalog"
)

// An account in the answers is the difference between a stick that stops to
// ask and one that does not. What matters as much is that the password never
// reaches the file: the answers say where to find it, the way Windows'
// admin_password already does, so a workspace left behind hands nobody
// anything.
func TestAnswersCarryTheAccountButNotThePassword(t *testing.T) {
	const pw = "hunter2"
	plan, err := appcatalog.ResolveUbuntu([]string{"vlc"})
	if err != nil {
		t.Fatal(err)
	}
	fplan, err := appcatalog.ResolveFedora([]string{"vlc"})
	if err != nil {
		t.Fatal(err)
	}
	acct := linuxAccount{User: "dusty", Hash: true}

	ubuntu := ubuntuUserData(plan, false, false, acct)
	for _, want := range []string{"identity:", "username: dusty", "hostname: dusty-pc", adminHashVar} {
		if !strings.Contains(ubuntu, want) {
			t.Errorf("ubuntu answers missing %q:\n%s", want, ubuntu)
		}
	}
	if strings.Contains(ubuntu, "interactive-sections") {
		t.Error("ubuntu answers carry an account and still ask for one")
	}

	fedora := fedoraKickstart(fplan, acct)
	for _, want := range []string{"user --name=dusty", "--iscrypted", "rootpw --lock", adminHashVar} {
		if !strings.Contains(fedora, want) {
			t.Errorf("kickstart missing %q:\n%s", want, fedora)
		}
	}

	// Neither file may contain the password, and neither may contain a hash
	// either: the hash arrives at render time through CLIVars.
	for name, out := range map[string]string{"ubuntu": ubuntu, "kickstart": fedora} {
		if strings.Contains(out, pw) {
			t.Errorf("%s answers contain the password itself", name)
		}
		if strings.Contains(out, "$6$") {
			t.Errorf("%s answers contain a hash rather than the variable", name)
		}
	}

	// And with no account, both go back to asking.
	if !strings.Contains(ubuntuUserData(plan, false, false, linuxAccount{}), "interactive-sections") {
		t.Error("ubuntu answers stopped asking for an account that was never supplied")
	}
	if strings.Contains(fedoraKickstart(fplan, linuxAccount{}), "user --name") {
		t.Error("kickstart made an account that was never asked for")
	}
}

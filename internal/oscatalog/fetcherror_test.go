package oscatalog

import (
	"errors"
	"net"
	"strings"
	"testing"
)

// The message is a function of the error, tested by handing it the error,
// because the last time a message was tested by reproducing the condition the
// test found a working PowerShell on two of the three CI runners and proved
// nothing. Nothing here touches the network or this machine's resolver.

func downloadDirsForTest() []string {
	return []string{`C:\Users\dusty\Downloads`, `C:\Users\dusty\Desktop`}
}

// Reported from an HP bench: the signed link was issued, and then the machine
// could not resolve Microsoft's CDN. The old message said Windows had not been
// downloaded yet and named two folders to put an ISO in — which reads as "go
// and download it", and a browser on that machine fails at the same step.
func TestANameThatDoesNotResolveIsNotMicrosoftRefusing(t *testing.T) {
	inner := &net.DNSError{
		Err:        "no such host",
		Name:       "software.download.prss.microsoft.com",
		IsNotFound: true,
	}
	err := windowsDownloadError(inner, "Windows 11", "11", downloadDirsForTest())
	msg := err.Error()
	for _, want := range []string{
		"could not reach Microsoft's download server",
		"software.download.prss.microsoft.com did not resolve",
		"this computer's network rather than Microsoft refusing",
		"check DNS",
		`C:\Users\dusty\Downloads`, // copying one from another machine still works
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the message does not say %q:\n%s", want, msg)
		}
	}
	// Not the way to choose a file by hand: that is a flag on the command
	// line and a button in the portal, and this message is printed by both.
	if strings.Contains(msg, "--iso") {
		t.Errorf("the message names a command-line flag, and the portal prints it too:\n%s", msg)
	}
	// The advice for the other failure must not be here: it sends somebody to
	// a browser that will fail at the same step.
	if strings.Contains(msg, "has not been downloaded on this computer yet") {
		t.Errorf("a DNS failure was reported as Microsoft refusing:\n%s", msg)
	}
	// And the original error is still readable underneath, because the host
	// and the exact words are what somebody searches for.
	if !strings.Contains(msg, "no such host") {
		t.Errorf("the message threw away what actually happened:\n%s", msg)
	}
}

func TestAServerThatRefusedStillGetsTheOrdinaryAdvice(t *testing.T) {
	// Microsoft answering and refusing: the rate limit, which is the common
	// case and the one the browser advice was written for.
	err := windowsDownloadError(errors.New("server returned 403 Forbidden"),
		"Windows 11", "11", downloadDirsForTest())
	msg := err.Error()
	if !strings.Contains(msg, "has not been downloaded on this computer yet") {
		t.Errorf("the ordinary advice went missing:\n%s", msg)
	}
	if strings.Contains(msg, "check DNS") {
		t.Errorf("a refusal was reported as a network problem:\n%s", msg)
	}
	if !strings.Contains(msg, `Win11_….iso`) {
		t.Errorf("the message no longer says what file to look for:\n%s", msg)
	}
}

func TestAConnectionThatTimedOutIsAlsoTheNetwork(t *testing.T) {
	inner := &net.OpError{
		Op:   "dial",
		Net:  "tcp",
		Addr: &net.TCPAddr{IP: net.ParseIP("199.232.210.172"), Port: 443},
		Err:  &timeoutErr{},
	}
	msg := windowsDownloadError(inner, "Windows 11", "11", downloadDirsForTest()).Error()
	if !strings.Contains(msg, "could not reach Microsoft's download server") ||
		!strings.Contains(msg, "199.232.210.172:443") {
		t.Errorf("a connection that timed out was not reported as the network:\n%s", msg)
	}
}

type timeoutErr struct{}

func (*timeoutErr) Error() string   { return "i/o timeout" }
func (*timeoutErr) Timeout() bool   { return true }
func (*timeoutErr) Temporary() bool { return true }

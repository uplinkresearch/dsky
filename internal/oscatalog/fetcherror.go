package oscatalog

import (
	"errors"
	"fmt"
	"net"
	"strings"
)

// Why fetching Windows failed, said as the thing that actually went wrong.
//
// Microsoft serves its ISOs through a signed link that expires, so DSKY asks
// for one and then downloads it. Two quite different things fail there and
// they had the same message.
//
// The usual one is Microsoft refusing: an address may download roughly once a
// day, and the answer is to fetch the ISO in a browser and point DSKY at it.
// That advice was printed for every failure.
//
// Reported from an HP bench: the link was issued fine and then the download
// died with "lookup software.download.prss.microsoft.com: no such host" --
// that machine's resolver would not answer for Microsoft's CDN. The message
// still said Windows had not been downloaded yet and named two folders to put
// an ISO in, which reads as "go and download it", and a browser on that
// machine would have failed at exactly the same step. The fix is DNS, and
// nothing on the screen said so.

// windowsDownloadError explains a failed Windows fetch. err is what the
// download returned, name is the entry's name, win is "10"/"11", and dirs are
// the folders DSKY looked in for an ISO already downloaded.
func windowsDownloadError(err error, name, win string, dirs []string) error {
	if host, why := neverReached(err); why != "" {
		// The folders, and not the way to choose a file by hand: that is a
		// flag on the command line and a button in the portal, and this one
		// message is printed by both.
		return fmt.Errorf("%w. This computer could not reach Microsoft's download server: %s %s. "+
			"That is this computer's network rather than Microsoft refusing, so downloading %s in a browser "+
			"here will fail at the same step -- check DNS. An ISO fetched on another machine still works: "+
			"put it in %s",
			err, host, why, name, strings.Join(dirs, " or "))
	}
	// Say where DSKY looked, so a download saved somewhere else, or under
	// another name, is recognisably the reason.
	return fmt.Errorf("%w. %s has not been downloaded on this computer yet, and DSKY found no Win%s_….iso in %s",
		err, name, win, strings.Join(dirs, " or "))
}

// neverReached reports whether the download never got as far as the server,
// and in that case which host and what happened. A server that answered and
// refused is not this: that is Microsoft's rate limit, and the ordinary advice
// is right for it.
func neverReached(err error) (host, why string) {
	var dns *net.DNSError
	if errors.As(err, &dns) {
		switch {
		case dns.IsNotFound:
			return dns.Name, "did not resolve"
		case dns.IsTimeout:
			return dns.Name, "could not be looked up before the lookup timed out"
		}
		return dns.Name, "could not be looked up"
	}
	var op *net.OpError
	if errors.As(err, &op) {
		addr := ""
		if op.Addr != nil {
			addr = op.Addr.String()
		}
		switch {
		case op.Timeout():
			return addr, "did not answer before the connection timed out"
		default:
			return addr, "refused the connection"
		}
	}
	return "", ""
}

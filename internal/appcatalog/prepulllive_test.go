package appcatalog

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestPrePullableLive reads the real installer manifest for every built-in
// Windows program and reports which of them can be put on a stick. It needs
// the network and, for ninety packages, a GitHub token; catalog-health runs it
// weekly beside TestWingetIDsLive.
//
// It does not fail on a package that cannot be pre-pulled: several genuinely
// cannot (Store packages, installers with dependency chains), and that is a
// thing to know rather than a thing to fix. It fails when one cannot be read
// at all, because that means the reader has stopped understanding a manifest
// shape winget publishes -- which is how an offline build would quietly start
// refusing programs it used to carry.
func TestPrePullableLive(t *testing.T) {
	if testing.Short() {
		t.Skip("network")
	}
	var canGo, cannot, checked int
	for _, a := range builtin {
		if a.Winget == "" {
			continue
		}
		plan, err := planPrePull(context.Background(), []string{a.Winget})
		switch {
		case errors.Is(err, ErrWingetUnchecked):
			if checked == 0 {
				t.Skipf("winget's package list could not be reached: %v", err)
			}
			t.Logf("read %d of them, then ran out of GitHub requests", checked)
			t.Logf("%d can be put on a stick, %d cannot", canGo, cannot)
			return
		case isRefusal(err):
			cannot++
			checked++
			var pe *PrePullError
			errors.As(err, &pe)
			t.Logf("%s: cannot ride on a stick: %s", a.Winget, strings.Join(pe.Reasons, "; "))
		case err != nil:
			checked++
			t.Errorf("%s: its manifest could not be read at all: %v", a.Winget, err)
		default:
			canGo++
			checked++
			in := plan[len(plan)-1]
			if in.URL == "" || in.SHA256 == "" {
				t.Errorf("%s: accepted with no url or hash: %+v", a.Winget, in)
			}
			extra := ""
			if len(plan) > 1 {
				var also []string
				for _, p := range plan[:len(plan)-1] {
					also = append(also, p.ID)
				}
				extra = " (with " + strings.Join(also, ", ") + ")"
			}
			// Ends in ": ok" like the other live checks, so the weekly job's
			// summary can drop the successes and print what rotted.
			t.Logf("%s: %s %s %s%s: ok", a.Winget, in.Version, in.Type, in.Filename(), extra)
		}
	}
	t.Logf("%d can be put on a stick, %d cannot", canGo, cannot)
}

// isRefusal is the build saying no, rather than the lookup failing.
func isRefusal(err error) bool {
	var pe *PrePullError
	return errors.As(err, &pe)
}

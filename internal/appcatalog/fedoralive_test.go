package appcatalog

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// fedoraRelease is the Fedora whose metadata the dnf names are checked
// against. Bumped with the catalog entry.
const fedoraRelease = "f44"

// dnfExists asks Fedora's own metadata service whether a binary package is in
// that release. 200 means yes and 400 means no such package.
//
// Retried, because it answers 400 to perfectly real packages often enough to
// matter: vlc and wireguard-tools both came back missing on a first pass and
// present on a second. A flaky check that fails the build is worse than no
// check, because the next person learns to ignore it.
func dnfExists(ctx context.Context, pkg string) (bool, error) {
	url := "https://apps.fedoraproject.org/mdapi/" + fedoraRelease + "/pkg/" + pkg
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(2 * time.Second)
		}
		code, _, err := liveGet(ctx, url, nil)
		if err != nil {
			lastErr = err
			continue
		}
		switch code {
		case 200:
			return true, nil
		case 400, 404:
			lastErr = nil
			continue // maybe; try again before believing it
		default:
			lastErr = fmt.Errorf("HTTP %d", code)
		}
	}
	return false, lastErr
}

func TestFedoraNamesLive(t *testing.T) {
	if testing.Short() {
		t.Skip("network")
	}
	ctx := context.Background()

	type problem struct{ id, msg string }
	var (
		mu       sync.Mutex
		problems []problem
		ok       int
	)
	bad := func(id, format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		problems = append(problems, problem{id, fmt.Sprintf(format, args...)})
	}
	good := func() { mu.Lock(); ok++; mu.Unlock() }

	sem := make(chan struct{}, 6)
	var wg sync.WaitGroup
	for _, a := range builtin {
		if a.Fedora == nil {
			continue
		}
		a := a
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			src := a.Fedora
			switch {
			case src.Dnf != "":
				exists, err := dnfExists(ctx, src.Dnf)
				if err != nil {
					bad(a.ID, "dnf %s: %v", src.Dnf, err)
					return
				}
				if !exists {
					bad(a.ID, "dnf package %q is not in Fedora %s", src.Dnf, fedoraRelease)
					return
				}
				good()
			case src.Flatpak != "":
				exists, err := flatpakExists(ctx, src.Flatpak)
				if err != nil {
					bad(a.ID, "flatpak %s: %v", src.Flatpak, err)
					return
				}
				if !exists {
					bad(a.ID, "Flathub has no app %q", src.Flatpak)
					return
				}
				verified, err := flatpakVerified(ctx, src.Flatpak)
				if err != nil {
					bad(a.ID, "flatpak %s: verification: %v", src.Flatpak, err)
					return
				}
				if !verified {
					bad(a.ID, "Flathub no longer says %q is published by its vendor", src.Flatpak)
					return
				}
				good()
			case src.Repo != "", src.Release != "":
				good() // checked by their own tests
			}
		}()
	}
	wg.Wait()

	t.Logf("%d Fedora program names checked and found", ok)
	for _, p := range problems {
		t.Errorf("%s: %s", p.id, p.msg)
	}
}

// Each vendor rpm repository is checked for the two things the first-boot
// script uses: the signing key, and the repository metadata it installs from.
func TestFedoraVendorReposLive(t *testing.T) {
	if testing.Short() {
		t.Skip("network")
	}
	ctx := context.Background()
	for _, repo := range fedoraRepos {
		for _, url := range []string{repo.GPGKey, repo.ProbeURL} {
			if url == "" {
				continue
			}
			code, _, err := liveGet(ctx, url, nil)
			if err != nil {
				t.Errorf("%s: %s: %v", repo.ID, url, err)
				continue
			}
			if code != 200 {
				t.Errorf("%s: %s: HTTP %d", repo.ID, url, code)
			}
		}
		t.Logf("%s: ok", repo.ID)
	}
}

// The two tables answer the same question for different machines, so a
// program in one and not the other should be a decision, not an oversight.
// This does not require them to match — Fedora has no Steam and no snaps —
// it just names the difference, so it is read rather than drifted into.
func TestLinuxTablesAreDeliberatelyDifferent(t *testing.T) {
	var ubuntuOnly, fedoraOnly []string
	for _, a := range builtin {
		switch {
		case a.Ubuntu != nil && a.Fedora == nil:
			ubuntuOnly = append(ubuntuOnly, a.ID)
		case a.Fedora != nil && a.Ubuntu == nil:
			fedoraOnly = append(fedoraOnly, a.ID)
		}
	}
	// Ubuntu carries these and Fedora cannot, for reasons written down in
	// fedora.go: no Steam, no codec packages, no snaps, and Flathub
	// verification that several publishers have not done.
	want := map[string]bool{
		"steam": true, "vscode": true, "powershell": true, "postman": true,
		"slack": true, "signal": true, "spotify": true, "opera": true,
		"vivaldi": true, "java21": true, "firefox": false,
	}
	for _, id := range ubuntuOnly {
		if !want[id] {
			t.Errorf("%s is offered on Ubuntu but not Fedora, and nothing says why", id)
		}
	}
	if len(fedoraOnly) != 0 {
		t.Errorf("offered on Fedora but not Ubuntu, which is surprising: %v", fedoraOnly)
	}
}

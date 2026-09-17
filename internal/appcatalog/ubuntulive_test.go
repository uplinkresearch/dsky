package appcatalog

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// The Ubuntu program table names apt packages, snaps and Flathub apps, and
// all three rename and disappear: a package is dropped between releases, a
// snap is renamed, a Flathub app is moved to a new application id. A wrong
// name here is invisible until someone's install finishes without the program
// they asked for, because one failure never stops the rest.
//
// So each name is looked up where it comes from, exactly as the table spells
// it. catalog-health runs this weekly beside TestWingetIDsLive.
//
// The releases DSKY offers Ubuntu programs on. Both are checked: a package
// that has been dropped from the newer release is a trap for the LTS that
// most fleets will move to.
var ubuntuReleases = []string{"noble", "resolute"} // 24.04 LTS, 26.04 LTS

func liveGet(ctx context.Context, url string, hdr map[string]string) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return 0, "", err
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: 45 * time.Second}).Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	// Enough of the body to tell an error page from a package page.
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, string(b), nil
}

// aptExists asks packages.ubuntu.com for one binary package in one release.
// The site answers 200 for a package it does not have, with an error page, so
// the title is what decides.
func aptExists(ctx context.Context, release, pkg string) (bool, error) {
	code, body, err := liveGet(ctx, "https://packages.ubuntu.com/"+release+"/"+pkg, nil)
	if err != nil {
		return false, err
	}
	if code != 200 {
		return false, fmt.Errorf("HTTP %d", code)
	}
	return strings.Contains(body, "Details of package "+pkg+" in "+release), nil
}

// snapExists asks the Snap Store. The series header is required.
func snapExists(ctx context.Context, name string) (bool, error) {
	code, _, err := liveGet(ctx, "https://api.snapcraft.io/v2/snaps/info/"+name,
		map[string]string{"Snap-Device-Series": "16"})
	if err != nil {
		return false, err
	}
	switch code {
	case 200:
		return true, nil
	case 404:
		return false, nil
	}
	return false, fmt.Errorf("HTTP %d", code)
}

// flatpakExists asks Flathub for the application id.
func flatpakExists(ctx context.Context, id string) (bool, error) {
	code, _, err := liveGet(ctx, "https://flathub.org/api/v2/appstream/"+id, nil)
	if err != nil {
		return false, err
	}
	switch code {
	case 200:
		return true, nil
	case 404:
		return false, nil
	}
	return false, fmt.Errorf("HTTP %d", code)
}

// flatpakVerified asks Flathub whether the app's publisher is who it claims to
// be. The table's rule is publisher-verified apps only (docs/plan-linux-apps.md),
// and verification is a thing that lapses: an app can be verified the day it is
// added to the table and not a year later, at which point DSKY would be
// installing a stranger's build of somebody's program.
func flatpakVerified(ctx context.Context, id string) (bool, error) {
	code, body, err := liveGet(ctx, "https://flathub.org/api/v2/verification/"+id+"/status", nil)
	if err != nil {
		return false, err
	}
	if code != 200 {
		return false, fmt.Errorf("HTTP %d", code)
	}
	var st struct {
		Verified bool `json:"verified"`
	}
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		return false, err
	}
	return st.Verified, nil
}

func TestUbuntuNamesLive(t *testing.T) {
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

	// Eight at a time: enough to finish ninety lookups quickly, few enough
	// that none of the three services sees a burst worth throttling.
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for _, a := range builtin {
		if a.Ubuntu == nil {
			continue
		}
		a := a
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			src := a.Ubuntu
			switch {
			case src.Apt != "":
				missing := []string{}
				for _, rel := range ubuntuReleases {
					exists, err := aptExists(ctx, rel, src.Apt)
					if err != nil {
						bad(a.ID, "apt %s in %s: %v", src.Apt, rel, err)
						return
					}
					if !exists {
						missing = append(missing, rel)
					}
				}
				if len(missing) > 0 {
					bad(a.ID, "apt package %q is not in %s", src.Apt, strings.Join(missing, " or "))
					return
				}
				good()
			case src.Snap != "":
				exists, err := snapExists(ctx, src.Snap)
				if err != nil {
					bad(a.ID, "snap %s: %v", src.Snap, err)
					return
				}
				if !exists {
					bad(a.ID, "snap %q is not in the Snap Store", src.Snap)
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
			case src.Repo != "":
				// A vendor repository is checked by its own test below.
				good()
			}
		}()
	}
	wg.Wait()

	t.Logf("%d Ubuntu program names checked and found", ok)
	for _, p := range problems {
		t.Errorf("%s: %s", p.id, p.msg)
	}
}

// Every vendor repository the first-boot script knows how to add is checked
// for the two things it uses: the signing key and the package list. A repo
// that has moved leaves the program uninstalled with a FAILED line in the log.
func TestUbuntuVendorReposLive(t *testing.T) {
	if testing.Short() {
		t.Skip("network")
	}
	ctx := context.Background()
	for _, repo := range ubuntuRepos {
		exists := map[string]bool{}
		for _, url := range []string{repo.KeyURL, repo.ProbeURL} {
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
				continue
			}
			exists[url] = true
		}
		if len(exists) > 0 {
			t.Logf("%s: ok", repo.ID)
		}
	}
}

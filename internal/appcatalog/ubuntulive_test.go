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
// The series DSKY offers Ubuntu programs on. Both are checked: a package that
// has been dropped from the newer one is a trap for the LTS that most fleets
// will move to.
var ubuntuSeries = []string{"noble", "resolute"} // 24.04 LTS, 26.04 LTS

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
				for _, rel := range ubuntuSeries {
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
			case src.Repo != "", src.Release != "":
				// Both have their own tests below.
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

// A program installed from a published release depends on three things that
// are outside DSKY: the release exists, the asset for this machine is in it,
// and the checksum file lists that asset by name. Any of them can change with
// a release the vendor makes on their own schedule — which, for DSKY itself,
// is every time it ships. So the current release is checked the way the
// first-boot script will read it, including the redirect it resolves the tag
// from.
func TestUbuntuReleasesLive(t *testing.T) {
	if testing.Short() {
		t.Skip("network")
	}
	ctx := context.Background()
	for _, rel := range ubuntuReleases {
		base := "https://github.com/" + rel.Repo + "/releases"
		req, err := http.NewRequestWithContext(ctx, "HEAD", base+"/latest", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := (&http.Client{Timeout: 45 * time.Second}).Do(req)
		if err != nil {
			t.Errorf("%s: %v", rel.ID, err)
			continue
		}
		resp.Body.Close()
		final := resp.Request.URL.String()
		_, tag, ok := strings.Cut(final, "/tag/")
		if !ok || tag == "" {
			t.Errorf("%s: /releases/latest did not redirect to a tag (%s)", rel.ID, final)
			continue
		}

		// amd64 and arm64 both, because the machine being imaged decides
		// which, and a release missing one is a silent skip on that hardware.
		sums := ""
		code, body, err := liveGet(ctx, base+"/download/"+tag+"/"+rel.Sums, nil)
		if err != nil || code != 200 {
			t.Errorf("%s: %s %s: HTTP %d %v", rel.ID, tag, rel.Sums, code, err)
			continue
		}
		sums = body
		for _, arch := range []string{"amd64", "arm64"} {
			asset := strings.NewReplacer("{tag}", tag, "{arch}", arch).Replace(rel.Asset)
			if !strings.Contains(sums, asset) {
				t.Errorf("%s: %s does not list %s", rel.ID, rel.Sums, asset)
				continue
			}
			code, _, err := liveGet(ctx, base+"/download/"+tag+"/"+asset, nil)
			if err != nil || code != 200 {
				t.Errorf("%s: %s: HTTP %d %v", rel.ID, asset, code, err)
				continue
			}
			t.Logf("%s: %s ok", rel.ID, asset)
		}
	}
}

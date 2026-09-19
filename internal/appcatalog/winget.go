package appcatalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// WingetPrefix marks a picker id that names a winget package directly rather
// than a program in the list: "winget:Brave.Brave".
const WingetPrefix = "winget:"

// wingetIDRe is the shape of a winget package id: dot-separated parts of
// letters, digits, + - _ (Notepad++.Notepad++, Microsoft.VCRedist.2015+.x64).
// It is also what keeps a typed id from carrying quotes or spaces into the
// first-boot script.
var wingetIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9+_-]*(\.[A-Za-z0-9+_-]+)+$`)

// ValidWingetID reports whether s is shaped like a winget package id.
func ValidWingetID(s string) bool { return len(s) <= 128 && wingetIDRe.MatchString(s) }

// ErrWingetNotFound means winget's package repository has no such package.
var ErrWingetNotFound = errors.New("no winget package with that id")

// ErrWingetUnchecked means the lookup itself failed — offline, or GitHub's
// rate limit — so nothing is known either way.
var ErrWingetUnchecked = errors.New("could not check winget's package list")

// WingetRepoAPI is the GitHub API root for winget's package repository. A
// variable so tests can point it at a fake.
var WingetRepoAPI = "https://api.github.com/repos/microsoft/winget-pkgs"

var (
	lookupMu    sync.Mutex
	lookupCache = map[string]lookupResult{}
)

type lookupResult struct {
	id string
	// dir is the version folder the id was found in, which is where the
	// installer manifest lives. Cached with the spelling because the same
	// walk produced both: the portal checks a typed id, and an offline build
	// then reads that package's installer manifest, and walking the
	// repository twice for one package is a dozen API calls against a limit
	// of 60 an hour without a token.
	dir string
	err error
}

// LookupWinget confirms a winget package exists and returns its id as the
// package spells it. winget matches ids case-insensitively, so "brave.brave"
// is fine to type, but the list shows the proper spelling.
//
// Microsoft publishes every package as a folder in microsoft/winget-pkgs:
// manifests/<first letter>/<each part of the id>/<version>/. Folder names are
// case-sensitive, so the lookup walks one level at a time matching names
// without regard to case, then reads a version folder's manifest file names
// for the spelling. A folder part-way down (Python.Python) is a group of
// packages, not a package, and is reported as not found.
//
// Directory listings through the contents API stop at 1,000 entries, which
// once made AnyDesk and Tailscale look missing; the git trees API used here
// lists a whole folder.
func LookupWinget(ctx context.Context, id string) (string, error) {
	if !ValidWingetID(id) {
		return "", fmt.Errorf("%q is not a winget package id (they look like Publisher.Package, such as Brave.Brave)", id)
	}
	canon, _, err := resolveWinget(ctx, id)
	return canon, err
}

// resolveWinget walks the repository to a version folder that really holds
// this package's manifests, and returns the id as that folder spells it
// together with the folder's path. Two callers want different halves of the
// same walk: the id check wants the spelling, and the offline pre-pull wants
// the folder, because the installer manifest lives in it.
func resolveWinget(ctx context.Context, id string) (string, string, error) {
	key := strings.ToLower(id)
	lookupMu.Lock()
	if r, ok := lookupCache[key]; ok && (r.err != nil || r.dir != "") {
		lookupMu.Unlock()
		return r.id, r.dir, r.err
	}
	lookupMu.Unlock()
	name, dir, err := walkToVersion(ctx, id)
	if err == nil || errors.Is(err, ErrWingetNotFound) {
		lookupMu.Lock()
		lookupCache[key] = lookupResult{id: name, dir: dir, err: err}
		lookupMu.Unlock()
	}
	return name, dir, err
}

// walkToVersion is resolveWinget without the cache.
func walkToVersion(ctx context.Context, id string) (string, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	path := "manifests/" + strings.ToLower(id[:1])
	for _, part := range strings.Split(id, ".") {
		entries, err := wingetTree(ctx, path)
		if err != nil {
			return "", "", err
		}
		name, ok := "", false
		for _, e := range entries {
			if e.Type == "tree" && strings.EqualFold(e.Path, part) {
				name, ok = e.Path, true
				break
			}
		}
		if !ok {
			return "", "", ErrWingetNotFound
		}
		path += "/" + name
	}

	// A package folder holds version folders, and a version folder holds the
	// manifests, named after the package. Newest-looking versions first.
	entries, err := wingetTree(ctx, path)
	if err != nil {
		return "", "", err
	}
	// Versions nearly always start with a digit; a few packages name them
	// otherwise, and those are tried after, since letter-named folders are
	// usually packages of their own (Mozilla.Firefox.ESR).
	var versions, others []string
	for _, e := range entries {
		switch {
		case e.Type != "tree" || e.Path == "":
		case e.Path[0] >= '0' && e.Path[0] <= '9':
			versions = append(versions, e.Path)
		default:
			others = append(others, e.Path)
		}
	}
	sort.Slice(versions, func(i, j int) bool { return versionLess(versions[j], versions[i]) })
	candidates := append(versions[:min(3, len(versions))], others[:min(3, len(others))]...)
	for _, v := range candidates {
		files, err := wingetTree(ctx, path+"/"+v)
		if err != nil {
			return "", "", err
		}
		for _, f := range files {
			if f.Type != "blob" || !strings.HasSuffix(f.Path, ".yaml") {
				continue
			}
			name := strings.TrimSuffix(f.Path, ".yaml")
			name = strings.TrimSuffix(name, ".installer")
			if i := strings.Index(name, ".locale."); i >= 0 {
				name = name[:i]
			}
			if strings.EqualFold(name, id) {
				return name, path + "/" + v, nil
			}
		}
	}
	return "", "", ErrWingetNotFound
}

type treeEntry struct {
	Path string `json:"path"`
	Type string `json:"type"`
}

func wingetTree(ctx context.Context, path string) ([]treeEntry, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, WingetRepoAPI+"/git/trees/master:"+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "dsky")
	// Without a token GitHub allows 60 requests an hour from an address, which
	// is a dozen lookups; CI and anyone with a token set get far more.
	for _, env := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
		if t := os.Getenv(env); t != "" {
			req.Header.Set("Authorization", "Bearer "+t)
			break
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrWingetUnchecked, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, ErrWingetNotFound
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests:
		return nil, fmt.Errorf("%w: GitHub's hourly limit was reached, try again later", ErrWingetUnchecked)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("%w: GitHub answered %s", ErrWingetUnchecked, resp.Status)
	}
	var body struct {
		Tree []treeEntry `json:"tree"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrWingetUnchecked, err)
	}
	return body.Tree, nil
}

// versionLess orders version folder names by their numeric parts, so 10.0
// sorts after 9.9.
func versionLess(a, b string) bool {
	pa, pb := versionParts(a), versionParts(b)
	for i := 0; i < len(pa) && i < len(pb); i++ {
		if pa[i] != pb[i] {
			return pa[i] < pb[i]
		}
	}
	if len(pa) != len(pb) {
		return len(pa) < len(pb)
	}
	return a < b
}

func versionParts(s string) []int {
	var out []int
	n, in := 0, false
	for _, r := range s {
		if r >= '0' && r <= '9' {
			n, in = n*10+int(r-'0'), true
			if n > 1<<30 {
				n = 1 << 30
			}
			continue
		}
		if in {
			out = append(out, n)
			n, in = 0, false
		}
	}
	if in {
		out = append(out, n)
	}
	return out
}

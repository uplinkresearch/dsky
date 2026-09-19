package appcatalog

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The list is data typed by hand: these catch the mistakes that are easy to
// make adding the next program and invisible until someone picks it.
func TestBuiltinListIsConsistent(t *testing.T) {
	ids, names, pkgs := map[string]bool{}, map[string]bool{}, map[string]bool{}
	cats := map[string]bool{}
	last := ""
	for _, a := range builtin {
		switch {
		case a.ID == "" || a.Name == "" || a.Category == "":
			t.Errorf("%+v: id, name and category are all required", a)
		case !idRe.MatchString(a.ID):
			t.Errorf("%s: id must be what --apps and custom ids accept", a.ID)
		case a.Custom != nil:
			t.Errorf("%s: built-in entries are never custom", a.ID)
		case a.Winget != "" && !ValidWingetID(a.Winget):
			t.Errorf("%s: winget id %q is not shaped like one", a.ID, a.Winget)
		}
		if ids[strings.ToLower(a.ID)] {
			t.Errorf("duplicate id %s", a.ID)
		}
		if names[strings.ToLower(a.Name)] {
			t.Errorf("duplicate name %s", a.Name)
		}
		if a.Winget != "" && pkgs[strings.ToLower(a.Winget)] {
			t.Errorf("%s: winget package %s is already listed", a.ID, a.Winget)
		}
		// Categories are contiguous, so the picker and `dsky apps` group
		// without sorting and nothing lands under a second heading.
		if a.Category != last && cats[a.Category] {
			t.Errorf("%s: category %q appears in two places in the list", a.ID, a.Category)
		}
		if a.Category == CustomCategory {
			t.Errorf("%s: %q is reserved for the operator's installers", a.ID, CustomCategory)
		}
		ids[strings.ToLower(a.ID)], names[strings.ToLower(a.Name)], pkgs[strings.ToLower(a.Winget)] = true, true, true
		cats[a.Category], last = true, a.Category
	}
	for _, s := range Sets {
		for _, id := range s.Apps {
			if a, ok := builtinByID(id); !ok || a.Winget == "" {
				t.Errorf("set %s names %s, which is not a built-in Windows program", s.ID, id)
			}
		}
	}
	for _, e := range NotInWinget {
		for _, a := range builtin {
			for _, w := range e.Words {
				if strings.Contains(strings.ToLower(a.Name), w) {
					t.Errorf("%s is listed as not in winget, but %s (%s) is in the list", e.Name, a.Name, a.Winget)
				}
			}
		}
	}
}

func TestResolveTypedWingetIDs(t *testing.T) {
	freshStore(t)
	w, c, err := Resolve([]string{"brave", "winget:Mozilla.Firefox", "winget:Microsoft.VCRedist.2015+.x64"})
	if err != nil || len(c) != 0 {
		t.Fatal(err, c)
	}
	want := []string{"Brave.Brave", "Mozilla.Firefox", "Microsoft.VCRedist.2015+.x64"}
	if strings.Join(w, " ") != strings.Join(want, " ") {
		t.Fatalf("winget = %v, want %v", w, want)
	}
	// Typed ids end up in a PowerShell script, so anything not shaped like a
	// package id is refused rather than quoted and hoped about.
	for _, bad := range []string{"winget:Brave", "winget:a.b'; rm", "winget:a b.c", "winget:.x"} {
		if _, _, err := Resolve([]string{bad}); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestCustomKeepsItsIDWhenTheListGrows(t *testing.T) {
	root := freshStore(t)
	// Stored before "anydesk" was built in, as it would be on disk.
	c := sample("anydesk")
	customMu.Lock()
	customs = []Custom{c}
	customMu.Unlock()
	a, ok := Get("anydesk")
	if !ok || a.Custom == nil {
		t.Fatalf("Get(anydesk) = %+v, want the operator's installer", a)
	}
	if _, err := UpdateCustom(root, "anydesk", func(c *Custom) { c.Name = "AnyDesk (custom)" }); err != nil {
		t.Fatalf("editing it: %v", err)
	}
}

// fakeRepo serves the git trees API for a tiny winget-pkgs.
func fakeRepo(t *testing.T, dirs map[string][]treeEntry) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/git/trees/master:")
		tree, ok := dirs[path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"tree": tree})
	}))
	t.Cleanup(srv.Close)
	old := WingetRepoAPI
	WingetRepoAPI = srv.URL
	t.Cleanup(func() {
		WingetRepoAPI = old
		lookupMu.Lock()
		lookupCache = map[string]lookupResult{}
		lookupMu.Unlock()
	})
}

func TestLookupWinget(t *testing.T) {
	d := func(names ...string) (out []treeEntry) {
		for _, n := range names {
			out = append(out, treeEntry{Path: n, Type: "tree"})
		}
		return
	}
	f := func(names ...string) (out []treeEntry) {
		for _, n := range names {
			out = append(out, treeEntry{Path: n, Type: "blob"})
		}
		return
	}
	fakeRepo(t, map[string][]treeEntry{
		"manifests/b":                    d("Brave", "Box"),
		"manifests/b/Brave":              d("Brave"),
		"manifests/b/Brave/Brave":        d("9.1", "10.0", "Beta"),
		"manifests/b/Brave/Brave/10.0":   f("Brave.Brave.installer.yaml", "Brave.Brave.locale.en-US.yaml", "Brave.Brave.yaml"),
		"manifests/p":                    d("Python"),
		"manifests/p/Python":             d("Python"),
		"manifests/p/Python/Python":      d("3", "3.13"),
		"manifests/p/Python/Python/3":    d("3.0.0"),
		"manifests/p/Python/Python/3.13": d("3.13.1"),
	})
	ctx := context.Background()
	if got, err := LookupWinget(ctx, "brave.BRAVE"); err != nil || got != "Brave.Brave" {
		t.Fatalf("brave.BRAVE = %q, %v", got, err)
	}
	// A group of packages is not a package.
	if _, err := LookupWinget(ctx, "Python.Python"); !errors.Is(err, ErrWingetNotFound) {
		t.Fatalf("Python.Python: %v, want not found", err)
	}
	if _, err := LookupWinget(ctx, "RustDesk.RustDesk"); !errors.Is(err, ErrWingetNotFound) {
		t.Fatalf("RustDesk: %v, want not found", err)
	}
	if _, err := LookupWinget(ctx, "not an id"); err == nil || errors.Is(err, ErrWingetNotFound) {
		t.Fatalf("malformed id: %v", err)
	}
}

func TestLookupWingetRateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rate limited", http.StatusForbidden)
	}))
	defer srv.Close()
	old := WingetRepoAPI
	WingetRepoAPI = srv.URL
	defer func() { WingetRepoAPI = old }()
	if _, err := LookupWinget(context.Background(), "Brave.Brave"); !errors.Is(err, ErrWingetUnchecked) {
		t.Fatalf("err = %v, want unchecked", err)
	}
	lookupMu.Lock()
	_, cached := lookupCache["brave.brave"]
	lookupMu.Unlock()
	if cached {
		t.Fatal("a failed check was cached as an answer")
	}
}

func TestVersionOrder(t *testing.T) {
	if !versionLess("9.9", "10.0") || versionLess("10.0", "9.9") || !versionLess("1.2", "1.2.1") {
		t.Fatal("versions are not ordered numerically")
	}
}

// TestWingetIDsLive checks every built-in package still exists in winget, as
// spelled. It needs the network and, for ninety packages, a GitHub token;
// catalog-health runs it weekly.
func TestWingetIDsLive(t *testing.T) {
	if testing.Short() {
		t.Skip("network")
	}
	checked := 0
	for _, a := range builtin {
		if a.Winget == "" {
			continue
		}
		got, err := LookupWinget(context.Background(), a.Winget)
		switch {
		case errors.Is(err, ErrWingetUnchecked):
			// GitHub's hourly limit, or no network. Nothing is known about
			// this id either way, and reporting that as a bad id is how a
			// green test turns red for a reason that has nothing to do with
			// the catalog. The run stops here: once the limit is reached
			// every id after this one says the same thing.
			if checked == 0 {
				t.Skipf("winget's package list could not be reached: %v", err)
			}
			t.Skipf("checked %d ids, then ran out of GitHub requests: %v", checked, err)
		case err != nil:
			t.Errorf("%s: %s: %v", a.ID, a.Winget, err)
		case got != a.Winget:
			t.Errorf("%s: winget spells it %s, the list has %s", a.ID, got, a.Winget)
		default:
			checked++
			t.Logf("%s: %s: ok", a.ID, a.Winget)
		}
	}
}

func TestResolveStarterSet(t *testing.T) {
	freshStore(t)
	w, _, err := Resolve([]string{"set:it", "brave"})
	if err != nil {
		t.Fatal(err)
	}
	if len(w) != 7 || w[0] != "Microsoft.Sysinternals.Suite" || w[6] != "Brave.Brave" {
		t.Fatalf("winget = %v", w)
	}
	if _, _, err := Resolve([]string{"set:nope"}); err == nil {
		t.Fatal("unknown set accepted")
	}
}

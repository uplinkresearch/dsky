package appcatalog

import (
	"strings"
	"testing"
)

func TestUbuntuTableIsConsistent(t *testing.T) {
	for id, src := range ubuntu {
		a, ok := builtinByID(id)
		if !ok {
			t.Errorf("ubuntu table names %s, which is not a built-in program", id)
			continue
		}
		if a.Ubuntu == nil {
			t.Errorf("%s: table entry not attached", id)
		}
		n := 0
		for _, v := range []string{src.Apt, src.Snap, src.Flatpak, src.Repo, src.Release} {
			if v != "" {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%s: needs exactly one Ubuntu source, has %d", id, n)
		}
		if src.Classic && src.Snap == "" {
			t.Errorf("%s: classic without a snap", id)
		}
	}
	// Left out on purpose: unofficial clients, and Windows-only programs.
	// AnyDesk and TeamViewer have come off this list: both vendors publish a
	// Linux client through their own apt repository, which is the rule Chrome
	// comes in under.
	for _, id := range []string{"teams", "notion", "zoom", "dropbox", "githubdesktop", "zotero", "webex", "sysinternals", "office"} {
		if _, ok := ubuntu[id]; ok {
			t.Errorf("%s is offered for Ubuntu", id)
		}
	}
	// Every repository a program names has to be one DSKY can actually add,
	// or the first boot logs a failure for a program that was offered.
	for id, src := range ubuntu {
		if src.Repo == "" {
			continue
		}
		if _, ok := UbuntuRepoByID(src.Repo); !ok {
			t.Errorf("%s names repository %q, which DSKY has no recipe for", id, src.Repo)
		}
	}
	for _, r := range ubuntuRepos {
		if r.ID == "" || r.Name == "" || r.KeyURL == "" || r.URL == "" || r.Suite == "" || r.Comps == "" || r.Package == "" {
			t.Errorf("repository %+v is missing a field the first-boot script needs", r)
		}
	}
	// The same for a published release: a program offered from one and then
	// not installable from it is a machine that comes up without it.
	for id, src := range ubuntu {
		if src.Release == "" {
			continue
		}
		if _, ok := UbuntuReleaseByID(src.Release); !ok {
			t.Errorf("%s names release %q, which DSKY has no recipe for", id, src.Release)
		}
	}
	for _, r := range ubuntuReleases {
		if r.ID == "" || r.Name == "" || r.Repo == "" || r.Asset == "" || r.Sums == "" || r.Binary == "" {
			t.Errorf("release %+v is missing a field the first-boot script needs", r)
		}
		// Without both, the asset name cannot be built for the machine in
		// front of it, and a checksum file listing every platform would let
		// the wrong one through.
		if !strings.Contains(r.Asset, "{tag}") || !strings.Contains(r.Asset, "{arch}") {
			t.Errorf("release %s: asset %q must name both {tag} and {arch}", r.ID, r.Asset)
		}
	}
}

func TestResolveUbuntu(t *testing.T) {
	freshStore(t)
	p, err := ResolveUbuntu([]string{"vlc", "brave", "vscode", "obsidian", "chrome", "epicgames", "gog", "vlc"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(p.Apt, ",") != "vlc" {
		t.Errorf("apt = %v", p.Apt)
	}
	if len(p.Snaps) != 2 || p.Snaps[0].Name != "brave" || p.Snaps[1].Name != "code" || !p.Snaps[1].Classic {
		t.Errorf("snaps = %+v", p.Snaps)
	}
	if strings.Join(p.Flatpaks, ",") != "md.obsidian.Obsidian,com.heroicgameslauncher.hgl" {
		t.Errorf("flatpaks = %v (Heroic must appear once for both launchers)", p.Flatpaks)
	}
	if strings.Join(p.Repos, ",") != RepoGoogleChrome || !p.FirstBoot() {
		t.Errorf("repos = %v", p.Repos)
	}

	// A starter set brings what Ubuntu has; Business PC's Acrobat, Teams and
	// VC++ are Windows programs.
	p, err = ResolveUbuntu([]string{"set:business"})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Repos) != 1 || strings.Join(p.Apt, ",") != "7zip" {
		t.Errorf("business set on Ubuntu = %+v", p)
	}

	for _, bad := range []string{"teams", "winget:Brave.Brave", "nosuchprogram"} {
		if _, err := ResolveUbuntu([]string{bad}); err == nil {
			t.Errorf("%s accepted for Ubuntu", bad)
		}
	}
	customMu.Lock()
	customs = []Custom{sample("macula")}
	customMu.Unlock()
	if _, err := ResolveUbuntu([]string{"macula"}); err == nil || !strings.Contains(err.Error(), "Windows installer") {
		t.Errorf("custom installer for Ubuntu: %v", err)
	}
}

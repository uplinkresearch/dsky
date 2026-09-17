package appcatalog

import (
	"fmt"
	"sort"
	"strings"
)

// UbuntuSource is where one program comes from on Ubuntu.
//
// The order of preference, and why (docs/plan-linux-apps.md):
//  1. Ubuntu's own archive (Apt), when it has the program.
//  2. The Snap Store (Snap), when the publisher is the vendor (verified) or
//     Snapcrafters (starred). The installer has a snaps section for them.
//  3. Flathub (Flatpak), publisher-verified apps only. Ubuntu doesn't ship
//     Flatpak, so the first one installs it, on first boot.
//  4. The vendor's own apt repository (Repo), only where that is the vendor's
//     official channel and nothing above is: Google Chrome, AnyDesk,
//     TeamViewer.
//  5. The vendor's published release binary (Release), for a program that is
//     packaged nowhere at all — DSKY itself, so far. Last, because nothing
//     updates it with the system afterwards.
//
// Unofficial clients are left out (Dusty's decision): Teams for Linux, the
// Notion snap, the Zoom and Zotero snaps, and Flathub apps whose publisher
// isn't verified (Zoom, Dropbox, AnyDesk, GitHub Desktop, Zotero, the Chrome
// wrapper). Every name below was checked on 2026-09-14 against
// packages.ubuntu.com for 24.04 and 26.04, the Snapcraft store API and the
// Flathub API.
type UbuntuSource struct {
	Apt     string `json:"apt,omitempty"`
	Snap    string `json:"snap,omitempty"`
	Classic bool   `json:"classic,omitempty"` // snap needs classic confinement
	Flatpak string `json:"flatpak,omitempty"`
	Repo    string `json:"repo,omitempty"` // a vendor repository DSKY knows how to add
	// Release is a program published as a release binary on GitHub rather
	// than packaged anywhere: rule 5, and the last resort. See UbuntuRelease.
	Release string `json:"release,omitempty"`
	// Note is shown beside the name when what installs isn't obviously the
	// program asked for.
	Note string `json:"note,omitempty"`
}

// RepoGoogleChrome is Google's apt repository for Chrome.
const RepoGoogleChrome = "google-chrome"

// RepoAnyDesk and RepoTeamViewer are those vendors' own apt repositories.
const (
	RepoAnyDesk    = "anydesk"
	RepoTeamViewer = "teamviewer"
)

// UbuntuRepo is a vendor's own apt repository: rule 4, for a program whose
// vendor ships it themselves and nowhere above. Everything the first-boot
// script needs in order to add one is here, so a new vendor is a table entry
// rather than another branch in a generated shell script.
//
// The key is fetched into /etc/apt/keyrings and named in the sources line with
// signed-by, which is how Debian and Ubuntu want a third-party repository
// added: apt-key is deprecated, and a key in trusted.gpg.d would be trusted
// for every repository on the machine rather than this one.
type UbuntuRepo struct {
	ID      string // the Repo value in the program table
	Name    string // what to call it in the log
	KeyURL  string // the vendor's armoured signing key
	URL     string // the repository root
	Suite   string // "stable", "all", …
	Comps   string // "main"
	Package string // what to install once it is added
	// AMD64Only marks a vendor that publishes for amd64 alone, so the script
	// says it skipped rather than failing on another architecture.
	AMD64Only bool
	// ProbeURL is a file that is there for as long as the repository is: its
	// Release file, which every apt repository has. For the weekly check.
	ProbeURL string
}

// ubuntuRepos are the vendor repositories DSKY knows how to add. Each comes
// from the vendor's own Linux installation instructions, and
// TestUbuntuVendorReposLive checks the key and the Release file weekly.
var ubuntuRepos = []UbuntuRepo{{
	ID:        RepoGoogleChrome,
	Name:      "Google Chrome",
	KeyURL:    "https://dl.google.com/linux/linux_signing_key.pub",
	URL:       "https://dl.google.com/linux/chrome/deb/",
	Suite:     "stable",
	Comps:     "main",
	Package:   "google-chrome-stable",
	AMD64Only: true,
	ProbeURL:  "https://dl.google.com/linux/chrome/deb/dists/stable/Release",
}, {
	ID:       RepoAnyDesk,
	Name:     "AnyDesk",
	KeyURL:   "https://keys.anydesk.com/repos/DEB-GPG-KEY",
	URL:      "http://deb.anydesk.com/",
	Suite:    "all",
	Comps:    "main",
	Package:  "anydesk",
	ProbeURL: "http://deb.anydesk.com/dists/all/Release",
}, {
	ID:       RepoTeamViewer,
	Name:     "TeamViewer",
	KeyURL:   "https://download.teamviewer.com/download/linux/signature/TeamViewer2017.asc",
	URL:      "https://linux.teamviewer.com/deb/",
	Suite:    "stable",
	Comps:    "main",
	Package:  "teamviewer",
	ProbeURL: "https://linux.teamviewer.com/deb/dists/stable/Release",
}}

// Where says in plain words where this program comes from on Ubuntu, for the
// pickers and for `dsky apps`. What installs is often not spelled the way the
// program is, and an operator checking a list wants to see that before the
// machine does.
func (s UbuntuSource) Where() string {
	switch {
	case s.Apt != "":
		return "apt: " + s.Apt
	case s.Snap != "":
		if s.Classic {
			return "snap: " + s.Snap + " (classic)"
		}
		return "snap: " + s.Snap
	case s.Flatpak != "":
		return "Flathub: " + s.Flatpak
	case s.Repo != "":
		if r, ok := UbuntuRepoByID(s.Repo); ok {
			return r.Name + "'s own apt repository"
		}
		return "the " + s.Repo + " repository"
	case s.Release != "":
		if r, ok := UbuntuReleaseByID(s.Release); ok {
			return "its published release, from github.com/" + r.Repo
		}
		return "a published release"
	}
	return ""
}

// ReleaseDSKY is DSKY's own published release.
const ReleaseDSKY = "dsky"

// UbuntuRelease is a program with no package anywhere, published as a release
// binary on GitHub: rule 5, and a last resort, because nothing updates it with
// the system afterwards. It is here at all because the alternative for such a
// program is the vendor's `curl … | sh`, and piping an unpinned script from a
// branch into a root shell is the one thing DSKY refuses to do anywhere else.
//
// What makes it acceptable is the same thing that makes the OS catalog
// acceptable: the bytes come from the vendor and are checked on arrival,
// against the checksum file published beside them in the same release.
type UbuntuRelease struct {
	ID   string
	Name string
	Repo string // owner/repo on GitHub
	// Asset is the file to fetch, and Sums the checksum file beside it in
	// the same release. "{tag}" and "{arch}" are filled in — the tag from
	// whichever release is current, the architecture from dpkg.
	Asset string
	Sums  string
	// Binary is what it is installed as in /usr/local/bin, which is on every
	// user's PATH; Aliases are other names for the same file.
	Binary  string
	Aliases []string
	// A launcher, for the desktop installs. Icon is an asset in the same
	// release, and is skipped if it cannot be fetched.
	Icon    string
	Exec    string
	Comment string
}

var ubuntuReleases = []UbuntuRelease{{
	ID:      ReleaseDSKY,
	Name:    "DSKY",
	Repo:    "uplinkresearch/dsky",
	Asset:   "dsky-{tag}-linux-{arch}",
	Sums:    "SHA256SUMS.txt",
	Binary:  "dsky",
	Aliases: []string{"compose"},
	Icon:    "dsky.png",
	Exec:    "dsky app",
	Comment: "Build and flash bootable OS installers",
}}

// UbuntuReleaseByID returns the release a program's Release field names.
func UbuntuReleaseByID(id string) (UbuntuRelease, bool) {
	for _, r := range ubuntuReleases {
		if r.ID == id {
			return r, true
		}
	}
	return UbuntuRelease{}, false
}

// UbuntuRepoByID returns the repository a program's Repo field names.
func UbuntuRepoByID(id string) (UbuntuRepo, bool) {
	for _, r := range ubuntuRepos {
		if r.ID == id {
			return r, true
		}
	}
	return UbuntuRepo{}, false
}

var ubuntu = map[string]UbuntuSource{
	"chrome":      {Repo: RepoGoogleChrome},
	"firefox":     {Snap: "firefox"},
	"brave":       {Snap: "brave"},
	"opera":       {Snap: "opera"},
	"vivaldi":     {Snap: "vivaldi"},
	"librewolf":   {Flatpak: "io.gitlab.librewolf-community"},
	"slack":       {Snap: "slack"},
	"thunderbird": {Snap: "thunderbird"},
	"discord":     {Snap: "discord"},
	"signal":      {Snap: "signal-desktop"},
	"telegram":    {Snap: "telegram-desktop"},

	"libreoffice": {Apt: "libreoffice"},
	"onlyoffice":  {Snap: "onlyoffice-desktopeditors"},
	"obsidian":    {Flatpak: "md.obsidian.Obsidian"},
	"calibre":     {Apt: "calibre"},

	"bitwarden": {Snap: "bitwarden"},
	"1password": {Flatpak: "com.onepassword.OnePassword"},
	"keepassxc": {Apt: "keepassxc"},

	"vlc":       {Apt: "vlc"},
	"spotify":   {Snap: "spotify"},
	"gimp":      {Apt: "gimp"},
	"obs":       {Apt: "obs-studio"},
	"audacity":  {Apt: "audacity"},
	"handbrake": {Apt: "handbrake"},
	"inkscape":  {Apt: "inkscape"},
	"blender":   {Apt: "blender"},
	"plex":      {Snap: "plex-desktop"},

	"tailscale":     {Snap: "tailscale"},
	"wireguard":     {Apt: "wireguard-tools"},
	"openvpn":       {Apt: "openvpn"},
	"remotedesktop": {Apt: "remmina", Note: "installs Remmina"},
	// AnyDesk and TeamViewer publish a Linux client themselves, with their own
	// apt repository, which is rule 4 exactly as Chrome is. They were left out
	// when the table was written for having no verified Flathub app — true,
	// and beside the point, because neither vendor ships through Flathub at
	// all. Checked against each vendor's own Linux instructions.
	"anydesk":    {Repo: RepoAnyDesk},
	"teamviewer": {Repo: RepoTeamViewer},

	"wireshark": {Apt: "wireshark"},
	"nmap":      {Apt: "nmap"},
	"bleachbit": {Apt: "bleachbit"},

	"7zip":        {Apt: "7zip"},
	"qbittorrent": {Apt: "qbittorrent"},

	"java21": {Apt: "openjdk-21-jre"},

	"vscode":     {Snap: "code", Classic: true},
	"git":        {Apt: "git"},
	"python":     {Apt: "python3"},
	"powershell": {Snap: "powershell", Classic: true},
	"nodejs":     {Apt: "nodejs"},
	// Docker Desktop is not published for Ubuntu as a package; docker.io is
	// the engine, which is what the word "Docker" means on a Linux machine.
	"docker": {Apt: "docker.io", Note: "installs the Docker engine, not Docker Desktop"},
	"postman":    {Snap: "postman"},
	"putty":      {Apt: "putty"},

	"dsky": {Release: ReleaseDSKY},

	"steam":     {Apt: "steam-installer"},
	"epicgames": {Flatpak: "com.heroicgameslauncher.hgl", Note: "installs Heroic Games Launcher"},
	"gog":       {Flatpak: "com.heroicgameslauncher.hgl", Note: "installs Heroic Games Launcher"},
}

func init() {
	for i := range builtin {
		if src, ok := ubuntu[builtin[i].ID]; ok {
			src := src
			builtin[i].Ubuntu = &src
		}
	}
}

// Target is an operating system a program list is resolved for.
type Target string

const (
	TargetWindows Target = "windows"
	TargetUbuntu  Target = "ubuntu"
)

// InstallsOn reports whether this program can be put on the target.
func (a App) InstallsOn(t Target) bool {
	switch t {
	case TargetUbuntu:
		return a.Ubuntu != nil
	default:
		return a.InstallsOnWindows()
	}
}

// UbuntuPlan is a program list resolved for Ubuntu: what the installer's own
// sections take, and what the first-boot script installs.
type UbuntuPlan struct {
	Apt      []string
	Snaps    []UbuntuSnap
	Flatpaks []string
	Repos    []string
	Releases []string
}

// UbuntuSnap is one snap for the installer's snaps section.
type UbuntuSnap struct {
	Name    string
	Classic bool
}

// FirstBoot reports whether the installed system has work to do once it comes
// up. Flatpaks and vendor repositories can only be done there. So, now, can
// confirming that the programs the installer was asked for actually arrived:
// a machine whose network was late finishes its install missing them silently,
// and the first-boot pass is what notices and puts them on. So any plan at all
// gets one.
func (p UbuntuPlan) FirstBoot() bool { return !p.Empty() }

// Empty reports whether nothing is to be installed.
func (p UbuntuPlan) Empty() bool {
	return len(p.Apt) == 0 && len(p.Snaps) == 0 && len(p.Flatpaks) == 0 &&
		len(p.Repos) == 0 && len(p.Releases) == 0
}

// ResolveUbuntu turns picker ids into an Ubuntu install plan. A program with
// no Ubuntu source is named in the error rather than dropped, as for Windows:
// nobody audits a fresh machine for the program that isn't there.
func ResolveUbuntu(ids []string) (UbuntuPlan, error) {
	var p UbuntuPlan
	seen := map[string]bool{}
	add := func(list *[]string, v string) {
		if !seen[v] {
			seen[v] = true
			*list = append(*list, v)
		}
	}
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if strings.HasPrefix(id, WingetPrefix) {
			return UbuntuPlan{}, fmt.Errorf("%s is a Windows package; it can't be installed on Ubuntu", strings.TrimPrefix(id, WingetPrefix))
		}
		if setID, isSet := strings.CutPrefix(id, "set:"); isSet {
			set, ok := SetByID(setID)
			if !ok {
				return UbuntuPlan{}, fmt.Errorf("no starter set %q (there are %s)", setID, setIDs())
			}
			// A starter set is a convenience: on Ubuntu it brings what it can.
			var avail []string
			for _, sid := range set.Apps {
				if a, ok := Get(sid); ok && a.Ubuntu != nil {
					avail = append(avail, sid)
				}
			}
			sub, err := ResolveUbuntu(avail)
			if err != nil {
				return UbuntuPlan{}, err
			}
			for _, v := range sub.Apt {
				add(&p.Apt, v)
			}
			for _, sn := range sub.Snaps {
				if !seen["snap:"+sn.Name] {
					seen["snap:"+sn.Name] = true
					p.Snaps = append(p.Snaps, sn)
				}
			}
			for _, v := range sub.Flatpaks {
				add(&p.Flatpaks, v)
			}
			for _, v := range sub.Repos {
				add(&p.Repos, v)
			}
			for _, v := range sub.Releases {
				add(&p.Releases, v)
			}
			continue
		}
		a, ok := Get(id)
		if !ok {
			return UbuntuPlan{}, &UnknownError{ID: id}
		}
		if a.Ubuntu == nil {
			if a.Custom != nil {
				return UbuntuPlan{}, fmt.Errorf("%s is a Windows installer you added; it can't be installed on Ubuntu", a.Name)
			}
			return UbuntuPlan{}, &UnavailableError{ID: id, Name: a.Name, OS: "Ubuntu"}
		}
		src := a.Ubuntu
		switch {
		case src.Apt != "":
			add(&p.Apt, src.Apt)
		case src.Snap != "":
			if !seen["snap:"+src.Snap] {
				seen["snap:"+src.Snap] = true
				p.Snaps = append(p.Snaps, UbuntuSnap{Name: src.Snap, Classic: src.Classic})
			}
		case src.Flatpak != "":
			add(&p.Flatpaks, src.Flatpak)
		case src.Repo != "":
			add(&p.Repos, src.Repo)
		case src.Release != "":
			add(&p.Releases, src.Release)
		}
	}
	sort.Strings(p.Repos)
	sort.Strings(p.Releases)
	return p, nil
}

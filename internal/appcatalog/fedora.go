package appcatalog

import (
	"fmt"
	"sort"
	"strings"
)

// FedoraSource is where one program comes from on Fedora and the RHEL family.
//
// The order of preference is the same shape as Ubuntu's, and the answers are
// different often enough that this is a separate table rather than a
// translation of that one:
//
//  1. Fedora's own repositories (Dnf).
//  2. Flathub (Flatpak), publisher-verified apps only. Unlike Ubuntu, Fedora
//     is comfortable with Flatpak and Workstation ships it configured, so
//     this carries more of the list here than it does there.
//  3. The vendor's own rpm repository (Repo), where that is the vendor's
//     official channel and nothing above is.
//  4. A published release binary (Release), for a program packaged nowhere.
//
// What is *not* here is as considered as what is. Fedora ships no VLC-style
// codec packages and no Steam, and DSKY does not enable RPM Fusion behind
// anyone's back: a third-party repository that large changes what the whole
// machine gets updates from, which is a decision for the person running it,
// not a side effect of ticking a box. Where that leaves a program with no
// acceptable source it is simply not offered on Fedora, and the picker says
// so by not listing it.
//
// Every name below was checked on 2026-09-17 against Fedora 44's own metadata
// (mdapi) and the Flathub API; TestFedoraNamesLive checks them weekly.
type FedoraSource struct {
	Dnf     string `json:"dnf,omitempty"`
	Flatpak string `json:"flatpak,omitempty"`
	Repo    string `json:"repo,omitempty"`
	Release string `json:"release,omitempty"`
	// Note is shown beside the name when what installs isn't obviously the
	// program asked for.
	Note string `json:"note,omitempty"`
}

// FedoraRepo is a vendor's own rpm repository — the same vendors as the
// Ubuntu side, publishing the same programs a different way. Written as a
// .repo file, with the vendor's key imported first.
type FedoraRepo struct {
	ID      string // the Repo value in the program table, shared with Ubuntu
	Name    string
	BaseURL string
	GPGKey  string
	Package string
	// AMD64Only marks a vendor that publishes for x86_64 alone, so the script
	// says it skipped rather than failing on another architecture.
	AMD64Only bool
	// ProbeURL is a file that is there for as long as the repository is, for
	// the weekly check.
	ProbeURL string
}

var fedoraRepos = []FedoraRepo{{
	ID:        RepoGoogleChrome,
	Name:      "Google Chrome",
	BaseURL:   "https://dl.google.com/linux/chrome/rpm/stable/x86_64",
	GPGKey:    "https://dl.google.com/linux/linux_signing_key.pub",
	Package:   "google-chrome-stable",
	AMD64Only: true,
	ProbeURL:  "https://dl.google.com/linux/chrome/rpm/stable/x86_64/repodata/repomd.xml",
}, {
	ID:       RepoAnyDesk,
	Name:     "AnyDesk",
	BaseURL:  "http://rpm.anydesk.com/fedora/x86_64/",
	GPGKey:   "https://keys.anydesk.com/repos/RPM-GPG-KEY",
	Package:  "anydesk",
	ProbeURL: "http://rpm.anydesk.com/fedora/x86_64/repodata/repomd.xml",
}, {
	ID:       RepoTeamViewer,
	Name:     "TeamViewer",
	BaseURL:  "https://linux.teamviewer.com/yum/stable/main/binary-x86_64/",
	GPGKey:   "https://download.teamviewer.com/download/linux/signature/TeamViewer2017.asc",
	Package:  "teamviewer",
	ProbeURL: "https://linux.teamviewer.com/yum/stable/main/binary-x86_64/repodata/repomd.xml",
}}

// FedoraRepoByID returns the rpm repository a program's Repo field names.
func FedoraRepoByID(id string) (FedoraRepo, bool) {
	for _, r := range fedoraRepos {
		if r.ID == id {
			return r, true
		}
	}
	return FedoraRepo{}, false
}

var fedora = map[string]FedoraSource{
	// Fedora's own repositories.
	"firefox":       {Dnf: "firefox"},
	"thunderbird":   {Dnf: "thunderbird"},
	"libreoffice":   {Dnf: "libreoffice"},
	"calibre":       {Dnf: "calibre"},
	"keepassxc":     {Dnf: "keepassxc"},
	"vlc":           {Dnf: "vlc"},
	"gimp":          {Dnf: "gimp"},
	"obs":           {Dnf: "obs-studio"},
	"audacity":      {Dnf: "audacity"},
	"inkscape":      {Dnf: "inkscape"},
	"blender":       {Dnf: "blender"},
	"wireguard":     {Dnf: "wireguard-tools"},
	"openvpn":       {Dnf: "openvpn"},
	"remotedesktop": {Dnf: "remmina", Note: "installs Remmina"},
	"wireshark":     {Dnf: "wireshark"},
	"nmap":          {Dnf: "nmap"},
	"bleachbit":     {Dnf: "bleachbit"},
	"7zip":          {Dnf: "7zip"},
	"qbittorrent":   {Dnf: "qbittorrent"},
	"git":           {Dnf: "git"},
	"python":        {Dnf: "python3"},
	"putty":         {Dnf: "putty"},
	"tailscale":     {Dnf: "tailscale"},
	"docker":        {Dnf: "moby-engine", Note: "installs the Docker engine, not Docker Desktop"},
	// Fedora packages Node.js by major version rather than under a plain
	// "nodejs", so this names the stream. The weekly check is what catches it
	// when Fedora moves on.
	"nodejs": {Dnf: "nodejs24", Note: "installs Node.js 24"},

	// Flathub, publisher-verified only.
	"brave":      {Flatpak: "com.brave.Browser"},
	"discord":    {Flatpak: "com.discordapp.Discord"},
	"telegram":   {Flatpak: "org.telegram.desktop"},
	"plex":       {Flatpak: "tv.plex.PlexDesktop"},
	"bitwarden":  {Flatpak: "com.bitwarden.desktop"},
	"onlyoffice": {Flatpak: "org.onlyoffice.desktopeditors"},
	"librewolf":  {Flatpak: "io.gitlab.librewolf-community"},
	"obsidian":   {Flatpak: "md.obsidian.Obsidian"},
	"1password":  {Flatpak: "com.onepassword.OnePassword"},
	"handbrake":  {Flatpak: "fr.handbrake.ghb"},
	"epicgames":  {Flatpak: "com.heroicgameslauncher.hgl", Note: "installs Heroic Games Launcher"},
	"gog":        {Flatpak: "com.heroicgameslauncher.hgl", Note: "installs Heroic Games Launcher"},

	// The vendor's own rpm repository.
	"chrome":     {Repo: RepoGoogleChrome},
	"anydesk":    {Repo: RepoAnyDesk},
	"teamviewer": {Repo: RepoTeamViewer},

	// Published release binary.
	"dsky": {Release: ReleaseDSKY},
}

func init() {
	for i := range builtin {
		if src, ok := fedora[builtin[i].ID]; ok {
			src := src
			builtin[i].Fedora = &src
		}
	}
}

// Where says in plain words where this program comes from on Fedora.
func (s FedoraSource) Where() string {
	switch {
	case s.Dnf != "":
		return "dnf: " + s.Dnf
	case s.Flatpak != "":
		return "Flathub: " + s.Flatpak
	case s.Repo != "":
		if r, ok := FedoraRepoByID(s.Repo); ok {
			return r.Name + "'s own rpm repository"
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

// FedoraPlan is a program list resolved for Fedora: what the kickstart's
// %packages section takes, and what the first boot installs.
type FedoraPlan struct {
	Dnf      []string
	Flatpaks []string
	Repos    []string
	Releases []string
}

// FirstBoot reports whether the installed system has work to do once it is up.
func (p FedoraPlan) FirstBoot() bool { return !p.Empty() }

// Empty reports whether nothing is to be installed.
func (p FedoraPlan) Empty() bool {
	return len(p.Dnf) == 0 && len(p.Flatpaks) == 0 && len(p.Repos) == 0 && len(p.Releases) == 0
}

// ResolveFedora turns picker ids into a Fedora install plan. A program with no
// Fedora source is named in the error rather than dropped, as for the others:
// nobody audits a fresh machine for the program that isn't there.
func ResolveFedora(ids []string) (FedoraPlan, error) {
	var p FedoraPlan
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
			return FedoraPlan{}, fmt.Errorf("%s is a Windows package; it can't be installed on Fedora", strings.TrimPrefix(id, WingetPrefix))
		}
		if setID, isSet := strings.CutPrefix(id, "set:"); isSet {
			set, ok := SetByID(setID)
			if !ok {
				return FedoraPlan{}, fmt.Errorf("no starter set %q (there are %s)", setID, setIDs())
			}
			// A starter set is a convenience: on Fedora it brings what it can.
			var avail []string
			for _, sid := range set.Apps {
				if a, ok := Get(sid); ok && a.Fedora != nil {
					avail = append(avail, sid)
				}
			}
			sub, err := ResolveFedora(avail)
			if err != nil {
				return FedoraPlan{}, err
			}
			for _, v := range sub.Dnf {
				add(&p.Dnf, v)
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
			return FedoraPlan{}, &UnknownError{ID: id}
		}
		if a.Fedora == nil {
			if a.Custom != nil {
				return FedoraPlan{}, fmt.Errorf("%s is a Windows installer you added; it can't be installed on Fedora", a.Name)
			}
			return FedoraPlan{}, &UnavailableError{ID: id, Name: a.Name, OS: "Fedora"}
		}
		src := a.Fedora
		switch {
		case src.Dnf != "":
			add(&p.Dnf, src.Dnf)
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

// Name is what to call this operating system in a sentence.
func (t Target) Name() string {
	switch t {
	case TargetUbuntu:
		return "Ubuntu"
	case TargetFedora:
		return "Fedora"
	default:
		return "Windows"
	}
}

// Targets are the operating systems a program list can be resolved for, in
// the order the pickers show them.
var Targets = []Target{TargetWindows, TargetUbuntu, TargetFedora}

// WhereOn says in plain words where this program comes from on the given
// operating system, or "" if it is not offered there.
func (a App) WhereOn(t Target) string {
	switch t {
	case TargetUbuntu:
		if a.Ubuntu != nil {
			return a.Ubuntu.Where()
		}
	case TargetFedora:
		if a.Fedora != nil {
			return a.Fedora.Where()
		}
	case TargetWindows:
		if a.Custom != nil {
			return "your own installer, " + a.Custom.Filename
		}
		if a.Winget != "" {
			return "winget: " + a.Winget
		}
	}
	return ""
}

// NoteOn is the caveat shown beside the name on that operating system, when
// what installs is not obviously the program asked for.
func (a App) NoteOn(t Target) string {
	switch t {
	case TargetUbuntu:
		if a.Ubuntu != nil {
			return a.Ubuntu.Note
		}
	case TargetFedora:
		if a.Fedora != nil {
			return a.Fedora.Note
		}
	}
	return ""
}

// InstallsOnly names the operating systems this program can be installed on,
// when that is not all of them — so a list can say "Ubuntu and Fedora" rather
// than leaving somebody to find out at first boot.
func (a App) InstallsOnly() string {
	var on []string
	for _, t := range Targets {
		if a.InstallsOn(t) {
			on = append(on, t.Name())
		}
	}
	if len(on) == 0 || len(on) == len(Targets) {
		return ""
	}
	if len(on) == 1 {
		return on[0] + " only"
	}
	return strings.Join(on[:len(on)-1], ", ") + " and " + on[len(on)-1] + " only"
}

// Package appcatalog is the list of programs Quick Install can add to a
// machine: a built-in set naming packages in the platform's own package
// manager — winget on Windows, apt on Debian/Ubuntu, so installers come from
// the vendor at first boot and nothing large rides on the media — plus any
// installers the operator has added themselves, which do ride on the media
// because no package manager has them. See custom.go.
package appcatalog

import (
	"fmt"
	"strings"
)

// App is one installable program.
type App struct {
	ID       string // short picker id, e.g. "chrome"
	Name     string
	Category string
	Winget   string // winget package id; empty = not available on Windows
	// Ubuntu is where the program comes from on Ubuntu; nil = not offered
	// there. Filled from the ubuntu table in ubuntu.go.
	Ubuntu *UbuntuSource
	// Fedora is the same for Fedora and the RHEL family, from fedora.go. The
	// two are separate tables rather than one with translations, because the
	// right answer differs often enough to be worth stating twice: Fedora
	// ships no Steam and no codec packages, and leans on Flathub where Ubuntu
	// leans on snaps.
	Fedora *FedoraSource
	Notes  string
	// Custom is set when this is an installer the operator supplied rather
	// than a package from winget. It carries the library blob and the silent
	// switches; see custom.go.
	Custom *Custom

	// What someone gets differs from "installed for everyone, ready to use"
	// in these ways, and the picker says so beside the name.
	PerUser bool // the package only ships a per-user installer
	Licence bool // needs a paid licence, subscription or account to be useful
	Large   bool // a download big enough that first boot visibly waits for it
	// Desktop: the program wants a graphical session, so a server entry does
	// not offer it. Ubuntu Server and Fedora Server install no desktop at all
	// -- no X, no Wayland, no GNOME -- and the picker was happily putting VLC,
	// Chrome and GIMP on them: the packages install cleanly and then there is
	// nothing to run them on. Anything that is only on Flathub is a desktop
	// program by construction, and a test below keeps that true as the tables
	// grow.
	Desktop bool
}

// InstallsOnWindows reports whether this program can actually be put on a
// Windows machine — either winget knows it, or the operator supplied the
// installer. The pickers filter on this, because offering an option that
// cannot run is worse than not offering it at all.
func (a App) InstallsOnWindows() bool { return a.Winget != "" || a.Custom != nil }

// Labels are the short phrases the pickers show beside a program's name.
func (a App) Labels() []string {
	var out []string
	if a.PerUser {
		out = append(out, "installs for the first account only")
	}
	if a.Licence {
		out = append(out, "needs a licence or account")
	}
	if a.Large {
		out = append(out, "large download")
	}
	if a.Desktop {
		out = append(out, "needs a desktop")
	}
	return out
}

// builtin is the shipped list, ordered by category then roughly by how often
// it is wanted — the order the picker shows.
//
// Every winget id was checked against microsoft/winget-pkgs, spelled as the
// package's own manifest spells it, and the weekly catalog-health run checks
// them again (TestWingetIDsLive). PerUser comes from the same manifests: it is
// set where every installer the package lists is Scope: user, which is what
// makes apps.ps1's machine-wide attempt fall back to the first account.
//
// Microsoft Edge is left out because Windows already has it.
var builtin = []App{
	{ID: "chrome", Name: "Google Chrome", Category: "Browsers", Winget: "Google.Chrome", Desktop: true},
	{ID: "firefox", Name: "Mozilla Firefox", Category: "Browsers", Winget: "Mozilla.Firefox", Desktop: true},
	{ID: "brave", Name: "Brave", Category: "Browsers", Winget: "Brave.Brave", Desktop: true},
	{ID: "opera", Name: "Opera", Category: "Browsers", Winget: "Opera.Opera", Desktop: true},
	{ID: "vivaldi", Name: "Vivaldi", Category: "Browsers", Winget: "Vivaldi.Vivaldi", Desktop: true},
	{ID: "librewolf", Name: "LibreWolf", Category: "Browsers", Winget: "LibreWolf.LibreWolf", Desktop: true},

	{ID: "zoom", Name: "Zoom", Category: "Communication", Winget: "Zoom.Zoom", Desktop: true},
	{ID: "teams", Name: "Microsoft Teams", Category: "Communication", Winget: "Microsoft.Teams", Desktop: true},
	{ID: "slack", Name: "Slack", Category: "Communication", Winget: "SlackTechnologies.Slack", PerUser: true, Desktop: true},
	{ID: "thunderbird", Name: "Mozilla Thunderbird", Category: "Communication", Winget: "Mozilla.Thunderbird", Desktop: true},
	{ID: "discord", Name: "Discord", Category: "Communication", Winget: "Discord.Discord", PerUser: true, Desktop: true},
	{ID: "signal", Name: "Signal", Category: "Communication", Winget: "OpenWhisperSystems.Signal", PerUser: true, Desktop: true},
	{ID: "telegram", Name: "Telegram Desktop", Category: "Communication", Winget: "Telegram.TelegramDesktop", PerUser: true, Desktop: true},
	{ID: "webex", Name: "Webex", Category: "Communication", Winget: "Cisco.Webex"},

	{ID: "adobereader", Name: "Adobe Acrobat Reader", Category: "Documents", Winget: "Adobe.Acrobat.Reader.64-bit", Desktop: true},
	{ID: "office", Name: "Microsoft 365 Apps (Word, Excel, Outlook…)", Category: "Documents", Winget: "Microsoft.Office", Licence: true, Large: true},
	{ID: "libreoffice", Name: "LibreOffice", Category: "Documents", Winget: "TheDocumentFoundation.LibreOffice", Desktop: true},
	{ID: "onlyoffice", Name: "ONLYOFFICE Desktop Editors", Category: "Documents", Winget: "ONLYOFFICE.DesktopEditors", Desktop: true},
	// Apache OpenOffice is the older project LibreOffice forked from, and
	// still what a lot of offices ask for by name. Both are offered because
	// asking for one and being given the other is a support call.
	{ID: "openoffice", Name: "Apache OpenOffice", Category: "Documents", Winget: "Apache.OpenOffice", Desktop: true},
	{ID: "foxitreader", Name: "Foxit PDF Reader", Category: "Documents", Winget: "Foxit.FoxitReader", Desktop: true},
	{ID: "pdf24", Name: "PDF24 Creator", Category: "Documents", Winget: "geeksoftwareGmbH.PDF24Creator"},
	{ID: "obsidian", Name: "Obsidian", Category: "Documents", Winget: "Obsidian.Obsidian", Desktop: true},
	{ID: "notion", Name: "Notion", Category: "Documents", Winget: "Notion.Notion", PerUser: true},
	{ID: "zotero", Name: "Zotero", Category: "Documents", Winget: "DigitalScholar.Zotero"},
	{ID: "calibre", Name: "calibre (e-books)", Category: "Documents", Winget: "calibre.calibre", Desktop: true},

	{ID: "bitwarden", Name: "Bitwarden", Category: "Passwords & security", Winget: "Bitwarden.Bitwarden", Desktop: true},
	{ID: "1password", Name: "1Password", Category: "Passwords & security", Winget: "AgileBits.1Password", Licence: true, Desktop: true},
	{ID: "keepassxc", Name: "KeePassXC", Category: "Passwords & security", Winget: "KeePassXCTeam.KeePassXC", Desktop: true},
	{ID: "malwarebytes", Name: "Malwarebytes", Category: "Passwords & security", Winget: "Malwarebytes.Malwarebytes"},

	{ID: "googledrive", Name: "Google Drive", Category: "Cloud storage", Winget: "Google.GoogleDrive"},
	{ID: "dropbox", Name: "Dropbox", Category: "Cloud storage", Winget: "Dropbox.Dropbox"},
	{ID: "onedrive", Name: "Microsoft OneDrive", Category: "Cloud storage", Winget: "Microsoft.OneDrive"},
	{ID: "box", Name: "Box Drive", Category: "Cloud storage", Winget: "Box.Box"},

	{ID: "vlc", Name: "VLC media player", Category: "Media", Winget: "VideoLAN.VLC", Desktop: true},
	{ID: "spotify", Name: "Spotify", Category: "Media", Winget: "Spotify.Spotify", PerUser: true, Desktop: true},
	{ID: "gimp", Name: "GIMP", Category: "Media", Winget: "GIMP.GIMP", Desktop: true},
	{ID: "paintdotnet", Name: "Paint.NET", Category: "Media", Winget: "dotPDN.PaintDotNet", Desktop: true},
	{ID: "obs", Name: "OBS Studio", Category: "Media", Winget: "OBSProject.OBSStudio", Desktop: true},
	{ID: "audacity", Name: "Audacity", Category: "Media", Winget: "Audacity.Audacity", Desktop: true},
	{ID: "handbrake", Name: "HandBrake", Category: "Media", Winget: "HandBrake.HandBrake", Desktop: true},
	{ID: "inkscape", Name: "Inkscape", Category: "Media", Winget: "Inkscape.Inkscape", Desktop: true},
	{ID: "blender", Name: "Blender", Category: "Media", Winget: "BlenderFoundation.Blender", Large: true, Desktop: true},
	{ID: "klite", Name: "K-Lite Codec Pack Standard", Category: "Media", Winget: "CodecGuide.K-LiteCodecPack.Standard"},
	{ID: "irfanview", Name: "IrfanView", Category: "Media", Winget: "IrfanSkiljan.IrfanView", Desktop: true},
	{ID: "greenshot", Name: "Greenshot", Category: "Media", Winget: "Greenshot.Greenshot"},
	{ID: "plex", Name: "Plex", Category: "Media", Winget: "Plex.Plex", Desktop: true},

	{ID: "teamviewer", Name: "TeamViewer", Category: "Remote access & VPN", Winget: "TeamViewer.TeamViewer", Licence: true, Desktop: true},
	{ID: "anydesk", Name: "AnyDesk", Category: "Remote access & VPN", Winget: "AnyDesk.AnyDesk", Licence: true, Desktop: true},
	{ID: "tailscale", Name: "Tailscale", Category: "Remote access & VPN", Winget: "Tailscale.Tailscale"},
	{ID: "wireguard", Name: "WireGuard", Category: "Remote access & VPN", Winget: "WireGuard.WireGuard"},
	{ID: "openvpn", Name: "OpenVPN Connect", Category: "Remote access & VPN", Winget: "OpenVPNTechnologies.OpenVPN"},
	{ID: "remotedesktop", Name: "Remote Desktop client", Category: "Remote access & VPN", Winget: "Microsoft.RemoteDesktopClient", Desktop: true},
	{ID: "mremoteng", Name: "mRemoteNG", Category: "Remote access & VPN", Winget: "mRemoteNG.mRemoteNG"},

	{ID: "sysinternals", Name: "Sysinternals Suite", Category: "IT tools", Winget: "Microsoft.Sysinternals.Suite"},
	{ID: "wireshark", Name: "Wireshark", Category: "IT tools", Winget: "WiresharkFoundation.Wireshark", Desktop: true},
	{ID: "nmap", Name: "Nmap", Category: "IT tools", Winget: "Insecure.Nmap", PerUser: true},
	{ID: "wiztree", Name: "WizTree", Category: "IT tools", Winget: "AntibodySoftware.WizTree"},
	{ID: "windirstat", Name: "WinDirStat", Category: "IT tools", Winget: "WinDirStat.WinDirStat"},
	{ID: "rufus", Name: "Rufus", Category: "IT tools", Winget: "Rufus.Rufus", Desktop: true},
	{ID: "cpuz", Name: "CPU-Z", Category: "IT tools", Winget: "CPUID.CPU-Z"},
	{ID: "hwmonitor", Name: "HWMonitor", Category: "IT tools", Winget: "CPUID.HWMonitor"},
	{ID: "hwinfo", Name: "HWiNFO", Category: "IT tools", Winget: "REALiX.HWiNFO"},
	{ID: "crystaldiskinfo", Name: "CrystalDiskInfo", Category: "IT tools", Winget: "CrystalDewWorld.CrystalDiskInfo"},
	{ID: "bleachbit", Name: "BleachBit", Category: "IT tools", Winget: "BleachBit.BleachBit", Desktop: true},
	// DSKY installs itself, which is less strange than it sounds: a bench
	// machine being imaged is usually the machine that images the next one.
	// Linux only for now — the Windows first boot installs through winget and
	// the operator's own installers, and DSKY is in neither.
	{ID: "dsky", Name: "DSKY", Category: "IT tools"},
	{ID: "winmerge", Name: "WinMerge", Category: "IT tools", Winget: "WinMerge.WinMerge"},

	{ID: "7zip", Name: "7-Zip", Category: "Utilities", Winget: "7zip.7zip"},
	{ID: "notepadplusplus", Name: "Notepad++", Category: "Utilities", Winget: "Notepad++.Notepad++", Desktop: true},
	{ID: "powertoys", Name: "Microsoft PowerToys", Category: "Utilities", Winget: "Microsoft.PowerToys"},
	{ID: "sharex", Name: "ShareX", Category: "Utilities", Winget: "ShareX.ShareX"},
	{ID: "everything", Name: "Everything (instant file search)", Category: "Utilities", Winget: "voidtools.Everything"},
	{ID: "flowlauncher", Name: "Flow Launcher", Category: "Utilities", Winget: "Flow-Launcher.Flow-Launcher", PerUser: true},
	{ID: "qbittorrent", Name: "qBittorrent", Category: "Utilities", Winget: "qBittorrent.qBittorrent", Desktop: true},

	{ID: "vcredist", Name: "Visual C++ Redistributable (2015 and later, x64)", Category: "Runtimes", Winget: "Microsoft.VCRedist.2015+.x64"},
	{ID: "dotnet8desktop", Name: ".NET 8 Desktop Runtime", Category: "Runtimes", Winget: "Microsoft.DotNet.DesktopRuntime.8"},
	{ID: "java21", Name: "Java 21 (Eclipse Temurin JRE)", Category: "Runtimes", Winget: "EclipseAdoptium.Temurin.21.JRE"},
	{ID: "oraclejava", Name: "Oracle Java 8", Category: "Runtimes", Winget: "Oracle.JavaRuntimeEnvironment"},

	{ID: "vscode", Name: "Visual Studio Code", Category: "Development", Winget: "Microsoft.VisualStudioCode", Desktop: true},
	{ID: "git", Name: "Git", Category: "Development", Winget: "Git.Git"},
	{ID: "python", Name: "Python 3", Category: "Development", Winget: "Python.Python.3.13"},
	{ID: "powershell", Name: "PowerShell 7", Category: "Development", Winget: "Microsoft.PowerShell"},
	{ID: "windowsterminal", Name: "Windows Terminal", Category: "Development", Winget: "Microsoft.WindowsTerminal", PerUser: true},
	{ID: "nodejs", Name: "Node.js LTS", Category: "Development", Winget: "OpenJS.NodeJS.LTS"},
	{ID: "docker", Name: "Docker Desktop", Category: "Development", Winget: "Docker.DockerDesktop", Licence: true, Large: true},
	{ID: "githubdesktop", Name: "GitHub Desktop", Category: "Development", Winget: "GitHub.GitHubDesktop"},
	{ID: "jetbrainstoolbox", Name: "JetBrains Toolbox", Category: "Development", Winget: "JetBrains.Toolbox", PerUser: true},
	{ID: "postman", Name: "Postman", Category: "Development", Winget: "Postman.Postman", PerUser: true, Desktop: true},
	{ID: "winscp", Name: "WinSCP", Category: "Development", Winget: "WinSCP.WinSCP"},
	{ID: "putty", Name: "PuTTY", Category: "Development", Winget: "PuTTY.PuTTY", Desktop: true},

	{ID: "steam", Name: "Steam", Category: "Games", Winget: "Valve.Steam", Desktop: true},
	{ID: "epicgames", Name: "Epic Games Launcher", Category: "Games", Winget: "EpicGames.EpicGamesLauncher", Desktop: true},
	{ID: "gog", Name: "GOG Galaxy", Category: "Games", Winget: "GOG.Galaxy", Desktop: true},
	{ID: "eaapp", Name: "EA app", Category: "Games", Winget: "ElectronicArts.EADesktop"},
	{ID: "ubisoftconnect", Name: "Ubisoft Connect", Category: "Games", Winget: "Ubisoft.Connect"},
}

// Set is a starter group of programs: one click adds them all, and they can
// then be removed one by one like any other pick.
type Set struct {
	ID   string
	Name string
	Apps []string // picker ids
}

// Sets are the starter groups. Games and torrent clients are deliberately in
// none of them: fine on a home reinstall, unwanted on most business PCs, and
// easy to add by name.
var Sets = []Set{
	{ID: "business", Name: "Business PC", Apps: []string{"chrome", "adobereader", "7zip", "zoom", "teams", "vcredist"}},
	{ID: "home", Name: "Home PC", Apps: []string{"chrome", "vlc", "7zip", "spotify", "discord"}},
	{ID: "it", Name: "IT technician", Apps: []string{"sysinternals", "wireshark", "wiztree", "notepadplusplus", "powershell", "putty"}},
}

// SetByID returns the starter set with this id.
func SetByID(id string) (Set, bool) {
	for _, s := range Sets {
		if strings.EqualFold(s.ID, id) {
			return s, true
		}
	}
	return Set{}, false
}

func setIDs() string {
	var ids []string
	for _, s := range Sets {
		ids = append(ids, "set:"+s.ID)
	}
	return strings.Join(ids, ", ")
}

// Elsewhere names programs people look for that winget does not have, so a
// search for one can say where to get it instead of finding nothing. Checked
// against microsoft/winget-pkgs at the same time as the list above.
type Elsewhere struct {
	Name  string
	Words []string // lowercase words that should find it
	Note  string
}

// NotInWinget is what a search that finds no program checks before saying
// nothing matched.
var NotInWinget = []Elsewhere{
	{Name: "Weave", Words: []string{"weave"}, Note: "Weave's desktop app isn't in winget — it is downloaded from the Weave portal, signed in. Get the installer from weavehelp.com and add it under Your installers."},
	{Name: "RustDesk", Words: []string{"rustdesk"}, Note: "RustDesk isn't in winget. Download its installer from rustdesk.com and add it under Your installers."},
	{Name: "FortiClient VPN", Words: []string{"forticlient", "fortinet"}, Note: "FortiClient VPN isn't in winget. Download its installer from fortinet.com and add it under Your installers."},
	{Name: "NVIDIA app", Words: []string{"nvidia", "geforce"}, Note: "The NVIDIA app isn't in winget. Download its installer from nvidia.com and add it under Your installers."},
	{Name: "Microsoft Edge", Words: []string{"edge"}, Note: "Microsoft Edge comes with Windows, so there is nothing to install."},
}

// Catalog returns the built-in program list followed by the operator's own
// installers, so every picker shows both without knowing the difference.
//
// An installer the operator added under an id that later became a built-in one
// wins: they chose it, and it is what their recipes already mean by that id.
func Catalog() []App {
	cs := CustomApps()
	out := make([]App, 0, len(builtin)+len(cs))
	mine := map[string]bool{}
	for _, c := range cs {
		mine[strings.ToLower(c.ID)] = true
	}
	for _, a := range builtin {
		if !mine[strings.ToLower(a.ID)] {
			out = append(out, a)
		}
	}
	for i := range cs {
		c := cs[i]
		cat := c.Category
		if cat == "" {
			cat = CustomCategory
		}
		out = append(out, App{ID: c.ID, Name: c.Name, Category: cat, Custom: &c})
	}
	return out
}

// Get returns the app with this picker id, or false.
func Get(id string) (App, bool) {
	for _, a := range Catalog() {
		if strings.EqualFold(a.ID, id) {
			return a, true
		}
	}
	return App{}, false
}

// Resolve splits picker ids — built-in and operator ids, a starter set as
// "set:business", or any winget package as "winget:Publisher.Package" — into the two things that have to happen at first
// boot: winget package ids to install from the network, and operator-supplied
// installers that ride on the media and are run directly. Order is preserved
// within each.
//
// An unknown id, or one with no way to install on Windows, is named in the
// error rather than silently dropped — a program the operator asked for and
// did not get is worth failing the build over, because nobody audits a
// freshly imaged machine for a missing agent.
func Resolve(ids []string) (winget []string, custom []Custom, err error) {
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if setID, isSet := strings.CutPrefix(id, "set:"); isSet {
			set, ok := SetByID(setID)
			if !ok {
				return nil, nil, fmt.Errorf("no starter set %q (there are %s)", setID, setIDs())
			}
			w, c, err := Resolve(set.Apps)
			if err != nil {
				return nil, nil, err
			}
			winget, custom = append(winget, w...), append(custom, c...)
			continue
		}
		if pkg, typed := strings.CutPrefix(id, WingetPrefix); typed {
			if !ValidWingetID(pkg) {
				return nil, nil, fmt.Errorf("%q is not a winget package id (they look like Publisher.Package, such as Brave.Brave)", pkg)
			}
			winget = append(winget, pkg)
			continue
		}
		a, ok := Get(id)
		if !ok {
			return nil, nil, &UnknownError{ID: id}
		}
		switch {
		case a.Custom != nil:
			custom = append(custom, *a.Custom)
		case a.Winget != "":
			winget = append(winget, a.Winget)
		default:
			return nil, nil, &UnavailableError{ID: id, Name: a.Name, OS: "Windows"}
		}
	}
	return winget, custom, nil
}

// WingetIDs maps picker ids to winget package ids. Kept for callers that only
// care about the network-installed half; Resolve is what a build wants.
func WingetIDs(ids []string) ([]string, error) {
	w, _, err := Resolve(ids)
	return w, err
}

// UnknownError is an id that is not in the catalog.
type UnknownError struct{ ID string }

func (e *UnknownError) Error() string {
	return "unknown program " + e.ID + " — see `dsky apps` for the list, or name any winget package as winget:Publisher.Package"
}

// UnavailableError is a known program with no package for the target OS.
type UnavailableError struct{ ID, Name, OS string }

func (e *UnavailableError) Error() string {
	return e.Name + " (" + e.ID + ") has no " + e.OS + " package in the catalog"
}

// Categories lists the distinct categories in catalog order.
func Categories() []string {
	var out []string
	seen := map[string]bool{}
	for _, a := range Catalog() {
		if !seen[a.Category] {
			seen[a.Category] = true
			out = append(out, a.Category)
		}
	}
	return out
}

package migrate

import (
	"regexp"
	"strings"
	"sync"

	"github.com/uplinkresearch/dsky/internal/appcatalog"
)

// The table DSKY ships is not a second list of software: it is the list the
// app picker already has. Ninety-odd packages, each spelled the way its own
// winget manifest spells it, each re-checked against winget-pkgs every week
// by the catalog-health run. Building the resolver's global table from that
// means the two cannot drift, and a package added for the picker helps a
// migration the same day.
//
// What is added here is only what a migration needs and a picker does not:
//
//   - The runtimes nobody chooses but everything needs -- Visual C++, .NET,
//     WebView2 -- which appear in every machine's uninstall list and must be
//     installed before the software that wants them.
//   - The software DSKY knows is not in winget at all, so that it comes out
//     as "a person installs this" with the note saying where from, rather
//     than as an application nobody could place.
//   - Edge, which comes with Windows: on a rebuilt machine there is nothing
//     to install, and saying so is better than leaving it in the list.

var (
	builtinOnce  sync.Once
	builtinTable *Table
)

// BuiltinTable is DSKY's own mapping table.
func BuiltinTable() *Table {
	builtinOnce.Do(func() { builtinTable = buildBuiltinTable() })
	return builtinTable
}

// KnownApps is what the name matcher matches against: the same packages, as
// names rather than patterns.
func KnownApps() []KnownApp {
	out := []KnownApp{}
	for _, a := range appcatalog.Catalog() {
		if a.Winget == "" || !a.InstallsOnWindows() {
			continue
		}
		out = append(out, KnownApp{Name: pickerName(a.Name), Ref: a.Winget, Method: MethodWinget})
	}
	for _, p := range prereqPackages {
		out = append(out, KnownApp{Name: p.name, Ref: p.ref, Method: MethodWinget})
	}
	return out
}

// pickerName strips the explaining half of a picker label: the picker says
// "Microsoft 365 Apps (Word, Excel, Outlook…)" because somebody is choosing
// from a list, and a machine's registry says "Microsoft 365 Apps for
// business".
func pickerName(name string) string {
	if i := strings.Index(name, " ("); i > 0 {
		name = name[:i]
	}
	return strings.TrimSpace(name)
}

// prereqPackages are the runtimes that are on every machine and that nobody
// picks from a list. Their ids are checked by TestBuiltinTableIDsLive, which
// runs with the rest of the live catalog checks each week.
var prereqPackages = []struct {
	name, ref, match string
}{
	{"Microsoft Visual C++ Redistributable", "Microsoft.VCRedist.2015+.x64", `^Microsoft Visual C\+\+ 20(1[5-9]|2[0-9]).*[Rr]edistributable.*\(x64\)`},
	{"Microsoft Visual C++ Redistributable (x86)", "Microsoft.VCRedist.2015+.x86", `^Microsoft Visual C\+\+ 20(1[5-9]|2[0-9]).*[Rr]edistributable.*\(x86\)`},
	{"Microsoft .NET Desktop Runtime 8", "Microsoft.DotNet.DesktopRuntime.8", `^Microsoft Windows Desktop Runtime - 8\.`},
	{"Microsoft .NET Desktop Runtime 6", "Microsoft.DotNet.DesktopRuntime.6", `^Microsoft Windows Desktop Runtime - 6\.`},
	{"Microsoft Edge WebView2 Runtime", "Microsoft.EdgeWebView2Runtime", `^Microsoft Edge WebView2 Runtime$`},
}

// comesWithWindows is software a rebuilt machine has without being asked, so
// there is nothing to install and nothing for the review to decide.
var comesWithWindows = []struct{ name, match, why string }{
	{"Microsoft Edge", `^Microsoft Edge$`, "Edge comes with Windows"},
	{"Microsoft Edge Update", `^Microsoft Edge Update$`, "part of Edge, which comes with Windows"},
	{"Microsoft OneDrive", `^Microsoft OneDrive$`, "OneDrive comes with Windows 11"},
	{"Microsoft Teams (personal)", `^Microsoft Teams$`, "Windows 11 ships the personal Teams; the work one is in the list"},
}

// oemUtility is the software a PC maker puts on the machines it sells. A
// rebuilt machine is usually a different make, and where it is the same make
// its own utility arrives with its drivers; either way nobody chose these and
// nobody misses them. DSKY's debloat step already removes several of them.
//
// Dropped, not hidden: they appear in the report under what was deliberately
// left behind, with this as the reason.
//
// The antivirus trial a machine shipped with is not here, though it is the
// same kind of thing: a practice that pays for that antivirus has the same
// name in its uninstall list as one that has never opened it, and dropping
// somebody's paid security software because it arrived with the PC is not a
// guess this tool gets to make.
var oemUtility = []string{
	`^Dell (SupportAssist|Optimizer|Digital Delivery|Update|Power Manager|Command)`,
	`^HP (Support Assistant|Documentation|Sure |Connection Optimizer|JumpStarts|Audio Switch|Programmable Key)`,
	`^Lenovo (Vantage|Welcome|Now|Utility|System Update|Smart)`,
	`^(ASUS|MyASUS|ASUSTeK)`,
	`^Acer (Care Center|Collection|Product Registration|Jumpstart)`,
	`^Intel(\(R\))? (Driver & Support Assistant|Optane|Rapid Storage)`,
	`^Killer (Performance Suite|Control Center)`,
}

// inboxApp is what Windows installs for every account whether anybody wants
// it or not. The scanner already drops the ones the image says it provisions;
// this is the backstop for machines that could not be asked, and for
// manifests written by hand.
var inboxApp = []string{
	`^Microsoft\.(MicrosoftSolitaireCollection|BingWeather|BingNews|GetHelp|Getstarted|Microsoft3DViewer|MicrosoftOfficeHub|MicrosoftStickyNotes|MixedReality\.Portal|People|SkypeApp|Todos|WindowsMaps|Xbox|YourPhone|ZuneMusic|ZuneVideo|Paint|PowerAutomateDesktop|Clipchamp)`,
	`^(Microsoft )?(Solitaire Collection|Mixed Reality Portal|Phone Link|Clipchamp|Paint 3D|3D Viewer)$`,
	`^Clipchamp\.Clipchamp`,
	`^MicrosoftCorporationII\.`,
}

func buildBuiltinTable() *Table {
	t := &Table{SchemaVersion: MappingVersion, Site: "DSKY"}

	// The runtimes first: their names are specific, and a general pattern
	// further down must not claim them.
	for _, p := range prereqPackages {
		t.Entries = append(t.Entries, Mapping{
			Match:      Match{DisplayName: p.match},
			Resolution: Resolution{Status: StatusResolved, Method: MethodWinget, Ref: p.ref, Order: OrderPrereq},
			Note:       "a runtime other software needs, installed first",
		})
	}
	for _, pattern := range oemUtility {
		t.Entries = append(t.Entries, Mapping{
			Match:      Match{DisplayName: pattern},
			Resolution: Resolution{Status: StatusDropped},
			Note:       "the PC maker's own utility; the new machine does not need it",
		})
	}
	for _, pattern := range inboxApp {
		t.Entries = append(t.Entries, Mapping{
			Match:      Match{DisplayName: pattern},
			Resolution: Resolution{Status: StatusDropped},
			Note:       "Windows installs this itself",
		})
	}
	for _, w := range comesWithWindows {
		t.Entries = append(t.Entries, Mapping{
			Match:      Match{DisplayName: w.match},
			Resolution: Resolution{Status: StatusDropped},
			Note:       w.why,
		})
	}
	// Then the picker's packages, matched on their own names.
	for _, a := range appcatalog.Catalog() {
		if a.Winget == "" || !a.InstallsOnWindows() {
			continue
		}
		name := pickerName(a.Name)
		t.Entries = append(t.Entries, Mapping{
			Match:      Match{DisplayName: `^` + regexp.QuoteMeta(name)},
			Resolution: Resolution{Status: StatusResolved, Method: MethodWinget, Ref: a.Winget, Order: OrderDefault},
		})
	}
	// And the software DSKY knows winget does not have: a person installs it,
	// and the note says where it comes from.
	for _, e := range appcatalog.NotInWinget {
		if strings.EqualFold(e.Name, "Microsoft Edge") {
			continue // already covered above, as something not to install
		}
		t.Entries = append(t.Entries, Mapping{
			Match:      Match{DisplayName: `^` + regexp.QuoteMeta(e.Name)},
			Resolution: Resolution{Status: StatusResolved, Method: MethodManual, Ref: e.Note, Order: OrderMax},
			Note:       "winget does not have it",
		})
	}
	return t
}

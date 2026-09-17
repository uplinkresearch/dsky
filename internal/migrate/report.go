package migrate

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"strings"
	"time"
)

// The report is the manifest as a page, and it is the first thing this feature
// ships that somebody outside the room reads: an office manager deciding
// whether the replacement PC is going to have what they need, an owner asked
// to approve a rebuild. So it is generated, never edited -- an edited report
// would disagree with the plan that gets built -- and it is one file with
// nothing linked from it, because it gets emailed, copied onto a stick and
// opened on a machine that may have no network.
//
// It leads with what needs a decision (blockers, then applications nobody has
// placed), because a page that opens with 90 rows of correctly-resolved
// software teaches the reader to scroll past the two that matter.

//go:embed report.html.tmpl
var reportFS embed.FS

var reportTmpl = template.Must(template.New("report.html.tmpl").Funcs(reportFuncs).ParseFS(reportFS, "report.html.tmpl"))

var reportFuncs = template.FuncMap{
	"bytes":   humanBytes,
	"when":    humanTime,
	"value":   settingValue,
	"join":    func(sep string, xs []string) string { return strings.Join(xs, sep) },
	"plus1":   func(i int) int { return i + 1 },
	"nonzero": func(i int) bool { return i != 0 },
	"mul100":  func(f float64) float64 { return f * 100 },
}

// ReportData is what the template is given: the manifest plus the few things
// it is easier to work out in Go than in a template.
type ReportData struct {
	M          *Manifest
	OS         string
	Counts     Counts
	Order      []App
	Manual     []App
	Unplaced   []App
	Dropped    []App
	Resolved   []App
	Blockers   []Compat
	Warnings   []Compat
	Infos      []Compat
	Ready      string // what stands in the way of approval, or ""
	Machine    string // manufacturer and model, without saying "HP HP"
	RenderedAt time.Time
	Version    string
}

// Report writes the manifest as one self-contained HTML page. osName is the
// Windows the target will run, which decides which settings count as
// appliable.
func Report(w io.Writer, m *Manifest, osName, version string) error {
	if osName == "" {
		osName = "win11"
	}
	d := ReportData{
		M: m, OS: osName, Counts: m.Count(osName),
		Order: m.InstallOrder(), Manual: m.Manual(),
		Machine:    machineName(m.Source.Hardware),
		RenderedAt: time.Now().UTC().Truncate(time.Second), Version: version,
	}
	for _, a := range m.Apps {
		switch a.Resolution.Status {
		case StatusUnmapped, StatusUnset:
			d.Unplaced = append(d.Unplaced, a)
		case StatusDropped:
			d.Dropped = append(d.Dropped, a)
		case StatusBlocked:
			d.Unplaced = append(d.Unplaced, a)
		case StatusResolved:
			d.Resolved = append(d.Resolved, a)
		}
	}
	for _, c := range m.Compat {
		switch c.Severity {
		case Blocker:
			d.Blockers = append(d.Blockers, c)
		case Warning:
			d.Warnings = append(d.Warnings, c)
		default:
			d.Infos = append(d.Infos, c)
		}
	}
	if err := m.ReadyToApprove(); err != nil {
		d.Ready = err.Error()
	}
	// A compat entry names its subject by application id, which is DSKY's
	// word for it, not the customer's. The page says what the person saw in
	// Settings -> Apps.
	named := map[string]string{}
	for _, a := range m.Apps {
		named[a.ID] = a.DisplayName
	}
	for _, list := range [][]Compat{d.Blockers, d.Warnings, d.Infos} {
		for i, c := range list {
			if name, ok := named[c.Subject]; ok {
				list[i].Subject = name
			}
		}
	}
	return reportTmpl.Execute(w, d)
}

// machineName is the manufacturer and model as one phrase. Vendors are
// inconsistent about whether the model already carries the maker's name --
// "HP EliteDesk 800 G4 SFF" from manufacturer "HP" -- and "HP HP EliteDesk"
// is the kind of detail that makes a report look machine-written.
func machineName(h Hardware) string {
	man := strings.TrimSpace(h.Manufacturer)
	model := strings.TrimSpace(h.Model)
	switch {
	case man == "":
		return model
	case model == "":
		return man
	case strings.HasPrefix(strings.ToLower(model), strings.ToLower(man)):
		return model
	}
	return man + " " + model
}

// humanBytes is sizes as a person says them. Two decimals on gigabytes
// because a disk that is 465.76 GB and one that is 465.80 GB are different
// disks when you are looking for the one you scanned.
func humanBytes(n int64) string {
	switch {
	case n <= 0:
		return "—"
	case n < 1<<10:
		return fmt.Sprintf("%d B", n)
	case n < 1<<20:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	case n < 1<<30:
		return fmt.Sprintf("%.0f MB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%.2f GB", float64(n)/(1<<30))
	}
}

// humanTime takes a time or a possibly-absent one, because the template says
// "when" about both a scan (always) and an approval (not yet).
func humanTime(v any) string {
	var t time.Time
	switch x := v.(type) {
	case time.Time:
		t = x
	case *time.Time:
		if x == nil {
			return "—"
		}
		t = *x
	default:
		return "—"
	}
	if t.IsZero() {
		return "—"
	}
	return t.UTC().Format("2006-01-02 15:04 UTC")
}

// settingValue prints a captured value as itself: a string without its
// quotes, anything else as the JSON it was captured as.
func settingValue(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "—"
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return string(raw)
}

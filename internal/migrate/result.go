package migrate

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

//go:embed result.html.tmpl
var resultTemplate embed.FS

// Record is what a machine wrote down about its own build: the record the
// first-boot agent leaves in dsky-migrate-result.json.
//
// Declared here as well as in the agent, and deliberately. The agent is the
// thing that reads a manifest on a machine, so this package must not import
// it; one small shape written twice is the price of that, and it is a price
// worth paying for the same reason BuildPlan is plain data.
type Record struct {
	Machine  string    `json:"machine"`
	Recipe   string    `json:"recipe,omitempty"`
	Agent    string    `json:"agent_version,omitempty"`
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished,omitempty"`
	Outcomes []Outcome `json:"outcomes"`
	Problems []string  `json:"problems,omitempty"`
}

// Outcome is one thing the plan asked for, and what became of it.
type Outcome struct {
	Kind  string `json:"kind"`
	Name  string `json:"name"`
	State string `json:"state"`
	Note  string `json:"note,omitempty"`
}

// Outcome states, matching what the agent writes.
const (
	StateDone     = "done"
	StateFailed   = "failed"
	StateAtSignIn = "at-sign-in"
)

// LoadRecord reads a record off a machine.
func LoadRecord(path string) (*Record, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r Record
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("%s is not a DSKY result file: %w", path, err)
	}
	if r.Machine == "" && len(r.Outcomes) == 0 {
		return nil, fmt.Errorf("%s has no machine and nothing in it", path)
	}
	return &r, nil
}

// RecordData is what the result page renders.
type RecordData struct {
	R          *Record
	Plan       *Manifest // may be nil: a record can be read without one
	Done       []Shown
	Waiting    []Shown
	Failed     []Shown
	RenderedAt time.Time
	Version    string
	Took       string
}

// Shown is one outcome with the name a person would use for it.
type Shown struct {
	Kind, Name, Note string
}

// ResultReport renders the record as a page.
//
// The grouping is the whole point, and it is not the order the agent did
// things in. A person reading this wants one question answered -- what do I
// have to do? -- so what needs a hand comes first, what is waiting for
// somebody to sign in comes next with an explanation that it is not a fault,
// and what is finished comes last, because nobody acts on it.
func RecordReport(w io.Writer, r *Record, plan *Manifest, version string) error {
	d := RecordData{R: r, Plan: plan, RenderedAt: time.Now().UTC().Truncate(time.Second), Version: version}
	if !r.Finished.IsZero() {
		d.Took = r.Finished.Sub(r.Started).Round(time.Second).String()
	}
	for _, o := range r.Outcomes {
		s := Shown{Kind: o.Kind, Name: displayFor(plan, o), Note: o.Note}
		switch o.State {
		case StateFailed:
			d.Failed = append(d.Failed, s)
		case StateAtSignIn:
			d.Waiting = append(d.Waiting, s)
		default:
			d.Done = append(d.Done, s)
		}
	}
	for _, g := range [][]Shown{d.Failed, d.Waiting, d.Done} {
		sort.SliceStable(g, func(i, j int) bool {
			if g[i].Kind != g[j].Kind {
				return g[i].Kind < g[j].Kind
			}
			return g[i].Name < g[j].Name
		})
	}
	t, err := template.New("result.html.tmpl").Funcs(template.FuncMap{
		"humanTime": humanTime,
	}).ParseFS(resultTemplate, "result.html.tmpl")
	if err != nil {
		return err
	}
	return t.Execute(w, d)
}

// displayFor turns what the machine recorded into what a person calls it.
//
// The agent knows a staged file and a winget id; the plan knows "ArcGIS Pro".
// The plan is here and the agent is not, which is the reason the page is
// rendered on this side rather than on the machine.
func displayFor(m *Manifest, o Outcome) string {
	if m == nil || o.Kind != KindApp {
		return o.Name
	}
	for _, a := range m.Apps {
		switch {
		case a.Resolution.Ref != "" && strings.EqualFold(a.Resolution.Ref, o.Name):
			return a.DisplayName
		case strings.EqualFold(installerID(a), strings.TrimSuffix(strings.TrimSuffix(o.Name, ".msi"), ".exe")):
			return a.DisplayName
		case strings.HasPrefix(strings.ToLower(o.Name), strings.ToLower(installerID(a))):
			return a.DisplayName
		}
	}
	return o.Name
}

// Outcome kinds, matching what the agent writes.
const (
	KindApp     = "app"
	KindSetting = "setting"
	KindPrinter = "printer"
	KindDrive   = "drive"
	KindDomain  = "domain"
)

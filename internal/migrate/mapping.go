package migrate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// A mapping table is a site's memory. The first machine at a dental practice
// has a dozen applications no package repository has ever heard of, and an
// operator decides for each one where it comes from and how it installs
// silently. Those decisions are the expensive part, and they are the same for
// the next machine in the same office -- so they are written down here, keyed
// by how the application names itself, and applied before anything is guessed.
//
// Two tables are consulted, the site's own first and then one shipped with
// DSKY for software common enough to be worth everybody's while. A site table
// lives with the workspace, not in the library: it is as much a record of the
// customer as the recipes are, and it belongs in whatever the operator backs
// up or keeps in git.

// MappingVersion is the mapping-table format this build writes.
const MappingVersion = "1.0"

// MappingName is the file a site's table lives in.
const MappingName = "mappings.json"

// Table is a list of decisions, tried in order. First match wins, so a
// specific pattern belongs above a general one.
type Table struct {
	SchemaVersion string    `json:"schema_version"`
	Site          string    `json:"site,omitempty"`
	Entries       []Mapping `json:"entries"`
}

// Mapping is one decision: what it matches, and what to do with it.
type Mapping struct {
	Match         Match          `json:"match"`
	Resolution    Resolution     `json:"resolution"`
	ConfigCapture *ConfigCapture `json:"config_capture,omitempty"`
	AddedBy       string         `json:"added_by,omitempty"`
	AddedAt       string         `json:"added_at,omitempty"`
	Note          string         `json:"note,omitempty"`
}

// Match is how an entry recognises an application. DisplayName is a regular
// expression matched case-insensitively against the name the application
// registered; Publisher, when given, must match too.
type Match struct {
	DisplayName string `json:"display_name"`
	Publisher   string `json:"publisher,omitempty"`
}

// LoadTable reads a mapping table. A missing file is not an error: a site with
// no decisions yet is the normal state of a new site.
func LoadTable(path string) (*Table, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &Table{SchemaVersion: MappingVersion, Entries: nil}, nil
	}
	if err != nil {
		return nil, err
	}
	t, err := ParseTable(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return t, nil
}

// ParseTable reads a mapping table from bytes.
func ParseTable(b []byte) (*Table, error) {
	dec := json.NewDecoder(strings.NewReader(strings.TrimPrefix(string(b), "\ufeff")))
	dec.DisallowUnknownFields()
	var t Table
	if err := dec.Decode(&t); err != nil {
		return nil, fmt.Errorf("not a mapping table DSKY understands: %w", err)
	}
	if err := t.Validate(); err != nil {
		return nil, err
	}
	return &t, nil
}

// Save writes the table, checked and indented; these files are read and
// edited by people as often as by DSKY.
func (t *Table) Save(path string) error {
	if t.SchemaVersion == "" {
		t.SchemaVersion = MappingVersion
	}
	if err := t.Validate(); err != nil {
		return err
	}
	b, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Validate checks the patterns compile and every entry says something. A
// pattern that does not compile is refused at load rather than silently
// matching nothing, which would look like the table being ignored.
func (t *Table) Validate() error {
	fail := func(format string, args ...any) error {
		return fmt.Errorf("mapping table: "+format, args...)
	}
	switch {
	case t.SchemaVersion == "":
		return fail("schema_version is required (this build writes %s)", MappingVersion)
	case !supportedVersion(t.SchemaVersion):
		return fail("schema_version %q is from a later DSKY — update DSKY to read it", t.SchemaVersion)
	}
	for i, e := range t.Entries {
		where := fmt.Sprintf("entries[%d]", i)
		if e.Match.DisplayName != "" {
			where = fmt.Sprintf("entries[%d] (%s)", i, e.Match.DisplayName)
		}
		if e.Match.DisplayName == "" {
			return fail("%s: match.display_name is required", where)
		}
		for _, p := range []string{e.Match.DisplayName, e.Match.Publisher} {
			if p == "" {
				continue
			}
			if _, err := regexp.Compile("(?i)" + p); err != nil {
				return fail("%s: %q is not a valid pattern: %v", where, p, err)
			}
		}
		switch e.Resolution.Status {
		case StatusResolved, StatusDropped, StatusBlocked:
		case StatusUnset:
			// A table entry with a method but no status is the common way to
			// write one by hand; it means resolved.
			if e.Resolution.Method == "" {
				return fail("%s: say how it installs (resolution.method) or that it is dropped", where)
			}
		default:
			return fail("%s: resolution.status %q makes no sense in a mapping table", where, e.Resolution.Status)
		}
		if e.Resolution.Status != StatusDropped && e.Resolution.Method != "" && e.Resolution.Method != MethodManual && e.Resolution.Ref == "" {
			return fail("%s: %s needs a ref saying what to install", where, e.Resolution.Method)
		}
	}
	return nil
}

// Lookup finds the decision for an application, and how the entry described
// it, for the report. The returned resolution is ready to store on the app:
// status and resolved_by are filled in, and order defaults are applied.
func (t *Table) Lookup(a App) (Resolution, *ConfigCapture, bool) {
	for _, e := range t.Entries {
		if !e.matches(a) {
			continue
		}
		r := e.Resolution
		if r.Status == StatusUnset {
			r.Status = StatusResolved
		}
		r.ResolvedBy = ByMapping
		r.Confidence = 1
		if r.Order == 0 && r.Status == StatusResolved {
			r.Order = OrderDefault
		}
		if r.Status != StatusResolved {
			r.Method, r.Ref, r.Args, r.Order = "", "", nil, 0
		}
		return r, e.ConfigCapture, true
	}
	return Resolution{}, nil, false
}

func (e Mapping) matches(a App) bool {
	name := regexp.MustCompile("(?i)" + e.Match.DisplayName)
	if !name.MatchString(a.DisplayName) {
		return false
	}
	if e.Match.Publisher == "" {
		return true
	}
	return regexp.MustCompile("(?i)" + e.Match.Publisher).MatchString(a.Publisher)
}

// Remember adds a decision an operator just made, replacing an entry with the
// same match so that reviewing the same application twice does not grow the
// file. The pattern is anchored on the name as it was registered, escaped: a
// name is not a pattern, and "Acrobat (64-bit)" as a pattern matches nothing.
func (t *Table) Remember(a App, r Resolution, cc *ConfigCapture, by string) {
	e := Mapping{
		Match:         Match{DisplayName: "^" + regexp.QuoteMeta(a.DisplayName) + "$"},
		Resolution:    Resolution{Status: r.Status, Method: r.Method, Ref: r.Ref, Args: r.Args, Order: r.Order},
		ConfigCapture: cc,
		AddedBy:       by,
		AddedAt:       time.Now().UTC().Format("2006-01-02"),
	}
	if a.Publisher != "" {
		e.Match.Publisher = "^" + regexp.QuoteMeta(a.Publisher) + "$"
	}
	if t.SchemaVersion == "" {
		t.SchemaVersion = MappingVersion
	}
	for i, old := range t.Entries {
		if old.Match == e.Match {
			e.Note = old.Note
			t.Entries[i] = e
			return
		}
	}
	t.Entries = append(t.Entries, e)
}

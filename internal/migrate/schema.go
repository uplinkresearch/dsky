package migrate

import (
	"encoding/json"
	"reflect"
	"strings"
	"time"
)

// The manifest is a contract, and a contract other tools read: a site's RMM
// might count what a rebuild is going to install, a spreadsheet might tally
// which machines still have blockers. So the format is published as a JSON
// Schema under docs/.
//
// That schema is generated from the Go types here rather than written by hand.
// A hand-written schema is a second copy of the format, and the second copy is
// the one that rots -- it says "confidence" long after the field was renamed,
// and nothing fails. Generated, with a test comparing the committed file to
// what the types produce, the two cannot disagree: change a field and the test
// says the schema needs regenerating (dsky migrate schema > docs/...).
//
// Reading and writing is still checked by Go, not by a schema validator:
// Parse refuses unknown fields and Validate says, in a sentence somebody can
// act on, what is wrong. A schema says "does not match pattern"; an operator
// at a bench needs "mapped_drives[0]: unc "S:\shared" is not a \\server\share
// path".

// schemaEnums are the fields whose values are a fixed set. Keyed by the JSON
// property name, which is unambiguous here -- "status" and "method" mean the
// same thing wherever they appear in this format.
var schemaEnums = map[string][]string{
	"source_kind": {SourceWin32, SourceAppx, SourceMSIX, SourceStore},
	"status":      {StatusResolved, StatusUnmapped, StatusBlocked, StatusDropped, ""},
	"method":      {MethodWinget, MethodMSI, MethodEXE, MethodMSIX, MethodScript, MethodManual},
	"resolved_by": {ByAuto, ByMapping, ByOperator},
	"severity":    {Blocker, Warning, Info},
	"strategy":    {DataUSMT, DataKFM, DataRedirect, DataNoneStrat},
	"arch":        {"x64", "x86", "arm64"},
}

// schemaDescriptions are the few properties whose meaning is not obvious from
// the name, for whoever reads the schema without the Go beside it.
var schemaDescriptions = map[string]string{
	"schema_version": "major.minor; a reader that understands major 1 can read any 1.x",
	"content_hash":   "SHA-256 over every section except approval; a build refuses a manifest whose hash does not match",
	"resolution":     "how this application is to be installed on the new machine",
	"config_capture": "files and registry keys holding this application's own settings; only ever from a curated recipe, never guessed",
	"apply":          "how to set this on each target OS, keyed by OS name (win11); absent or empty means captured for the report but not applied",
	"odj_blob_path":  "offline domain join file produced at build time; never a credential",
	"compat":         "what a person has to decide about; an unacknowledged blocker prevents approval",
	"order":          "install sequencing, lower first; 10 for prerequisites, 50 default",
	"confidence":     "0 to 1; how sure an automatic match is",
}

// JSONSchema is the published schema for the manifest and the mapping table,
// as indented JSON.
func JSONSchema() []byte {
	defs := map[string]any{}
	manifest := schemaFor(reflect.TypeOf(Manifest{}), defs)
	table := schemaFor(reflect.TypeOf(Table{}), defs)
	s := map[string]any{
		"$schema":     "https://json-schema.org/draft/2020-12/schema",
		"$id":         "https://github.com/uplinkresearch/dsky/docs/migrate-manifest.schema.json",
		"title":       "DSKY migration manifest " + SchemaVersion,
		"description": "One Windows PC, and how each part of it is to be recreated on new hardware. Written by dsky migrate scan, filled in by resolve, approved by review, consumed by build. See docs/plan-migrate.md.",
		"$defs":       defs,
		"oneOf":       []any{manifest, table},
	}
	b, _ := json.MarshalIndent(s, "", "  ")
	return append(b, '\n')
}

var timeType = reflect.TypeOf(time.Time{})
var rawType = reflect.TypeOf(json.RawMessage{})

// schemaFor describes one Go type, adding named object types to defs and
// referring to them, so that App or Resolution is described once.
func schemaFor(t reflect.Type, defs map[string]any) map[string]any {
	switch {
	case t == timeType:
		return map[string]any{"type": "string", "format": "date-time"}
	case t == rawType:
		return map[string]any{"description": "any JSON value, kept exactly as captured"}
	}
	switch t.Kind() {
	case reflect.Ptr:
		inner := schemaFor(t.Elem(), defs)
		return map[string]any{"oneOf": []any{inner, map[string]any{"type": "null"}}}
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int32, reflect.Int64:
		return map[string]any{"type": "integer"}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	case reflect.Slice:
		return map[string]any{"type": "array", "items": schemaFor(t.Elem(), defs)}
	case reflect.Map:
		return map[string]any{"type": "object", "additionalProperties": schemaFor(t.Elem(), defs)}
	case reflect.Struct:
		name := t.Name()
		if _, done := defs[name]; !done {
			defs[name] = map[string]any{} // placeholder, so a self-reference terminates
			defs[name] = objectSchema(t, defs)
		}
		return map[string]any{"$ref": "#/$defs/" + name}
	default:
		return map[string]any{}
	}
}

func objectSchema(t reflect.Type, defs map[string]any) map[string]any {
	props := map[string]any{}
	var required []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		if name == "" {
			name = f.Name
		}
		p := schemaFor(f.Type, defs)
		if enum, ok := schemaEnums[name]; ok && f.Type.Kind() == reflect.String {
			vals := make([]any, len(enum))
			for j, v := range enum {
				vals[j] = v
			}
			p["enum"] = vals
		}
		if d, ok := schemaDescriptions[name]; ok {
			p["description"] = d
		}
		if name == "confidence" {
			p["minimum"], p["maximum"] = 0, 1
		}
		if name == "order" {
			p["minimum"], p["maximum"] = 0, OrderMax
		}
		props[name] = p
		// A field that is always written is required; "omitempty" is exactly
		// the marker for one that may be left out.
		if !strings.Contains(opts, "omitempty") {
			required = append(required, name)
		}
	}
	out := map[string]any{
		"type":                 "object",
		"properties":           props,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		out["required"] = required
	}
	if t.Name() == "Manifest" {
		out["title"] = "manifest"
	}
	if t.Name() == "Table" {
		out["title"] = "mapping table"
	}
	return out
}

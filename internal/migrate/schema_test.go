package migrate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The published schema is generated from the types in this package, so the two
// can never say different things. When this fails, the format changed: run
//
//	go run ./cmd/dsky migrate schema > docs/migrate-manifest.schema.json
//
// and write what changed in docs/migrate-schema-changelog.md.
func TestPublishedSchemaMatchesTheTypes(t *testing.T) {
	path := filepath.Join("..", "..", "docs", "migrate-manifest.schema.json")
	committed, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(committed) != string(JSONSchema()) {
		t.Errorf("docs/migrate-manifest.schema.json is out of date — regenerate it:\n"+
			"  go run ./cmd/dsky migrate schema > %s", path)
	}
}

// The schema has to describe the fixtures: same property names, same shape.
// This is the cheap version of running a validator — every key in a fixture
// exists in the schema, and every required key is in the fixture.
func TestSchemaCoversTheFixtures(t *testing.T) {
	var schema struct {
		Defs map[string]struct {
			Properties map[string]json.RawMessage `json:"properties"`
			Required   []string                   `json:"required"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(JSONSchema(), &schema); err != nil {
		t.Fatal(err)
	}
	man, ok := schema.Defs["Manifest"]
	if !ok {
		t.Fatal("the schema does not define the manifest")
	}
	for _, name := range fixtures {
		b, err := os.ReadFile(filepath.Join("testdata", name+".json"))
		if err != nil {
			t.Fatal(err)
		}
		var top map[string]json.RawMessage
		if err := json.Unmarshal(b, &top); err != nil {
			t.Fatal(err)
		}
		for key := range top {
			if _, ok := man.Properties[key]; !ok {
				t.Errorf("%s: the schema has no %q", name, key)
			}
		}
		for _, key := range man.Required {
			if _, ok := top[key]; !ok {
				t.Errorf("%s: the schema requires %q and the fixture has no such key", name, key)
			}
		}
	}
	// Spot-check that the enums came through, since they are the part a
	// reader outside DSKY relies on most.
	if s := string(JSONSchema()); !strings.Contains(s, `"unmapped"`) || !strings.Contains(s, `"folder_redirection"`) {
		t.Error("the schema is missing the value lists")
	}
}

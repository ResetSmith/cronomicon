package settings

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// FX-E6 — the report struct and the spec's required list, pinned together.
//
// The spec described the pre-v0.57.8 conversion report for a YEAR after the
// backend stopped emitting it: eight required fields with no assignment
// anywhere, two schemas referenced by nothing real, and a "HARD BLOCKER" note
// about a hazard that could no longer occur. A schema-validating client
// rejected every valid response, and nothing here failed — because the two
// halves of the contract lived in different files with no test between them.
// This is that test. Rename a field, add one, or drop one on either side and
// it fails naming the drift.
func TestRbacPreflightReportMatchesTheSpec(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "openapi.yaml"))
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}
	var spec struct {
		Components struct {
			Schemas map[string]struct {
				Required   []string             `yaml:"required"`
				Properties map[string]yaml.Node `yaml:"properties"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(data, &spec); err != nil {
		t.Fatalf("parse spec: %v", err)
	}
	schema, ok := spec.Components.Schemas["RbacPreflightReport"]
	if !ok {
		t.Fatal("spec has no RbacPreflightReport schema")
	}

	structFields := map[string]bool{}
	rt := reflect.TypeFor[RbacPreflightReport]()
	for field := range rt.Fields() {
		tag, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if tag != "" && tag != "-" {
			structFields[tag] = true
		}
	}

	// Every REQUIRED spec field must exist on the struct: a required field the
	// backend never emits makes every valid response invalid to a validating
	// client, which is precisely the defect this test exists to prevent.
	for _, req := range schema.Required {
		if !structFields[req] {
			t.Errorf("spec requires %q and the Go report has no such field — a schema-validating "+
				"client will reject every response", req)
		}
	}
	// And every struct field must be declared in the spec: an emitted field the
	// spec omits is invisible to generated clients and rots into folklore.
	specProps := map[string]bool{}
	for name := range schema.Properties {
		specProps[name] = true
	}
	var missing []string
	for f := range structFields {
		if !specProps[f] {
			missing = append(missing, f)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("the Go report emits %v and the spec does not declare them", missing)
	}
}

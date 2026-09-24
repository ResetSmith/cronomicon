package api_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/seed"
	"gopkg.in/yaml.v3"
)

// TestIdTypesMatchTheSpec is K-4 (VF-17).
//
// openapi.yaml declared `id: { type: integer }` on fifteen schemas. Seven of
// them return a UUIDv7 string, and had done since those endpoints shipped —
// every settings-family entity. Nothing caught it because nothing compared the
// declaration to the response, which is the same mechanism that let VF-15's
// inert enum values survive: the spec said one thing, the code did another, and
// no test held them together.
//
// So this asserts the property directly. It probes the live API rather than
// reasoning about Go types, because the wire shape is what a client sees and
// what the generated frontend types are built from — the `?? -1` idiom in six
// call sites is what a `number` declaration on a UUID talks people into.
//
// Both kinds are listed on purpose. A guard that only knew about strings would
// pass if someone "fixed" Job's integer id into a string, so the integer rows
// are load-bearing.
func TestIdTypesMatchTheSpec(t *testing.T) {
	ts, pool := newTestServer(t)
	if err := seed.Seed(context.Background(), pool, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("seed: %v", err)
	}
	client, _ := devLoginWithCSRF(t, ts)
	declared := loadDeclaredIDTypes(t)

	cases := []struct {
		schema   string
		endpoint string
	}{
		// UUIDv7-keyed: every one of these was declared `integer` until v0.52.24.
		{"Scope", "/scopes"},
		{"EnvVar", "/env-vars"},
		{"EnvSecret", "/env-secrets"},
		{"SshHost", "/ssh/hosts"},
		{"SshBastion", "/ssh/bastions"},
		{"AlertRule", "/alerts"},
		// Genuinely integer — rowid-keyed tables. Here so the guard fails in both
		// directions rather than only rewarding `string`.
		{"Job", "/jobs"},
		{"Workflow", "/workflows"},
		{"ActivityEntry", "/activity"},
		{"ChangeLogEntry", "/change-log"},
	}

	for _, tc := range cases {
		t.Run(tc.schema, func(t *testing.T) {
			want, ok := declared[tc.schema]
			if !ok {
				t.Fatalf("openapi.yaml declares no id for %s — if the schema was renamed, update this table", tc.schema)
			}
			req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1"+tc.endpoint, nil)
			r, err := client.Do(req)
			if err != nil {
				t.Fatalf("GET %s: %v", tc.endpoint, err)
			}
			defer r.Body.Close()
			if r.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(r.Body)
				t.Fatalf("GET %s = %d — %s", tc.endpoint, r.StatusCode, body)
			}
			var payload any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode %s: %v", tc.endpoint, err)
			}
			id, err := firstItemID(payload)
			if err != nil {
				// An empty collection cannot prove anything, and silently passing
				// is how a guard rots. The seed is expected to populate all of
				// these; if one legitimately becomes empty, drop it from the table.
				t.Fatalf("%s: %v (seed no longer covers this endpoint?)", tc.endpoint, err)
			}
			got := jsonKind(id)
			if got != want {
				t.Errorf("%s.id is declared %q in openapi.yaml but %s returns %s (%v) — "+
					"a client generated from this spec gets the wrong type, and arithmetic or "+
					"numeric sorting on it is a bug waiting to happen (VF-17)",
					tc.schema, want, tc.endpoint, got, id)
			}
		})
	}
}

// jsonKind names the JSON type of a decoded value in the spec's vocabulary.
func jsonKind(v any) string {
	switch v.(type) {
	case float64:
		return "integer"
	case string:
		return "string"
	case nil:
		return "null"
	default:
		return "other"
	}
}

// firstItemID pulls items[0].id out of either a bare array or a paged envelope.
func firstItemID(payload any) (any, error) {
	var items []any
	switch p := payload.(type) {
	case []any:
		items = p
	case map[string]any:
		raw, ok := p["items"].([]any)
		if !ok {
			return nil, errNoItems
		}
		items = raw
	default:
		return nil, errNoItems
	}
	if len(items) == 0 {
		return nil, errEmptyCollection
	}
	obj, ok := items[0].(map[string]any)
	if !ok {
		return nil, errNoItems
	}
	id, ok := obj["id"]
	if !ok {
		return nil, errNoID
	}
	return id, nil
}

type conformanceErr string

func (e conformanceErr) Error() string { return string(e) }

const (
	errNoItems         = conformanceErr("response is neither an array nor a paged envelope with items")
	errEmptyCollection = conformanceErr("collection is empty")
	errNoID            = conformanceErr("first item has no id field")
)

// loadDeclaredIDTypes reads the declared type of each schema's `id` property.
// An `allOf` schema declares it in one of its members, so every branch is
// searched — AlertRule and SshHost are both built that way.
func loadDeclaredIDTypes(t *testing.T) map[string]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "openapi.yaml"))
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}
	var spec struct {
		Components struct {
			Schemas map[string]yaml.Node `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(data, &spec); err != nil {
		t.Fatalf("parse spec: %v", err)
	}
	out := map[string]string{}
	for name, node := range spec.Components.Schemas {
		if k := findIDType(&node); k != "" {
			out[name] = k
		}
	}
	if len(out) == 0 {
		t.Fatal("no schema declares an id — wrong file, or the parse is broken")
	}
	return out
}

// findIDType walks a schema node for a `properties.id.type`, descending through
// allOf/oneOf/anyOf members. It stops at the first hit, which is safe because a
// schema declaring `id` twice with different types would be malformed anyway.
func findIDType(node *yaml.Node) string {
	if node.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(node.Content); i += 2 {
			key, val := node.Content[i].Value, node.Content[i+1]
			if key == "properties" && val.Kind == yaml.MappingNode {
				for j := 0; j+1 < len(val.Content); j += 2 {
					if val.Content[j].Value != "id" {
						continue
					}
					idNode := val.Content[j+1]
					for k := 0; k+1 < len(idNode.Content); k += 2 {
						if idNode.Content[k].Value == "type" {
							return idNode.Content[k+1].Value
						}
					}
				}
			}
			if key == "allOf" || key == "oneOf" || key == "anyOf" {
				for _, member := range val.Content {
					if kind := findIDType(member); kind != "" {
						return kind
					}
				}
			}
		}
	}
	return ""
}

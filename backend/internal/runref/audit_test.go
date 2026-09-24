package runref

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestAuditDetails verifies the dispatch-audit payload (P1.6): deterministic,
// sorted (kind then name), scope-tagged, and — critically — value-free.
func TestAuditDetails(t *testing.T) {
	refs := []ResolvedRef{
		{Kind: KindVar, Name: "REGION", Source: "stored"},
		{Kind: KindSecret, Name: "DB_PASS", Source: "vault"},
		{Kind: KindSecret, Name: "API_KEY", Source: "stored"},
	}
	got := AuditDetails(refs, "prod")

	// Parse back so the assertion is order-tolerant on the JSON encoding but exact
	// on the ordering of the References array (which AuditDetails must sort).
	var parsed struct {
		Scope      string `json:"scope"`
		References []struct {
			Kind, Name, Source string
		} `json:"references"`
	}
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("AuditDetails produced invalid JSON %q: %v", got, err)
	}
	if parsed.Scope != "prod" {
		t.Errorf("scope = %q, want prod", parsed.Scope)
	}
	// Sorted by kind (secret < var), then name (API_KEY < DB_PASS).
	want := []struct{ Kind, Name, Source string }{
		{"secret", "API_KEY", "stored"},
		{"secret", "DB_PASS", "vault"},
		{"var", "REGION", "stored"},
	}
	if len(parsed.References) != len(want) {
		t.Fatalf("got %d refs, want %d: %s", len(parsed.References), len(want), got)
	}
	for i, w := range want {
		r := parsed.References[i]
		if r.Kind != w.Kind || r.Name != w.Name || r.Source != w.Source {
			t.Errorf("ref[%d] = %+v, want %+v", i, r, w)
		}
	}

	// Determinism: same input → identical output regardless of input order.
	shuffled := []ResolvedRef{refs[2], refs[0], refs[1]}
	if AuditDetails(shuffled, "prod") != got {
		t.Errorf("AuditDetails is not order-deterministic")
	}
}

// TestAuditDetailsNoValues is the security guard: no reference VALUE may ever appear
// in the audit payload — only names and provenance.
func TestAuditDetailsNoValues(t *testing.T) {
	refs := []ResolvedRef{{Kind: KindSecret, Name: "DB_PASS", Source: "stored"}}
	got := AuditDetails(refs, "prod")
	// AuditDetails receives no values by construction (ResolvedRef has no value
	// field); this asserts the shape stays name/source only.
	if strings.Contains(got, "value") {
		t.Errorf("audit payload must not carry values: %s", got)
	}
}

// TestAuditDetailsEmpty: a defensive empty set still yields valid JSON.
func TestAuditDetailsEmpty(t *testing.T) {
	got := AuditDetails(nil, "")
	if !json.Valid([]byte(got)) {
		t.Errorf("empty AuditDetails is not valid JSON: %q", got)
	}
}

// TestParseAuditReferences round-trips AuditDetails → ParseAuditReferences (P1.8):
// the run-detail panel reconstructs the injected reference set (kind, name, derived
// reference) from the audit payload. Malformed input yields nil.
func TestParseAuditReferences(t *testing.T) {
	refs := []ResolvedRef{
		{Kind: KindSecret, Name: "DB_PASS", Source: "stored"},
		{Kind: KindVar, Name: "REGION", Source: "stored"},
	}
	got := ParseAuditReferences(AuditDetails(refs, "prod"))
	if len(got) != 2 {
		t.Fatalf("got %d bindings, want 2: %+v", len(got), got)
	}
	byRef := map[string]Binding{}
	for _, b := range got {
		byRef[b.Reference] = b
	}
	if b, ok := byRef["CRONOMICON_SECRET_DB_PASS"]; !ok || b.Kind != KindSecret || b.Name != "DB_PASS" {
		t.Errorf("secret binding wrong: %+v", got)
	}
	if b, ok := byRef["CRONOMICON_VAR_REGION"]; !ok || b.Kind != KindVar {
		t.Errorf("var binding wrong: %+v", got)
	}
	if ParseAuditReferences("{not json") != nil {
		t.Errorf("malformed input should yield nil")
	}
}

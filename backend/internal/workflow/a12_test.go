package workflow

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/execspec"
)

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestParseOutputMarker covers the A12 stdout marker grammar.
func TestParseOutputMarker(t *testing.T) {
	cases := []struct {
		line         string
		wantK, wantV string
		wantOK       bool
	}{
		{"::amadeus-output name=DB_HOST::pg-prod-01", "DB_HOST", "pg-prod-01", true},
		{"::amadeus-output name=TOKEN::a b c", "TOKEN", "a b c", true},
		{"::amadeus-output name=EMPTY::", "EMPTY", "", true},
		{"regular log line", "", "", false},
		{"::amadeus-output name=bad-key::x", "", "", false}, // hyphen not allowed in key
		{"  ::amadeus-output name=X::y", "", "", false},     // must start at column 0
	}
	for _, c := range cases {
		k, v, ok := execspec.ParseOutputMarker(c.line)
		if ok != c.wantOK || k != c.wantK || v != c.wantV {
			t.Errorf("ParseOutputMarker(%q) = (%q,%q,%v), want (%q,%q,%v)", c.line, k, v, ok, c.wantK, c.wantV, c.wantOK)
		}
	}
}

// TestEngineInterJobOutputs verifies the A12 propagation seam: loadOutputs reads a
// child run's outputs_json, resolveInputs maps an upstream output to a downstream
// step's env, childEnvJSON merges it over the workflow env, and EvaluateCondition's
// output_match reads the populated JobResult.Outputs.
func TestEngineInterJobOutputs(t *testing.T) {
	pool, err := db.Open(filepath.Join(t.TempDir(), "a12.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pool.Close()
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	e := New(pool, discardLog())
	ctx := context.Background()

	// A finished run with captured outputs.
	if _, err := pool.Exec(`INSERT INTO runs(id, job_name, run_type, status, triggered_by, trigger_kind, outputs_json, created_at)
		VALUES('t-up','build','bash','success','tester','workflow','{"ARTIFACT":"app-1.2.3"}','t')`); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	// loadOutputs.
	got := e.loadOutputs(ctx, "t-up")
	if got["ARTIFACT"] != "app-1.2.3" {
		t.Fatalf("loadOutputs = %v, want ARTIFACT=app-1.2.3", got)
	}

	// resolveInputs: downstream step consumes the upstream output as DEPLOY_ART.
	results := map[string]*JobResult{"build": {Status: "success", Outputs: got}}
	step := Step{Type: "job", Name: "deploy", Inputs: map[string]InputRef{
		"DEPLOY_ART": {FromStep: "build", FromOutput: "ARTIFACT"},
		"MISSING":    {FromStep: "build", FromOutput: "nope"},
	}}
	env := e.resolveInputs(step, results)
	if env["DEPLOY_ART"] != "app-1.2.3" {
		t.Errorf("resolveInputs DEPLOY_ART = %q, want app-1.2.3", env["DEPLOY_ART"])
	}
	if v, ok := env["MISSING"]; !ok || v != "" {
		t.Errorf("missing upstream output should resolve to \"\" deterministically, got %q ok=%v", v, ok)
	}

	// childEnvJSON layers job-env (none here) → parent env (empty) → inputs.
	merged := e.childEnvJSON(ctx, "no-such-wf", "amadeus", "no-such-job", env)
	if !merged.Valid || merged.String == "" {
		t.Fatalf("childEnvJSON produced no env")
	}

	// output_match now has data to evaluate.
	cond := &Condition{Type: "output_match", JobRef: "build", Field: "ARTIFACT", Operator: "==", Value: "app-1.2.3"}
	if !EvaluateCondition(cond, results) {
		t.Errorf("output_match should match the captured output")
	}
}

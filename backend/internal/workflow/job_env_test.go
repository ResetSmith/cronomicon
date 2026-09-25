package workflow

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/db"
)

// TestChildEnvJSONLayersJobEnv covers JC16: a workflow-step child run layers the
// step's job-level env (JC10 base) beneath the step inputs (inputs win on key
// collision). The parent workflow env is NULL here; the full job → parent → input
// precedence is exercised by the envmerge unit tests.
func TestChildEnvJSONLayersJobEnv(t *testing.T) {
	pool, err := db.Open(filepath.Join(t.TempDir(), "jc16.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pool.Close()
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := pool.Exec(`INSERT INTO jobs (uid, name, source, run_type, enabled, env_json, synced_at)VALUES ('uid-'||'step1', 'step1','cronomicon','bash',1,'{"X":"job","Y":"job"}','t')`); err != nil {
		t.Fatalf("seed job: %v", err)
	}
	e := New(pool, discardLog())

	// Parent NULL (no such workflow run) ⇒ job-env base + step input; input wins Y.
	got := e.childEnvJSON(context.Background(), "no-such-wf", "cronomicon", "step1", map[string]string{"Y": "in"})
	if !got.Valid {
		t.Fatalf("childEnvJSON returned NULL, want merged env")
	}
	var m map[string]string
	_ = json.Unmarshal([]byte(got.String), &m)
	if !reflect.DeepEqual(m, map[string]string{"X": "job", "Y": "in"}) {
		t.Errorf("child env = %v, want {X:job, Y:in} (job-env base, step input wins)", m)
	}

	// A step whose job has no job-level env and no inputs ⇒ NULL (R2 no-op).
	if _, err := pool.Exec(`INSERT INTO jobs (uid, name, source, run_type, enabled, synced_at)VALUES ('uid-'||'plain', 'plain','cronomicon','bash',1,'t')`); err != nil {
		t.Fatalf("seed plain job: %v", err)
	}
	if got := e.childEnvJSON(context.Background(), "no-such-wf", "cronomicon", "plain", nil); got.Valid {
		t.Errorf("childEnvJSON for an env-less step = %q, want NULL", got.String)
	}
}

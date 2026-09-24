package envmerge

import (
	"reflect"
	"testing"
)

func TestMergePrecedenceAndNoOp(t *testing.T) {
	job := map[string]string{"X": "job", "Y": "job"}
	sched := map[string]string{"X": "sched"}
	override := map[string]string{"X": "run"}

	// JC11 scheduled precedence: job-env base, schedule wins on collision.
	if got := Merge(job, sched); !reflect.DeepEqual(got, map[string]string{"X": "sched", "Y": "job"}) {
		t.Errorf("Merge(job, sched) = %v, want {X:sched, Y:job}", got)
	}
	// Full 3-layer chain: per-run override wins over both.
	if got := Merge(job, sched, override); !reflect.DeepEqual(got, map[string]string{"X": "run", "Y": "job"}) {
		t.Errorf("Merge(job, sched, override) = %v, want {X:run, Y:job}", got)
	}
	// Manual path: no schedule layer — override wins over job-env, job-only key survives.
	if got := Merge(job, override); !reflect.DeepEqual(got, map[string]string{"X": "run", "Y": "job"}) {
		t.Errorf("Merge(job, override) = %v, want {X:run, Y:job}", got)
	}
	// Strict no-op: all-empty layers yield nil so env_json stays NULL (R2).
	if got := Merge(nil, map[string]string{}, nil); got != nil {
		t.Errorf("Merge(empty...) = %v, want nil", got)
	}
	// Inputs are not mutated.
	if !reflect.DeepEqual(job, map[string]string{"X": "job", "Y": "job"}) {
		t.Errorf("Merge mutated an input layer: %v", job)
	}
}

func TestMergeJSON(t *testing.T) {
	// job-env base under schedule env (schedule wins).
	if got := MergeJSON(`{"X":"job","Y":"job"}`, `{"X":"sched"}`); got != `{"X":"sched","Y":"job"}` {
		t.Errorf("MergeJSON = %q, want {\"X\":\"sched\",\"Y\":\"job\"}", got)
	}
	// Empty/malformed layers are skipped; a single layer round-trips byte-for-byte.
	if got := MergeJSON("", `{"A":"1"}`, "not json"); got != `{"A":"1"}` {
		t.Errorf("MergeJSON skip-empty = %q, want {\"A\":\"1\"}", got)
	}
	// All-empty ⇒ "" (NULL no-op).
	if got := MergeJSON("", "", ""); got != "" {
		t.Errorf("MergeJSON(empty...) = %q, want empty string", got)
	}
}

func TestParse(t *testing.T) {
	if got := Parse(""); got != nil {
		t.Errorf("Parse(\"\") = %v, want nil", got)
	}
	if got := Parse("{bad"); got != nil {
		t.Errorf("Parse(malformed) = %v, want nil", got)
	}
	if got := Parse(`{"K":"V"}`); !reflect.DeepEqual(got, map[string]string{"K": "V"}) {
		t.Errorf("Parse = %v, want {K:V}", got)
	}
}

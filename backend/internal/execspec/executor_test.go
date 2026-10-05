package execspec

import (
	"context"
	"strings"
	"testing"
)

// TestResolveExecutorPrecedence walks the resolver's five rungs and its two
// refusals in one table, because the rungs only mean anything relative to each
// other: each row differs from a neighbour in exactly the input that should
// change the answer.
func TestResolveExecutorPrecedence(t *testing.T) {
	f := newUnclaimFixture(t)
	f.exec(`INSERT INTO scopes(id,name,source,created_at) VALUES('s-open','open','cronomicon','t')`)
	f.exec(`INSERT INTO scopes(id,name,source,created_at) VALUES('s-dmz','dmz-web','cronomicon','t')`)
	f.exec(`INSERT INTO scope_runners(scope_id,runner_id,runner_name,bound_at) VALUES('s-dmz','r-gone','runner-dmz-01','t')`)
	// Jobs: no executor, explicit ssh, explicit runner; an ansible job with none.
	f.exec(`INSERT INTO jobs(uid,name,source,run_type,synced_at) VALUES('u-plain','plain','git','bash','t')`)
	f.exec(`INSERT INTO jobs(uid,name,source,run_type,executor,synced_at) VALUES('u-ssh','wants-ssh','git','bash','ssh','t')`)
	f.exec(`INSERT INTO jobs(uid,name,source,run_type,executor,synced_at) VALUES('u-runner','wants-runner','git','bash','runner','t')`)
	f.exec(`INSERT INTO jobs(uid,name,source,run_type,synced_at) VALUES('u-play','play','git','ansible','t')`)
	f.exec(`INSERT INTO jobs(uid,name,source,run_type,executor,synced_at) VALUES('u-play-ssh','play-ssh','git','ansible','ssh','t')`)

	setDefault := func(v string) {
		f.exec(`DELETE FROM settings WHERE key='defaultExecutor'`)
		if v != "" {
			f.exec(`INSERT INTO settings(key,value) VALUES('defaultExecutor',?)`, v)
		}
	}

	cases := []struct {
		name                          string
		uid, runType, scope, override string
		globalDefault                 string
		wantExecutor, wantRefusal     string
	}{
		// The two defaults.
		{"shell job, nothing set → the run type's default", "u-plain", "bash", "open", "", "", "ssh", ""},
		{"ansible job, nothing set → the run type's default", "u-play", "ansible", "open", "", "", "runner", ""},
		{"global default runner", "u-plain", "bash", "open", "", "runner", "runner", ""},
		{"global default ssh", "u-plain", "bash", "open", "", "ssh", "ssh", ""},
		// The drift this file settles: a fleet-wide ssh default falls through for
		// a run type ssh cannot execute, on EVERY path — it used to be a 422 on
		// the manual trigger and a fall-through on the scheduled one.
		{"global default ssh, ansible job → falls through", "u-play", "ansible", "open", "", "ssh", "runner", ""},

		// The scope binding: above both defaults...
		{"bound scope, shell job, nothing set → runner", "u-plain", "bash", "dmz-web", "", "", "runner", ""},
		{"bound scope beats a global default of ssh", "u-plain", "bash", "dmz-web", "", "ssh", "runner", ""},
		{"no scope is not a bound scope", "u-plain", "bash", "", "", "", "ssh", ""},
		{"a scope name with no row is not a bound scope", "u-plain", "bash", "never-created", "", "", "ssh", ""},
		// ...and below both explicit choices, where ssh is refused, not moved.
		{"bound scope, job says ssh → refused", "u-ssh", "bash", "dmz-web", "", "", "ssh", CodeScopeRequiresRunner},
		{"bound scope, run says ssh → refused", "u-plain", "bash", "dmz-web", "ssh", "", "ssh", CodeScopeRequiresRunner},
		{"bound scope, job says ssh but the run says runner", "u-ssh", "bash", "dmz-web", "runner", "", "runner", ""},
		{"bound scope, job says runner", "u-runner", "bash", "dmz-web", "", "", "runner", ""},
		// On a bound scope the BINDING is the refusal, even where ssh would also
		// be wrong for the run type: it is the one every producer acts on.
		{"bound scope, ansible job says ssh → the binding refuses", "u-play-ssh", "ansible", "dmz-web", "", "", "ssh", CodeScopeRequiresRunner},
		{"bound scope, ansible run says ssh → the binding refuses", "u-play", "ansible", "dmz-web", "ssh", "", "ssh", CodeScopeRequiresRunner},

		// Explicit choices on an unbound scope.
		{"job says ssh", "u-ssh", "bash", "open", "", "runner", "ssh", ""},
		{"job says runner", "u-runner", "bash", "open", "", "ssh", "runner", ""},
		{"the run's choice beats the job's", "u-runner", "bash", "open", "ssh", "", "ssh", ""},

		// ssh cannot run a toolchain type when someone ASKS for it.
		{"ansible job that says ssh", "u-play-ssh", "ansible", "open", "", "", "ssh", CodeInvalidExecutor},
		{"ansible run that says ssh", "u-play", "ansible", "open", "ssh", "", "ssh", CodeInvalidExecutor},
		{"an override outside the enum", "u-plain", "bash", "open", "local", "", "", CodeInvalidExecutor},
	}
	for _, c := range cases {
		setDefault(c.globalDefault)
		got := ResolveExecutor(context.Background(), f.pool, ExecutorQuery{
			JobUID: c.uid, RunType: c.runType, Scope: c.scope, Override: c.override,
		})
		if got.Err != nil {
			t.Errorf("%s: unexpected error %v", c.name, got.Err)
			continue
		}
		if got.Executor != c.wantExecutor {
			t.Errorf("%s: executor = %q, want %q", c.name, got.Executor, c.wantExecutor)
		}
		code := ""
		if got.Refusal != nil {
			code = got.Refusal.Code
		}
		if code != c.wantRefusal {
			t.Errorf("%s: refusal = %q, want %q", c.name, code, c.wantRefusal)
		}
		if got.ScopeRefused() != (c.wantRefusal == CodeScopeRequiresRunner) {
			t.Errorf("%s: ScopeRefused() = %v", c.name, got.ScopeRefused())
		}
	}
}

// The refusal names the scope and says which of the two explicit rungs asked
// for ssh — "the job" sends the reader to the definition, "this run" to the
// request they just made.
func TestScopeRequiresRunnerRefusalSaysWhoAsked(t *testing.T) {
	f := newUnclaimFixture(t)
	f.exec(`INSERT INTO scopes(id,name,source,created_at) VALUES('s-dmz','dmz-web','cronomicon','t')`)
	f.exec(`INSERT INTO scope_runners(scope_id,runner_id,runner_name,bound_at) VALUES('s-dmz','r1','runner-dmz-01','t')`)
	f.exec(`INSERT INTO jobs(uid,name,source,run_type,executor,synced_at) VALUES('u-ssh','wants-ssh','git','bash','ssh','t')`)
	f.exec(`INSERT INTO jobs(uid,name,source,run_type,synced_at) VALUES('u-plain','plain','git','bash','t')`)

	byJob := ResolveExecutor(context.Background(), f.pool, ExecutorQuery{JobUID: "u-ssh", RunType: "bash", Scope: "dmz-web"})
	byRun := ResolveExecutor(context.Background(), f.pool, ExecutorQuery{JobUID: "u-plain", RunType: "bash", Scope: "dmz-web", Override: "ssh"})
	if byJob.Refusal == nil || byRun.Refusal == nil {
		t.Fatalf("refusals = %+v / %+v, want both refused", byJob.Refusal, byRun.Refusal)
	}
	if !strings.Contains(byJob.Refusal.Message, "dmz-web") || !strings.Contains(byJob.Refusal.Message, "this job's executor") {
		t.Errorf("job refusal = %q, want it to name the scope and the job's setting", byJob.Refusal.Message)
	}
	if !strings.Contains(byRun.Refusal.Message, "dmz-web") || !strings.Contains(byRun.Refusal.Message, "this run asks") {
		t.Errorf("run refusal = %q, want it to name the scope and the run's choice", byRun.Refusal.Message)
	}
}

// TestResolveExecutorReadsTheJobByIdentity: two same-named cronomicon jobs,
// which R2 allows across agencies, each get their OWN executor when the producer
// passes a uid. Without one the (name, source) pair is all there is, and it
// stays a single-row fallback for producers and legacy rows that have none.
func TestResolveExecutorReadsTheJobByIdentity(t *testing.T) {
	f := newUnclaimFixture(t)
	f.exec(`INSERT INTO jobs(uid,name,source,run_type,executor,synced_at) VALUES('u-a','twin','cronomicon','bash','runner','t')`)
	f.exec(`INSERT INTO jobs(uid,name,source,run_type,executor,synced_at) VALUES('u-b','twin','cronomicon','bash','ssh','t')`)
	for uid, want := range map[string]string{"u-a": "runner", "u-b": "ssh"} {
		got := ResolveExecutor(context.Background(), f.pool, ExecutorQuery{
			JobUID: uid, JobSource: "cronomicon", JobName: "twin", RunType: "bash",
		})
		if got.Err != nil || got.Executor != want {
			t.Errorf("uid %s: executor = %q (err %v), want %q", uid, got.Executor, got.Err, want)
		}
	}
	// A uid that matches no job is a run with no job-level choice, not an error.
	got := ResolveExecutor(context.Background(), f.pool, ExecutorQuery{JobUID: "u-none", RunType: "bash"})
	if got.Err != nil || got.Executor != "ssh" {
		t.Errorf("unknown uid: executor = %q (err %v), want the run type's default", got.Executor, got.Err)
	}
}

// A producer with no uid reads the job by (name, source), and the binding and
// the refusal apply exactly as they do by identity.
func TestResolveExecutorByNameOnABoundScope(t *testing.T) {
	f := newUnclaimFixture(t)
	f.exec(`INSERT INTO scopes(id,name,source,created_at) VALUES('s-dmz','dmz-web','cronomicon','t')`)
	f.exec(`INSERT INTO scope_runners(scope_id,runner_id,runner_name,bound_at) VALUES('s-dmz','r1','runner-dmz-01','t')`)
	f.exec(`INSERT INTO jobs(name,source,run_type,synced_at) VALUES('plain','cronomicon','bash','t')`)
	f.exec(`INSERT INTO jobs(name,source,run_type,executor,synced_at) VALUES('legacy','cronomicon','bash','ssh','t')`)

	got := ResolveExecutor(context.Background(), f.pool, ExecutorQuery{
		JobSource: "cronomicon", JobName: "plain", RunType: "bash", Scope: "dmz-web"})
	if got.Err != nil || got.Executor != "runner" || got.Refusal != nil {
		t.Errorf("plain by name = %+v, want runner", got)
	}
	got = ResolveExecutor(context.Background(), f.pool, ExecutorQuery{
		JobSource: "cronomicon", JobName: "legacy", RunType: "bash", Scope: "dmz-web"})
	if !got.ScopeRefused() {
		t.Errorf("legacy by name = %+v, want the scope refusal", got)
	}
}

// TestResolveExecutorFailsClosedWhenItCannotRead: a resolver that cannot read
// the scope's binding must say so. The alternative is to carry on as if the
// scope were unbound, and for a shell job that answer is "ssh" — the confined
// job leaves from the control plane because a query was interrupted. Every
// producer treats Err as "do not produce this run".
func TestResolveExecutorFailsClosedWhenItCannotRead(t *testing.T) {
	f := newUnclaimFixture(t)
	f.exec(`INSERT INTO scopes(id,name,source,created_at) VALUES('s-dmz','dmz-web','cronomicon','t')`)
	f.exec(`INSERT INTO scope_runners(scope_id,runner_id,runner_name,bound_at) VALUES('s-dmz','r1','runner-dmz-01','t')`)
	f.exec(`INSERT INTO jobs(uid,name,source,run_type,synced_at) VALUES('u-plain','plain','git','bash','t')`)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // every read on this context fails
	got := ResolveExecutor(ctx, f.pool, ExecutorQuery{JobUID: "u-plain", RunType: "bash", Scope: "dmz-web"})
	if got.Err == nil {
		t.Fatalf("resolution on an unreadable database = %+v, want an error", got)
	}
	if got.Executor == ExecutorSSH && got.Refusal == nil {
		t.Errorf("an unreadable binding resolved to a clean ssh: %+v", got)
	}
	if bound, err := ScopeIsBound(ctx, f.pool, "dmz-web"); err == nil {
		t.Errorf("ScopeIsBound on an unreadable database = %v with no error, want an error", bound)
	}
}

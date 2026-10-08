package runref

import (
	"context"
	"strings"
	"testing"
)

// LR-47 — a shell run that binds an SSH key is refused when no registered
// agent could take it: the local runner cannot deliver a key file, and a run
// queued for a runner that does not exist holds the fleet cap. It is not
// refused when an agent exists and is merely offline (that is an ordinary
// wait), and a toolchain run type was never the server's to take.
func TestKeyBindingsNeedAgent(t *testing.T) {
	f := newOwnerFixture(t) // agencies ag-a (TeamA), ag-b (TeamB)
	ctx := context.Background()
	job := Owner{Kind: "job", Source: "git", Name: "deploy"}
	script := Owner{Kind: "script", Name: "scripts/deploy.sh"}
	const now = "2026-01-01T00:00:00Z"
	// Scopes: one of TeamA's, one of TeamA's bound to a runner, one of TeamB's.
	for _, sc := range [][2]string{{"sc-a", "a-open"}, {"sc-bound", "a-bound"}, {"sc-b", "b-open"}} {
		f.exec(`INSERT INTO scopes (id, name, source, created_at) VALUES (?, ?, 'cronomicon', ?)`, sc[0], sc[1], now)
		agency := "ag-a"
		if sc[0] == "sc-b" {
			agency = "ag-b"
		}
		f.exec(`INSERT INTO scope_agencies (scope_id, agency_id) VALUES (?, ?)`, sc[0], agency)
	}
	runner := func(id, kind, owner, status string) {
		f.exec(`INSERT INTO runners (id, name, kind, status, registered_at, created_at, owner_agency)
		        VALUES (?, ?, ?, ?, ?, ?, ?)`, id, id, kind, status, now, now, owner)
	}
	reset := func() {
		f.exec(`DELETE FROM scope_runners`)
		f.exec(`DELETE FROM runners`)
	}
	key := map[Owner][]Binding{job: {{Kind: KindKey, Name: "deploy_key"}}}

	perRunKey := []Binding{{Kind: KindKey, Name: "adhoc_key"}}
	cases := []struct {
		name    string
		bind    map[Owner][]Binding
		perRun  []Binding
		runType string
		scope   string
		fleet   func()
		want    []string // references: the run's own first, then in owner order
	}{
		{"no bindings", nil, nil, "bash", "a-open", func() {}, nil},
		{"a secret is not a key", map[Owner][]Binding{job: {{Kind: KindSecret, Name: "DB_PASS"}}}, nil, "bash", "a-open", func() {}, nil},
		{"a key and no runner at all", key, nil, "bash", "a-open", func() {}, []string{"CRONOMICON_KEY_deploy_key"}},
		{"a key and only the local runner", key, nil, "bash", "a-open", func() {
			runner("local", "server", "global", "online")
			f.exec(`INSERT INTO runner_agencies (runner_id, agency_id) VALUES ('local', 'ag-a')`)
		}, []string{"CRONOMICON_KEY_deploy_key"}},
		{"a key and an agent of the scope's agency, offline", key, nil, "bash", "a-open", func() {
			runner("r-a", "agent", "ag-a", "offline")
		}, nil},
		{"a key and an agent of ANOTHER agency", key, nil, "bash", "a-open", func() {
			runner("r-b", "agent", "ag-b", "online")
		}, []string{"CRONOMICON_KEY_deploy_key"}},
		{"a key, a bound scope, and the agent is not the one bound", key, nil, "bash", "a-bound", func() {
			runner("r-a", "agent", "ag-a", "online")
			runner("local", "server", "global", "online")
			f.exec(`INSERT INTO runner_agencies (runner_id, agency_id) VALUES ('local', 'ag-a')`)
			f.exec(`INSERT INTO scope_runners (scope_id, runner_id, runner_name, bound_by, bound_at) VALUES ('sc-bound', 'local', 'local', 't', ?)`, now)
		}, []string{"CRONOMICON_KEY_deploy_key"}},
		{"a key, a bound scope, and the agent is bound", key, nil, "bash", "a-bound", func() {
			runner("r-a", "agent", "ag-a", "online")
			f.exec(`INSERT INTO scope_runners (scope_id, runner_id, runner_name, bound_by, bound_at) VALUES ('sc-bound', 'r-a', 'r-a', 't', ?)`, now)
		}, nil},
		{"a key and no scope: Global's agent", key, nil, "bash", "", func() {
			runner("r-g", "agent", "global", "online")
		}, nil},
		{"a key and no scope: an agency's agent does not serve Global", key, nil, "bash", "", func() {
			runner("r-a", "agent", "ag-a", "online")
		}, []string{"CRONOMICON_KEY_deploy_key"}},
		{"an ansible run waits for a capable agent, as it always has", key, nil, "ansible", "a-open", func() {}, nil},
		{"an aliased key is named by its alias", map[Owner][]Binding{job: {{Kind: KindKey, Name: "deploy_key", As: "GIT_KEY"}}}, nil, "bash", "a-open", func() {},
			[]string{"CRONOMICON_KEY_GIT_KEY"}},
		{"the job's key and the script's", map[Owner][]Binding{
			job: {{Kind: KindKey, Name: "deploy_key"}}, script: {{Kind: KindSecret, Name: "X"}, {Kind: KindKey, Name: "script_key"}},
		}, nil, "python", "a-open", func() {}, []string{"CRONOMICON_KEY_deploy_key", "CRONOMICON_KEY_script_key"}},
		// A key added to this one run counts like a declared one.
		{"a key named only on the run, and no agent", nil, perRunKey, "bash", "a-open", func() {},
			[]string{"CRONOMICON_KEY_adhoc_key"}},
		{"a key named only on the run, and an agent", nil, perRunKey, "bash", "a-open", func() {
			runner("r-a", "agent", "ag-a", "online")
		}, nil},
		// A scope name with no scope row is Global's, as the run writer stamps it.
		{"a key and a scope name that is no scope: Global's agent", key, nil, "bash", "no-such-scope", func() {
			runner("r-g", "agent", "global", "online")
		}, nil},
		{"a key and a scope name that is no scope: no Global agent", key, nil, "bash", "no-such-scope", func() {
			runner("r-a", "agent", "ag-a", "online")
		}, []string{"CRONOMICON_KEY_deploy_key"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reset()
			tc.fleet()
			bind(t, f, job)
			bind(t, f, script)
			for o, bs := range tc.bind {
				bind(t, f, o, bs...)
			}
			got, err := KeyBindingsNeedAgent(ctx, f.pool, []Owner{job, script}, tc.perRun, tc.runType, tc.scope)
			if err != nil {
				t.Fatal(err)
			}
			var refs []string
			for _, b := range got {
				refs = append(refs, b.InjectReference())
			}
			if strings.Join(refs, ",") != strings.Join(tc.want, ",") {
				t.Errorf("refs = %v, want %v", refs, tc.want)
			}
		})
	}
}

func TestKeyBindingRefusalNamesTheReferenceAndTheWayOut(t *testing.T) {
	if got := KeyBindingRefusal(nil); got != "" {
		t.Errorf("empty refusal = %q", got)
	}
	one := KeyBindingRefusal([]Binding{{Kind: KindKey, Name: "k", As: "GIT_KEY"}})
	for _, want := range []string{"CRONOMICON_KEY_GIT_KEY", "agent", "Secret"} {
		if !strings.Contains(one, want) {
			t.Errorf("refusal %q does not mention %q", one, want)
		}
	}
	if strings.Contains(one, "more") {
		t.Errorf("single-key refusal must not say 'more': %q", one)
	}
	two := KeyBindingRefusal([]Binding{{Kind: KindKey, Name: "a"}, {Kind: KindKey, Name: "b"}})
	if !strings.Contains(two, "(and 1 more)") {
		t.Errorf("two-key refusal = %q, want '(and 1 more)'", two)
	}
	if !strings.HasPrefix(ReasonKeyBindingNeedsAgent, "Skipped: ") {
		t.Errorf("stored reason must follow the 'Skipped: …' sentence convention: %q", ReasonKeyBindingNeedsAgent)
	}
}

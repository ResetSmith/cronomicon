package runref

import (
	"context"
	"strings"
	"testing"
)

// KB-1 — the check keys on the RESOLVED executor and on binding kind, nothing
// else: no ownership, no scope, no existence check on the credential row.
func TestKeyBindingsOnSSH(t *testing.T) {
	f := newOwnerFixture(t)
	ctx := context.Background()
	job := Owner{Kind: "job", Source: "git", Name: "deploy"}
	script := Owner{Kind: "script", Name: "scripts/deploy.sh"}

	cases := []struct {
		name     string
		bind     map[Owner][]Binding
		executor string
		want     []string // references, in owner order
	}{
		{"no bindings, ssh", nil, "ssh", nil},
		{"secret only, ssh", map[Owner][]Binding{job: {{Kind: KindSecret, Name: "DB_PASS"}}}, "ssh", nil},
		{"key, runner", map[Owner][]Binding{job: {{Kind: KindKey, Name: "deploy_key"}}}, "runner", nil},
		{"key, empty executor", map[Owner][]Binding{job: {{Kind: KindKey, Name: "deploy_key"}}}, "", nil},
		{"key, ssh", map[Owner][]Binding{job: {{Kind: KindKey, Name: "deploy_key"}}}, "ssh", []string{"CRONOMICON_KEY_deploy_key"}},
		{"aliased key, ssh", map[Owner][]Binding{job: {{Kind: KindKey, Name: "deploy_key", As: "GIT_KEY"}}}, "ssh", []string{"CRONOMICON_KEY_GIT_KEY"}},
		{"job key + script key, ssh", map[Owner][]Binding{
			job: {{Kind: KindKey, Name: "deploy_key"}}, script: {{Kind: KindSecret, Name: "X"}, {Kind: KindKey, Name: "script_key"}},
		}, "ssh", []string{"CRONOMICON_KEY_deploy_key", "CRONOMICON_KEY_script_key"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bind(t, f, job)
			bind(t, f, script)
			for o, bs := range tc.bind {
				bind(t, f, o, bs...)
			}
			got, err := KeyBindingsOnSSH(ctx, f.pool, []Owner{job, script}, tc.executor)
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
	for _, want := range []string{"CRONOMICON_KEY_GIT_KEY", "runner", "Secret"} {
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
	if !strings.HasPrefix(ReasonKeyBindingOnSSH, "Skipped: ") {
		t.Errorf("stored reason must follow the 'Skipped: …' sentence convention: %q", ReasonKeyBindingOnSSH)
	}
}

package runref

import "testing"

func TestOverrideBindings(t *testing.T) {
	t.Run("empty and malformed envelopes yield nil", func(t *testing.T) {
		for _, in := range []string{"", "{", "[]", `{"env":{"A":"b"}}`, `{"references":"nope"}`} {
			if got := OverrideBindings(in); got != nil {
				t.Errorf("OverrideBindings(%q) = %v, want nil", in, got)
			}
		}
	})

	t.Run("parses, derives references, drops invalid, dedupes", func(t *testing.T) {
		in := `{"env":{"A":"b"},"references":[
			{"kind":"secret","name":"DB_PASS"},
			{"kind":"var","name":"REGION"},
			{"kind":"key","name":"deploy"},
			{"kind":"secret","name":"DB_PASS"},
			{"kind":"bogus","name":"X"},
			{"kind":"var","name":""}
		]}`
		got := OverrideBindings(in)
		want := []Binding{
			{Kind: KindSecret, Name: "DB_PASS", Reference: "AMADEUS_SECRET_DB_PASS"},
			{Kind: KindVar, Name: "REGION", Reference: "AMADEUS_VAR_REGION"},
			{Kind: KindKey, Name: "deploy", Reference: "AMADEUS_KEY_deploy"},
		}
		if len(got) != len(want) {
			t.Fatalf("got %d bindings, want %d: %v", len(got), len(want), got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("binding %d = %+v, want %+v", i, got[i], want[i])
			}
		}
	})
}

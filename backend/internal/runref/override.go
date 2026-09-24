package runref

import "encoding/json"

// OverrideBindings extracts the per-run reference additions from a run's
// override_json envelope (the F3 ad-hoc envelope; "references" is written by the
// manual runJob trigger only). These are the operator's one-off additions on top
// of the job's/script's declared bindings — the run-time counterpart of the
// reference_bindings table, resolved by the same dispatch resolver under the same
// scope/agency rules (V2-11, narrowed to stored-reference ADDITIONS).
//
// Tolerance mirrors execspec.OverrideHosts: an empty/absent/malformed envelope
// yields nil rather than an error — a run with no envelope is the common case, and
// dispatch must not fail on a decode problem in an auxiliary audit blob. Entries
// are validated defensively even though runJob validates at enqueue (an invalid
// kind or empty name is dropped, never resolved), deduped by kind+name+alias
// (RA-4), and carry the derived AMADEUS_<SECTION>_<name> reference like every read
// path.
//
// RA-7: an entry's optional `as` is the alias the operator attached the row under.
// A MALFORMED alias is dropped to "" rather than dropping the whole entry — the
// binding still names a row the actor was authorized to attach, and injecting it
// under its own name is the conservative reading. (runJob validates the alias at
// enqueue, so this only fires on a hand-edited envelope.)
func OverrideBindings(overrideJSON string) []Binding {
	if overrideJSON == "" {
		return nil
	}
	var envelope struct {
		References []struct {
			Kind string `json:"kind"`
			Name string `json:"name"`
			As   string `json:"as"`
		} `json:"references"`
	}
	if err := json.Unmarshal([]byte(overrideJSON), &envelope); err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []Binding
	for _, r := range envelope.References {
		k := Kind(r.Kind)
		if !ValidKind(k) || r.Name == "" {
			continue
		}
		alias := r.As
		if alias != "" && ValidateName(k, alias) != nil {
			alias = ""
		}
		b := Binding{Kind: k, Name: r.Name, As: alias, Reference: k.Reference(r.Name)}
		key := DedupeKey(b)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, b)
	}
	return out
}

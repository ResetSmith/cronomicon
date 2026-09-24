// Package envmerge layers run environment maps in precedence order for the Job
// Composer V2 job-level env feature (JC11). The effective env of a run is built by
// overlaying, least- to most-specific: job-level env (JC10 base) → schedule env →
// per-run override (manual runs have no schedule layer; workflow steps layer
// job-env → parent-workflow env → step inputs). Later layers win on key collision.
//
// It is a leaf package (stdlib only) so the scheduler, api, and workflow enqueue
// sites can share one implementation without an import cycle. The merge is a strict
// no-op when every layer is empty — an all-empty merge yields nil / "" so callers
// leave runs.env_json NULL and pre-feature behavior is preserved exactly (R2).
package envmerge

import "maps"

import "encoding/json"

// Merge overlays env layers in order; later layers win on key collision. Returns
// nil when the merged result is empty so callers can leave env_json NULL.
func Merge(layers ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range layers {
		maps.Copy(out, m)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Parse decodes a JSON-object env string into a map. Returns nil for an empty or
// malformed string, so an absent layer contributes nothing to a Merge.
func Parse(s string) map[string]string {
	if s == "" {
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil
	}
	return m
}

// MergeJSON parses JSON-object env layers, merges them (later wins), and returns the
// marshaled result — or "" when the result is empty (the NULL no-op). Empty or
// malformed layers are skipped. encoding/json marshals map keys in sorted order, so
// a single non-empty JSON layer round-trips byte-for-byte.
func MergeJSON(layers ...string) string {
	maps := make([]map[string]string, 0, len(layers))
	for _, s := range layers {
		maps = append(maps, Parse(s))
	}
	merged := Merge(maps...)
	if merged == nil {
		return ""
	}
	b, _ := json.Marshal(merged)
	return string(b)
}

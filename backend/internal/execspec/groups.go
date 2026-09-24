package execspec

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/ResetSmith/cronomicon/internal/inventory"
)

// Group/limit targeting (M3, the ansible-inventory plan §8). ResolveRun is the
// single (scope, hosts[], groups[]) → (targets, --limit) mapper both executors
// share, so the SSH host-set and the runner's ansible --limit can't drift.

// OverrideGroups extracts the chosen target groups from a run's override_json
// envelope, mirroring OverrideHosts. Returns nil for an empty/absent/malformed
// envelope.
func OverrideGroups(overrideJSON string) []string {
	if overrideJSON == "" {
		return nil
	}
	var env struct {
		Groups []string `json:"groups"`
	}
	if err := json.Unmarshal([]byte(overrideJSON), &env); err != nil {
		return nil
	}
	return env.Groups
}

// OverrideAnsibleLimit extracts the raw `--limit` passthrough (the ansible-only
// escape hatch, OD-15) from override_json. Empty when absent. SSH never sees this
// — the trigger boundary 422s a raw ansibleLimit on the SSH path.
func OverrideAnsibleLimit(overrideJSON string) string {
	if overrideJSON == "" {
		return ""
	}
	var env struct {
		AnsibleLimit string `json:"ansibleLimit"`
	}
	if err := json.Unmarshal([]byte(overrideJSON), &env); err != nil {
		return ""
	}
	return env.AnsibleLimit
}

// AnsibleLimit builds an `ansible --limit` expression from explicit host names
// UNION literal group NAMES (OD-16) — a comma-joined OR pattern. Groups are
// passed by NAME (ansible recomputes membership from the raw inventory for exact
// semantics). Any name failing inventory.ValidName is REFUSED (never emitted) so
// an ansible pattern metacharacter (`, : ! & *` / whitespace) can never broaden
// the run or desync the SSH host set (§4.5). Empty inputs ⇒ "" ⇒ no --limit.
func AnsibleLimit(hosts, groups []string) string {
	parts := make([]string, 0, len(hosts)+len(groups))
	for _, h := range hosts {
		if inventory.ValidName(h) {
			parts = append(parts, h)
		}
	}
	for _, g := range groups {
		if inventory.ValidName(g) {
			parts = append(parts, g)
		}
	}
	return strings.Join(parts, ",")
}

// RunLimit computes a run's effective `ansible --limit` from the raw override
// envelope, folding in the job definition's single-host pin (runs.target_host,
// TG-3). This is what the runner manifest ships, in BOTH inventory modes.
//
// Precedence, extending the pre-existing manifest logic by one step:
//   - a raw `ansibleLimit` passthrough (OD-15) wins outright — an operator writing
//     a raw pattern has taken manual control of targeting, pin included. The pin is
//     NOT folded in, so a metachar pin is harmless on this branch;
//   - otherwise PinnedAnsibleLimit over the envelope's hosts/groups.
//
// NOTE the caller's obligation on the second branch: AnsibleLimit silently REFUSES
// a name that fails inventory.ValidName, so a metachar pin yields an empty limit —
// i.e. NO --limit, i.e. the full inventory. That is the exact silent-widen this
// module forbids, so a pin reaching here must already have been validated
// (compose-time 422, trigger-boundary 422) or is refused at the manifest boundary
// (409). See ValidName.
func RunLimit(overrideJSON, targetHost string) string {
	if raw := OverrideAnsibleLimit(overrideJSON); raw != "" {
		return raw
	}
	return PinnedAnsibleLimit(OverrideHosts(overrideJSON), OverrideGroups(overrideJSON), targetHost)
}

// PinnedAnsibleLimit is AnsibleLimit with the single-host pin applied. It is the
// structured half of RunLimit, split out because ResolveRun already holds parsed
// hosts/groups and has no override envelope to re-parse.
//
// CRITICAL — the precedence here MIRRORS ResolveTargets, and must keep doing so:
// a non-empty targetHost wins OUTRIGHT and the hosts/groups subset is ignored,
// exactly as ResolveTargets returns the single pinned Target and ignores its hosts
// argument. That is what keeps the SSH host set and the ansible --limit identical
// (the parity invariant this module exists to enforce). Unioning them instead
// would make --limit BROADER than the set SSH dials — a comma in an ansible
// pattern is a union, not an intersection — which is the drift, in the dangerous
// direction.
//
// The two are never both set today: the trigger boundary CLEARS target_host
// whenever a hosts/groups subset is supplied (execution_mount.go). This function
// does not rely on that, so the invariant holds even if that clearing is ever
// removed.
func PinnedAnsibleLimit(hosts, groups []string, targetHost string) string {
	if targetHost != "" {
		return AnsibleLimit([]string{targetHost}, nil)
	}
	return AnsibleLimit(hosts, groups)
}

// ValidName reports whether a host/group NAME is safe to feed `ansible --limit`
// (no pattern metacharacter). It re-exports inventory.ValidName so the trigger
// boundary can reject a metachar host loudly rather than have AnsibleLimit drop
// it silently (which would broaden an ansible run or desync the two executors).
func ValidName(name string) bool { return inventory.ValidName(name) }

// ScopeGroups returns the group names of a scope's parsed projection, mirroring
// ScopeHosts. It is the membership source for the trigger-boundary group check.
func ScopeGroups(ctx context.Context, db *sql.DB, scope string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT sg.name
		FROM scope_groups sg
		JOIN scopes s ON s.id = sg.scope_id
		WHERE s.name = ?
		ORDER BY sg.name`, scope)
	if err != nil {
		return nil, fmt.Errorf("read scope groups: %w", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		names = append(names, n)
	}
	return names, rows.Err()
}

// ResolveRun expands a run's targeting — explicit host subset + target groups —
// into the shared Target set AND the ansible `--limit` expression (§8.1). Group
// members are expanded from the projection (TRANSITIVELY through [group:children],
// to mirror exactly how `ansible --limit <group>` resolves from the raw inventory)
// and unioned with the explicit host subset, then resolved through the SAME
// per-host path as ResolveTargets — so the SSH executor dials the identical set
// the --limit names. Empty hosts+groups ⇒ full scope fan-out + no --limit (legacy).
//
// CRITICAL parity rule: a targeting selection that resolves to ZERO hosts (e.g. an
// empty or child-only group whose children have no members) must NOT fall through
// to a full-scope fan-out — that would silently WIDEN the blast radius beyond the
// operator's selection. We short-circuit to an empty target set instead.
func ResolveRun(ctx context.Context, db *sql.DB, scope, targetHost string, hosts, groups []string) (targets []Target, limit string, err error) {
	// TG-3: the pin is part of the limit, not just of the Target set. Before this,
	// an ansible run pinned to one host shipped NO --limit and ran the whole
	// inventory while the SSH executor dialed exactly one host — the precise
	// two-executor drift this module exists to prevent.
	limit = PinnedAnsibleLimit(hosts, groups, targetHost)

	requested := len(hosts) > 0 || len(groups) > 0
	effective := dedupStrings(hosts)
	if len(groups) > 0 {
		members, gerr := expandGroupMembers(ctx, db, scope, groups)
		if gerr != nil {
			return nil, "", gerr
		}
		seen := make(map[string]bool, len(effective))
		for _, h := range effective {
			seen[h] = true
		}
		for _, m := range members {
			if !seen[m] {
				seen[m] = true
				effective = append(effective, m)
			}
		}
	}

	// Targeting was requested but resolved to nothing ⇒ run nothing (not the whole
	// scope). A bare targetHost is handled by ResolveTargets and isn't "requested"
	// targeting here, so it is excluded.
	if targetHost == "" && requested && len(effective) == 0 {
		return nil, limit, nil
	}

	targets, err = ResolveTargets(ctx, db, scope, targetHost, effective)
	return targets, limit, err
}

// expandGroupMembers expands groups to their member hosts, following
// [group:children] nesting TRANSITIVELY (with a cycle guard) so the SSH executor's
// Target set equals what `ansible --limit <group>` resolves from the raw inventory.
// It loads the scope's full group graph in two drained queries (no nested cursor —
// SQLite pool rule), then walks it in memory.
func expandGroupMembers(ctx context.Context, db *sql.DB, scope string, groups []string) ([]string, error) {
	childMap, err := readGroupEdges(ctx, db, scope,
		`SELECT sgc.parent, sgc.child FROM scope_group_children sgc JOIN scopes s ON s.id = sgc.scope_id WHERE s.name = ?`)
	if err != nil {
		return nil, err
	}
	hostMap, err := readGroupEdges(ctx, db, scope,
		`SELECT sgh.group_name, sgh.host FROM scope_group_hosts sgh JOIN scopes s ON s.id = sgh.scope_id WHERE s.name = ?`)
	if err != nil {
		return nil, err
	}
	var out []string
	outSeen := map[string]bool{}
	visited := map[string]bool{}
	var visit func(g string)
	visit = func(g string) {
		if visited[g] {
			return // cycle / already-expanded guard
		}
		visited[g] = true
		for _, h := range hostMap[g] {
			if !outSeen[h] {
				outSeen[h] = true
				out = append(out, h)
			}
		}
		for _, c := range childMap[g] {
			visit(c)
		}
	}
	for _, g := range groups {
		visit(g)
	}
	return out, nil
}

// readGroupEdges runs a (key, value) query and groups values by key, fully
// draining the cursor before returning (so the next query gets a free connection).
func readGroupEdges(ctx context.Context, db *sql.DB, scope, query string) (map[string][]string, error) {
	rows, err := db.QueryContext(ctx, query, scope)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string][]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		m[k] = append(m[k], v)
	}
	return m, rows.Err()
}

func dedupStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// ── Phase 3 · advanced ansible run options (RP-16) ──────────────────────────

// AnsibleOpts is the per-run advanced ansible option set an operator supplied at
// trigger time. Like the raw `--limit` above it lives ONLY in the run's
// override_json envelope — there are no jobs/runs columns and no producer fold,
// because these are the flags reached for AT trigger time ("dry-run this once",
// "just the certs tag"), not standing job properties.
//
// Names-only/plain-value by construction: every field is a flag, a NAME, or an
// operator-typed literal. Nothing here is resolved against stored material.
type AnsibleOpts struct {
	Check      bool              `json:"ansibleCheck,omitempty"`
	Diff       bool              `json:"ansibleDiff,omitempty"`
	Tags       []string          `json:"ansibleTags,omitempty"`
	SkipTags   []string          `json:"ansibleSkipTags,omitempty"`
	Verbosity  int               `json:"ansibleVerbosity,omitempty"`
	Become     bool              `json:"ansibleBecome,omitempty"`
	BecomeUser string            `json:"ansibleBecomeUser,omitempty"`
	ExtraVars  map[string]string `json:"ansibleExtraVars,omitempty"`
}

// Any reports whether the operator set any advanced option at all. It is the
// predicate behind the protocol-v8 gate: a run that carries none is wire- and
// behavior-identical to a pre-Phase-3 run and must stay claimable by any agent.
func (o AnsibleOpts) Any() bool {
	// Verbosity is tested for != 0, not > 0: a NEGATIVE value is nonsense input
	// that must reach the boundary's range check and 422 there, rather than be
	// silently swallowed as "no options set". (The agent still only renders a
	// -v run for v > 0 — strings.Repeat panics on a negative count.)
	return o.Check || o.Diff || len(o.Tags) > 0 || len(o.SkipTags) > 0 ||
		o.Verbosity != 0 || o.Become || o.BecomeUser != "" || len(o.ExtraVars) > 0
}

// AnsibleOptions extracts the advanced option set from override_json. A blank or
// unparseable envelope yields the zero value (no options), matching every other
// Override* reader: a malformed envelope must degrade to "nothing was
// overridden", never to a half-applied run.
func AnsibleOptions(overrideJSON string) AnsibleOpts {
	if overrideJSON == "" {
		return AnsibleOpts{}
	}
	var o AnsibleOpts
	if err := json.Unmarshal([]byte(overrideJSON), &o); err != nil {
		return AnsibleOpts{}
	}
	return o
}

// AnsibleIdentityVars are the two connection variables the "connect as" identity
// owns (Phase 2). Operator-supplied extra-vars may not set them: `-e` is the
// same tier identity uses and the last occurrence wins, so allowing these would
// let a run connect as someone other than what its own audit record claims.
// Every OTHER ansible_* name stays allowed — ansible_python_interpreter and
// friends are legitimate and common.
var AnsibleIdentityVars = []string{"ansible_user", "ansible_ssh_private_key_file"}

// ReservedAnsibleVar reports whether an extra-var name is identity-owned.
func ReservedAnsibleVar(name string) bool {
	return slices.Contains(AnsibleIdentityVars, name)
}

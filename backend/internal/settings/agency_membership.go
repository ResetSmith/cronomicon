package settings

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// Agency membership across the four entity types that carry isolation
// (the agencies plan Phase 2, T2.6/T2.7 — migration 670).
//
// Runners already have this (SetRunnerAgencies, M2); scopes, secrets, variables
// and SSH keys get the same shape here, so "what belongs to DSS?" becomes one
// question with one answer instead of four mechanisms on two axes.
//
// THESE TABLES ARE NOW LOAD-BEARING (Phase 3). claimRun intersects a run's agency
// snapshot against runner_agencies, and reference resolution applies the AG-Q1(b)
// clause using secret_agencies / env_var_agencies / ssh_credential_agencies. A
// membership edit therefore changes which runs can reach which secrets — which is
// why every write here is audited, and why the endpoints are ConfigureApp-gated
// rather than sitting behind the weaker env-var permission.
//
// The governing convention throughout: an EMPTY membership set means "no agency
// restriction", NOT "reachable from nowhere". That is what keeps every global row
// global, what made migration 670's backfill behavior-preserving, and what keeps
// AG-Q5's key tightening from being a cliff on upgrade.

// ErrUnknownMember is returned when membership references an entity id that is not
// in the corresponding catalog — the write-time integrity guard, mirroring
// ErrUnknownRunner on the runner side (mapped 422).
var ErrUnknownMember = errors.New("unknown membership target")

// ErrOwnerRemoval is returned when a membership write would drop an entity's OWNING
// agency from its own visibility set (RA-15). Mapped 422 by the API.
var ErrOwnerRemoval = errors.New("cannot remove an entity's owning agency from its membership")

// MemberKind selects which entity type a membership assignment targets. The wire
// value is the kind segment of the endpoint path (scope / secret / env-var /
// ssh-credential), so the API layer never maps strings to tables itself.
type MemberKind string

const (
	MemberScope         MemberKind = "scope"
	MemberSecret        MemberKind = "secret"
	MemberEnvVar        MemberKind = "env-var"
	MemberSSHCredential MemberKind = "ssh-credential"
)

// memberTables maps a kind to its join table, the join table's entity column, and
// the catalog table the entity must exist in. Every value here is a compile-time
// constant — none of it is ever built from user input.
type memberTable struct {
	join    string // the membership join table
	col     string // its entity-id column
	catalog string // the table the entity id must exist in
	label   string // audit/display noun
}

func memberTableFor(k MemberKind) (memberTable, bool) {
	switch k {
	case MemberScope:
		return memberTable{"scope_agencies", "scope_id", "scopes", "scope"}, true
	case MemberSecret:
		return memberTable{"secret_agencies", "secret_id", "secrets", "secret"}, true
	case MemberEnvVar:
		return memberTable{"env_var_agencies", "env_var_id", "env_vars", "variable"}, true
	case MemberSSHCredential:
		return memberTable{"ssh_credential_agencies", "credential_id", "ssh_credentials", "SSH key"}, true
	}
	return memberTable{}, false
}

// AgencyMembership is one entity's agency set. The matrix endpoint is a list of
// these; PUT replaces the set for each posted entity and leaves the rest alone —
// the replace-per-row model of the scope-restrictions and runner-agencies matrices.
type AgencyMembership struct {
	ID        string   `json:"id"`
	AgencyIDs []string `json:"agencyIds"`
}

// ListAgencyMembership returns the entity→agency grid for one kind. Only entities
// with at least one agency appear — an absent row means "no membership", which
// under AG-Q1(b) is what keeps a global secret visible everywhere.
func ListAgencyMembership(ctx context.Context, database *sql.DB, kind MemberKind) ([]AgencyMembership, error) {
	t, ok := memberTableFor(kind)
	if !ok {
		return nil, fmt.Errorf("invalid membership kind %q", string(kind))
	}
	rows, err := database.QueryContext(ctx,
		`SELECT `+t.col+`, agency_id FROM `+t.join+` ORDER BY `+t.col+`, agency_id`)
	if err != nil {
		// Best-effort on a pre-670 schema, matching ListRunnerAgencies.
		return []AgencyMembership{}, nil
	}
	defer rows.Close()
	byEntity := map[string][]string{}
	var order []string
	for rows.Next() {
		var eid, aid string
		if err := rows.Scan(&eid, &aid); err != nil {
			return nil, err
		}
		if _, seen := byEntity[eid]; !seen {
			order = append(order, eid)
		}
		byEntity[eid] = append(byEntity[eid], aid)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]AgencyMembership, 0, len(order))
	for _, eid := range order {
		out = append(out, AgencyMembership{ID: eid, AgencyIDs: byEntity[eid]})
	}
	return out, nil
}

// SetAgencyMembership replaces the agency set of each POSTED entity. Every entity
// and agency id is validated against its catalog FIRST — fail-closed, no partial
// write — so a typo cannot leave half a matrix applied. Mirrors SetRunnerAgencies
// exactly.
//
// The scopes.agency_id mirror this used to dual-write is gone with the column
// (migration 700, T3.9): scope_agencies is the only binding now, and a scope may
// hold several agencies without anything truncating the set.
func SetAgencyMembership(ctx context.Context, database *sql.DB, kind MemberKind, assignments []AgencyMembership, actor string) error {
	t, ok := memberTableFor(kind)
	if !ok {
		return fmt.Errorf("invalid membership kind %q", string(kind))
	}
	for _, a := range assignments {
		var ec int
		_ = database.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+t.catalog+` WHERE id=?`, a.ID).Scan(&ec)
		if ec == 0 {
			return ErrUnknownMember
		}
		for _, aid := range a.AgencyIDs {
			var ac int
			_ = database.QueryRowContext(ctx, `SELECT COUNT(*) FROM agencies WHERE id=?`, aid).Scan(&ac)
			if ac == 0 {
				return ErrUnknownAgency
			}
		}
		// RA-15: an OWNED row must keep its owner in its visibility set. Membership is
		// what the resolver intersects against, so dropping the owner would leave a row
		// its owning department cannot reach — and, worse, that no longer resolves for
		// the very runs it was created for, while still occupying the owner's slot in
		// the (key, scope, owner) uniqueness key. That state is not expressible through
		// any other route and there is no reading of it that is what the operator meant.
		// Ownership TRANSFER is a separate, deliberate action; this is the accidental
		// case, and it is refused.
		if owner := entityOwner(ctx, database, t.catalog, a.ID); owner != "" {
			if !containsID(a.AgencyIDs, owner) {
				return fmt.Errorf("%w: %s %s is owned by that agency, so it cannot be removed from it — transfer ownership first",
					ErrOwnerRemoval, t.label, a.ID)
			}
		}
	}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	for _, a := range assignments {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+t.join+` WHERE `+t.col+`=?`, a.ID); err != nil {
			return err
		}
		seen := map[string]bool{}
		ids := append([]string(nil), a.AgencyIDs...)
		sort.Strings(ids)
		for _, aid := range ids {
			if seen[aid] {
				continue
			}
			seen[aid] = true
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO `+t.join+` (`+t.col+`, agency_id) VALUES (?, ?)`, a.ID, aid); err != nil {
				return err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	// T2.7 — membership is an ACCESS-CONTROL fact (it decides which runs can reach
	// which secrets), so every change belongs in change_log alongside the agency CRUD
	// it sits beside. The detail names the entities touched, never a value.
	audit(ctx, database, actor, "Agencies", "membership-updated",
		t.label+" agencies", membershipAuditDetail(assignments))
	return nil
}

// membershipAuditDetail summarizes an assignment batch for the audit row: entity
// ids and their resulting agency counts. Ids only — no names, no values — and
// capped so a bulk matrix save cannot write an unbounded row.
func membershipAuditDetail(assignments []AgencyMembership) string {
	const cap = 20
	parts := make([]string, 0, len(assignments))
	for i, a := range assignments {
		if i == cap {
			parts = append(parts, fmt.Sprintf("… and %d more", len(assignments)-cap))
			break
		}
		parts = append(parts, fmt.Sprintf("%s=%d", a.ID, len(a.AgencyIDs)))
	}
	return strings.Join(parts, " ")
}

// AgencyMemberCounts returns, per agency id, how many rows of each entity kind
// belong to it. Used by the delete guard (T2.8) and, later, the Phase-4 matrix.
func AgencyMemberCounts(ctx context.Context, database *sql.DB, agencyID string) (map[MemberKind]int, error) {
	out := map[MemberKind]int{}
	for _, k := range []MemberKind{MemberScope, MemberSecret, MemberEnvVar, MemberSSHCredential} {
		t, _ := memberTableFor(k)
		var n int
		// Best-effort per table: a pre-670 schema reports zero rather than failing the
		// whole guard, which would make an agency undeletable on an old schema.
		if err := database.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM `+t.join+` WHERE agency_id=?`, agencyID).Scan(&n); err == nil {
			out[k] = n
		}
	}
	return out, nil
}

// ── The at-a-glance surface (Phase 4, T4.1/T4.2) ────────────────────────────
//
// The originating debugging session was lost because no surface answered "what
// belongs to DSS?" in one view — the operator had to hold five facts across four
// mechanisms at once. These two reads are that view's data.

// MatrixRow is one entity in the membership matrix: which kind it is, what it is
// called, and which agencies it belongs to. Scope is the entity's own scope where
// it has one ("" = global), so the matrix can show WHY two same-named rows differ.
type MatrixRow struct {
	Kind      string   `json:"kind"`
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Scope     string   `json:"scope"`
	AgencyIDs []string `json:"agencyIds"`
}

// AgencyMatrix is the whole grid: the agency columns plus every row that could
// belong to one, INCLUDING rows with no membership. That inclusion is the point —
// an empty row is not missing information, it is the statement "unrestricted,
// reachable from everywhere", which is exactly the fact an operator needs when
// deciding whether assigning membership will break something.
type AgencyMatrix struct {
	Agencies []Agency    `json:"agencies"`
	Rows     []MatrixRow `json:"rows"`
}

// BuildAgencyMatrix assembles the grid. Every catalog is read in full and joined
// in Go against one membership query per kind — never a query inside an open
// cursor (the pool deadlock in db.maxOpenConns).
func BuildAgencyMatrix(ctx context.Context, database *sql.DB) (*AgencyMatrix, error) {
	out := &AgencyMatrix{Agencies: []Agency{}, Rows: []MatrixRow{}}
	agencies, err := ListAgencies(ctx, database)
	if err != nil {
		return nil, err
	}
	out.Agencies = agencies

	// kind → (entity id → agency ids), one query per kind.
	membership := map[string]map[string][]string{}
	for _, k := range []MemberKind{MemberScope, MemberSecret, MemberEnvVar, MemberSSHCredential} {
		list, err := ListAgencyMembership(ctx, database, k)
		if err != nil {
			return nil, err
		}
		m := map[string][]string{}
		for _, a := range list {
			m[a.ID] = a.AgencyIDs
		}
		membership[string(k)] = m
	}
	runnerMembers, err := ListRunnerAgencies(ctx, database)
	if err != nil {
		return nil, err
	}
	runnerMap := map[string][]string{}
	for _, r := range runnerMembers {
		runnerMap[r.RunnerID] = r.AgencyIDs
	}

	add := func(kind, query string, member map[string][]string) error {
		rows, err := database.QueryContext(ctx, query)
		if err != nil {
			return nil // best-effort per catalog: one missing table must not blank the grid
		}
		defer rows.Close()
		for rows.Next() {
			var r MatrixRow
			var scope sql.NullString
			if err := rows.Scan(&r.ID, &r.Name, &scope); err != nil {
				return err
			}
			r.Kind, r.Scope = kind, scope.String
			r.AgencyIDs = member[r.ID]
			if r.AgencyIDs == nil {
				r.AgencyIDs = []string{}
			}
			out.Rows = append(out.Rows, r)
		}
		return rows.Err()
	}
	if err := add("scope", `SELECT id, name, '' FROM scopes ORDER BY name`, membership["scope"]); err != nil {
		return nil, err
	}
	if err := add("secret", `SELECT id, key, COALESCE(scope,'') FROM secrets ORDER BY key, scope`, membership["secret"]); err != nil {
		return nil, err
	}
	if err := add("env-var", `SELECT id, key, COALESCE(scope,'') FROM env_vars ORDER BY key, scope`, membership["env-var"]); err != nil {
		return nil, err
	}
	if err := add("ssh-credential", `SELECT id, label, '' FROM ssh_credentials ORDER BY label`, membership["ssh-credential"]); err != nil {
		return nil, err
	}
	if err := add("runner", `SELECT id, name, '' FROM runners ORDER BY name`, runnerMap); err != nil {
		return nil, err
	}
	return out, nil
}

// AgencyMember is one entity inside an agency, for the per-agency detail view.
type AgencyMember struct {
	Kind   string `json:"kind"`
	ID     string `json:"id"`
	Name   string `json:"name"`
	Scope  string `json:"scope,omitempty"`
	Status string `json:"status,omitempty"` // runners only
}

// AgencyDetail answers "what is in this agency, and will anything actually run
// here?" (T4.2/T4.3).
type AgencyDetail struct {
	Agency  Agency         `json:"agency"`
	Members []AgencyMember `json:"members"`
	// OnlineRunners is the count of ONLINE members of this agency. ZERO is the trap
	// from the originating investigation: every run targeting this agency queues
	// forever, and until now that was visible only on the run, one run at a time.
	OnlineRunners int `json:"onlineRunners"`
	// QueuedRuns is how many runs are waiting on this agency right now. Read
	// together with OnlineRunners it turns "runs will queue forever" from a
	// prediction into a count.
	QueuedRuns int `json:"queuedRuns"`
}

// BuildAgencyDetail loads one agency's contents. Returns (nil, nil) when the
// agency does not exist.
func BuildAgencyDetail(ctx context.Context, database *sql.DB, agencyID string) (*AgencyDetail, error) {
	ag, err := GetAgency(ctx, database, agencyID)
	if err != nil || ag == nil {
		return nil, err
	}
	d := &AgencyDetail{Agency: *ag, Members: []AgencyMember{}}

	collect := func(kind, query string) error {
		rows, err := database.QueryContext(ctx, query, agencyID)
		if err != nil {
			return nil // best-effort, as above
		}
		defer rows.Close()
		for rows.Next() {
			var m AgencyMember
			var scope, status sql.NullString
			if err := rows.Scan(&m.ID, &m.Name, &scope, &status); err != nil {
				return err
			}
			m.Kind, m.Scope, m.Status = kind, scope.String, status.String
			d.Members = append(d.Members, m)
		}
		return rows.Err()
	}
	if err := collect("scope", `
		SELECT s.id, s.name, '', '' FROM scope_agencies sa
		JOIN scopes s ON s.id = sa.scope_id WHERE sa.agency_id = ? ORDER BY s.name`); err != nil {
		return nil, err
	}
	if err := collect("secret", `
		SELECT s.id, s.key, COALESCE(s.scope,''), '' FROM secret_agencies m
		JOIN secrets s ON s.id = m.secret_id WHERE m.agency_id = ? ORDER BY s.key`); err != nil {
		return nil, err
	}
	if err := collect("env-var", `
		SELECT v.id, v.key, COALESCE(v.scope,''), '' FROM env_var_agencies m
		JOIN env_vars v ON v.id = m.env_var_id WHERE m.agency_id = ? ORDER BY v.key`); err != nil {
		return nil, err
	}
	if err := collect("ssh-credential", `
		SELECT c.id, c.label, '', '' FROM ssh_credential_agencies m
		JOIN ssh_credentials c ON c.id = m.credential_id WHERE m.agency_id = ? ORDER BY c.label`); err != nil {
		return nil, err
	}
	if err := collect("runner", `
		SELECT r.id, r.name, '', r.status FROM runner_agencies m
		JOIN runners r ON r.id = m.runner_id WHERE m.agency_id = ? ORDER BY r.name`); err != nil {
		return nil, err
	}
	for _, m := range d.Members {
		if m.Kind == "runner" && m.Status == "online" {
			d.OnlineRunners++
		}
	}
	// Queued runs targeting this agency, from the migration-690 index (the same
	// structure claimRun probes, so this count and dispatch cannot disagree).
	_ = database.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT r.id) FROM run_agencies rag
		JOIN runs r     ON r.id = rag.run_id
		JOIN agencies a ON a.name = rag.agency
		WHERE a.id = ? AND r.status = 'queued'`, agencyID).Scan(&d.QueuedRuns)
	return d, nil
}

// entityOwner reads an entity's owning agency id, or "" when unowned / unknown /
// on a pre-830 schema (where the column does not exist yet). `table` is a
// compile-time constant from memberTableFor, never input.
func entityOwner(ctx context.Context, database *sql.DB, table, id string) string {
	var owner string
	if err := database.QueryRowContext(ctx,
		`SELECT COALESCE(owner_agency,'') FROM `+table+` WHERE id = ?`, id).Scan(&owner); err != nil {
		return ""
	}
	return owner
}

func containsID(ids []string, want string) bool {
	return slices.Contains(ids, want)
}

// ── Agency-scoped membership (RB-22) ─────────────────────────────────────────
//
// The inverse write axis. SetAgencyMembership replaces ONE ENTITY's agency list;
// SetAgencyMembers replaces ONE AGENCY's member list. The distinction is not
// cosmetic — it is what makes concurrent administration safe. Under the
// entity-centric setter, an agency-centric editor would have to read an entity's
// full agencyIds, modify it, and write it back: two admins editing two DIFFERENT
// departments would race on the same rows, and the loser's change silently
// vanishes. This setter touches only rows WHERE agency_id = this agency, so edits
// to different agencies cannot interact by construction.

// AgencyMemberRef names one entity for the agency-scoped setter. Kind takes the
// same wire values as AgencyMember/MatrixRow ("scope", "secret", "env-var",
// "ssh-credential", "runner").
type AgencyMemberRef struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// agencyMemberTables extends memberTableFor with the runner kind, which predates
// the unified membership machinery and keeps its own join table.
func agencyMemberTableFor(kind string) (memberTable, bool) {
	if kind == "runner" {
		return memberTable{"runner_agencies", "runner_id", "runners", "runner"}, true
	}
	return memberTableFor(MemberKind(kind))
}

// ValidAgencyMemberKind reports whether k is a kind the agency-scoped setter
// accepts — the four unified kinds plus runner.
func ValidAgencyMemberKind(k string) bool {
	_, ok := agencyMemberTableFor(k)
	return ok
}

// AgencyMembersDelta is what a SetAgencyMembers call would change, computed
// against the same rows the setter deletes and inserts. The API layer needs it
// BEFORE writing: the RF-1b re-homing guard authorizes each ADDED entity
// individually, and a denial must name the entity rather than fail the batch
// opaquely.
type AgencyMembersDelta struct {
	Added   []AgencyMemberRef
	Removed []AgencyMemberRef
}

// ComputeAgencyMembersDelta diffs the desired member set against the agency's
// current rows. Order is deterministic (kind, then id) so audit rows and error
// messages are stable.
func ComputeAgencyMembersDelta(ctx context.Context, database *sql.DB, agencyID string, desired []AgencyMemberRef) (*AgencyMembersDelta, error) {
	current := map[string]bool{}
	for _, kind := range []string{"scope", "secret", "env-var", "ssh-credential", "runner"} {
		t, _ := agencyMemberTableFor(kind)
		rows, err := database.QueryContext(ctx,
			`SELECT `+t.col+` FROM `+t.join+` WHERE agency_id = ?`, agencyID)
		if err != nil {
			continue // best-effort per catalog, matching AgencyMatrix
		}
		for rows.Next() {
			var eid string
			if err := rows.Scan(&eid); err != nil {
				rows.Close()
				return nil, err
			}
			current[kind+"\x00"+eid] = true
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}

	want := map[string]AgencyMemberRef{}
	for _, m := range desired {
		want[m.Kind+"\x00"+m.ID] = m
	}
	d := &AgencyMembersDelta{Added: []AgencyMemberRef{}, Removed: []AgencyMemberRef{}}
	for key, m := range want {
		if !current[key] {
			d.Added = append(d.Added, m)
		}
	}
	for key := range current {
		if _, keep := want[key]; !keep {
			kind, id, _ := strings.Cut(key, "\x00")
			d.Removed = append(d.Removed, AgencyMemberRef{Kind: kind, ID: id})
		}
	}
	sortRefs := func(refs []AgencyMemberRef) {
		sort.Slice(refs, func(i, j int) bool {
			if refs[i].Kind != refs[j].Kind {
				return refs[i].Kind < refs[j].Kind
			}
			return refs[i].ID < refs[j].ID
		})
	}
	sortRefs(d.Added)
	sortRefs(d.Removed)
	return d, nil
}

// SetAgencyMembers replaces one agency's member set across all five kinds.
//
// Validation is fail-closed and complete before any write, mirroring
// SetAgencyMembership: the agency and every desired entity must exist, and a
// removal may not strip an entity from the agency that OWNS it (RA-15 — ownership
// transfer is a deliberate separate act, and a membership edit that leaves a row
// unreachable by its own department is never what the operator meant).
//
// Only rows with agency_id = agencyID are deleted or inserted. An entity's
// memberships in OTHER agencies are untouched — that isolation is this function's
// reason to exist.
func SetAgencyMembers(ctx context.Context, database *sql.DB, agencyID string, desired []AgencyMemberRef, actor string) (*AgencyMembersDelta, error) {
	var ac int
	_ = database.QueryRowContext(ctx, `SELECT COUNT(*) FROM agencies WHERE id=?`, agencyID).Scan(&ac)
	if ac == 0 {
		return nil, ErrUnknownAgency
	}
	for _, m := range desired {
		t, ok := agencyMemberTableFor(m.Kind)
		if !ok {
			return nil, fmt.Errorf("%w: invalid member kind %q", ErrUnknownMember, m.Kind)
		}
		var ec int
		_ = database.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+t.catalog+` WHERE id=?`, m.ID).Scan(&ec)
		if ec == 0 {
			return nil, fmt.Errorf("%w: %s %s", ErrUnknownMember, t.label, m.ID)
		}
	}

	delta, err := ComputeAgencyMembersDelta(ctx, database, agencyID, desired)
	if err != nil {
		return nil, err
	}
	for _, m := range delta.Removed {
		t, _ := agencyMemberTableFor(m.Kind)
		if entityOwner(ctx, database, t.catalog, m.ID) == agencyID {
			return nil, fmt.Errorf("%w: %s %s is owned by this agency, so it cannot be removed from it — transfer ownership first",
				ErrOwnerRemoval, t.label, m.ID)
		}
	}

	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck
	for _, m := range delta.Removed {
		t, _ := agencyMemberTableFor(m.Kind)
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM `+t.join+` WHERE `+t.col+` = ? AND agency_id = ?`, m.ID, agencyID); err != nil {
			return nil, err
		}
	}
	for _, m := range delta.Added {
		t, _ := agencyMemberTableFor(m.Kind)
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO `+t.join+` (`+t.col+`, agency_id) VALUES (?, ?)`, m.ID, agencyID); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}

	// Membership is an access-control fact (T2.7); the audit names the agency and
	// the ids that moved, never a value. A no-op save writes no row — an audit
	// trail of "nothing changed" entries buries the entries that matter.
	if len(delta.Added)+len(delta.Removed) > 0 {
		audit(ctx, database, actor, "Agencies", "membership-updated",
			"agency members: "+agencyID, agencyMembersAuditDetail(delta))
	}
	return delta, nil
}

// agencyMembersAuditDetail renders a delta for change_log: ids only, capped like
// membershipAuditDetail so a bulk save cannot write an unbounded row.
func agencyMembersAuditDetail(d *AgencyMembersDelta) string {
	var parts []string
	render := func(verb string, refs []AgencyMemberRef) {
		if len(refs) == 0 {
			return
		}
		names := make([]string, 0, len(refs))
		for i, m := range refs {
			if i == 8 {
				names = append(names, fmt.Sprintf("… %d more", len(refs)-i))
				break
			}
			names = append(names, m.Kind+":"+m.ID)
		}
		parts = append(parts, verb+" "+strings.Join(names, ", "))
	}
	render("added", d.Added)
	render("removed", d.Removed)
	return strings.Join(parts, "; ")
}

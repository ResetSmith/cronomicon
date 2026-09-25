package settings

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/ResetSmith/cronomicon/internal/auditlog"
	"strconv"
	"strings"
	"time"

	"github.com/ResetSmith/cronomicon/internal/db"
)

// Agency is the wire representation of an agency — a network-isolation zone that
// runners are assigned to (M2) and scopes bind to (agency-support.md M1). The
// catalog is operator-managed; CRUD is ConfigureApp-gated at the API layer.
type Agency struct {
	ID             string  `json:"id"`
	Name           string  `json:"name"`
	Description    *string `json:"description"`
	CreatedBy      string  `json:"createdBy,omitempty"`
	CreatedAt      string  `json:"createdAt,omitempty"`
	LastModifiedBy string  `json:"lastModifiedBy,omitempty"`
	LastModifiedAt string  `json:"lastModifiedAt,omitempty"`
	// OnlineRunnerCount is the number of ONLINE runners assigned to this agency
	// (agency-support.md M4 coverage view). 0 ⇒ jobs bound here will wait — no
	// runner can claim them. Computed per list read; not stored.
	OnlineRunnerCount int `json:"onlineRunnerCount"`
}

// AgencyInput is the caller-supplied create/update payload.
type AgencyInput struct {
	Name        string
	Description *string
}

var (
	// ErrAgencyInUse is returned when deleting an agency still referenced by a
	// scope (and, from M2, a runner). The catalog blocks the delete (mapped 409).
	//
	// Callers wanting to tell the operator WHICH references block the delete should
	// type-assert to *AgencyInUseError, which wraps this sentinel — every existing
	// errors.Is(err, ErrAgencyInUse) check keeps working unchanged.
	ErrAgencyInUse = errors.New("agency is still in use")
	// ErrUnknownAgency is returned when a scope is bound to an agency id that is
	// not in the catalog — the write-time integrity guard (agency-support.md §2.3;
	// mapped 422).
	ErrUnknownAgency = errors.New("unknown agency")
)

// ListAgencies returns the agency catalog ordered by name.
func ListAgencies(ctx context.Context, database *sql.DB) ([]Agency, error) {
	rows, err := database.QueryContext(ctx,
		`SELECT id, name, description, created_by, created_at, last_modified_by, last_modified_at
		 FROM agencies ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list agencies: %w", err)
	}
	defer rows.Close()
	out := []Agency{}
	for rows.Next() {
		a, err := scanAgency(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()

	// M4 coverage: online-runner count per agency (one grouped query, no per-row
	// nested cursor). Best-effort — a pre-440 schema yields zero counts.
	counts := map[string]int{}
	if crows, cerr := database.QueryContext(ctx, `
		SELECT ra.agency_id, COUNT(DISTINCT rn.id)
		FROM runner_agencies ra JOIN runners rn ON rn.id = ra.runner_id
		WHERE rn.status = 'online'
		GROUP BY ra.agency_id`); cerr == nil {
		for crows.Next() {
			var aid string
			var n int
			if err := crows.Scan(&aid, &n); err == nil {
				counts[aid] = n
			}
		}
		crows.Close()
	}
	for i := range out {
		out[i].OnlineRunnerCount = counts[out[i].ID]
	}
	return out, nil
}

// GetAgency fetches one agency by id; returns (nil, nil) if not found.
func GetAgency(ctx context.Context, database *sql.DB, id string) (*Agency, error) {
	row := database.QueryRowContext(ctx,
		`SELECT id, name, description, created_by, created_at, last_modified_by, last_modified_at
		 FROM agencies WHERE id=?`, id)
	a, err := scanAgency(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &a, nil
}

func scanAgency(rs interface{ Scan(...any) error }) (Agency, error) {
	var a Agency
	var desc, createdBy, lmBy, lmAt sql.NullString
	if err := rs.Scan(&a.ID, &a.Name, &desc, &createdBy, &a.CreatedAt, &lmBy, &lmAt); err != nil {
		return a, err
	}
	if desc.Valid {
		a.Description = &desc.String
	}
	a.CreatedBy = createdBy.String
	a.LastModifiedBy = lmBy.String
	a.LastModifiedAt = lmAt.String
	return a, nil
}

// CreateAgency inserts a new agency. A duplicate name surfaces as a UNIQUE
// constraint error (mapped 409 at the handler).
func CreateAgency(ctx context.Context, database *sql.DB, inp AgencyInput, actor string) (*Agency, error) {
	name := strings.TrimSpace(inp.Name)
	if name == "" {
		return nil, fmt.Errorf("agency name is required")
	}
	id := db.NewID()
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := database.ExecContext(ctx,
		`INSERT INTO agencies (id, name, description, created_by, created_at, last_modified_by, last_modified_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, name, inp.Description, actor, now, actor, now)
	if err != nil {
		return nil, fmt.Errorf("create agency: %w", err)
	}
	audit(ctx, database, actor, "Agencies", "created", name, "")
	return GetAgency(ctx, database, id)
}

// UpdateAgency renames / re-describes an agency. Returns (nil, nil) if not found.
func UpdateAgency(ctx context.Context, database *sql.DB, id string, inp AgencyInput, actor string) (*Agency, error) {
	name := strings.TrimSpace(inp.Name)
	if name == "" {
		return nil, fmt.Errorf("agency name is required")
	}
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := database.ExecContext(ctx,
		`UPDATE agencies SET name=?, description=?, last_modified_by=?, last_modified_at=? WHERE id=?`,
		name, inp.Description, actor, now, id)
	if err != nil {
		return nil, fmt.Errorf("update agency: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, nil
	}
	audit(ctx, database, actor, "Agencies", "updated", name, "")
	return GetAgency(ctx, database, id)
}

// AgencyInUseError reports WHICH references block an agency delete. It wraps
// ErrAgencyInUse, so callers testing the sentinel are unaffected.
//
// RA-Q22 (rev 7) is why this exists. The guard counts four independent reference
// classes and used to collapse them into one undifferentiated sentinel, which the
// API then rendered as "still referenced by one or more scopes" — actively WRONG
// whenever the real blocker was a runner, a membership row, or ownership. Three of
// the four classes have a UI path to clear them, so a vague refusal is merely
// annoying. Ownership has none: with owner transfer deferred, an owned row can only
// be cleared by DELETING it, and for a stored secret that destroys the value. An
// operator retiring a department must be told that specifically, or they will clear
// scopes, re-home runners, empty the membership matrix, and still be refused with no
// idea what is left.
// Scope references are NOT a field here: since migration 700 they are membership
// rows like any other kind, counted in Members[MemberScope].
type AgencyInUseError struct {
	Runners int                // runner_agencies (M2)
	Members map[MemberKind]int // migration-670 membership joins (T2.8), incl. scopes
	Secrets int                // secrets.owner_agency         (RA-15)
	EnvVars int                // env_vars.owner_agency        (RA-15)
	Keys    int                // ssh_credentials.owner_agency (RA-19)
}

func (e *AgencyInUseError) Error() string {
	return "agency is still in use: " + strings.Join(e.Blockers(), "; ")
}

// Unwrap keeps errors.Is(err, ErrAgencyInUse) true for every pre-existing caller.
func (e *AgencyInUseError) Unwrap() error { return ErrAgencyInUse }

// Owned reports whether an OWNERSHIP reference blocks the delete — the one class
// with no clearing path short of destroying the row (RA-Q22).
func (e *AgencyInUseError) Owned() int { return e.Secrets + e.EnvVars + e.Keys }

// Blockers renders one actionable phrase per non-zero reference class, ordered
// easiest-to-clear first so the operator's next action is the first thing read.
func (e *AgencyInUseError) Blockers() []string {
	var out []string
	add := func(n int, one, many, fix string) {
		if n == 0 {
			return
		}
		noun := many
		if n == 1 {
			noun = one
		}
		out = append(out, strconv.Itoa(n)+" "+noun+" ("+fix+")")
	}
	add(e.Runners, "runner", "runners", "re-enroll the runner in another agency")
	// Scopes first among the membership kinds: an agency exists to own scopes, so
	// that is the likeliest blocker and the one with the plainest remedy.
	add(e.Members[MemberScope], "scope", "scopes", "rebind the scope to another agency")
	for _, k := range []MemberKind{MemberSecret, MemberEnvVar, MemberSSHCredential} {
		add(e.Members[k], string(k)+" membership", string(k)+" memberships",
			"remove it on the Membership matrix")
	}
	// Ownership last and phrased differently ON PURPOSE: the others name an edit,
	// this one names a destructive re-creation, because owner transfer is not built.
	const ownFix = "owner transfer is not available; the row must be re-created under another department"
	add(e.Secrets, "owned secret", "owned secrets", ownFix+" — REVEAL THE VALUE FIRST, deleting a stored secret destroys it")
	add(e.EnvVars, "owned variable", "owned variables", ownFix)
	add(e.Keys, "owned SSH credential", "owned SSH credentials", ownFix+" — the key material is destroyed with the row")
	return out
}

// DeleteAgency removes an agency. It returns *AgencyInUseError (wrapping
// ErrAgencyInUse, mapped 409) when the agency is still referenced — by a scope (M1),
// a runner (M2), a membership row (T2.8), or an OWNED entity (RA-15/RA-19).
// Historical runs.agency name snapshots never block deletion (immutable facts).
func DeleteAgency(ctx context.Context, database *sql.DB, id, actor string) (bool, error) {
	existing, err := GetAgency(ctx, database, id)
	if err != nil || existing == nil {
		return false, err
	}
	// ⚠️ The M1 scope guard used to live here as
	//     SELECT COUNT(*) FROM scopes WHERE agency_id = ?
	// with its error discarded. Migration 700 DROPPED scopes.agency_id (670 moved the
	// binding into scope_agencies), so from 700 onward that query failed on every call
	// and the discarded error left the count at 0 — a guard that read as enforced and
	// counted nothing. Removed rather than repaired: scope references are already
	// counted below by AgencyMemberCounts(MemberScope) over scope_agencies, which is
	// where the binding actually lives. Nothing regresses, and the struct no longer
	// carries a field that can only ever be zero.
	var runnerRefs int
	_ = database.QueryRowContext(ctx, `SELECT COUNT(*) FROM runner_agencies WHERE agency_id=?`, id).Scan(&runnerRefs)
	// T2.8 — the guard must count the migration-670 membership tables too. Without
	// this, deleting an agency that only holds SECRET or KEY members succeeds and the
	// ON DELETE CASCADE silently drops that membership: an access-control fact
	// disappears with no 409 and no way to notice. The scope case is caught above
	// only because scopes.agency_id has no cascade — which is luck, not design.
	members, _ := AgencyMemberCounts(ctx, database, id)
	memberRefs := 0
	for _, n := range members {
		memberRefs += n
	}
	// RA-15 — OWNERSHIP counted explicitly, not inferred. An owner is always enrolled
	// in its own row's membership (the create path sets both, and SetAgencyMembership
	// refuses to break the pair), so in practice memberRefs already covers this. But
	// the two facts live in different tables, and an agency deleted out from under an
	// owned row would leave `owner_agency` pointing at nothing — a row that occupies a
	// slot in the (key, scope, owner) uniqueness key on behalf of a department that no
	// longer exists. Cheap to check, and it makes the invariant a stated rule rather
	// than a consequence of another one.
	//
	// Counted PER TABLE rather than summed: the operator's remedy differs by kind
	// (a stored secret's value is destroyed with the row; a variable's is not), and
	// AgencyInUseError has to be able to say which.
	owned := map[string]int{}
	for _, table := range []string{"secrets", "env_vars", "ssh_credentials"} {
		var n int
		// Best-effort per table, matching AgencyMemberCounts: a pre-830 schema has no
		// owner_agency column, and failing the whole guard there would make every
		// agency undeletable on an old schema.
		if err := database.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM `+table+` WHERE owner_agency = ?`, id).Scan(&n); err == nil {
			owned[table] = n
		}
	}
	ownerRefs := owned["secrets"] + owned["env_vars"] + owned["ssh_credentials"]
	if runnerRefs > 0 || memberRefs > 0 || ownerRefs > 0 {
		return false, &AgencyInUseError{
			Runners: runnerRefs,
			Members: members,
			Secrets: owned["secrets"],
			EnvVars: owned["env_vars"],
			Keys:    owned["ssh_credentials"],
		}
	}
	res, err := database.ExecContext(ctx, `DELETE FROM agencies WHERE id=?`, id)
	if err != nil {
		return false, fmt.Errorf("delete agency: %w", err)
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		audit(ctx, database, actor, "Agencies", "deleted", existing.Name, "")
	}
	return n > 0, nil
}

// SetScopeAgency binds (or, with a nil/empty id, clears) a scope's agency. It is
// an operator overlay valid for BOTH git- and cronomicon-source scopes (agency is a
// deployment fact the GitOps repo does not own), and survives re-sync because
// upsertScopes never writes agency_id. Returns (nil, nil) if the scope is not
// found, or ErrUnknownAgency (mapped 422) if the agency id is not in the catalog.
func SetScopeAgency(ctx context.Context, database *sql.DB, scopeID string, agencyID *string, actor string) (*Scope, error) {
	sc, err := GetScope(ctx, database, scopeID)
	if err != nil || sc == nil {
		return nil, err
	}
	var val any
	if agencyID != nil && strings.TrimSpace(*agencyID) != "" {
		aid := strings.TrimSpace(*agencyID)
		var exists int
		_ = database.QueryRowContext(ctx, `SELECT COUNT(*) FROM agencies WHERE id=?`, aid).Scan(&exists)
		if exists == 0 {
			return nil, ErrUnknownAgency
		}
		val = aid
	}
	// T3.9 — scopes.agency_id is GONE (migration 700); scope_agencies is the only
	// binding now. This endpoint is kept because it is the 1:1 affordance the Scopes
	// tab has always used, and a scope with one agency is still the common case; it
	// simply writes the join table. Setting a SECOND agency requires the N:M matrix
	// (PUT /scope-agencies), which this endpoint would otherwise silently truncate.
	if _, err := database.ExecContext(ctx, `DELETE FROM scope_agencies WHERE scope_id=?`, scopeID); err != nil {
		return nil, fmt.Errorf("set scope agency: %w", err)
	}
	if val != nil {
		if _, err := database.ExecContext(ctx,
			`INSERT INTO scope_agencies (scope_id, agency_id) VALUES (?, ?)`, scopeID, val); err != nil {
			return nil, fmt.Errorf("set scope agency: %w", err)
		}
	}
	detail := "cleared"
	if val != nil {
		detail = fmt.Sprintf("%v", val)
	}
	audit(ctx, database, actor, "Scopes", "agency-set", sc.Scope, detail)
	return GetScope(ctx, database, scopeID)
}

// ── Runner ↔ agency membership (M2) ───────────────────────────────────────────

// ErrUnknownRunner is returned when membership references a runner id not in the
// fleet — the write-time integrity guard for the runner side (mapped 422).
var ErrUnknownRunner = errors.New("unknown runner")

// RunnerAgencies is one runner's agency-membership set (M2). The matrix endpoint
// is a list of these; PUT replaces the set for each posted runner.
type RunnerAgencies struct {
	RunnerID  string   `json:"runnerId"`
	AgencyIDs []string `json:"agencyIds"`
}

// ListRunnerAgencies returns the runner→agency membership grid (only runners with
// at least one agency appear). Best-effort: a pre-440 schema yields an empty grid.
func ListRunnerAgencies(ctx context.Context, database *sql.DB) ([]RunnerAgencies, error) {
	rows, err := database.QueryContext(ctx,
		`SELECT runner_id, agency_id FROM runner_agencies ORDER BY runner_id, agency_id`)
	if err != nil {
		return []RunnerAgencies{}, nil
	}
	defer rows.Close()
	byRunner := map[string][]string{}
	var order []string
	for rows.Next() {
		var rid, aid string
		if err := rows.Scan(&rid, &aid); err != nil {
			return nil, err
		}
		if _, seen := byRunner[rid]; !seen {
			order = append(order, rid)
		}
		byRunner[rid] = append(byRunner[rid], aid)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]RunnerAgencies, 0, len(order))
	for _, rid := range order {
		out = append(out, RunnerAgencies{RunnerID: rid, AgencyIDs: byRunner[rid]})
	}
	return out, nil
}

// SetRunnerAgencies replaces the membership set for each POSTED runner (other
// runners are untouched — the scope-restrictions replace-per-row model). Every
// runner and agency id is validated against the catalog FIRST (fail-closed, no
// partial write) → ErrUnknownRunner / ErrUnknownAgency (both mapped 422).
// Operator-assigned only; the agent never self-declares membership (§2.1).
func SetRunnerAgencies(ctx context.Context, database *sql.DB, assignments []RunnerAgencies, actor string) error {
	for _, a := range assignments {
		var rc int
		_ = database.QueryRowContext(ctx, `SELECT COUNT(*) FROM runners WHERE id=?`, a.RunnerID).Scan(&rc)
		if rc == 0 {
			return ErrUnknownRunner
		}
		for _, aid := range a.AgencyIDs {
			var ac int
			_ = database.QueryRowContext(ctx, `SELECT COUNT(*) FROM agencies WHERE id=?`, aid).Scan(&ac)
			if ac == 0 {
				return ErrUnknownAgency
			}
		}
	}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	ts := time.Now().UTC().Format(time.RFC3339)
	var changed []string // "name: A, B" per runner whose set actually changed
	for _, a := range assignments {
		// DRF-5: what was the set BEFORE, so an unchanged runner writes no row —
		// the matrix UI posts every runner it shows, and a feed entry per
		// untouched runner would drown the one that moved.
		before, err := runnerAgencyIDs(ctx, tx, a.RunnerID)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM runner_agencies WHERE runner_id=?`, a.RunnerID); err != nil {
			return err
		}
		seen := map[string]bool{}
		after := make([]string, 0, len(a.AgencyIDs))
		for _, aid := range a.AgencyIDs {
			if seen[aid] {
				continue
			}
			seen[aid] = true
			after = append(after, aid)
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO runner_agencies (runner_id, agency_id) VALUES (?, ?)`, a.RunnerID, aid); err != nil {
				return err
			}
		}
		if sameSet(before, after) {
			continue
		}
		var name string
		if err := tx.QueryRowContext(ctx, `SELECT name FROM runners WHERE id=?`, a.RunnerID).Scan(&name); err != nil {
			return err
		}
		summary := "agencies set: " + strings.Join(agencyNamesFor(ctx, tx, after), ", ")
		if len(after) == 0 {
			// RB-22: say what the empty set MEANS. This runner now serves every
			// department's untagged work, which is MORE reach, not less.
			summary = "removed from all agencies (now general pool)"
		}
		// In the transaction, like DRF-1/DRF-4: a membership change is an
		// isolation change, and one without its audit row must not exist. Target
		// and RunnerName follow the drain/keyscan convention so ?runner= finds it.
		if err := auditlog.WriteActivity(ctx, tx, auditlog.ActivityParams{
			At:         ts,
			Kind:       "config",
			Outcome:    "success",
			Actor:      actor,
			Category:   "Agencies",
			Target:     "runner:" + name,
			RunnerName: name,
			Summary:    summary,
		}); err != nil {
			return err
		}
		changed = append(changed, name+": "+summary)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	// The Change Log keeps its one "membership-updated" row per request (its
	// category/action vocabulary is what the Change Log tab filters on), now
	// with details naming what moved. The generic activity row it used to pair
	// with is gone: the per-runner rows above replace it, and the old one said
	// only "membership-updated runner agencies" — invisible to ?runner=.
	if len(changed) > 0 {
		_ = writeChangeLog(ctx, database, actor, "Agencies", "membership-updated", "runner agencies",
			strings.Join(changed, "; "))
	}
	return nil
}

// runnerAgencyIDs reads a runner's current membership inside a transaction.
func runnerAgencyIDs(ctx context.Context, tx *sql.Tx, runnerID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT agency_id FROM runner_agencies WHERE runner_id=? ORDER BY agency_id`, runnerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// agencyNamesFor resolves ids to display names for an audit summary; an id
// that no longer resolves is shown as itself rather than dropped, so the row
// still says what was written.
func agencyNamesFor(ctx context.Context, tx *sql.Tx, ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		var n string
		if err := tx.QueryRowContext(ctx, `SELECT name FROM agencies WHERE id=?`, id).Scan(&n); err != nil || n == "" {
			n = id
		}
		out = append(out, n)
	}
	return out
}

// sameSet compares two id lists as sets.
func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := make(map[string]bool, len(a))
	for _, x := range a {
		m[x] = true
	}
	for _, y := range b {
		if !m[y] {
			return false
		}
	}
	return true
}

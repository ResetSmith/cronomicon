package settings

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ResetSmith/cronomicon/internal/agencyid"
	"github.com/ResetSmith/cronomicon/internal/auditlog"
	"github.com/ResetSmith/cronomicon/internal/auth"

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
	// Builtin marks Global, the one agency every installation has (LR-21,
	// migration 1220). It cannot be renamed or deleted, and no access grant may
	// name it: it belongs to the global administrators.
	Builtin bool `json:"builtin"`
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

	// ErrScopeAgencyFixed is returned by a write that would give a scope from
	// a repository any agency but that repository's (2.4.0, GR-18; Global's
	// repository too since 2.4.3, question 58). The repository states whose
	// the scope is; it leaves that agency when its file leaves the repository.
	// Mapped 409 `scope_agency_fixed`.
	ErrScopeAgencyFixed = errors.New("this scope comes from a repository and belongs to that repository's agency: " +
		"it cannot be moved while its inventory file is there; to give another agency its hosts, clone it")

	// ErrAgencyRequired is returned by a membership write that would leave an
	// entity in no agency at all (LR-26). Every scope, runner, secret, variable
	// and key belongs to at least one; to make something Global's, name Global.
	// Removing the last agency used to be HOW a row became "global" — for a
	// secret that means usable by every agency — which made a delete the way to
	// take the most consequential decision there is. Mapped 422.
	ErrAgencyRequired = errors.New("every entity belongs to at least one agency — name Global to make it Global's")

	// ErrGlobalMixed is returned when a membership write names Global together
	// with another agency (LR-25). Global's rows are already usable by every
	// agency; "Global and Finance" says nothing "Global" does not, and reads as a
	// restriction it is not. Mapped 422.
	ErrGlobalMixed = errors.New("an entity is Global's or an agency's, never both")

	// ErrOneAgency is returned when a write would put a scope, a secret, a
	// variable or an SSH key in more than one agency (LR-7, LR-54). Each belongs
	// to exactly one, or to Global; what two agencies both need is a copy in
	// each, or a Global row. Mapped 422 `one_agency`. (A runner's serve list is
	// a different rule and has its own writer.)
	ErrOneAgency = errors.New("a scope, secret, variable or SSH key belongs to exactly one agency — another agency gets one by a copy; a change of its agency is a global administrator's repair")

	// ErrOwnerConflict is returned when moving a secret, a variable or an SSH key
	// to an agency would collide with a row that agency already owns under the
	// same name (and scope). Mapped 409 `owner_conflict`.
	ErrOwnerConflict = errors.New("the agency already has a row with this name")

	// ErrBuiltinAgency is returned by an attempt to rename or delete Global
	// (LR-21). Runs name their agency by NAME, and code and triggers name its id.
	ErrBuiltinAgency = errors.New("the Global agency is built in: it cannot be renamed or deleted")

	// ErrAgencyNameReserved is returned when another agency would be called
	// Global, in any letter case.
	ErrAgencyNameReserved = errors.New("the name Global is reserved for the built-in agency")
)

// ListAgencies returns the agency catalog ordered by name.
func ListAgencies(ctx context.Context, database *sql.DB) ([]Agency, error) {
	rows, err := database.QueryContext(ctx,
		`SELECT id, name, description, created_by, created_at, last_modified_by, last_modified_at, builtin
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
		`SELECT id, name, description, created_by, created_at, last_modified_by, last_modified_at, builtin
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
	if err := rs.Scan(&a.ID, &a.Name, &desc, &createdBy, &a.CreatedAt, &lmBy, &lmAt, &a.Builtin); err != nil {
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
	// LR-78: this changes what an access grant reaches (or what it is shown as),
	// so the grant snapshot is told on the way out — after the write, on every
	// return path, a failed half-write included.
	defer auth.GrantsChanged()
	name := strings.TrimSpace(inp.Name)
	if name == "" {
		return nil, fmt.Errorf("agency name is required")
	}
	id := db.NewID()
	if agencyid.IsGlobalName(name) {
		return nil, ErrAgencyNameReserved
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := database.ExecContext(ctx,
		`INSERT INTO agencies (id, name, description, created_by, created_at, last_modified_by, last_modified_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, name, inp.Description, actor, now, actor, now)
	if err != nil {
		return nil, fmt.Errorf("create agency: %w", err)
	}
	audit(ctx, database, agencyid.Global, actor, "Agencies", "created", name, "")
	return GetAgency(ctx, database, id)
}

// UpdateAgency renames / re-describes an agency. Returns (nil, nil) if not found.
func UpdateAgency(ctx context.Context, database *sql.DB, id string, inp AgencyInput, actor string) (*Agency, error) {
	// LR-78: this changes what an access grant reaches (or what it is shown as),
	// so the grant snapshot is told on the way out — after the write, on every
	// return path, a failed half-write included.
	defer auth.GrantsChanged()
	name := strings.TrimSpace(inp.Name)
	if name == "" {
		return nil, fmt.Errorf("agency name is required")
	}
	if id == agencyid.Global && name != agencyid.GlobalName {
		return nil, ErrBuiltinAgency
	}
	if id != agencyid.Global && agencyid.IsGlobalName(name) {
		return nil, ErrAgencyNameReserved
	}
	now := time.Now().UTC().Format(time.RFC3339)
	// The rename and its propagation to waiting runs are one transaction: a run
	// stores its agencies by NAME, and the claim matches those names against the
	// live catalog, so a rename that reached the catalog and not the runs would
	// leave them claimable by nobody (see renameAgencyOnWaitingRuns).
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("update agency: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	var oldName string
	if err := tx.QueryRowContext(ctx, `SELECT name FROM agencies WHERE id = ?`, id).Scan(&oldName); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("update agency: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE agencies SET name=?, description=?, last_modified_by=?, last_modified_at=? WHERE id=?`,
		name, inp.Description, actor, now, id); err != nil {
		return nil, fmt.Errorf("update agency: %w", err)
	}
	if oldName != name {
		if err := renameAgencyOnWaitingRuns(ctx, tx, oldName, name); err != nil {
			return nil, fmt.Errorf("update agency: carry the new name onto waiting runs: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("update agency: %w", err)
	}
	audit(ctx, database, agencyid.Global, actor, "Agencies", "updated", name, "")
	return GetAgency(ctx, database, id)
}

// renameAgencyOnWaitingRuns carries an agency's new name onto every run that
// has not finished: queued and running rows of `runs` (their agencies_json and
// the run_agencies index beside it) and parked rows of `pending_runs`.
//
// A run's agency snapshot is a list of NAMES — every other table stores the id
// — and it is read by name three times after the run is written: by the claim
// (runner/poll.go matches run_agencies.agency against the names of the agencies
// a runner serves), by reference resolution (runref matches it against a
// secret's, variable's or key's agencies), and by promotion of a parked run,
// which replays the frozen value. Before v2.3.0 a rename touched none of them:
// a run queued under the old name could be claimed by no runner, resolved none
// of its agency's secrets, and dropped out of the agency's own queued count,
// until someone renamed the agency back. (A new agency created under the old
// name would have adopted it.)
//
// Finished runs keep the name they ran under. That is history, and nothing
// reads it to make a decision.
//
// The parked snapshot is not a JSON array: pending_runs.params_json is the
// marshalled scheduler.EnqueueParams, whose AgenciesJSON field is a STRING that
// holds the array (`"AgenciesJSON":"[\"Tax\"]"`), and may be "" or absent; a
// workflow-kind row has no params at all. Only that one key is rewritten; every
// other key keeps the value it was frozen with.
func renameAgencyOnWaitingRuns(ctx context.Context, tx *sql.Tx, oldName, newName string) error {
	// runs.agencies_json, for the rows the index says carry the old name.
	if _, err := tx.ExecContext(ctx, `
		UPDATE runs
		   SET agencies_json = (
		         SELECT json_group_array(CASE WHEN je.value = ?1 THEN ?2 ELSE je.value END)
		           FROM json_each(runs.agencies_json) je)
		 WHERE status IN ('queued','running')
		   AND EXISTS (SELECT 1 FROM json_each(runs.agencies_json) je WHERE je.value = ?1)`,
		oldName, newName); err != nil {
		return err
	}
	// The index. OR IGNORE: a run that already lists the new name (it cannot
	// today, names being unique, but the index must not be what fails a rename)
	// keeps its one row, and the leftover old row is removed below.
	if _, err := tx.ExecContext(ctx, `
		UPDATE OR IGNORE run_agencies SET agency = ?2
		 WHERE agency = ?1
		   AND run_id IN (SELECT id FROM runs WHERE status IN ('queued','running'))`,
		oldName, newName); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM run_agencies
		 WHERE agency = ?1
		   AND run_id IN (SELECT id FROM runs WHERE status IN ('queued','running'))`,
		oldName); err != nil {
		return err
	}

	// Parked runs. Read them all first: a write inside an open cursor on the same
	// transaction is not safe to rely on.
	type parked struct{ id, params string }
	var rows []parked
	rs, err := tx.QueryContext(ctx, `
		SELECT id, params_json FROM pending_runs
		 WHERE params_json IS NOT NULL AND params_json LIKE '%AgenciesJSON%'`)
	if err != nil {
		return err
	}
	for rs.Next() {
		var p parked
		if err := rs.Scan(&p.id, &p.params); err != nil {
			rs.Close()
			return err
		}
		rows = append(rows, p)
	}
	if err := rs.Err(); err != nil {
		rs.Close()
		return err
	}
	rs.Close()
	for _, p := range rows {
		next, changed, err := renameAgencyInParams(p.params, oldName, newName)
		if err != nil || !changed {
			// An unreadable snapshot is left alone: promotion will fail it with its
			// own error, which says more than a rename refusing to proceed.
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE pending_runs SET params_json = ? WHERE id = ?`, next, p.id); err != nil {
			return err
		}
	}
	return nil
}

// renameAgencyInParams rewrites one name inside the AgenciesJSON string of a
// marshalled scheduler.EnqueueParams, leaving every other key untouched.
func renameAgencyInParams(params, oldName, newName string) (string, bool, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(params), &fields); err != nil {
		return "", false, err
	}
	raw, ok := fields["AgenciesJSON"]
	if !ok {
		return "", false, nil
	}
	var inner string
	if err := json.Unmarshal(raw, &inner); err != nil || inner == "" {
		return "", false, err
	}
	var names []string
	if err := json.Unmarshal([]byte(inner), &names); err != nil {
		return "", false, err
	}
	changed := false
	for i, n := range names {
		if n == oldName {
			names[i], changed = newName, true
		}
	}
	if !changed {
		return "", false, nil
	}
	innerOut, err := json.Marshal(names)
	if err != nil {
		return "", false, err
	}
	if fields["AgenciesJSON"], err = json.Marshal(string(innerOut)); err != nil {
		return "", false, err
	}
	out, err := json.Marshal(fields)
	if err != nil {
		return "", false, err
	}
	return string(out), true, nil
}

// AgencyInUseError reports WHICH references block an agency delete. It wraps
// ErrAgencyInUse, so callers testing the sentinel are unaffected.
//
// RA-Q22 (rev 7) is why this exists. The guard counts four independent reference
// classes and used to collapse them into one undifferentiated sentinel, which the
// API then rendered as "still referenced by one or more scopes" — actively WRONG
// whenever the real blocker was a runner, a membership row, or ownership. Three of
// the four classes had a UI path to clear them, so a vague refusal was merely
// annoying. Ownership had none until 2.3.0: an owned row could only be cleared by
// DELETING it, and for a stored secret that destroys the value. It can be moved
// now (LR-54), but an operator retiring a department must still be told which
// rows are in the way, or they will clear scopes, re-home runners, and still be
// refused with no idea what is left.
// Scope references are NOT a field here: since migration 700 they are membership
// rows like any other kind, counted in Members[MemberScope].
type AgencyInUseError struct {
	Runners int                // runner_agencies (M2)
	Members map[MemberKind]int // migration-670 membership joins (T2.8), incl. scopes
	Secrets int                // secrets.owner_agency         (RA-15)
	EnvVars int                // env_vars.owner_agency        (RA-15)
	Keys    int                // ssh_credentials.owner_agency (RA-19)
	// Hosts and Bastions are the hand-written host records and the bastions the
	// agency owns (LR-69). Unlike an owned secret they CAN be handed on: each is
	// given another owner on its own form.
	Hosts    int
	Bastions int
	// Repository is 1 when the agency has a repository connected and the
	// delete was not given a way to take it (DeleteAgencyWith with a nil
	// hook). Since 2.4.0 Phase I3 the route deletes an empty agency WITH its
	// repository (IN-13, the owner's answer to question 37), and what the
	// repository supplied goes with it: it is all still in Git.
	Repository int
	// Definitions and Calendars are what the agency built in the app: jobs,
	// workflows and schedules (in the recycle bin too), and calendars (I3).
	// Until then they were not counted, and a delete left them owned by an
	// agency that no longer existed.
	Definitions int
	Calendars   int
	// AlertRules and AlertTargets are the alert rules and push targets the
	// agency owns (2.5.0 C4, AL-1 and AL-2).
	AlertRules   int
	AlertTargets int
	// Grants, ServiceAccounts and Tokens are what the delete would take with
	// it silently: access grants and service accounts are ON DELETE CASCADE,
	// and an unspent registration token would enrol an agent into an agency
	// that is gone. OwnedRunners are runners it owns that serve nothing
	// (runner_agencies does not count them). Counted here since 2.4.2 (present
	// defects 62 and 63): the first two were the route's own checks, which the
	// create route's rollback did not ask.
	Grants          int
	ServiceAccounts int
	Tokens          int
	OwnedRunners    int
}

func (e *AgencyInUseError) Error() string {
	return "agency is still in use: " + strings.Join(e.Blockers(), "; ")
}

// Unwrap keeps errors.Is(err, ErrAgencyInUse) true for every pre-existing caller.
func (e *AgencyInUseError) Unwrap() error { return ErrAgencyInUse }

// Owned reports whether an OWNERSHIP reference blocks the delete — the one class
// with no clearing path short of destroying the row (RA-Q22).
func (e *AgencyInUseError) Owned() int { return e.Secrets + e.EnvVars + e.Keys }

// records is the host records and bastions the agency owns.
func (e *AgencyInUseError) records() int { return e.Hosts + e.Bastions }

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
	add(e.Repository, "connected repository", "connected repositories", "delete the agency through its route, which takes the repository with it")
	add(e.Definitions, "job, workflow or schedule built in the app (or not of its repository)",
		"jobs, workflows and schedules built in the app (or not of its repository)",
		"delete them, and purge them from the recycle bin; an agency's own definitions are not handed to another")
	add(e.Calendars, "calendar built in the app", "calendars built in the app", "delete them")
	add(e.AlertRules, "alert rule", "alert rules", "delete them")
	add(e.AlertTargets, "push target", "push targets", "delete them")
	add(e.Grants, "access grant", "access grants", "remove those grants first: deleting the agency would revoke them")
	add(e.ServiceAccounts, "active service account", "active service accounts", "revoke those first")
	add(e.Tokens, "unspent registration token", "unspent registration tokens",
		"revoke it: an agent enrolled with it would belong to an agency that is gone")
	add(e.Runners, "runner", "runners", "re-enroll the runner in another agency")
	add(e.OwnedRunners, "runner it owns that serves no agency", "runners it owns that serve no agency", "remove the runner")
	// Scopes first among the membership kinds: an agency exists to own scopes, so
	// that is the likeliest blocker and the one with the plainest remedy.
	// The remedies are what an agency's administrator can do since §0a (IN-7):
	// delete, or copy into another agency; a move is a global administrator's
	// repair (until 2.4.2 the remedies still said to move each row).
	add(e.Members[MemberScope], "scope", "scopes", "delete it, or clone it to another agency first; a global administrator can repair its agency")
	for _, k := range []MemberKind{MemberSecret, MemberEnvVar, MemberSSHCredential} {
		add(e.Members[k], string(k)+" membership", string(k)+" memberships",
			"remove it from this agency; a global administrator can repair its agency")
	}
	// Ownership. Deleting is what an operator in a hurry reaches for, and for a
	// stored secret or a key it destroys the only copy, so the warning stays.
	const repairFix = "a global administrator can repair its agency to the one that should have it"
	add(e.Secrets, "owned secret", "owned secrets", repairFix+"; do NOT delete it to get past this, deleting a stored secret destroys its value")
	add(e.EnvVars, "owned variable", "owned variables", "create it again in the agency that needs it and delete it here, or "+repairFix)
	add(e.Keys, "owned SSH credential", "owned SSH credentials", repairFix+"; do NOT delete it to get past this, deleting it destroys the key material")
	add(e.Hosts, "host record", "host records", "delete it; a global administrator can repair its agency")
	add(e.Bastions, "bastion", "bastions", "delete it; a global administrator can repair its agency")
	return out
}

// RepoTake removes an agency's repository with the agency (the gitlab
// Registry's RemoveWithAgency: settings cannot import gitlab). It is called
// once the agency has been counted and found empty, BEFORE the delete's
// transaction begins, and does there what must not wait on the transaction's
// write lock (it stops the repository's sync service). remove deletes the
// repository's rows inside the transaction; finish is told whether the
// transaction committed, and either removes what is outside the database (the
// clone) or puts the service back.
type RepoTake func(ctx context.Context, repoID string) (remove func(tx *sql.Tx) error, finish func(committed bool), err error)

// rowQuerier is a *sql.DB or a *sql.Tx.
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// countCascades fills what the delete would take with it silently and no
// trigger refuses (present defects 62 and 63). Asked before the transaction,
// for the refusal's wording, and again inside it, where nothing else can
// write: the write lock is taken at BEGIN (db.go, _txlock=immediate).
func countCascades(ctx context.Context, q rowQuerier, id string, inUse *AgencyInUseError) error {
	now := time.Now().UTC().Format(time.RFC3339)
	if err := q.QueryRowContext(ctx, `SELECT
	      (SELECT COUNT(*) FROM access_grants WHERE agency_id = ?1),
	      (SELECT COUNT(*) FROM service_accounts WHERE agency_id = ?1 AND revoked_at IS NULL),
	      (SELECT COUNT(*) FROM registration_tokens WHERE agency_id = ?1
	          AND revoked_at IS NULL AND used_at IS NULL AND expires_at > ?2),
	      (SELECT COUNT(*) FROM runners r WHERE r.owner_agency = ?1
	          AND NOT EXISTS (SELECT 1 FROM runner_agencies ra WHERE ra.runner_id = r.id))`, id, now).
		Scan(&inUse.Grants, &inUse.ServiceAccounts, &inUse.Tokens, &inUse.OwnedRunners); err != nil {
		return fmt.Errorf("delete agency: count its grants, service accounts, registration tokens and runners: %w", err)
	}
	return nil
}

func (e *AgencyInUseError) cascades() int {
	return e.Grants + e.ServiceAccounts + e.Tokens + e.OwnedRunners
}

// DeleteAgencyWith is DeleteAgency for an agency that may have its repository
// connected (§0a, IN-13; Phase I3; the owner's answer to question 37): an
// agency is deleted when it is EMPTY, and its repository connection with it.
// What the repository supplied (its Git jobs, workflows, scripts, schedules,
// scopes and calendars) does not make it non-empty: it goes with the
// connection, and it is all still in Git. Everything else does, and is named.
//
// takeRepo, given, removes the repository with what it supplied (RepoTake).
// With a nil takeRepo a connected repository blocks the delete, as before.
// The repository's rows and the agency go in ONE transaction (2.4.2, present
// defect 63): until then the repository was taken first, and a delete the
// trigger then refused left the agency without it.
func DeleteAgencyWith(ctx context.Context, database *sql.DB, id, actor string, takeRepo RepoTake) (bool, error) {
	// LR-78: this changes what an access grant reaches (or what it is shown as),
	// so the grant snapshot is told on the way out — after the write, on every
	// return path, a failed half-write included.
	defer auth.GrantsChanged()
	existing, err := GetAgency(ctx, database, id)
	if err != nil || existing == nil {
		return false, err
	}
	if id == agencyid.Global {
		return false, ErrBuiltinAgency
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
	// Every count below FAILS the delete when it cannot be read. Each used to be
	// best-effort ("a pre-670 schema has no such table"), which made an unreadable
	// table read as "nothing references this agency" — and the cascade then took
	// the membership of everything that did. Migrations run before anything
	// serves, so there is no old schema to be lenient towards.
	var runnerRefs int
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM runner_agencies WHERE agency_id=?`, id).Scan(&runnerRefs); err != nil {
		return false, fmt.Errorf("delete agency: count runners: %w", err)
	}
	// T2.8 — the guard must count the migration-670 membership tables too. Without
	// this, deleting an agency that only holds SECRET or KEY members succeeds and the
	// ON DELETE CASCADE silently drops that membership: an access-control fact
	// disappears with no 409 and no way to notice. The scope case is caught above
	// only because scopes.agency_id has no cascade — which is luck, not design.
	members, err := AgencyMemberCounts(ctx, database, id)
	if err != nil {
		return false, fmt.Errorf("delete agency: count members: %w", err)
	}
	// The scopes its own repository supplied go with the repository (I3).
	var repoID string
	if err := database.QueryRowContext(ctx, `SELECT COALESCE((SELECT id FROM git_repos WHERE agency_id = ?), '')`, id).Scan(&repoID); err != nil {
		return false, fmt.Errorf("delete agency: read its repository: %w", err)
	}
	if repoID != "" && takeRepo != nil {
		var repoScopes int
		if err := database.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM scope_agencies sa JOIN scopes s ON s.id = sa.scope_id
			  WHERE sa.agency_id = ? AND s.source = 'git' AND s.repo_id = ?`, id, repoID).Scan(&repoScopes); err != nil {
			return false, fmt.Errorf("delete agency: count its repository's scopes: %w", err)
		}
		members[MemberScope] -= repoScopes
	}
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
		if err := database.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM `+table+` WHERE owner_agency = ?`, id).Scan(&n); err != nil {
			return false, fmt.Errorf("delete agency: count owned %s: %w", table, err)
		}
		owned[table] = n
	}
	ownerRefs := owned["secrets"] + owned["env_vars"] + owned["ssh_credentials"]
	inUse := &AgencyInUseError{
		Runners: runnerRefs,
		Members: members,
		Secrets: owned["secrets"],
		EnvVars: owned["env_vars"],
		Keys:    owned["ssh_credentials"],
	}
	// LR-69: hand-written host records and bastions the agency owns. An imported
	// record is its scope's and is already counted with the scope.
	if err := database.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM ssh_hosts WHERE owner_agency = ? AND scope_id IS NULL`, id).Scan(&inUse.Hosts); err != nil {
		return false, fmt.Errorf("delete agency: count host records: %w", err)
	}
	if err := database.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM bastions WHERE owner_agency = ?`, id).Scan(&inUse.Bastions); err != nil {
		return false, fmt.Errorf("delete agency: count bastions: %w", err)
	}
	if err := database.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM alert_config WHERE owner_agency = ?`, id).Scan(&inUse.AlertRules); err != nil {
		return false, fmt.Errorf("delete agency: count alert rules: %w", err)
	}
	if err := database.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM alert_targets WHERE owner_agency = ?`, id).Scan(&inUse.AlertTargets); err != nil {
		return false, fmt.Errorf("delete agency: count push targets: %w", err)
	}
	if repoID != "" && takeRepo == nil {
		inUse.Repository = 1
	}
	// What it built in the app (I3): binned rows too, which a restore would
	// bring back owned by nobody. And a Git row its repository does not hold
	// (one that records no repository: none since 1310 outside the demo seed),
	// which the take would leave and the trigger refuses — every row the
	// trigger asks about that the take does not remove (from the review of I3).
	if err := countAgencyDefinitions(ctx, database, id, repoID, takeRepo != nil, inUse); err != nil {
		return false, err
	}
	if err := countCascades(ctx, database, id, inUse); err != nil {
		return false, err
	}
	if runnerRefs > 0 || memberRefs > 0 || ownerRefs > 0 || inUse.records() > 0 || inUse.Repository > 0 ||
		inUse.Definitions > 0 || inUse.Calendars > 0 || inUse.AlertRules > 0 || inUse.AlertTargets > 0 || inUse.cascades() > 0 {
		return false, inUse
	}
	var removeRepo func(tx *sql.Tx) error
	finish := func(bool) {}
	if repoID != "" {
		rm, fin, err := takeRepo(ctx, repoID)
		if err != nil {
			return false, err
		}
		removeRepo, finish = rm, fin
	}
	committed := false
	defer func() { finish(committed) }()
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("delete agency: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	// What no trigger refuses, asked again where nothing else can write.
	again := &AgencyInUseError{Members: map[MemberKind]int{}}
	if err := countCascades(ctx, tx, id, again); err != nil {
		return false, err
	}
	if again.cascades() > 0 {
		return false, again
	}
	if removeRepo != nil {
		if err := removeRepo(tx); err != nil {
			return false, err
		}
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM agencies WHERE id=?`, id)
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		_ = tx.Rollback()
		// The counts above and the transaction are separate, so something can
		// join the agency in between. The database refuses the delete itself
		// (trigger agencies_no_delete_in_use, migrations 1220 and 1450): that is
		// the guard, and the counts are how the refusal gets its wording. The
		// transaction is rolled back, the repository with it, and the caller is
		// told what arrived.
		if strings.Contains(err.Error(), "agency_in_use") {
			inUse := &AgencyInUseError{Members: map[MemberKind]int{}}
			_ = database.QueryRowContext(ctx, `SELECT COUNT(*) FROM runner_agencies WHERE agency_id=?`, id).Scan(&inUse.Runners)
			if m, merr := AgencyMemberCounts(ctx, database, id); merr == nil {
				inUse.Members = m
			}
			_ = database.QueryRowContext(ctx, `SELECT COUNT(*) FROM secrets WHERE owner_agency = ?`, id).Scan(&inUse.Secrets)
			_ = database.QueryRowContext(ctx, `SELECT COUNT(*) FROM env_vars WHERE owner_agency = ?`, id).Scan(&inUse.EnvVars)
			_ = database.QueryRowContext(ctx, `SELECT COUNT(*) FROM ssh_credentials WHERE owner_agency = ?`, id).Scan(&inUse.Keys)
			_ = database.QueryRowContext(ctx, `SELECT COUNT(*) FROM ssh_hosts WHERE owner_agency = ?`, id).Scan(&inUse.Hosts)
			_ = database.QueryRowContext(ctx, `SELECT COUNT(*) FROM bastions WHERE owner_agency = ?`, id).Scan(&inUse.Bastions)
			// And what 1450 added (from the review of I3: a refusal after the
			// repository was taken answered with no blocker named).
			_ = database.QueryRowContext(ctx, `SELECT COUNT(*) FROM git_repos WHERE agency_id = ?`, id).Scan(&inUse.Repository)
			_ = countAgencyDefinitions(ctx, database, id, "", false, inUse)
			_ = database.QueryRowContext(ctx, `SELECT COUNT(*) FROM runners WHERE owner_agency = ?`, id).Scan(&inUse.OwnedRunners)
			_ = database.QueryRowContext(ctx, `SELECT COUNT(*) FROM alert_config WHERE owner_agency = ?`, id).Scan(&inUse.AlertRules)
			_ = database.QueryRowContext(ctx, `SELECT COUNT(*) FROM alert_targets WHERE owner_agency = ?`, id).Scan(&inUse.AlertTargets)
			return false, inUse
		}
		return false, fmt.Errorf("delete agency: %w", err)
	}
	committed = true
	n, _ := res.RowsAffected()
	if n > 0 {
		audit(ctx, database, agencyid.Global, actor, "Agencies", "deleted", existing.Name, "")
	}
	return n > 0, nil
}

// countAgencyDefinitions fills inUse's Definitions and Calendars: the jobs,
// workflows, schedules and calendars the agency owns, except — when its
// repository is to be taken (taking) — the Git rows of that repository,
// which go with it (a Git calendar records its agency only, and the take
// removes the agency's).
func countAgencyDefinitions(ctx context.Context, database *sql.DB, id, repoID string, taking bool, inUse *AgencyInUseError) error {
	notTaken := `1`
	if taking {
		// COALESCE: a Git row with no repository must count, and
		// `repo_id = ?` is NULL for it, which NOT leaves NULL.
		notTaken = `NOT (source = 'git' AND COALESCE(repo_id, '') = ?2)`
	}
	if err := database.QueryRowContext(ctx, `SELECT
	      (SELECT COUNT(*) FROM jobs      WHERE owner_agency = ?1 AND `+notTaken+`)
	    + (SELECT COUNT(*) FROM workflows WHERE owner_agency = ?1 AND `+notTaken+`)
	    + (SELECT COUNT(*) FROM schedules WHERE owner_agency = ?1 AND `+notTaken+`)`, id, repoID).Scan(&inUse.Definitions); err != nil {
		return fmt.Errorf("delete agency: count its definitions: %w", err)
	}
	calendars := `SELECT COUNT(*) FROM calendars WHERE owner_agency = ?1`
	if taking {
		calendars += ` AND source <> 'git'`
	}
	if err := database.QueryRowContext(ctx, calendars, id).Scan(&inUse.Calendars); err != nil {
		return fmt.Errorf("delete agency: count its calendars: %w", err)
	}
	return nil
}

// SetScopeAgency binds (or, with a nil/empty id, clears) a scope's agency. It is
// an operator overlay valid for BOTH git- and cronomicon-source scopes (agency is a
// deployment fact the GitOps repo does not own), and survives re-sync because
// upsertScopes never writes agency_id. Returns (nil, nil) if the scope is not
// found, or ErrUnknownAgency (mapped 422) if the agency id is not in the catalog.
func SetScopeAgency(ctx context.Context, database *sql.DB, scopeID string, agencyID *string, actor string) (*Scope, error) {
	// LR-78: this changes what an access grant reaches (or what it is shown as),
	// so the grant snapshot is told on the way out — after the write, on every
	// return path, a failed half-write included.
	defer auth.GrantsChanged()
	sc, err := GetScope(ctx, database, scopeID)
	if err != nil || sc == nil {
		return nil, err
	}
	// A scope always has an agency (LR-26). This route's "no agency" (a null or
	// empty agencyId, the Scopes tab's "none" option) has always meant "nobody's
	// in particular"; that is Global now, said as a row.
	var val any = agencyid.Global
	if agencyID != nil && strings.TrimSpace(*agencyID) != "" {
		aid := strings.TrimSpace(*agencyID)
		var exists int
		if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM agencies WHERE id=?`, aid).Scan(&exists); err != nil {
			return nil, fmt.Errorf("set scope agency: %w", err)
		}
		if exists == 0 {
			return nil, ErrUnknownAgency
		}
		val = aid
	}
	if fixed, err := ScopeAgencyFixedTo(ctx, database, scopeID); err != nil {
		return nil, fmt.Errorf("set scope agency: %w", err)
	} else if fixed != "" && val != fixed {
		return nil, ErrScopeAgencyFixed
	}
	// T3.9 — scopes.agency_id is GONE (migration 700); scope_agencies is the only
	// binding now. This endpoint is kept because it is the 1:1 affordance the Scopes
	// tab has always used, and a scope with one agency is still the common case; it
	// simply writes the join table. Setting a SECOND agency requires the N:M matrix
	// (PUT /scope-agencies), which this endpoint would otherwise silently truncate.
	//
	// One transaction: the delete and the insert are a replacement, and a scope
	// caught between them belongs to no agency — the state every reader refuses.
	from := RowAgency(ctx, database, "scopes", "owner_agency", scopeID)
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("set scope agency: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := presetScopeOwner(ctx, tx, scopeID, fmt.Sprint(val)); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM scope_agencies WHERE scope_id=?`, scopeID); err != nil {
		return nil, fmt.Errorf("set scope agency: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO scope_agencies (scope_id, agency_id) VALUES (?, ?)`, scopeID, val); err != nil {
		return nil, fmt.Errorf("set scope agency: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("set scope agency: %w", err)
	}
	// The scope's agency after the move, naming the one it left (D1).
	detail := fmt.Sprintf("%v (was %s)", val, from)
	audit(ctx, database, RowAgency(ctx, database, "scopes", "owner_agency", scopeID), actor, "Scopes", "agency-set", sc.Scope, detail)
	return GetScope(ctx, database, scopeID)
}

// ScopeAgencyFixedTo is the agency a scope is held to because its inventory
// is in that agency's repository (2.4.0, GR-18; Global's repository too since
// 2.4.3, question 58), or "" when the scope may be moved: one built in the app,
// or one that is not there. Migration 1470's trigger refuses the same write;
// this is where the refusal gets its own error and its own answer.
func ScopeAgencyFixedTo(ctx context.Context, q interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}, scopeID string) (string, error) {
	var agency string
	err := q.QueryRowContext(ctx, `
		SELECT g.agency_id FROM scopes sc JOIN git_repos g ON g.id = sc.repo_id
		 WHERE sc.id = ? AND sc.source = 'git'`, scopeID).Scan(&agency)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return agency, err
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
		// Global beside another agency is refused for an agent (its list is its
		// owner). The local runner is the one runner whose list may hold both
		// (MA-11): it serves whoever a global administrator names.
		local, err := IsLocalRunner(ctx, database, a.RunnerID)
		if err != nil {
			return err
		}
		if !local {
			if err := ValidateAgencySet(a.AgencyIDs); err != nil {
				return err
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
		owner, before, local, err := runnerPlacement(ctx, tx, a.RunnerID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrUnknownRunner // deregistered between the check above and here
		}
		if err != nil {
			return err
		}
		// MA-11: an agent serves exactly its owner; a legacy placement only
		// shrinks; the local runner takes any non-empty list. Judged against
		// the list as it stands NOW, before the delete.
		if err := CheckRunnerPlacement(local, owner, before, a.AgencyIDs); err != nil {
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
		var name, runnerOwner string
		if err := tx.QueryRowContext(ctx, `SELECT name, `+agencyid.ColumnOwnerSQL("", "owner_agency")+` FROM runners WHERE id=?`,
			a.RunnerID).Scan(&name, &runnerOwner); err != nil {
			return err
		}
		summary := "agencies set: " + strings.Join(agencyNamesFor(ctx, tx, after), ", ")
		if len(after) == 1 && after[0] == agencyid.Global {
			// RB-22: say what the move MEANS. This runner now serves Global's
			// work — every run with no scope — which is a change of who it works
			// for, not merely a removal.
			summary = "moved to Global (it now serves Global's runs, and no department's)"
		}
		// In the transaction, like DRF-1/DRF-4: a membership change is an
		// isolation change, and one without its audit row must not exist. Target
		// and RunnerName follow the drain/keyscan convention so ?runner= finds it.
		if err := auditlog.WriteActivity(ctx, tx, auditlog.ActivityParams{
			AgencyID:   runnerOwner, // the runner's (C1)
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
	// Several runners, possibly several agencies': Global's, with the list (D2);
	// each runner's own agency has its Activity row above.
	if len(changed) > 0 {
		_ = writeChangeLog(ctx, database, agencyid.Global, actor, "Agencies", "membership-updated", "runner agencies",
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

// ValidateAgencySet applies the two rules every membership write obeys since
// Global became an agency (migration 1220): the set is not empty (LR-26), and
// Global is not named beside another agency (LR-25). The database enforces the
// second with a trigger as well; checking here turns a constraint error into a
// sentence, before anything is written.
func ValidateAgencySet(agencyIDs []string) error {
	if len(agencyIDs) == 0 {
		return ErrAgencyRequired
	}
	named := 0
	global := false
	for _, id := range agencyIDs {
		if id == agencyid.Global {
			global = true
		} else {
			named++
		}
	}
	if global && named > 0 {
		return ErrGlobalMixed
	}
	return nil
}

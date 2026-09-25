package settings

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/envref"
	"github.com/ResetSmith/cronomicon/internal/secrets"
	"github.com/ResetSmith/cronomicon/internal/tagutil"
)

// EnvVar mirrors the wire shape.
type EnvVar struct {
	ID             string  `json:"id"`
	Key            string  `json:"key"`
	Reference      string  `json:"reference"` // derived CRONOMICON_VAR_<key> (read-only; namespace contract)
	Value          string  `json:"value"`
	Scope          *string `json:"scope"`
	Description    *string `json:"description"`
	CreatedBy      string  `json:"createdBy"`
	CreatedAt      string  `json:"createdAt"`
	LastModifiedBy string  `json:"lastModifiedBy"`
	LastModifiedAt string  `json:"lastModifiedAt"`
	// Tags are operator-authored, SQLite-only labels (migration 470). Written via
	// PUT /api/v1/env-var-tags/{id}, never sourced from Git; always serialized
	// ([] when none). Mirrors the api.parseTags decoder (a sibling pkg can't import
	// it without a cycle, so the tiny decoder is local — see parseTags below).
	Tags []string `json:"tags"`
	// OwnerAgency is the NAME of the department that owns this row (RA-15, Phase E),
	// or "" for shared infrastructure. Ownership is what lets two departments hold
	// the same key in the same scope; it is derived from the creating actor's
	// department and is displayed so a same-key pair can be told apart at a glance.
	// The column stores the agency ID (rename-safe); this is resolved for display.
	OwnerAgency string `json:"ownerAgency"`
}

// EnvVarInput is the caller-supplied payload.
type EnvVarInput struct {
	Key         string
	Value       string
	Scope       *string
	Description *string
	// OwnerAgency is the owning agency ID (RA-15, Phase E); "" = shared. Set at
	// INSERT — see secrets.CreateInput.OwnerAgency for why it cannot be patched on.
	OwnerAgency string
}

// ListEnvVars returns all env_var rows.
func ListEnvVars(ctx context.Context, database *sql.DB) ([]EnvVar, error) {
	rows, err := database.QueryContext(ctx,
		`SELECT e.id, e.key, e.value, e.scope, e.description, e.created_by, e.created_at,
		        e.last_modified_by, e.last_modified_at, e.tags, COALESCE(ag.name,'')
		 FROM env_vars e LEFT JOIN agencies ag ON ag.id = e.owner_agency
		 ORDER BY e.created_at`)
	if err != nil {
		return nil, fmt.Errorf("list env_vars: %w", err)
	}
	defer rows.Close()
	var out []EnvVar
	for rows.Next() {
		var ev EnvVar
		var scope, description, tags sql.NullString
		if err := rows.Scan(&ev.ID, &ev.Key, &ev.Value, &scope, &description,
			&ev.CreatedBy, &ev.CreatedAt, &ev.LastModifiedBy, &ev.LastModifiedAt, &tags, &ev.OwnerAgency); err != nil {
			return nil, err
		}
		if scope.Valid {
			ev.Scope = &scope.String
		}
		if description.Valid {
			ev.Description = &description.String
		}
		ev.Tags = tagutil.Parse(tags.String)
		ev.Reference = envref.VarReference(ev.Key)
		out = append(out, ev)
	}
	return out, rows.Err()
}

// GetEnvVar fetches a single row by ID.
func GetEnvVar(ctx context.Context, database *sql.DB, id string) (*EnvVar, error) {
	row := database.QueryRowContext(ctx,
		`SELECT e.id, e.key, e.value, e.scope, e.description, e.created_by, e.created_at,
		        e.last_modified_by, e.last_modified_at, e.tags, COALESCE(ag.name,'')
		 FROM env_vars e LEFT JOIN agencies ag ON ag.id = e.owner_agency WHERE e.id=?`, id)
	var ev EnvVar
	var scope, description, tags sql.NullString
	if err := row.Scan(&ev.ID, &ev.Key, &ev.Value, &scope, &description,
		&ev.CreatedBy, &ev.CreatedAt, &ev.LastModifiedBy, &ev.LastModifiedAt, &tags, &ev.OwnerAgency); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if scope.Valid {
		ev.Scope = &scope.String
	}
	if description.Valid {
		ev.Description = &description.String
	}
	ev.Tags = tagutil.Parse(tags.String)
	ev.Reference = envref.VarReference(ev.Key)
	return &ev, nil
}

// ErrKeyConflict is returned when an env var's (key, scope) collides with an
// existing secret of the same key+scope. A13 unifies env vars + secrets into one
// "Env Var" namespace, so a key must be unique ACROSS both tables per scope
// (each table already enforces its own UNIQUE(key, scope)).
var ErrKeyConflict = errors.New("a secret with this key already exists for this scope")

// secretKeyExists reports whether a secret already uses (key, scope) — NULL and ”
// scope both mean "global" (COALESCE). A correlated/standalone query, never a
// nested iterator (pool deadlock — see db.maxOpenConns).
// RA-16 (Phase E): the conflict is now scoped to the same OWNER as well. A secret
// and a variable still may not share a key within one department's rows — that is
// the collision A13 exists to prevent, since both derive an CRONOMICON_* reference an
// author would read as one thing — but TeamA's secret X no longer blocks TeamB's
// variable X. The check narrows in step with the uniqueness key; leaving it on
// (key, scope) alone would have made Phase E half-real, allowing two departments'
// secrets but refusing a department's variable because another department owns a
// secret it cannot even see.
func secretKeyExists(ctx context.Context, database *sql.DB, key string, scope *string, owner string) bool {
	var n int
	if err := database.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM secrets
		 WHERE key = ? AND COALESCE(scope,'') = COALESCE(?,'') AND COALESCE(owner_agency,'') = ?`,
		key, scope, owner).Scan(&n); err != nil {
		return false // missing table / query error ⇒ don't block (defensive)
	}
	return n > 0
}

// CreateEnvVar inserts a new env_var and writes a change_log row.
func CreateEnvVar(ctx context.Context, database *sql.DB, inp EnvVarInput, actor string) (*EnvVar, error) {
	if err := envref.ValidateRowName(inp.Key); err != nil {
		return nil, err
	}
	if secretKeyExists(ctx, database, inp.Key, inp.Scope, inp.OwnerAgency) {
		return nil, ErrKeyConflict
	}
	now := time.Now().UTC().Format(time.RFC3339)
	id := db.NewID()
	_, err := database.ExecContext(ctx,
		`INSERT INTO env_vars (id, key, value, scope, description, created_by, created_at, last_modified_by, last_modified_at, owner_agency)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, inp.Key, inp.Value, inp.Scope, inp.Description, actor, now, actor, now, inp.OwnerAgency)
	if err != nil {
		return nil, fmt.Errorf("create env_var: %w", err)
	}
	secrets.RedactionSourceChanged() // AM-4b: a multi-line value is key material
	audit(ctx, database, actor, "Env Vars", "created", inp.Key, "")
	return GetEnvVar(ctx, database, id)
}

// UpdateEnvVar replaces the value/scope/description.
func UpdateEnvVar(ctx context.Context, database *sql.DB, id string, inp EnvVarInput, actor string) (*EnvVar, error) {
	if err := envref.ValidateRowName(inp.Key); err != nil {
		return nil, err
	}
	// RA-16: the A13 conflict is judged against THIS ROW'S owner, read from the row
	// rather than taken from the payload — ownership is not editable through this
	// route (transfer is an admin action on the Membership matrix), so trusting an
	// unset input field here would check the wrong department's rows.
	if secretKeyExists(ctx, database, inp.Key, inp.Scope, rowOwner(ctx, database, "env_vars", id)) {
		return nil, ErrKeyConflict
	}
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := database.ExecContext(ctx,
		`UPDATE env_vars SET key=?, value=?, scope=?, description=?, last_modified_by=?, last_modified_at=? WHERE id=?`,
		inp.Key, inp.Value, inp.Scope, inp.Description, actor, now, id)
	if err != nil {
		return nil, fmt.Errorf("update env_var: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return nil, nil
	}
	secrets.RedactionSourceChanged() // AM-4b
	audit(ctx, database, actor, "Env Vars", "updated", inp.Key, "")
	return GetEnvVar(ctx, database, id)
}

// DeleteEnvVar removes an env_var row.
func DeleteEnvVar(ctx context.Context, database *sql.DB, id, actor string) (bool, error) {
	// Fetch key for audit before delete.
	ev, _ := GetEnvVar(ctx, database, id)
	res, err := database.ExecContext(ctx, `DELETE FROM env_vars WHERE id=?`, id)
	if err != nil {
		return false, fmt.Errorf("delete env_var: %w", err)
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		secrets.RedactionSourceChanged() // AM-4b
	}
	if n > 0 && ev != nil {
		audit(ctx, database, actor, "Env Vars", "deleted", ev.Key, "")
	}
	return n > 0, nil
}

// rowOwner reads an entity's owning agency id ("" when unowned, or on any error —
// the unowned reading is the one that matches every pre-Phase-E row). `table` is a
// compile-time constant, never input.
func rowOwner(ctx context.Context, database *sql.DB, table, id string) string {
	var owner string
	if err := database.QueryRowContext(ctx,
		`SELECT COALESCE(owner_agency,'') FROM `+table+` WHERE id = ?`, id).Scan(&owner); err != nil {
		return ""
	}
	return owner
}

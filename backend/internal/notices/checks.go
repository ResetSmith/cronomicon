package notices

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ResetSmith/cronomicon/internal/agencyid"
)

// The checks below find conditions the database can be in but the application
// does not create: what an upgrade left for a person to decide, and damage.
// Each enumerates its condition and hands the whole list to Reconcile, so a
// notice appears when the condition does and resolves itself when it is put
// right. They read; they change nothing but the notices table.

// RunChecks runs every check. A check that fails is reported and does not stop
// the others: an inbox that is partly stale is still more use than none.
func RunChecks(ctx context.Context, database *sql.DB) error {
	var errs []string
	for _, c := range []struct {
		name string
		run  func(context.Context, *sql.DB) error
	}{
		{KindAgencyRenamed, checkAgencyRenamed},
		{KindSharedOwnership, checkSharedOwnership},
		{KindOrphaned, checkOrphaned},
	} {
		if err := c.run(ctx, database); err != nil {
			errs = append(errs, c.name+": "+err.Error())
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("notices: %s", strings.Join(errs, "; "))
	}
	return nil
}

// Refresher runs the checks at most once per interval, however many readers
// ask, so opening the inbox shows the present state without every request
// paying for it. The first caller in an interval runs them; the rest wait for
// that run and share its result.
type Refresher struct {
	Interval time.Duration

	mu   sync.Mutex
	last time.Time
	err  error
}

// Refresh runs the checks if the interval has passed since the last run.
func (r *Refresher) Refresh(ctx context.Context, database *sql.DB) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.last.IsZero() && time.Since(r.last) < r.Interval {
		return r.err
	}
	// The checks must not die with the request that happened to trigger them:
	// the next reader inside the interval would be shown a half-reconciled list.
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	r.err = RunChecks(runCtx, database)
	r.last = time.Now()
	return r.err
}

// checkAgencyRenamed only resolves. The notice is written by migration 1220,
// which alone knows it renamed something; it is over when the agency has been
// given a name of its own, or is gone.
func checkAgencyRenamed(ctx context.Context, database *sql.DB) error {
	_, err := database.ExecContext(ctx, `
		UPDATE notices SET resolved_at = ?
		 WHERE kind = ? AND resolved_at IS NULL
		   AND NOT EXISTS (SELECT 1 FROM agencies a
		                    WHERE a.id = notices.subject AND a.name LIKE '% (renamed)')`,
		now(), KindAgencyRenamed)
	return err
}

// ownedKinds are the three kinds of row that have an owner as well as members.
var ownedKinds = []struct {
	kind, noun, table, nameCol, scopeExpr, join, col string
}{
	{"secret", "secret", "secrets", "key", "COALESCE(t.scope, '')", "secret_agencies", "secret_id"},
	{"env-var", "variable", "env_vars", "key", "COALESCE(t.scope, '')", "env_var_agencies", "env_var_id"},
	{"ssh-credential", "SSH key", "ssh_credentials", "label", "''", "ssh_credential_agencies", "credential_id"},
}

// checkSharedOwnership lists rows that Global owns and only some agencies may
// use. Nothing in 2.3.0 creates one: they are what several agencies shared
// before it, and what migration 1220 would not give to one agency because doing
// so would have changed which row some run resolves. They work exactly as they
// did. They are listed because only a global administrator can change one, and
// because "one agency per row" is the rule everything created since follows.
func checkSharedOwnership(ctx context.Context, database *sql.DB) error {
	var found []Finding
	for _, k := range ownedKinds {
		rows, err := database.QueryContext(ctx, `
			SELECT t.id, t.`+k.nameCol+`, `+k.scopeExpr+`,
			       (SELECT group_concat(name, ', ') FROM (
			            SELECT a.name AS name FROM `+k.join+` m JOIN agencies a ON a.id = m.agency_id
			             WHERE m.`+k.col+` = t.id ORDER BY a.name))
			  FROM `+k.table+` t
			 WHERE t.owner_agency = ?
			   AND EXISTS (SELECT 1 FROM `+k.join+` m WHERE m.`+k.col+` = t.id AND m.agency_id <> ?)
			 ORDER BY t.id`, agencyid.Global, agencyid.Global)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id, name, scope string
			var members sql.NullString
			if err := rows.Scan(&id, &name, &scope, &members); err != nil {
				rows.Close()
				return err
			}
			where := ""
			if scope != "" {
				where = " in scope " + scope
			}
			found = append(found, Finding{
				AgencyID: agencyid.Global,
				Subject:  k.kind + ":" + id,
				Detail: fmt.Sprintf("The %s %s%s is owned by Global and usable only by %s. It works as it did before 2.3.0. "+
					"To settle it, give it to one agency (set its agencies to that one), or make it Global's for every "+
					"agency to use; if several agencies need their own, create a copy in each first.",
					k.noun, name, where, members.String),
			})
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
	}
	return Reconcile(ctx, database, KindSharedOwnership, found)
}

// checkOrphaned lists rows that belong to no agency. The database gives every
// row Global's membership at birth and the application never removes a row's
// last, so one of these was changed around both. No run can use it and every
// route refuses it except a global administrator assigning it an agency, which
// is what the notice asks for.
func checkOrphaned(ctx context.Context, database *sql.DB) error {
	var found []Finding
	for _, k := range []struct{ kind, noun, table, nameCol, join, col string }{
		{"scope", "scope", "scopes", "name", "scope_agencies", "scope_id"},
		{"runner", "runner", "runners", "name", "runner_agencies", "runner_id"},
		{"secret", "secret", "secrets", "key", "secret_agencies", "secret_id"},
		{"env-var", "variable", "env_vars", "key", "env_var_agencies", "env_var_id"},
		{"ssh-credential", "SSH key", "ssh_credentials", "label", "ssh_credential_agencies", "credential_id"},
	} {
		rows, err := database.QueryContext(ctx, `
			SELECT t.id, t.`+k.nameCol+` FROM `+k.table+` t
			 WHERE NOT EXISTS (SELECT 1 FROM `+k.join+` m WHERE m.`+k.col+` = t.id)
			 ORDER BY t.id`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id, name string
			if err := rows.Scan(&id, &name); err != nil {
				rows.Close()
				return err
			}
			found = append(found, Finding{
				AgencyID: agencyid.Global,
				Subject:  k.kind + ":" + id,
				Detail: fmt.Sprintf("The %s %s belongs to no agency, so nothing can use it and nobody but a global "+
					"administrator can change it. Assign it to an agency, or to Global, from the agency editor.",
					k.noun, name),
			})
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
	}
	return Reconcile(ctx, database, KindOrphaned, found)
}

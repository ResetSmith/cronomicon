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
		{KindScopeSeveralAgencies, checkScopeSeveralAgencies},
		{KindTargetHostOutsideScope, checkTargetHostOutsideScope},
		{KindRecordKeyOutsideOwner, checkRecordKeyOutsideOwner},
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

// checkScopeSeveralAgencies lists scopes in more than one agency. A scope
// belongs to exactly one since 2.3.0 (LR-7) and no route puts one in two; these
// are from before, and nothing removed them automatically. Such a scope still
// runs for each of its agencies. It is filed under Global because settling it —
// choosing the one agency — takes authority over every agency it is in.
func checkScopeSeveralAgencies(ctx context.Context, database *sql.DB) error {
	rows, err := database.QueryContext(ctx, `
		SELECT sc.id, sc.name,
		       (SELECT group_concat(name, ', ') FROM (
		            SELECT a.name AS name FROM scope_agencies m JOIN agencies a ON a.id = m.agency_id
		             WHERE m.scope_id = sc.id ORDER BY a.name))
		  FROM scopes sc
		 WHERE (SELECT COUNT(*) FROM scope_agencies m WHERE m.scope_id = sc.id) > 1
		 ORDER BY sc.id`)
	if err != nil {
		return err
	}
	var found []Finding
	for rows.Next() {
		var id, name string
		var agencies sql.NullString
		if err := rows.Scan(&id, &name, &agencies); err != nil {
			rows.Close()
			return err
		}
		found = append(found, Finding{
			AgencyID: agencyid.Global,
			Subject:  id,
			Detail: fmt.Sprintf("The scope %s is in several agencies (%s). A scope belongs to one agency since 2.3.0; "+
				"this one still runs for each of them until its agency is set, except that a name both agencies hold "+
				"(a secret, a variable, a key) is refused for its runs rather than picked between. Set its agency to "+
				"the one that owns its hosts; if several agencies run against the same hosts, give each a scope of its own.",
				name, agencies.String),
		})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	return Reconcile(ctx, database, KindScopeSeveralAgencies, found)
}

// checkTargetHostOutsideScope lists jobs whose fixed target_host is not one of
// their scope's hosts (LR-71). Until 2.3.0 the name was resolved against every
// host record there was; now such a run fails for that host, at the one place
// every producer resolves through. The notice goes to the scope's agency, whose
// job and whose scope it is (Global for a scope in several, or in Global).
func checkTargetHostOutsideScope(ctx context.Context, database *sql.DB) error {
	rows, err := database.QueryContext(ctx, `
		SELECT COALESCE(NULLIF(j.uid, ''), j.source || ':' || j.name), j.name, j.source, j.scope, j.target_host,
		       COALESCE((SELECT CASE WHEN COUNT(*) = 1 THEN MIN(sa.agency_id) END
		                   FROM scope_agencies sa WHERE sa.scope_id = sc.id), ?)
		  FROM jobs j JOIN scopes sc ON sc.name = j.scope
		 WHERE j.deleted_at IS NULL
		   AND COALESCE(j.target_host, '') <> ''
		   -- a scope that lists no hosts has no membership to be outside of
		   AND EXISTS (SELECT 1 FROM scope_hosts sh WHERE sh.scope_id = sc.id)
		   AND NOT EXISTS (SELECT 1 FROM scope_hosts sh WHERE sh.scope_id = sc.id AND sh.host = j.target_host)
		 ORDER BY j.name, j.source`, agencyid.Global)
	if err != nil {
		return err
	}
	var found []Finding
	for rows.Next() {
		var subject, name, source, scope, host, agency string
		if err := rows.Scan(&subject, &name, &source, &scope, &host, &agency); err != nil {
			rows.Close()
			return err
		}
		where := "edit the job"
		if source == "git" {
			where = "change the job's file in Git"
		}
		found = append(found, Finding{
			AgencyID: agency,
			Subject:  subject,
			Detail: fmt.Sprintf("The job %s targets the host %s, which is not one of the hosts of its scope %s. "+
				"A job runs only against hosts of its own scope: it cannot be started by hand or by a token, and a "+
				"scheduled run fails for that host wherever the server resolves the host itself. "+
				"Add the host to the scope, or %s to name one of the scope's hosts.",
				name, host, scope, where),
		})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	return Reconcile(ctx, database, KindTargetHostOutsideScope, found)
}

// recordKeyNameCheck, when set, finds bastions whose key is named by NAME (a
// secret, variable or key label) and does not resolve for the bastion's owner.
// The resolution rules live with the code that loads keys (sshexec), which this
// leaf package cannot import, so the server installs the function at boot.
var recordKeyNameCheck func(ctx context.Context, database *sql.DB) ([]Finding, error)

// SetRecordKeyNameCheck installs the by-name half of the record-key check. It
// returns findings of kind KindRecordKeyOutsideOwner, which are reconciled
// together with the by-id ones: one kind, one reconciliation.
func SetRecordKeyNameCheck(fn func(ctx context.Context, database *sql.DB) ([]Finding, error)) {
	recordKeyNameCheck = fn
}

// checkRecordKeyOutsideOwner lists hand-written host records and bastions whose
// SSH key is neither their owner's nor Global's (LR-72). A write refuses that
// now; a record from before 2.3.0, when it belonged to nobody, may still name
// any key. What that costs differs by kind, and the notice says which:
//
//   - a BASTION's key is loaded for its owner on every run that routes through
//     it, so such a bastion fails those runs;
//   - a HOST RECORD's key is loaded for the run's own agency, so runs of the
//     key's agency still connect, runs of any other fail for that host, and
//     "Test connection" (which asks for the owner) fails.
func checkRecordKeyOutsideOwner(ctx context.Context, database *sql.DB) error {
	var found []Finding
	for _, k := range []struct{ kind, noun, table, nameCol, where, cost string }{
		{"ssh-host", "host record", "ssh_hosts", "hostname", "t.scope_id IS NULL AND ",
			"Only runs of the key's own agency can connect with it, and Test connection fails."},
		{"bastion", "bastion", "bastions", "name", "",
			"Every run that routes through it fails."},
	} {
		rows, err := database.QueryContext(ctx, `
			SELECT t.id, t.`+k.nameCol+`, t.owner_agency, COALESCE((SELECT name FROM agencies WHERE id = t.owner_agency), t.owner_agency), c.label
			  FROM `+k.table+` t JOIN ssh_credentials c ON c.id = t.auth_credential_id
			 WHERE `+k.where+`NOT EXISTS (
			         SELECT 1 FROM ssh_credential_agencies m
			          WHERE m.credential_id = c.id AND m.agency_id IN (t.owner_agency, ?))
			 ORDER BY t.id`, agencyid.Global)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id, name, owner, ownerName, label string
			if err := rows.Scan(&id, &name, &owner, &ownerName, &label); err != nil {
				rows.Close()
				return err
			}
			found = append(found, Finding{
				AgencyID: owner,
				Subject:  k.kind + ":" + id,
				Detail: fmt.Sprintf("The %s %s belongs to %s and names the SSH key %s, which is neither %s's nor Global's. %s "+
					"Give the %s a key of its own agency or one that is Global's, or give the %s to the agency whose key it names.",
					k.noun, name, ownerName, label, ownerName, k.cost, k.noun, k.noun),
			})
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
	}
	if recordKeyNameCheck != nil {
		byName, err := recordKeyNameCheck(ctx, database)
		if err != nil {
			return err
		}
		found = append(found, byName...)
	}
	return Reconcile(ctx, database, KindRecordKeyOutsideOwner, found)
}

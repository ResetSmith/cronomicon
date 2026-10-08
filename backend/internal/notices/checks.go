package notices

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ResetSmith/cronomicon/internal/agencyid"
	"github.com/ResetSmith/cronomicon/internal/vaultpath"
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
		{KindVaultPathOutsidePrefix, checkVaultPathOutsidePrefix},
		{KindLegacyPlacement, checkLegacyPlacement},
		{"run_placement", checkRunPlacement},
		{KindShellJobRequires, checkShellJobRequires},
		{KindNoRunnerForShellJobs, checkNoRunnerForShellJobs},
		{KindHostKeyConflict, checkCarriedKeyConflicts},
		{KindLocalRunnerHostKeys, checkLocalRunnerHostKeys},
		{KindLeftoverExecutorKey, checkLeftoverExecutorKey},
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

// checkSharedOwnership lists secrets, variables and SSH keys that are not simply
// one agency's or Global's. Nothing in 2.3.0 creates one; they are from before:
//
//   - owned by Global and usable by only some agencies: what several agencies
//     shared, and what migration 1220 would not give to one agency because doing
//     so would have changed which row some run resolves;
//   - owned by one agency and shared with others, which 2.2 allowed.
//
// They work exactly as they did. They are listed because "one agency per row"
// is the rule everything created since follows, and because who may change one
// is not what its member list suggests: a Global-owned row is a global
// administrator's, and an agency-owned row's Vault path is its owner's alone.
// Filed under Global, whose administrators can settle either kind.
func checkSharedOwnership(ctx context.Context, database *sql.DB) error {
	var found []Finding
	for _, k := range ownedKinds {
		rows, err := database.QueryContext(ctx, `
			SELECT t.id, t.`+k.nameCol+`, `+k.scopeExpr+`, t.owner_agency,
			       COALESCE((SELECT name FROM agencies WHERE id = t.owner_agency), t.owner_agency),
			       (SELECT group_concat(name, ', ') FROM (
			            SELECT a.name AS name FROM `+k.join+` m JOIN agencies a ON a.id = m.agency_id
			             WHERE m.`+k.col+` = t.id ORDER BY a.name))
			  FROM `+k.table+` t
			 WHERE (t.owner_agency = ? AND EXISTS (SELECT 1 FROM `+k.join+` m WHERE m.`+k.col+` = t.id AND m.agency_id <> ?))
			    OR (SELECT COUNT(*) FROM `+k.join+` m WHERE m.`+k.col+` = t.id) > 1
			 ORDER BY t.id`, agencyid.Global, agencyid.Global)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id, name, scope, owner, ownerName string
			var members sql.NullString
			if err := rows.Scan(&id, &name, &scope, &owner, &ownerName, &members); err != nil {
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
				Detail: fmt.Sprintf("The %s %s%s is owned by %s and usable by %s. It works as it did before 2.3.0. "+
					"To settle it, give it to one agency (set its agencies to that one), or make it Global's for every "+
					"agency to use; if several agencies need their own, create a copy in each first.",
					k.noun, name, where, ownerName, members.String),
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

// checkVaultPathOutsidePrefix lists Vault-backed secrets and SSH keys that an
// agency owns and whose path is not inside the Vault paths assigned to that
// agency (LR-80, LR-81). The prefix rule is applied when such a row is written
// or moved, never when a run resolves it, so these go on working: removing a
// prefix does not revoke what was written under it, and on the day of the
// upgrade no agency has a prefix at all, so every agency-owned Vault-backed row
// is listed until a global administrator assigns one. Filed under Global, whose
// administrators assign prefixes. Rows that are Global's have no limit.
func checkVaultPathOutsidePrefix(ctx context.Context, database *sql.DB) error {
	prefixes := map[string][]string{}
	rows, err := database.QueryContext(ctx, `SELECT agency_id, prefix FROM agency_vault_prefixes ORDER BY agency_id, prefix`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var a, p string
		if err := rows.Scan(&a, &p); err != nil {
			rows.Close()
			return err
		}
		prefixes[a] = append(prefixes[a], p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	var found []Finding
	for _, k := range []struct{ kind, noun, table, nameCol, isVault string }{
		// Each store's own question for "is this row Vault-backed".
		{"secret", "secret", "secrets", "key", "(t.source <> 'stored' OR COALESCE(t.vault_ref, '') <> '')"},
		{"ssh-credential", "SSH key", "ssh_credentials", "label", "(t.source = 'vault' OR COALESCE(t.vault_ref, '') <> '')"},
	} {
		rows, err := database.QueryContext(ctx, `
			SELECT t.id, t.`+k.nameCol+`, t.owner_agency, COALESCE((SELECT name FROM agencies WHERE id = t.owner_agency), t.owner_agency),
			       COALESCE(t.vault_ref, '')
			  FROM `+k.table+` t
			 WHERE t.owner_agency <> ? AND `+k.isVault+`
			 ORDER BY t.id`, agencyid.Global)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id, name, owner, ownerName, ref string
			if err := rows.Scan(&id, &name, &owner, &ownerName, &ref); err != nil {
				rows.Close()
				return err
			}
			if ok, perr := vaultpath.Allowed(ref, prefixes[owner]); perr == nil && ok {
				continue
			}
			path, _ := vaultpath.Split(ref)
			why := "is outside the Vault paths assigned to " + ownerName + " (" + strings.Join(prefixes[owner], ", ") + ")"
			if len(prefixes[owner]) == 0 {
				why = "and " + ownerName + " has no Vault paths assigned"
			}
			found = append(found, Finding{
				AgencyID: agencyid.Global,
				Subject:  k.kind + ":" + id,
				Detail: fmt.Sprintf("The %s %s belongs to %s and reads the Vault path %s, which %s. It still works; "+
					"it cannot be edited or moved until that is settled. Assign %s a Vault path prefix that covers it, "+
					"or make the %s Global's if every agency is meant to use it.",
					k.noun, name, ownerName, path, why, ownerName, k.noun),
			})
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
	}
	return Reconcile(ctx, database, KindVaultPathOutsidePrefix, found)
}

// checkLegacyPlacement lists the agents whose serve list is not exactly their
// owner (MA-9, MA-28). An agent serves the one agency that owns it since 2.3.0
// and no route makes another shape; these served several agencies in 2.2, and
// the upgrade left them serving the same ones under Global's ownership (LR-64).
//
// Nothing about how such a runner works changes (MA-10), so the notice is a
// warning with a remedy and not a fault. It resolves itself when the list has
// been narrowed to one agency and the runner handed to it, or the row is gone.
// The remedy is given in the order that keeps a bound scope from closing:
// re-bind before narrowing.
//
// A runner with NO serve row is not this: it is damage, and checkOrphaned's.
// Nor is the local runner, the one runner that has a serve list by design
// (MA-14).
func checkLegacyPlacement(ctx context.Context, database *sql.DB) error {
	rows, err := database.QueryContext(ctx, `
		SELECT rn.id, rn.name, COALESCE(oa.name, rn.owner_agency),
		       (SELECT group_concat(name, ', ') FROM (
		            SELECT a.name AS name FROM runner_agencies m JOIN agencies a ON a.id = m.agency_id
		             WHERE m.runner_id = rn.id ORDER BY a.name)),
		       (SELECT COUNT(*) FROM runner_agencies m WHERE m.runner_id = rn.id)
		  FROM runners rn LEFT JOIN agencies oa ON oa.id = rn.owner_agency
		 WHERE rn.kind <> 'server'
		   AND EXISTS (SELECT 1 FROM runner_agencies m WHERE m.runner_id = rn.id)
		   AND NOT ((SELECT COUNT(*) FROM runner_agencies m WHERE m.runner_id = rn.id) = 1
		            AND EXISTS (SELECT 1 FROM runner_agencies m
		                         WHERE m.runner_id = rn.id AND m.agency_id = rn.owner_agency))
		 ORDER BY rn.id`)
	if err != nil {
		return err
	}
	var found []Finding
	for rows.Next() {
		var id, name, owner string
		var serves sql.NullString
		var n int
		if err := rows.Scan(&id, &name, &owner, &serves, &n); err != nil {
			rows.Close()
			return err
		}
		detail := fmt.Sprintf("The runner %s serves %s and is owned by %s. Since 2.3.0 an agent serves exactly the agency "+
			"that owns it; this one predates that and keeps working as it did, with the same toolchains and keys on a "+
			"machine these agencies share. It can be narrowed and never widened, and only a global administrator manages it. ",
			name, serves.String, owner)
		if n == 1 {
			detail += "It serves one agency now: hand it to that agency (Hand to an agency) and this is settled."
		} else {
			detail += "To settle it, in this order: enrol an agent for each agency it serves; re-bind each of that agency's " +
				"scopes that is bound to this runner to the new agent, scope by scope; copy the approved host keys for " +
				"that agency's hosts to the new agent; then take the agency off this runner, or deregister it. An agent " +
				"per agency need not mean a machine per agency: a second agent can run beside the first on the same " +
				"machine, under an OS user of its own (the install command's Instance name, --instance)."
		}
		found = append(found, Finding{AgencyID: agencyid.Global, Subject: id, Detail: detail})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	return Reconcile(ctx, database, KindLegacyPlacement, found)
}

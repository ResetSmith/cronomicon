package settings

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/notices"
	"github.com/ResetSmith/cronomicon/internal/secrets"
)

// KindAgencyVaultUnreachable — an agency's own Vault did not answer a login
// (2.5.1 SG-28): its Vault-backed secrets and SSH keys fail to resolve until
// it does, and never fall back to the installation's Vault (SG-21). Subject:
// the agency's id. Filed under that agency. It clears when the Vault answers
// or the connection is removed.
const KindAgencyVaultUnreachable = "agency_vault_unreachable"

// agencyVaultHealthWait is how long the check waits for one Vault.
const agencyVaultHealthWait = 3 * time.Second

// CheckAgencyVaults logs in to each agency's own Vault and reconciles
// agency_vault_unreachable. The Vaults are asked at once, each given
// agencyVaultHealthWait; one that has not answered by then is reported as not
// answering (its login goes on in the background and ends with the client's
// own timeout).
func CheckAgencyVaults(ctx context.Context, database *sql.DB, appCfg *config.Config) error {
	rows, err := database.QueryContext(ctx, `
		SELECT v.agency_id, COALESCE(a.name, v.agency_id), v.addr
		  FROM agency_vaults v LEFT JOIN agencies a ON a.id = v.agency_id
		 ORDER BY v.agency_id`)
	if err != nil {
		return err
	}
	type target struct{ id, name, addr string }
	var targets []target
	for rows.Next() {
		var t target
		if err := rows.Scan(&t.id, &t.name, &t.addr); err != nil {
			rows.Close()
			return err
		}
		targets = append(targets, t)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	problems := make([]chan string, len(targets))
	for i, t := range targets {
		ch := make(chan string, 1)
		problems[i] = ch
		go func() { ch <- agencyVaultProblem(ctx, database, appCfg, t.id) }()
	}
	timeout := time.After(agencyVaultHealthWait)
	var found []notices.Finding
	for i, t := range targets {
		var problem string
		select {
		case problem = <-problems[i]:
		case <-timeout:
			// The wait is shared: once it has passed, a Vault that has not
			// answered yet is reported as not answering.
			select {
			case problem = <-problems[i]:
			default:
				problem = fmt.Sprintf("it did not answer within %s", agencyVaultHealthWait)
			}
		}
		if problem == "" {
			continue
		}
		found = append(found, notices.Finding{
			AgencyID: t.id,
			Subject:  t.id,
			Detail: fmt.Sprintf("%s's own Vault at %s cannot be used: %s. Its Vault-backed secrets and SSH keys "+
				"do not resolve until it can, and runs that need them fail; they are never read from the "+
				"installation's Vault instead. Check the Vault, or its connection in Settings → Agency Vault.",
				t.name, t.addr, problem),
		})
	}
	return notices.Reconcile(ctx, database, KindAgencyVaultUnreachable, found)
}

// agencyVaultProblem is why the agency's own Vault cannot be used, or "".
func agencyVaultProblem(ctx context.Context, database *sql.DB, appCfg *config.Config, agency string) string {
	rt, found, err := ReadAgencyVaultRuntime(ctx, database, appCfg, agency)
	if err != nil {
		return "its connection cannot be read: " + err.Error()
	}
	if !found {
		return ""
	}
	vc, err := AgencyVaultClient(ctx, database, appCfg, rt)
	if err != nil {
		return err.Error()
	}
	if l, ok := vc.(secrets.VaultLogin); ok {
		if err := l.Login(); err != nil {
			return err.Error()
		}
	}
	return ""
}

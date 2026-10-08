package runner

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"

	"golang.org/x/crypto/ssh"

	"github.com/ResetSmith/cronomicon/internal/agencyid"
	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/hostkeys"
	"github.com/ResetSmith/cronomicon/internal/knownhostsline"
	"github.com/ResetSmith/cronomicon/internal/notices"
	"github.com/ResetSmith/cronomicon/internal/settings"
)

// What a parked key's note says when it was not carried. The conflict note is
// read by the inbox (notices.CarriedKeyConflictNote must equal it).
const (
	noteSameKey  = "the same key was already approved for this address"
	noteConflict = notices.CarriedKeyConflictNote
)

// carriedByUpgrade is the ledger actor of a key the 2.3.0 upgrade carried over
// from a record a global administrator owned, or that was imported for a scope.
// A key carried from a record an AGENCY wrote by hand is recorded with that
// agency in the actor (carriedByUpgradeFor), because serverPin must be able to
// tell: such a key is that agency's opinion, and is shown as "the server's
// key" only to a runner that serves that agency.
const carriedByUpgrade = "upgrade"

func carriedByUpgradeFor(agencyID string) string {
	if agencyID == "" || agencyID == agencyid.Global {
		return carriedByUpgrade
	}
	return carriedByUpgrade + ":agency:" + agencyID
}

// CarryServerHostKeys turns the host keys the server had captured before 2.3.0
// (parked by migration 1270 when their columns were dropped) into approved
// ledger rows for the local runner, so that every host the server was
// connecting to goes on being connected to. It runs at every start and does
// work once: a parked row is stamped when it has been handled.
//
// A carried key is recorded as what it is — source 'carried', actor the
// upgrade, one batch — not dressed as a reviewed approval. It never replaces a
// key already in force for the local runner: an operator's decision made since
// the upgrade outranks what was captured before it.
//
// The known_hosts host a key is recorded under is the one the engine verifies
// against: the record's dial address (hostkeys.Pattern(hostkeys.Target)), for
// a host and for a bastion alike. Never a name: a name is not a machine.
func CarryServerHostKeys(ctx context.Context, database *sql.DB, log *slog.Logger) (carried int, err error) {
	localID, err := settings.LocalRunnerID(ctx, database)
	if err != nil {
		return 0, err
	}
	if localID == "" {
		return 0, settings.ErrNoLocalRunner
	}
	type parked struct {
		id                              int64
		kind, recordID, name            string
		hostname, address, scopeID, key sql.NullString
		owner                           string
		port                            sql.NullInt64
	}
	rows, err := database.QueryContext(ctx, `
		SELECT id, kind, record_id, name, hostname, address, port, scope_id, owner_agency, host_key
		  FROM carried_server_host_keys WHERE carried_at IS NULL ORDER BY id`)
	if err != nil {
		return 0, fmt.Errorf("carry server host keys: %w", err)
	}
	var todo []parked
	for rows.Next() {
		var p parked
		if err := rows.Scan(&p.id, &p.kind, &p.recordID, &p.name, &p.hostname, &p.address, &p.port, &p.scopeID, &p.owner, &p.key); err != nil {
			rows.Close()
			return 0, fmt.Errorf("carry server host keys: %w", err)
		}
		todo = append(todo, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("carry server host keys: %w", err)
	}
	if len(todo) == 0 {
		return 0, nil
	}

	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck
	ts := now()
	batchID := db.NewID()
	skipped := 0
	for _, p := range todo {
		note := ""
		key, _, _, _, perr := ssh.ParseAuthorizedKey([]byte(p.key.String))
		if perr != nil {
			note = "the stored key could not be read"
		}
		var scopeName string
		if p.scopeID.Valid && p.scopeID.String != "" {
			_ = tx.QueryRowContext(ctx, `SELECT name FROM scopes WHERE id = ?`, p.scopeID.String).Scan(&scopeName)
		}
		// The dial address, as execspec.Target.DialAddr and hostkeys.Target
		// compute it for the record: its address, or its name with none.
		addr := p.address.String
		if addr == "" {
			addr = p.name
			if p.kind == "bastion" && p.hostname.String != "" {
				addr = p.hostname.String
			}
		}
		target := addr
		if p.port.Int64 != 0 && p.port.Int64 != 22 {
			target = net.JoinHostPort(addr, strconv.FormatInt(p.port.Int64, 10))
		}
		pattern := hostkeys.Pattern(target)
		if note == "" && !validScanTarget(target) {
			note = "the record has no usable address"
		}
		wrote, fingerprint := false, ""
		if note == "" {
			fingerprint = ssh.FingerprintSHA256(key)
			var inForce sql.NullString
			err := tx.QueryRowContext(ctx, `
				SELECT fingerprint FROM host_key_ledger
				 WHERE runner_id = ? AND host = ? AND key_type = ?
				   AND decision = 'approved' AND superseded_at IS NULL
				 ORDER BY id DESC LIMIT 1`, localID, pattern, key.Type()).Scan(&inForce)
			switch {
			case errors.Is(err, sql.ErrNoRows):
				if err := writeLedger(ctx, tx, batchID, localID, settings.LocalRunnerName, carriedByUpgradeFor(p.owner), ts, []ledgerEntry{{
					scopeID: p.scopeID.String, scopeName: scopeName, host: pattern, hostName: p.name,
					keyType: key.Type(), fingerprint: fingerprint,
					line: knownhostsline.Render(pattern, key), decision: "approved", source: "carried",
				}}); err != nil {
					return 0, fmt.Errorf("carry server host keys: %w", err)
				}
				wrote = true
			case err != nil:
				return 0, fmt.Errorf("carry server host keys: %w", err)
			case inForce.String == fingerprint:
				// Another record for the same address had the same key (a host
				// imported for two scopes, a bastion that is also a host
				// record), or an operator has approved it since. Nothing lost.
				note = noteSameKey
			default:
				// A DIFFERENT key is in force for this address. The server
				// trusts one key per address and type: either an operator has
				// decided since the upgrade, or two records share an address
				// and had different keys — two machines behind different
				// bastions, or a host that was re-keyed between the two
				// captures. The first one stands; this one is reported
				// (notices.checkCarriedKeyConflicts).
				note = noteConflict
			}
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE carried_server_host_keys SET pattern = NULLIF(?, ''), fingerprint = NULLIF(?, '') WHERE id = ?`,
			pattern, fingerprint, p.id); err != nil {
			return 0, fmt.Errorf("carry server host keys: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE carried_server_host_keys SET carried_at = ?, note = NULLIF(?, '') WHERE id = ?`, ts, note, p.id); err != nil {
			return 0, fmt.Errorf("carry server host keys: %w", err)
		}
		switch {
		case wrote:
			carried++
		case note == noteSameKey:
			// handled, and nothing to say
		default:
			skipped++
			if log != nil {
				log.Warn("local runner: a stored host key was not carried into its ledger",
					"kind", p.kind, "name", p.name, "host", pattern, "reason", note)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	if log != nil {
		log.Warn("local runner: the host keys this server had captured are now its approved keys — "+
			"from 2.3.0 it connects only to hosts with an approved key, and captures none on first connect "+
			"(scan and approve new hosts under Runners → Local runner → Host keys)",
			"carried", carried, "not_carried", skipped, "batch", batchID)
	}
	if carried > 0 {
		_ = settings.WriteChangeLog(ctx, database, carriedByUpgrade, "Host Keys", "carried",
			"runner:"+settings.LocalRunnerName,
			fmt.Sprintf("%d host key(s) the server had captured before 2.3.0; batch %s", carried, batchID))
	}
	return carried, nil
}

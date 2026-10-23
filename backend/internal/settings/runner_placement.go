package settings

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/ResetSmith/cronomicon/internal/agencyid"
	"github.com/ResetSmith/cronomicon/internal/auditlog"
)

// A runner's placement is two facts: who OWNS it (runners.owner_agency) and
// what it SERVES (runner_agencies, the agencies whose runs it claims). For an
// agent they are the same fact: it serves exactly its owner, which its
// registration token named, and that is the whole of its placement (LR-58,
// LR-61, MA-1). There is no shared agent.
//
// One shape predates that rule and is allowed to go on existing: a runner that
// served several agencies in 2.2 is Global-owned with its serve list unchanged,
// a "legacy placement" (LR-64, MA-9). Since §0a (I1) it serves its owner only
// (the claim joins on owner_agency): the other agencies on its list are listed
// in the notice, not served. It can be narrowed, never widened; narrowed to one
// agency it can be handed to that agency, which ends it.

var (
	// ErrServeListFixed is returned when a write would give an agent a serve
	// list that is neither exactly its owner nor a narrowing of the list it had.
	// Mapped 422 `serve_list_fixed`.
	ErrServeListFixed = errors.New("an agent serves exactly the agency that owns it: its serve list cannot be widened or changed. " +
		"To serve another agency, enrol another agent for it")

	// ErrOwnerChangeRefused is returned for any owner change but the one that is
	// allowed: a Global-owned agent that serves exactly one agency, handed to
	// that agency. Mapped 422 `owner_change_refused`.
	ErrOwnerChangeRefused = errors.New("an agent's owner changes only one way: a Global-owned agent that serves exactly one agency " +
		"may be handed to that agency. To move an agent any other way, deregister it and enrol it again")

	// ErrLocalRunnerGlobalOnly is returned when a write would give the local
	// runner any serve list but its owner (§0a, IN-3; since 2.5.3 its owner is
	// the agency a global administrator gives it, CA-8). Mapped 422
	// `local_runner_serves_owner`.
	ErrLocalRunnerGlobalOnly = errors.New("the local runner serves the agency it belongs to and no other: " +
		"a global administrator gives it to an agency under Settings → Local runner")
)

// CheckRunnerPlacement is THE invariant (MA-11), called by every writer of
// runners.owner_agency or runner_agencies, with the serve list as it stood
// before the write, read inside the writer's own transaction (the setters
// delete every row and insert again, so "before" cannot be read afterwards).
//
// For an agent, the list after the write must be exactly the owner, or a
// non-empty subset of the list before it. Never empty. So a new shape is
// impossible and an old one only shrinks.
//
// local is the local runner (Phase A), whose serve list is its owner and
// nothing else: since 2.5.3 the agency a global administrator gives it, or the
// built-in agency's while it belongs to none (CA-8, CA-9); Global alone from
// §0a (IN-3); before that whatever non-empty list a global administrator gave
// it (MA-11).
func CheckRunnerPlacement(local bool, owner string, before, after []string) error {
	after = dedupeIDs(after)
	if len(after) == 0 {
		return ErrAgencyRequired
	}
	if local {
		if len(after) != 1 || after[0] != owner {
			return ErrLocalRunnerGlobalOnly
		}
		return nil
	}
	if len(after) == 1 && after[0] == owner {
		return nil
	}
	for _, a := range after {
		if !slices.Contains(before, a) {
			return ErrServeListFixed
		}
	}
	return nil
}

// runnerPlacement reads a runner's owner, its serve list and whether it is the
// local runner, inside a transaction.
func runnerPlacement(ctx context.Context, tx *sql.Tx, runnerID string) (owner string, serves []string, local bool, err error) {
	var kind string
	if err = tx.QueryRowContext(ctx, `SELECT owner_agency, kind FROM runners WHERE id = ?`, runnerID).Scan(&owner, &kind); err != nil {
		return "", nil, false, err
	}
	serves, err = runnerAgencyIDs(ctx, tx, runnerID)
	return owner, serves, kind == RunnerKindServer, err
}

// IsLegacyPlacement reports whether an AGENT's serve list is anything but
// exactly its owner (MA-9). The local runner has a serve list by design and is
// never one: ask only about agents.
func IsLegacyPlacement(owner string, serves []string) bool {
	return len(serves) != 1 || serves[0] != owner
}

// SetRunnerOwner hands a Global-owned agent that serves exactly one agency to
// that agency (MA-12). It is the only owner change there is, and it ends a
// legacy placement: afterwards the agent is the agency's own, administered by
// the agency. Everything else is refused: agency to Global and agency to agency
// would move an agent's caches and key files to someone who did not put them
// there; for those, deregister and enrol again.
func SetRunnerOwner(ctx context.Context, database *sql.DB, runnerID, agencyID, actor string) error {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	owner, serves, local, err := runnerPlacement(ctx, tx, runnerID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrUnknownRunner
	}
	if err != nil {
		return err
	}
	if owner == agencyID {
		return nil // already theirs
	}
	// The local runner changes agency by SetLocalRunnerOwner alone (2.5.3,
	// CA-8): it is this server, and a change of its agency is a global
	// administrator's, with what it clears.
	if local || owner != agencyid.Global || agencyID == agencyid.Global ||
		len(serves) != 1 || serves[0] != agencyID {
		return ErrOwnerChangeRefused
	}
	if err := CheckRunnerPlacement(false, agencyID, serves, serves); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE runners SET owner_agency = ? WHERE id = ?`, agencyID, runnerID); err != nil {
		return err
	}
	var name string
	if err := tx.QueryRowContext(ctx, `SELECT name FROM runners WHERE id = ?`, runnerID).Scan(&name); err != nil {
		return err
	}
	// In the transaction: who administers a runner is an isolation fact, and a
	// change of it without its audit row must not exist.
	if err := auditlog.WriteActivity(ctx, tx, auditlog.ActivityParams{
		AgencyID:   agencyID, // the agency it was handed to; the summary says it was Global's (D1)
		At:         time.Now().UTC().Format(time.RFC3339),
		Kind:       "config",
		Outcome:    "success",
		Actor:      actor,
		Category:   "Agencies",
		Target:     "runner:" + name,
		RunnerName: name,
		Summary: fmt.Sprintf("handed to %s: it was Global's and served only that agency; its administrators manage it now",
			firstOr(agencyNamesFor(ctx, tx, []string{agencyID}), agencyID)),
	}); err != nil {
		return err
	}
	return tx.Commit()
}

func firstOr(s []string, fallback string) string {
	if len(s) > 0 && s[0] != "" {
		return s[0]
	}
	return fallback
}

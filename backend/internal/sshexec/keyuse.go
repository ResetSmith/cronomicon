package sshexec

import (
	"context"
	"database/sql"
	"errors"

	"github.com/ResetSmith/cronomicon/internal/runref"
)

// The local runner's connection keys in the record of which runs used which
// secret and key (2.5.1 SG2.2). A run's injected references are recorded by
// the dispatch audit; the key a host record or a bastion names is loaded here,
// at connect, and recorded here too. An agent's connection keys are its own
// files, not Cronomicon rows, and cannot be recorded.
//
// The run's connect path carries a keyUse on its context (withKeyUse); a probe
// ("Test connection") carries none and records nothing.

type keyUseKey struct{}

type keyUse struct {
	traceID string
	via     string // host_auth | bastion_auth
}

// withKeyUse marks ctx as run traceID's connect path; the keys loaded under it
// are recorded as used by the run.
func withKeyUse(ctx context.Context, traceID, via string) context.Context {
	return context.WithValue(ctx, keyUseKey{}, keyUse{traceID: traceID, via: via})
}

// withKeyVia keeps ctx's run and says the keys now loaded authenticate a
// bastion (or the host). No-op on a context that is not a run's.
func withKeyVia(ctx context.Context, via string) context.Context {
	if u, ok := ctx.Value(keyUseKey{}).(keyUse); ok {
		u.via = via
		return context.WithValue(ctx, keyUseKey{}, u)
	}
	return ctx
}

// noteKeyUse records that the run on ctx connected with row id of kind (a
// stored secret holding a private key, or an SSH key credential). A recording
// failure fails the connect: a run must not use a secret untraced, the rule
// the dispatch audit keeps (M6).
func noteKeyUse(ctx context.Context, db *sql.DB, kind runref.Kind, id, name string) error {
	u, ok := ctx.Value(keyUseKey{}).(keyUse)
	if !ok || u.traceID == "" || id == "" {
		return nil
	}
	table, label := "secrets", "key"
	if kind == runref.KindKey {
		table, label = "ssh_credentials", "label"
	}
	var owner, rowName string
	err := db.QueryRowContext(ctx,
		`SELECT COALESCE(owner_agency, ''), COALESCE(`+label+`, '') FROM `+table+` WHERE id = ?`, id). //nolint:gosec // fixed identifiers
		Scan(&owner, &rowName)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if name == "" {
		name = rowName
	}
	return runref.RecordUses(ctx, db, u.traceID, []runref.ResolvedRef{{Kind: kind, Name: name, RowID: id, Owner: owner, Origin: u.via}})
}

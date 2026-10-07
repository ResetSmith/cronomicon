package db_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/runref"
)

// Migration 1220 gives every unowned secret, variable and key an owner, and an
// owner is a TIER: runref consults "your agency's row" before it looks at the
// scope. So the migration can change which row a run resolves, with no error
// and nothing in the run log — the worst kind of upgrade defect, and one that a
// handful of hand-picked fixtures does not find (the first draft of the
// backfill passed its hand-picked fixtures and was wrong).
//
// This test does not pick. It builds every arrangement of up to three rows
// sharing one key, over three scopes and every membership-and-owner shape the
// write path can produce, seeds them at schema 1210, upgrades, and asks the
// real resolver for every run that could exist. Each answer must be the one the
// 2.2 rule gave: the same row, the same "nothing", or the same refusal.

const (
	wAlpha = "Alpha" // agency names; the ids are ag-<lowercase>
	wBeta  = "Beta"
)

// wrow is one secret, variable or key as the PREVIOUS release stored it.
type wrow struct {
	id      string
	scope   string   // "" = global scope
	members []string // agency names; none = "no agency", the old shared marker
	owner   string   // agency name; "" = unowned
}

func (r wrow) String() string {
	return fmt.Sprintf("{scope=%q members=%v owner=%q}", r.scope, r.members, r.owner)
}

// resolve22 is the 2.2 resolution rule, transcribed from runref.lookupScoped and
// pickOwned as they stood before the Global agency: a row is visible when it has
// no membership or its membership meets the run's agencies, and its scope is
// global or the run's; the owned tier beats the shared tier; within a tier the
// scope-exact row beats the global one; more than one survivor is a refusal.
func resolve22(rows []wrow, runScope string, run []string) (id string, ambiguous bool) {
	var owned, shared []wrow
	for _, r := range rows {
		if r.scope != "" && r.scope != runScope {
			continue
		}
		if len(r.members) > 0 && !slices.ContainsFunc(r.members, func(m string) bool { return slices.Contains(run, m) }) {
			continue
		}
		switch {
		case r.owner == "":
			shared = append(shared, r)
		case slices.Contains(run, r.owner):
			owned = append(owned, r)
		}
	}
	tier := owned
	if len(tier) == 0 {
		tier = shared
	}
	if len(tier) == 0 {
		return "", false
	}
	var exact []wrow
	for _, r := range tier {
		if runScope != "" && r.scope == runScope {
			exact = append(exact, r)
		}
	}
	if len(exact) > 0 {
		tier = exact
	}
	if len(tier) > 1 {
		return "", true
	}
	return tier[0].id, false
}

// The membership-and-owner shapes the write path produces: an owner is always a
// member (the setters enrol it), and an unowned row may be in none, one or both.
var wShapes = []struct {
	members []string
	owner   string
}{
	{nil, ""},
	{[]string{wAlpha}, ""},
	{[]string{wBeta}, ""},
	{[]string{wAlpha, wBeta}, ""},
	{[]string{wAlpha}, wAlpha},
	{[]string{wBeta}, wBeta},
	{[]string{wAlpha, wBeta}, wAlpha},
	{[]string{wAlpha, wBeta}, wBeta},
}

// worlds returns every multiset of 1..size row kinds that the table's UNIQUE
// constraint admits. scopes is {""} for keys, which have no scope.
func worlds(scopes []string, size int, valid func([]wrow) bool) [][]wrow {
	var kinds []wrow
	for _, sc := range scopes {
		for _, sh := range wShapes {
			kinds = append(kinds, wrow{scope: sc, members: sh.members, owner: sh.owner})
		}
	}
	var out [][]wrow
	var pick func(from int, cur []wrow)
	pick = func(from int, cur []wrow) {
		if len(cur) > 0 && valid(cur) {
			out = append(out, slices.Clone(cur))
		}
		if len(cur) == size {
			return
		}
		for i := from; i < len(kinds); i++ {
			pick(i, append(cur, kinds[i]))
		}
	}
	pick(0, nil)
	return out
}

// UNIQUE (key, scope, owner_agency) with a NULL global scope: two rows collide
// only when they share a NAMED scope and an owner. (Two global-scoped rows may
// share everything — the pre-existing hole migration 830 left open on purpose.)
func scopedRowsValid(rows []wrow) bool {
	for i := range rows {
		for j := i + 1; j < len(rows); j++ {
			if rows[i].scope != "" && rows[i].scope == rows[j].scope && rows[i].owner == rows[j].owner {
				return false
			}
		}
	}
	return true
}

// UNIQUE (label, owner_agency): no two keys with one label share an owner.
func keyRowsValid(rows []wrow) bool {
	for i := range rows {
		for j := i + 1; j < len(rows); j++ {
			if rows[i].owner == rows[j].owner {
				return false
			}
		}
	}
	return true
}

func agencyID(name string) string { return "ag-" + strings.ToLower(name) }

func TestMigrate1220ResolvesEveryReferenceAsBefore(t *testing.T) {
	// Which agencies each of the two scopes belongs to. The last two are the
	// state Phase G2 retires — a scope in several agencies — where one run
	// carries both and two departments' rows can meet.
	configs := []struct {
		name   string
		s1, s2 []string
		size   int
	}{
		{"one agency per scope", []string{wAlpha}, []string{wBeta}, 3},
		{"second scope in no agency", []string{wAlpha}, nil, 3},
		{"both scopes in one agency", []string{wAlpha}, []string{wAlpha}, 2},
		{"a scope in two agencies", []string{wAlpha, wBeta}, []string{wBeta}, 3},
		{"a shared scope and an unassigned one", []string{wAlpha, wBeta}, nil, 2},
	}
	if testing.Short() {
		for i := range configs {
			configs[i].size = 2
		}
	}
	for _, cfg := range configs {
		t.Run(cfg.name, func(t *testing.T) {
			t.Parallel()
			pool, err := db.Open(filepath.Join(t.TempDir(), "w.db"))
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer pool.Close()
			if err := db.MigrateTo(pool, 1210); err != nil {
				t.Fatalf("migrate to 1210: %v", err)
			}
			const ts = "2026-09-01T00:00:00Z"
			tx, err := pool.Begin()
			if err != nil {
				t.Fatal(err)
			}
			exec := func(q string, args ...any) {
				t.Helper()
				if _, err := tx.Exec(q, args...); err != nil {
					t.Fatalf("seed: %v\n%s", err, q)
				}
			}
			for _, a := range []string{wAlpha, wBeta} {
				exec(`INSERT INTO agencies (id, name, created_at) VALUES (?, ?, ?)`, agencyID(a), a, ts)
			}
			for sc, ags := range map[string][]string{"s1": cfg.s1, "s2": cfg.s2} {
				exec(`INSERT INTO scopes (id, name, source, created_at) VALUES (?, ?, 'cronomicon', ?)`, "sc-"+sc, sc, ts)
				for _, a := range ags {
					exec(`INSERT INTO scope_agencies (scope_id, agency_id) VALUES (?, ?)`, "sc-"+sc, agencyID(a))
				}
			}

			type table struct {
				kind      runref.Kind
				insert    string // id, key, scope, created_at, owner
				member    string
				worlds    [][]wrow
				hasScopes bool
			}
			// Built once per table: seeding writes each row's id into its world.
			scoped := func() [][]wrow { return worlds([]string{"", "s1", "s2"}, cfg.size, scopedRowsValid) }
			tables := []table{
				{runref.KindSecret,
					`INSERT INTO secrets (id, key, scope, source, created_at, owner_agency) VALUES (?, ?, ?, 'stored', ?, ?)`,
					`INSERT INTO secret_agencies (secret_id, agency_id) VALUES (?, ?)`, scoped(), true},
				{runref.KindVar,
					`INSERT INTO env_vars (id, key, scope, value, created_at, owner_agency) VALUES (?, ?, ?, 'v', ?, ?)`,
					`INSERT INTO env_var_agencies (env_var_id, agency_id) VALUES (?, ?)`, scoped(), true},
				{runref.KindKey,
					`INSERT INTO ssh_credentials (id, label, source, created_at, owner_agency) VALUES (?, ?, 'stored', ?, ?)`,
					`INSERT INTO ssh_credential_agencies (credential_id, agency_id) VALUES (?, ?)`,
					worlds([]string{""}, 3, keyRowsValid), false},
			}
			// Every world gets a key of its own, so the worlds cannot see each
			// other; the ids say which world and which row.
			for ti := range tables {
				tb := &tables[ti]
				for wi := range tb.worlds {
					key := fmt.Sprintf("K%d_%d", ti, wi)
					for ri := range tb.worlds[wi] {
						r := &tb.worlds[wi][ri]
						r.id = fmt.Sprintf("%s.%d", key, ri)
						owner := ""
						if r.owner != "" {
							owner = agencyID(r.owner)
						}
						if tb.hasScopes {
							var scope any
							if r.scope != "" {
								scope = r.scope
							}
							exec(tb.insert, r.id, key, scope, ts, owner)
						} else {
							exec(tb.insert, r.id, key, ts, owner)
						}
						for _, m := range r.members {
							exec(tb.member, r.id, agencyID(m))
						}
					}
				}
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}

			if err := db.Migrate(pool); err != nil {
				t.Fatalf("migrate to head: %v", err)
			}

			// The runs that can exist: a job with no scope, and a job in each
			// scope, carrying that scope's agencies. A run that had NO agency
			// carried the empty set then and carries Global now.
			runs := []struct {
				scope    string
				agencies []string
			}{{"", nil}, {"s1", cfg.s1}, {"s2", cfg.s2}}
			ctx := context.Background()
			checked, failures := 0, 0
			for ti, tb := range tables {
				for wi, world := range tb.worlds {
					key := fmt.Sprintf("K%d_%d", ti, wi)
					for _, run := range runs {
						if !tb.hasScopes && run.scope != "" && len(run.agencies) == 0 {
							continue // the same question as the unscoped run
						}
						scope := run.scope
						if !tb.hasScopes {
							scope = ""
						}
						wantID, wantAmbiguous := resolve22(world, scope, run.agencies)
						now := run.agencies
						if len(now) == 0 {
							now = []string{"Global"}
						}
						gotID, found, err := runref.LookupEntityID(ctx, pool, tb.kind, key, scope, now)
						gotAmbiguous := errors.Is(err, runref.ErrAmbiguousReference)
						if err != nil && !gotAmbiguous {
							t.Fatalf("lookup %s: %v", key, err)
						}
						if !found {
							gotID = ""
						}
						checked++
						if gotID != wantID || gotAmbiguous != wantAmbiguous {
							failures++
							if failures <= 12 {
								t.Errorf("%s %s, run in scope %q with agencies %v:\n  rows %v\n  2.2 resolved %q (refused=%v)\n  now resolves %q (refused=%v)\n  owners now: %s",
									tb.kind, key, run.scope, run.agencies, world, wantID, wantAmbiguous, gotID, gotAmbiguous,
									ownersNow(t, pool, tb.kind, key))
							}
						}
					}
				}
			}
			if failures > 0 {
				t.Fatalf("%d of %d resolutions changed across the upgrade", failures, checked)
			}
			t.Logf("%d resolutions unchanged", checked)

			// And the migration did give rows away: an equivalence that holds
			// because nothing was promoted would be worthless.
			for _, q := range []string{
				`SELECT COUNT(*) FROM secrets WHERE owner_agency NOT IN ('global')`,
				`SELECT COUNT(*) FROM env_vars WHERE owner_agency NOT IN ('global')`,
				`SELECT COUNT(*) FROM ssh_credentials WHERE owner_agency NOT IN ('global')`,
			} {
				var n int
				if err := pool.QueryRow(q).Scan(&n); err != nil || n == 0 {
					t.Fatalf("%s = %d, %v: no row has an agency for its owner", q, n, err)
				}
			}
			for _, tbl := range []string{"secrets", "env_vars", "ssh_credentials"} {
				var n int
				if err := pool.QueryRow(`SELECT COUNT(*) FROM ` + tbl + ` WHERE owner_agency = ''`).Scan(&n); err != nil || n != 0 {
					t.Fatalf("%s: %d rows are still unowned (%v)", tbl, n, err)
				}
			}
		})
	}
}

func ownersNow(t *testing.T, pool *sql.DB, kind runref.Kind, key string) string {
	t.Helper()
	q := map[runref.Kind]string{
		runref.KindSecret: `SELECT id, owner_agency FROM secrets WHERE key = ? ORDER BY id`,
		runref.KindVar:    `SELECT id, owner_agency FROM env_vars WHERE key = ? ORDER BY id`,
		runref.KindKey:    `SELECT id, owner_agency FROM ssh_credentials WHERE label = ? ORDER BY id`,
	}[kind]
	rows, err := pool.Query(q, key)
	if err != nil {
		return err.Error()
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id, owner string
		if err := rows.Scan(&id, &owner); err != nil {
			return err.Error()
		}
		out = append(out, id+"="+owner)
	}
	return strings.Join(out, " ")
}

package settings

import (
	"context"
	"testing"
)

// 2.5.3 CA-16, question 213: the first start makes Operations when only
// Central exists, once.
func TestFirstAgencyIsMadeOnceWhenOnlyCentralExists(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()

	id, _, err := EnsureFirstAgency(ctx, pool)
	if err != nil || id == "" {
		t.Fatalf("first start = %q, %v; want Operations made", id, err)
	}
	a, err := GetAgency(ctx, pool, id)
	if err != nil || a == nil || a.Name != FirstAgencyName || a.Builtin || a.CreatedBy != firstAgencyActor {
		t.Fatalf("the agency made = %+v, %v; want an ordinary Operations, made by %q", a, err, firstAgencyActor)
	}
	var logged int
	if err := pool.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM change_log WHERE category = 'Agencies' AND action = 'created' AND target = ? AND actor = ?`,
		FirstAgencyName, firstAgencyActor).Scan(&logged); err != nil || logged != 1 {
		t.Errorf("change-log rows for its creation = %d, %v; want 1", logged, err)
	}

	// The next start makes nothing.
	if again, why, err := EnsureFirstAgency(ctx, pool); err != nil || again != "" || why != "it ran before" {
		t.Errorf("second start = %q, %q, %v; want nothing, it ran before", again, why, err)
	}
	// Deleted, it is not made again.
	if _, err := pool.ExecContext(ctx, `DELETE FROM agencies WHERE id = ?`, id); err != nil {
		t.Fatalf("delete Operations: %v", err)
	}
	if again, _, err := EnsureFirstAgency(ctx, pool); err != nil || again != "" {
		t.Errorf("start after deleting it = %q, %v; want nothing made", again, err)
	}
}

// An installation with an agency (a dev seed, an upgrade that had one) gets
// none, and the step is recorded, so a later deletion of every agency does not
// bring Operations.
func TestFirstAgencyIsNotMadeWhereAnAgencyExists(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	if _, err := CreateAgency(ctx, pool, AgencyInput{Name: "Tax"}, "test"); err != nil {
		t.Fatalf("create Tax: %v", err)
	}
	if id, why, err := EnsureFirstAgency(ctx, pool); err != nil || id != "" || why != "an agency exists" {
		t.Fatalf("start with Tax = %q, %q, %v; want nothing, an agency exists", id, why, err)
	}
	if _, err := pool.ExecContext(ctx, `DELETE FROM agencies WHERE name = 'Tax'`); err != nil {
		t.Fatalf("delete Tax: %v", err)
	}
	if id, _, err := EnsureFirstAgency(ctx, pool); err != nil || id != "" {
		t.Errorf("start after deleting every agency = %q, %v; want nothing made", id, err)
	}
}

// A start stopped after making the agency and before recording the step makes
// no second one.
func TestFirstAgencyAStoppedStartMakesNoSecond(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	if _, _, err := EnsureFirstAgency(ctx, pool); err != nil {
		t.Fatalf("first start: %v", err)
	}
	if _, err := pool.ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, firstAgencyKey); err != nil {
		t.Fatalf("forget the step: %v", err)
	}
	if id, why, err := EnsureFirstAgency(ctx, pool); err != nil || id != "" || why != "an agency exists" {
		t.Errorf("restart = %q, %q, %v; want nothing, an agency exists", id, why, err)
	}
	var n int
	if err := pool.QueryRowContext(ctx, `SELECT COUNT(*) FROM agencies WHERE name = ?`, FirstAgencyName).Scan(&n); err != nil || n != 1 {
		t.Errorf("Operations agencies = %d, %v; want 1", n, err)
	}
}

// Central renamed Operations: the name is taken; nothing is made, the step is
// recorded, and the first-agency card asks for a name instead.
func TestFirstAgencyNotMadeWhenCentralHasItsName(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	if _, err := pool.ExecContext(ctx, `UPDATE agencies SET name = ? WHERE id = 'global'`, FirstAgencyName); err != nil {
		t.Fatalf("rename Central: %v", err)
	}
	if id, why, err := EnsureFirstAgency(ctx, pool); err != nil || id != "" || why != "the built-in agency is named Operations" {
		t.Errorf("start = %q, %q, %v; want nothing, the name taken", id, why, err)
	}
}

package gitlab

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/calendar"
)

// CAL-9 — the Git authoring surface for calendar bindings.
//
// §5 flags CAL-9 as the item "easy to delete by mistake", and the pre-commit
// review proved the point: two real bugs lived exactly here (first-class
// schedule bindings escaped validation entirely, and were never persisted onto
// the catalog row). These tests fence both.

func seedTestCalendar(t *testing.T, pool *sql.DB, name string, global bool) {
	t.Helper()
	g := 0
	if global {
		g = 1
	}
	if _, err := pool.ExecContext(context.Background(),
		`INSERT INTO calendars(source, name, global, record_suppressed, created_at)
		 VALUES('amadeus', ?, ?, 0, '2026-01-01T00:00:00Z')`, name, g); err != nil {
		t.Fatalf("seed calendar %s: %v", name, err)
	}
}

// An inline entry's binding survives the round trip onto the runtime row.
func TestScheduleEntryCalendarBindingsPersist(t *testing.T) {
	pool := mustOpenDB(t)
	svc := &Service{db: pool, cloneDir: t.TempDir()}
	seedTestCalendar(t, pool, "holidays", false)

	j := JobYAML{}
	j.APIVersion = requiredAPIVersion
	j.Kind = "Job"
	j.Metadata.Name = "patch"
	j.Spec.RunType = "bash"
	j.Spec.ConcurrencyPolicy = "Allow"
	j.Spec.Schedules = []ScheduleEntry{{
		Name: "nightly", Cron: "0 0 2 * * *", SkipCalendars: []string{"holidays"},
	}}

	tx, err := pool.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339)
	if err := svc.upsertJobs(context.Background(), tx, []JobYAML{j}, nil, nil, now, "sha1"); err != nil {
		t.Fatalf("upsertJobs: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var skip sql.NullString
	if err := pool.QueryRow(
		`SELECT skip_calendars FROM definition_schedules WHERE owner_name='patch' AND name='nightly'`).Scan(&skip); err != nil {
		t.Fatalf("read binding: %v", err)
	}
	if skip.String != `["holidays"]` {
		t.Errorf("skip_calendars = %q, want [\"holidays\"] — a Git-authored job must be able to name an operator-authored calendar", skip.String)
	}
}

// A first-class schedule's binding must reach the CATALOG row, not just the
// runtime rows. resolveComposeSchedules reads the catalog when an IN-APP job
// binds a scheduleRef, so a NULL here means an in-app job silently loses the
// calendar policy of the Git schedule it references — §2.5's reusable-policy
// story failing across the source boundary.
func TestFirstClassScheduleBindingsReachCatalogRow(t *testing.T) {
	pool := mustOpenDB(t)
	svc := &Service{db: pool, cloneDir: t.TempDir()}
	seedTestCalendar(t, pool, "holidays", false)

	sd := ScheduleYAML{}
	sd.APIVersion = requiredAPIVersion
	sd.Kind = "Schedule"
	sd.Metadata.Name = "weeknights"
	sd.Spec.Cron = "0 0 22 * * 1-5"
	sd.Spec.SkipCalendars = []string{"holidays"}

	resolved, errs := svc.resolveSchedules([]ScheduleYAML{sd})
	if len(errs) > 0 {
		t.Fatalf("resolveSchedules: %v", errs)
	}
	tx, err := pool.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()
	if err := svc.upsertSchedules(context.Background(), tx, resolved, time.Now().UTC().Format(time.RFC3339), "sha1"); err != nil {
		t.Fatalf("upsertSchedules: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var skip sql.NullString
	if err := pool.QueryRow(`SELECT skip_calendars FROM schedules WHERE name='weeknights'`).Scan(&skip); err != nil {
		t.Fatalf("read catalog binding: %v", err)
	}
	if skip.String != `["holidays"]` {
		t.Errorf("catalog skip_calendars = %q, want [\"holidays\"]; an in-app job binding this ref would otherwise lose the policy", skip.String)
	}

	// And the ref expansion copies it down onto the entry.
	entries := mergeScheduleRefs(nil, []string{"weeknights"}, resolved)
	if len(entries) != 1 || len(entries[0].SkipCalendars) != 1 || entries[0].SkipCalendars[0] != "holidays" {
		t.Errorf("expanded entry = %+v, want the binding copied down", entries)
	}
}

// The polarity tag in the content hash: two opposite firing rules must not share
// a digest, while an unbound schedule keeps its pre-feature hash exactly.
func TestScheduleContentHashDistinguishesPolarity(t *testing.T) {
	const preFeature = "sha256:08e61c0459c6b42a23caf850960bc536f2df917f281c38615fea55f30f1089ee"
	if got := scheduleContentHash("0 0 2 * * *", nil, "", "", "", nil, nil); got != preFeature {
		t.Fatalf("unbound hash = %q, want the pre-feature digest %q", got, preFeature)
	}
	skip := scheduleContentHash("0 0 2 * * *", nil, "", "", "", []string{"holidays"}, nil)
	only := scheduleContentHash("0 0 2 * * *", nil, "", "", "", nil, []string{"holidays"})
	if skip == only {
		t.Error(`"never run on holidays" and "only run on holidays" hashed identically — the segments must be polarity-tagged`)
	}
	if skip == preFeature || only == preFeature {
		t.Error("a bound schedule kept the unbound digest")
	}
}

// calendar.ValidateBinding is the one contract; these are the refusals the sync
// path must apply, matching the API's 422 exactly.
func TestSyncCalendarValidationRefusals(t *testing.T) {
	pool := mustOpenDB(t)
	seedTestCalendar(t, pool, "holidays", false)
	seedTestCalendar(t, pool, "freeze", true)

	known, global, err := calendar.LoadFlags(context.Background(), pool)
	if err != nil {
		t.Fatalf("load flags: %v", err)
	}

	for _, tc := range []struct {
		name       string
		skip, only []string
		wantErr    bool
	}{
		{"valid skip", []string{"holidays"}, nil, false},
		{"valid only", nil, []string{"holidays"}, false},
		{"unknown name", []string{"holidayz"}, nil, true},
		{"both roles", []string{"holidays"}, []string{"holidays"}, true},
		{"global as only", nil, []string{"freeze"}, true},
		{"global as skip is fine", []string{"freeze"}, nil, false},
		{"no bindings", nil, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, verr := calendar.ValidateBinding(tc.skip, tc.only, known, global)
			if (verr != "") != tc.wantErr {
				t.Errorf("verr = %q, wantErr = %v", verr, tc.wantErr)
			}
		})
	}
}

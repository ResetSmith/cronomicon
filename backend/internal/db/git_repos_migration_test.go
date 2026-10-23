package db

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/golang-migrate/migrate/v4"
)

// TestMigrate1310GitRepos takes a database as a released version left it,
// with the connection and the sync state in their two singleton tables, and
// checks that 1310 carries all of it onto Global's row of git_repos and stamps
// every Git row with that repository. Then the way back.
//
// It starts from 1210 (2.2.3), from 1280 (2.3) and from 1300 (the migration
// before): the owner's decision of 2026-10-09 is that a migration of 2.4.0 is
// tested from both released lines.
func TestMigrate1310GitRepos(t *testing.T) {
	for _, from := range []uint{1210, 1280, 1300} {
		t.Run(fmt.Sprintf("a saved connection, from %d", from), func(t *testing.T) {
			h := openAt(t, from)
			h.exec(`INSERT INTO gitlab_config (id, base_url, project_path, webhook_secret, branch,
			            pat_enc, bot_name, bot_email, write_branch, repo_url, token_expiry_notify_days,
			            webhook_enabled, webhook_events_push, webhook_events_mr, webhook_events_tag,
			            webhook_secret_enc, webhook_secret_prev_enc, webhook_overlap_until,
			            last_modified_by, last_modified_at)
			        VALUES (1, 'legacy-base', 'legacy/path', 'legacy-clear-secret', 'legacy-branch',
			            'enc-token', 'defs-bot', 'defs-bot@example.com', 'release', 'https://git.example/org/defs.git', 14,
			            1, 1, 0, 1,
			            'enc-secret', 'enc-secret-prev', '2026-10-09T12:00:00Z',
			            'admin@example.com', '2026-10-01T00:00:00Z')`)
			h.exec(`INSERT INTO git_sync_state (id, last_sha, last_synced_at, last_status) VALUES (1, 'abc123', '2026-10-08T00:00:00Z', 'partial')`)
			h.exec(`INSERT INTO jobs(uid, name, source, run_type, synced_at) VALUES('j-git', 'from-git', 'git', 'bash', 't')`)
			h.exec(`INSERT INTO jobs(uid, name, source, run_type, synced_at) VALUES('j-app', 'from-app', 'cronomicon', 'bash', 't')`)
			h.exec(`INSERT INTO workflows(uid, name, source, steps, synced_at) VALUES('w-git', 'flow-git', 'git', '[]', 't')`)
			h.exec(`INSERT INTO workflows(uid, name, source, steps, synced_at) VALUES('w-app', 'flow-app', 'cronomicon', '[]', 't')`)
			h.exec(`INSERT INTO scopes(id, name, source, created_at) VALUES('sc-git', 'scope-git', 'git', 't')`)
			h.exec(`INSERT INTO scopes(id, name, source, created_at) VALUES('sc-app', 'scope-app', 'cronomicon', 't')`)
			h.exec(`INSERT INTO git_sync_events(triggered_by, sha, status, started_at, finished_at) VALUES('webhook', 'abc123', 'success', 't', 't')`)
			h.exec(`INSERT INTO schedule_pushes(at, actor, schedule_file, status, created_at) VALUES('t', 'a@example.com', 'jobs/x.yaml', 'success', 't')`)

			h.to(1310)

			// The connection and the sync state, on one row.
			got := h.str(`SELECT id || '|' || agency_id || '|' || url || '|' || branch || '|' || token_enc || '|' || bot_name || '|' || bot_email
			              || '|' || token_expiry_notify_days || '|' || webhook_enabled || webhook_events_push || webhook_events_mr || webhook_events_tag
			              || '|' || webhook_secret_enc || '|' || webhook_secret_prev_enc || '|' || webhook_overlap_until
			              || '|' || last_sha || '|' || last_synced_at || '|' || last_status
			              || '|' || last_modified_by || '|' || last_modified_at || '|' || COALESCE(created_by, '(none)')
			              FROM git_repos`)
			want := "global|global|https://git.example/org/defs.git|release|enc-token|defs-bot|defs-bot@example.com" +
				"|14|1101|enc-secret|enc-secret-prev|2026-10-09T12:00:00Z|abc123|2026-10-08T00:00:00Z|partial" +
				"|admin@example.com|2026-10-01T00:00:00Z|(none)"
			if got != want {
				t.Errorf("Global's repository row:\n got %s\nwant %s", got, want)
			}
			if n := h.count(`SELECT COUNT(*) FROM git_repos`); n != 1 {
				t.Errorf("git_repos rows = %d, want Global's alone", n)
			}
			for _, gone := range []string{"gitlab_config", "git_sync_state"} {
				if n := h.count(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, gone); n != 0 {
					t.Errorf("the table %s is still there", gone)
				}
			}
			// Every Git row says which repository; a row built in the app says none.
			for _, c := range []struct{ q, want string }{
				{`SELECT COALESCE(repo_id,'(none)') FROM jobs WHERE uid='j-git'`, "global"},
				{`SELECT COALESCE(repo_id,'(none)') FROM jobs WHERE uid='j-app'`, "(none)"},
				{`SELECT COALESCE(repo_id,'(none)') FROM workflows WHERE uid='w-git'`, "global"},
				{`SELECT COALESCE(repo_id,'(none)') FROM workflows WHERE uid='w-app'`, "(none)"},
				{`SELECT COALESCE(repo_id,'(none)') FROM scopes WHERE id='sc-git'`, "global"},
				{`SELECT COALESCE(repo_id,'(none)') FROM scopes WHERE id='sc-app'`, "(none)"},
				{`SELECT repo_id FROM git_sync_events`, "global"},
				{`SELECT repo_id FROM schedule_pushes`, "global"},
			} {
				if got := h.str(c.q); got != c.want {
					t.Errorf("%s = %q, want %q", c.q, got, c.want)
				}
			}
			if n := h.count(`SELECT COUNT(*) FROM pragma_table_info('runs') WHERE name='checkout_repo'`); n != 1 {
				t.Errorf("runs.checkout_repo is not there")
			}

			// And back: the two singletons, with what the row held.
			h.to(1300)
			back := h.str(`SELECT repo_url || '|' || write_branch || '|' || pat_enc || '|' || bot_name || '|' || bot_email
			               || '|' || token_expiry_notify_days || '|' || webhook_enabled || webhook_events_push || webhook_events_mr || webhook_events_tag
			               || '|' || webhook_secret_enc || '|' || webhook_secret_prev_enc || '|' || webhook_overlap_until
			               || '|' || last_modified_by || '|' || last_modified_at
			               FROM gitlab_config WHERE id=1`)
			wantBack := "https://git.example/org/defs.git|release|enc-token|defs-bot|defs-bot@example.com" +
				"|14|1101|enc-secret|enc-secret-prev|2026-10-09T12:00:00Z|admin@example.com|2026-10-01T00:00:00Z"
			if back != wantBack {
				t.Errorf("gitlab_config after the way back:\n got %s\nwant %s", back, wantBack)
			}
			if st := h.str(`SELECT last_sha || '|' || last_synced_at || '|' || last_status FROM git_sync_state WHERE id=1`); st != "abc123|2026-10-08T00:00:00Z|partial" {
				t.Errorf("git_sync_state after the way back = %q", st)
			}
			for _, tc := range [][2]string{{"jobs", "repo_id"}, {"workflows", "repo_id"}, {"scopes", "repo_id"},
				{"git_sync_events", "repo_id"}, {"schedule_pushes", "repo_id"}, {"runs", "checkout_repo"}} {
				if n := h.count(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name=?`, tc[0], tc[1]); n != 0 {
					t.Errorf("%s.%s is still there after the way back", tc[0], tc[1])
				}
			}
			if n := h.count(`SELECT COUNT(*) FROM jobs`) + h.count(`SELECT COUNT(*) FROM workflows`) + h.count(`SELECT COUNT(*) FROM scopes`); n != 6 {
				t.Errorf("definitions after the way back = %d, want the six there were", n)
			}
			// And up again: the same row.
			h.to(1310)
			if again := h.str(`SELECT url || '|' || branch || '|' || token_enc || '|' || last_sha FROM git_repos WHERE id='global'`); again != "https://git.example/org/defs.git|release|enc-token|abc123" {
				t.Errorf("Global's row after down and up = %q", again)
			}
		})
	}

	// An installation configured by environment alone, or not configured at
	// all, has a row in NEITHER singleton. Global's row is written all the
	// same, with the webhook on: the policy read "no row" as enabled, and a row
	// with the flags off would refuse every delivery after the upgrade.
	t.Run("no connection was ever saved", func(t *testing.T) {
		h := openAt(t, 1300)
		h.to(1310)
		got := h.str(`SELECT id || '|' || agency_id || '|' || url || '|' || branch || '|' || COALESCE(token_enc,'(none)')
		              || '|' || webhook_enabled || webhook_events_push || webhook_events_mr || webhook_events_tag
		              || '|' || COALESCE(webhook_secret_enc,'(none)') || '|' || COALESCE(last_sha,'(none)') || '|' || COALESCE(last_status,'(none)')
		              FROM git_repos`)
		if want := "global|global||main|(none)|1111|(none)|(none)|(none)"; got != want {
			t.Errorf("Global's row on an installation that never saved a connection:\n got %s\nwant %s", got, want)
		}
		h.to(1300)
		if n := h.count(`SELECT COUNT(*) FROM git_sync_state`); n != 0 {
			t.Errorf("the way back wrote a sync state where no sync had recorded one (%d rows)", n)
		}
	})

	// A sync state with no connection row (configured by environment, and
	// synced): the state is carried.
	t.Run("a sync state and no connection row", func(t *testing.T) {
		h := openAt(t, 1300)
		h.exec(`INSERT INTO git_sync_state (id, last_sha, last_synced_at, last_status) VALUES (1, 'feed42', 't', 'success')`)
		h.to(1310)
		if got := h.str(`SELECT url || '|' || webhook_enabled || '|' || last_sha || '|' || last_status FROM git_repos WHERE id='global'`); got != "|1|feed42|success" {
			t.Errorf("Global's row = %q, want no URL, the webhook on and the sync state carried", got)
		}
	})

	// A connection row that has the webhook OFF is carried as it is. (That
	// includes the row a first rotation of the webhook secret created with the
	// flags at 0, present defect 16: the upgrade cannot tell it from an
	// operator's choice, and does not guess.)
	t.Run("a webhook that is off stays off", func(t *testing.T) {
		h := openAt(t, 1300)
		h.exec(`INSERT INTO gitlab_config (id, webhook_secret_enc) VALUES (1, 'enc-secret')`)
		h.to(1310)
		if got := h.str(`SELECT webhook_enabled || webhook_events_push || webhook_events_mr || webhook_events_tag || '|' || webhook_secret_enc || '|' || branch || '|' || bot_name FROM git_repos WHERE id='global'`); got != "0000|enc-secret|main|cronomicon-bot" {
			t.Errorf("Global's row = %q, want the flags as they were", got)
		}
	})
}

// migrationHarness is a database held at a chosen schema version.
type migrationHarness struct {
	t    *testing.T
	pool *sql.DB
	m    *migrate.Migrate
}

func openAt(t *testing.T, version uint) *migrationHarness {
	t.Helper()
	pool, err := Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	m, err := migrator(pool)
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}
	h := &migrationHarness{t: t, pool: pool, m: m}
	h.to(version)
	return h
}

func (h *migrationHarness) to(version uint) {
	h.t.Helper()
	if err := h.m.Migrate(version); err != nil && err != migrate.ErrNoChange {
		h.t.Fatalf("migrate to %d: %v", version, err)
	}
}

func (h *migrationHarness) exec(q string, args ...any) {
	h.t.Helper()
	if _, err := h.pool.Exec(q, args...); err != nil {
		h.t.Fatalf("exec: %v\n%s", err, q)
	}
}

func (h *migrationHarness) str(q string, args ...any) string {
	h.t.Helper()
	var s sql.NullString
	if err := h.pool.QueryRow(q, args...).Scan(&s); err != nil {
		h.t.Fatalf("query: %v\n%s", err, q)
	}
	return s.String
}

func (h *migrationHarness) count(q string, args ...any) int {
	h.t.Helper()
	var n int
	if err := h.pool.QueryRow(q, args...).Scan(&n); err != nil {
		h.t.Fatalf("query: %v\n%s", err, q)
	}
	return n
}

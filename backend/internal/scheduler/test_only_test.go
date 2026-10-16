package scheduler

import (
	"context"
	"database/sql"
)

// Test-only since 2.4.2: nothing in the server calls these any more (the review of 2.4.x).

// IsPaused is the DB-level twin of (*Scheduler).isPaused, exported for trigger
// paths that live outside this package (ET-D's file-arrival watcher).
//
// Nothing downstream of Enqueue consults paused_jobs — the pause is checked by
// each trigger path, one gate per producer (see the reaction.go note). A new
// producer that skips this check fires paused jobs.
func IsPaused(ctx context.Context, database *sql.DB, source, ownerKind, name string) bool {
	return IsPausedUID(ctx, database, source, ownerKind, name, "")
}

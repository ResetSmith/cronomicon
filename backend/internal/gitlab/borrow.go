package gitlab

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"maps"

	"github.com/ResetSmith/cronomicon/internal/calendar"
	"github.com/ResetSmith/cronomicon/internal/repoid"
)

// References resolve in the definition's own repository, then in Global's
// (2.4.0, GR-16).
//
// A job names its script and its schedules; a reaction names what it watches.
// Until 2.4.0 there was one repository to find them in. With a repository per
// agency, an agency's job may use what the installation's own repository
// supplies to everyone: a script, a schedule, a definition to react to. It
// may not use another agency's.
//
// "Then in Global's" means: when this repository HAS NO FILE of that name. A
// repository that has a script `deploy` which does not validate does not get
// Global's `deploy` in its place: the job is told its script is broken, and
// runs nothing, rather than running a body its author did not mean.
//
// What is found in Global's repository is read from the tables as Global's
// last sync left them. Global's next sync carries its edits to the
// definitions that use it (upsertScripts, upsertSchedules).

// borrowing is what the definitions of the sync that is under way take from
// Global's repository. For Global's own sync it finds nothing.
type borrowing struct {
	s   *Service
	ctx context.Context
	// The names this repository has a file for, whether or not it validated.
	ownScripts, ownScheds map[string]bool
	scripts               map[string]resolvedScript
	scheds                map[string]resolvedSchedule
	// Names already asked for and not found.
	noScript, noSched map[string]bool
}

func (s *Service) newBorrowing(ctx context.Context, scripts []ScriptYAML, scheds []ScheduleYAML) *borrowing {
	b := &borrowing{
		s: s, ctx: ctx,
		ownScripts: map[string]bool{}, ownScheds: map[string]bool{},
		scripts: map[string]resolvedScript{}, scheds: map[string]resolvedSchedule{},
		noScript: map[string]bool{}, noSched: map[string]bool{},
	}
	for _, sc := range scripts {
		b.ownScripts[sc.Metadata.Name] = true
	}
	for _, sd := range scheds {
		b.ownScheds[sd.Metadata.Name] = true
	}
	return b
}

// script is Global's script of a name, when this repository has no file of
// that name and Global's repository has one.
func (b *borrowing) script(name string) (resolvedScript, bool) {
	if b == nil || b.s.repo() == repoid.Global || name == "" || b.ownScripts[name] || b.noScript[name] {
		return resolvedScript{}, false
	}
	if rs, ok := b.scripts[name]; ok {
		return rs, true
	}
	var rs resolvedScript
	err := b.s.db.QueryRowContext(b.ctx, `
		SELECT uid, run_type, COALESCE(command,''), COALESCE(script,''), COALESCE(script_path,''),
		       COALESCE(executor,''), COALESCE(description,''), COALESCE(content_hash,''),
		       COALESCE(source_path,''), COALESCE(project_root,''),
		       COALESCE(warnings,'[]'), COALESCE(variables,'[]'), COALESCE(prompts_json,'[]')
		  FROM scripts WHERE repo_id = ? AND name = ?`, repoid.Global, name).
		Scan(&rs.uid, &rs.runType, &rs.command, &rs.script, &rs.scriptPath, &rs.executor, &rs.description,
			&rs.contentHash, &rs.sourcePath, &rs.projectRoot, &rs.warnings, &rs.variables, &rs.prompts)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			b.s.logError("git sync: read the installation's own script of a name", "script", name, "err", err)
		}
		b.noScript[name] = true
		return resolvedScript{}, false
	}
	b.scripts[name] = rs
	return rs, true
}

// schedule is Global's schedule of a name, on the same terms.
func (b *borrowing) schedule(name string) (resolvedSchedule, bool) {
	if b == nil || b.s.repo() == repoid.Global || name == "" || b.ownScheds[name] || b.noSched[name] {
		return resolvedSchedule{}, false
	}
	if rs, ok := b.scheds[name]; ok {
		return rs, true
	}
	var rs resolvedSchedule
	var env, skip, only string
	err := b.s.db.QueryRowContext(b.ctx, `
		SELECT COALESCE(uid,''), COALESCE(cron,''), COALESCE(env,''), COALESCE(description,''),
		       COALESCE(content_hash,''), COALESCE(source_path,''), COALESCE(start_at,''), COALESCE(end_at,''),
		       COALESCE(interval,''), COALESCE(skip_calendars,''), COALESCE(only_calendars,'')
		  FROM schedules WHERE source = 'git' AND repo_id = ? AND name = ? AND deleted_at IS NULL`, repoid.Global, name).
		Scan(&rs.uid, &rs.cron, &env, &rs.description, &rs.contentHash, &rs.sourcePath, &rs.startAt, &rs.endAt,
			&rs.interval, &skip, &only)
	if err != nil || rs.uid == "" {
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			b.s.logError("git sync: read the installation's own schedule of a name", "schedule", name, "err", err)
		}
		b.noSched[name] = true
		return resolvedSchedule{}, false
	}
	if env != "" {
		_ = json.Unmarshal([]byte(env), &rs.env)
	}
	rs.skipCals, rs.onlyCals = calendar.ParseNames(skip), calendar.ParseNames(only)
	b.scheds[name] = rs
	return rs, true
}

// withScripts is the repository's own scripts together with the ones its jobs
// take from Global's. The repository's own are never shadowed: only a name it
// has no file for is ever borrowed.
func (b *borrowing) withScripts(own map[string]resolvedScript) map[string]resolvedScript {
	if b == nil || len(b.scripts) == 0 {
		return own
	}
	out := maps.Clone(b.scripts)
	maps.Copy(out, own)
	return out
}

func (b *borrowing) withScheds(own map[string]resolvedSchedule) map[string]resolvedSchedule {
	if b == nil || len(b.scheds) == 0 {
		return own
	}
	out := maps.Clone(b.scheds)
	maps.Copy(out, own)
	return out
}

// globalHas reports whether Global's repository supplies a job or a workflow
// of a name: what a reaction in an agency's repository may watch when its own
// repository has none.
func (s *Service) globalHas(ctx context.Context, kind, name string) bool {
	if s.repo() == repoid.Global {
		return false
	}
	var one int
	//nolint:gosec // the table is one of two literals
	err := s.db.QueryRowContext(ctx,
		"SELECT 1 FROM "+ownerTable(kind)+" WHERE source = 'git' AND repo_id = ? AND name = ?", repoid.Global, name).Scan(&one)
	return err == nil
}

// otherRepositories reports whether any repository but Global's is connected.
func otherRepositories(ctx context.Context, tx *sql.Tx) bool {
	var n int
	_ = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM git_repos WHERE id <> ?`, repoid.Global).Scan(&n)
	return n > 0
}

// jobsAgencySQL is the one agency of an in-app job, as a scalar subquery over
// the row `jobs`: its scope's agency when the scope has exactly one, and NULL
// otherwise (no scope, or a scope from before 2.3.0 that several share).
const jobsAgencySQL = `(SELECT CASE WHEN COUNT(*) = 1 THEN MIN(sa.agency_id) END
                         FROM scope_agencies sa JOIN scopes sc ON sc.id = sa.scope_id
                        WHERE sc.name = jobs.scope)`

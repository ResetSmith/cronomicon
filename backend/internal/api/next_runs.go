package api

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/ResetSmith/cronomicon/internal/cronutil"
)

// nextRunOwner is one row of the jobs or workflows list whose Next Run the
// batch projects: its identity, and whether it is paused (a paused row projects
// none, as the detail path).
type nextRunOwner struct {
	uid, source, name string
	paused            bool
}

// key is what the batch's answer is filed under: the uid, or for a row with
// none its (source, name).
func (o nextRunOwner) key() string {
	if o.uid != "" {
		return o.uid
	}
	return o.source + "\x00" + o.name
}

// batchNextRuns projects the Next Run of each owner of one kind ("job" or
// "workflow") from its definition_schedules entries, in ONE query for the page
// (CC.9), drained before anything else runs (the SQLite pool rule). Each entry
// is projected through the full Spec (cron, interval, window: FX-F1), and an
// owner's Next Run is the earliest of its entries' (earliestNextRunAt).
//
// An entry is its owner's by owner_uid, as the scheduler joins it; one that
// records no uid (from before 1050) by its owner's (source, name), as the
// paused join beside these lists does. Until 2.5.0 C12 both lists matched
// every entry by (source, name), so FIN's `backup` showed TAX's next fire,
// and a paused TAX `backup` hid FIN's (present defect 98, question 118).
func (s *Server) batchNextRuns(ctx context.Context, kind string, owners []nextRunOwner) map[string]*string {
	out := map[string]*string{}
	if len(owners) == 0 {
		return out
	}
	uids := make([]any, 0, len(owners))
	names := make([]any, 0, len(owners))
	seenName := map[string]bool{}
	for _, o := range owners {
		if o.uid != "" {
			uids = append(uids, o.uid)
		}
		if !seenName[o.name] {
			seenName[o.name] = true
			names = append(names, o.name)
		}
	}
	uidIn := "''"
	if len(uids) > 0 {
		uidIn = strings.Repeat(",?", len(uids))[1:]
	}
	args := append(append([]any{kind}, uids...), names...)
	rows, err := s.db.QueryContext(ctx, `
		SELECT COALESCE(owner_uid,''), owner_source, owner_name, cron,
		       COALESCE(interval,''), COALESCE(start_at,''), COALESCE(end_at,'')
		  FROM definition_schedules
		 WHERE owner_kind = ?
		   AND (owner_uid IN (`+uidIn+`)
		        OR (owner_uid IS NULL AND owner_name IN (`+strings.Repeat(",?", len(names))[1:]+`)))
		 ORDER BY owner_source, owner_name, position, name`, args...)
	if err != nil {
		// The projection degrades to "no next run" for the page rather than
		// failing the list; it reads as "nothing scheduled", so say so.
		s.log.Warn(kind+"s list: batch next-run query failed; page shows no projections", "err", err)
		return out
	}
	type entrySpec struct{ cron, interval, startAt, endAt string }
	byUID := map[string][]entrySpec{}
	byName := map[string][]entrySpec{} // source NUL name, entries with no uid
	for rows.Next() {
		var uid, src, nm string
		var e entrySpec
		if rows.Scan(&uid, &src, &nm, &e.cron, &e.interval, &e.startAt, &e.endAt) != nil {
			continue
		}
		if uid != "" {
			byUID[uid] = append(byUID[uid], e)
		} else {
			byName[src+"\x00"+nm] = append(byName[src+"\x00"+nm], e)
		}
	}
	rows.Close() // drain BEFORE any further queries (SQLite pool rule)

	now := time.Now()
	appLoc := s.appLocation()
	for _, o := range owners {
		if o.paused {
			continue // matches the detail path: a paused owner projects no next run
		}
		es := byName[o.source+"\x00"+o.name]
		if o.uid != "" {
			es = append(append([]entrySpec{}, byUID[o.uid]...), es...)
		}
		if len(es) == 0 {
			continue
		}
		entries := make([]scheduleEntryResp, 0, len(es))
		for _, e := range es {
			resp := scheduleEntryResp{}
			win := cronutil.NewWindow(
				windowBound(sql.NullString{String: e.startAt, Valid: e.startAt != ""}),
				windowBound(sql.NullString{String: e.endAt, Valid: e.endAt != ""}))
			spec := cronutil.Spec{Cron: e.cron, Interval: e.interval, Window: win}
			if next, ok := cronutil.NextSpec(spec, now, appLoc); ok {
				resp.NextRunAt = rfc3339Ptr(next)
			}
			entries = append(entries, resp)
		}
		out[o.key()] = earliestNextRunAt(entries)
	}
	return out
}

package gitlab

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"github.com/ResetSmith/cronomicon/internal/notices"
	"github.com/ResetSmith/cronomicon/internal/repoid"
)

// A script or a schedule that another repository still uses is not pruned
// (2.4.0, GR-17).
//
// An agency's job may use a script or a schedule of the installation's own
// repository (GR-16). When its file leaves that repository, deleting the row
// would take the body, or the timing, from under a job of a repository whose
// committers did nothing: the job's next sync would find its name resolving
// to nothing and drop it. The row is kept instead, as it was when its file
// left, and the agency that uses it is told, with which of its definitions
// those are and what to do. It goes at the first sync of its own repository at
// which nothing of another repository's uses it. (The precedent is the scope
// that is bound to runners and has runs waiting: a prune that waits.)
//
// "Uses" is a Git definition of ANOTHER repository joined to the row by its
// uid. A definition built in the app is not held to this: what happens to it
// when its script or schedule leaves Git is older than this rule and is its
// own (a script: the job keeps its copy and is joined again if the name
// returns; a schedule: it goes on firing, and the inbox says so).

// usedElsewhere, for each kind, is the predicate "a Git definition of a
// repository other than ?  is joined to this row", over the row's uid. Each
// takes the syncing repository's id, the schedules' twice.
const (
	scriptUsedElsewhere = `uid IN (SELECT j.script_uid FROM jobs j
	                                 WHERE j.source = 'git' AND j.script_uid IS NOT NULL
	                                   AND j.repo_id IS NOT NULL AND j.repo_id <> ?)`
	scheduleUsedElsewhere = `uid IN (SELECT d.schedule_uid FROM definition_schedules d
	                                   WHERE d.schedule_uid IS NOT NULL AND d.owner_uid IN (
	                                         SELECT uid FROM jobs      WHERE source = 'git' AND repo_id IS NOT NULL AND repo_id <> ?
	                                         UNION ALL
	                                         SELECT uid FROM workflows WHERE source = 'git' AND repo_id IS NOT NULL AND repo_id <> ?))`
)

// deferredUse is one repository's use of one row whose prune is deferred.
type deferredUse struct {
	kind   string // script | schedule
	uid    string
	name   string
	repo   string
	agency string
	users  []string // that repository's definitions that use it
}

func (d deferredUse) subject() string { return d.kind + ":" + d.uid + ":" + d.repo }

// noteDeferredPrunes keeps the inbox in step with the prunes this sync
// deferred, in the sync's transaction: one notice for each repository that
// still uses a row whose file has left, filed under that repository's agency
// and naming only that repository's definitions.
//
// It is Global's sync that defers (only Global's rows are used by another
// repository), so that is where a notice is opened, and resolved once the row
// has gone or is used no more. An agency's sync resolves its own: the notice
// should not wait for Global's next sync after the agency did what it asks.
//
// scriptsOK and schedsOK say whether this sync pruned the kind at all. A kind
// whose prune was skipped (a file of it did not parse) has stale rows that
// were not "removed", and nothing is said about them either way.
func (s *Service) noteDeferredPrunes(ctx context.Context, tx *sql.Tx, now string, scriptsOK, schedsOK bool) error {
	open, err := openDeferredNotices(ctx, tx)
	if err != nil {
		return err
	}
	if s.repo() != repoid.Global {
		// Its own notices: resolved when the repository no longer uses the row.
		for _, subject := range open {
			kind, uid, repo, ok := splitDeferredSubject(subject)
			if !ok || repo != s.repo() {
				continue
			}
			uses, err := s.stillUses(ctx, tx, kind, uid)
			if err != nil {
				return err
			}
			if !uses {
				if err := notices.Resolve(ctx, tx, notices.KindPruneDeferred, subject); err != nil {
					return err
				}
			}
		}
		return nil
	}

	var standing []deferredUse
	if scriptsOK {
		uses, err := deferredUses(ctx, tx, "script", `
			SELECT sc.uid, sc.name, j.repo_id, g.agency_id, j.name
			  FROM scripts sc
			  JOIN jobs j      ON j.script_uid = sc.uid AND j.source = 'git' AND j.repo_id IS NOT NULL AND j.repo_id <> ?1
			  JOIN git_repos g ON g.id = j.repo_id
			 WHERE sc.repo_id = ?1 AND sc.synced_at < ?2 AND sc.source_path LIKE 'scripts/%'`, s.repo(), now)
		if err != nil {
			return err
		}
		standing = append(standing, uses...)
	}
	if schedsOK {
		uses, err := deferredUses(ctx, tx, "schedule", `
			SELECT sd.uid, sd.name, o.repo_id, g.agency_id, o.kind || ' ' || o.name
			  FROM schedules sd
			  JOIN definition_schedules d ON d.schedule_uid = sd.uid
			  JOIN (SELECT uid, repo_id, name, 'job' AS kind      FROM jobs      WHERE source = 'git'
			        UNION ALL
			        SELECT uid, repo_id, name, 'workflow' AS kind FROM workflows WHERE source = 'git') o
			       ON o.uid = d.owner_uid AND o.repo_id IS NOT NULL AND o.repo_id <> ?1
			  JOIN git_repos g ON g.id = o.repo_id
			 WHERE sd.source = 'git' AND sd.repo_id = ?1 AND sd.synced_at < ?2`, s.repo(), now)
		if err != nil {
			return err
		}
		standing = append(standing, uses...)
	}
	still := map[string]bool{}
	for _, d := range standing {
		still[d.subject()] = true
		what, own := "script", "a script of its own of that name, or point them at another"
		if d.kind == "schedule" {
			what, own = "schedule", "a schedule of its own of that name, or give them another timing"
		}
		detail := fmt.Sprintf("The %s %s was removed from the installation's own repository, and this agency's repository still uses it: %s. "+
			"It is kept as it was when it was removed, and they go on using it, until none of them names it. "+
			"Give this agency's repository %s. This notice clears when it is done.",
			what, d.name, strings.Join(d.users, ", "), own)
		if err := notices.Upsert(ctx, tx, notices.KindPruneDeferred, notices.Finding{
			AgencyID: d.agency, Subject: d.subject(), Detail: detail,
		}); err != nil {
			return err
		}
	}
	for _, subject := range open {
		kind, _, _, ok := splitDeferredSubject(subject)
		if !ok || still[subject] {
			continue
		}
		if (kind == "script" && !scriptsOK) || (kind == "schedule" && !schedsOK) {
			continue
		}
		if err := notices.Resolve(ctx, tx, notices.KindPruneDeferred, subject); err != nil {
			return err
		}
	}
	return nil
}

// deferredUses reads one kind's deferred rows and who uses them. The query
// yields a row per (deferred row, user): uid, name, the user's repository and
// its agency, and the user's name.
func deferredUses(ctx context.Context, tx *sql.Tx, kind, query string, args ...any) ([]deferredUse, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byKey := map[string]*deferredUse{}
	var order []string
	for rows.Next() {
		var uid, name, repo, agency, user string
		if err := rows.Scan(&uid, &name, &repo, &agency, &user); err != nil {
			return nil, err
		}
		key := uid + ":" + repo
		d, seen := byKey[key]
		if !seen {
			d = &deferredUse{kind: kind, uid: uid, name: name, repo: repo, agency: agency}
			byKey[key] = d
			order = append(order, key)
		}
		d.users = append(d.users, user)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]deferredUse, 0, len(order))
	for _, key := range order {
		d := byKey[key]
		sort.Strings(d.users)
		d.users = compactStrings(d.users)
		out = append(out, *d)
	}
	return out, nil
}

// compactStrings drops adjacent repeats from a sorted slice.
func compactStrings(in []string) []string {
	out := in[:0]
	for i, v := range in {
		if i == 0 || v != in[i-1] {
			out = append(out, v)
		}
	}
	return out
}

func openDeferredNotices(ctx context.Context, tx *sql.Tx) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT subject FROM notices WHERE kind = ? AND resolved_at IS NULL`, notices.KindPruneDeferred)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var subject string
		if err := rows.Scan(&subject); err != nil {
			return nil, err
		}
		out = append(out, subject)
	}
	return out, rows.Err()
}

// splitDeferredSubject takes a notice's subject apart: "<kind>:<uid>:<repo>".
func splitDeferredSubject(subject string) (kind, uid, repo string, ok bool) {
	parts := strings.SplitN(subject, ":", 3)
	if len(parts) != 3 || (parts[0] != "script" && parts[0] != "schedule") {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}

// stillUses reports whether a Git definition of this repository is joined to
// the script or the schedule of a uid.
func (s *Service) stillUses(ctx context.Context, tx *sql.Tx, kind, uid string) (bool, error) {
	q := `SELECT EXISTS (SELECT 1 FROM jobs WHERE source = 'git' AND repo_id = ?1 AND script_uid = ?2)`
	if kind == "schedule" {
		q = `SELECT EXISTS (SELECT 1 FROM definition_schedules d WHERE d.schedule_uid = ?2 AND d.owner_uid IN (
		         SELECT uid FROM jobs      WHERE source = 'git' AND repo_id = ?1
		         UNION ALL
		         SELECT uid FROM workflows WHERE source = 'git' AND repo_id = ?1))`
	}
	var uses bool
	err := tx.QueryRowContext(ctx, q, s.repo(), uid).Scan(&uses)
	return uses, err
}

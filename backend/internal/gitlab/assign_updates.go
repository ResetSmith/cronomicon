package gitlab

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"

	"github.com/ResetSmith/cronomicon/internal/notices"
)

// "An update is available" (2.5.0 C10.3; question 93).
//
// Each assignment's record names, for every file it wrote, the origin's hash
// at the commit it was copied from and the hash of what was committed. A file
// of the origin's repository that is not that any more (changed, or removed)
// is an update the target agency has not taken; a file of the agency's own
// repository that is not what was committed is the agency's change. One notice
// per assignment (per target agency and origin definition), filed under the
// target agency, naming both. It is resolved when the origin's files are as
// they were copied again (an update was assigned, which replaces the record,
// or the origin went back), and when the agency has removed its copy or the
// origin its definition, whose record then goes.
//
// The check runs after every sync (of either side) and when the Notices page
// is opened. An assignment whose commit the agency's last sync has not reached
// yet is not judged: its notice, if it has one, is left as it is.

// CheckAssignmentUpdates reconciles the notices of every assignment.
func (r *Registry) CheckAssignmentUpdates(ctx context.Context) error {
	rows, err := r.db.QueryContext(ctx, `
		SELECT a.id, a.kind, a.origin_agency, a.origin_repo, a.origin_uid, a.origin_name, a.origin_path,
		       a.target_agency, a.target_repo, a.target_path, a.commit_sha,
		       COALESCE(oa.name, a.origin_agency), COALESCE(ta.name, a.target_agency)
		  FROM assignments a
		  LEFT JOIN agencies oa ON oa.id = a.origin_agency
		  LEFT JOIN agencies ta ON ta.id = a.target_agency
		 ORDER BY a.created_at`)
	if err != nil {
		return err
	}
	type assignment struct {
		id, kind, originAgency, originRepo, originUID, name, originPath      string
		targetAgency, targetRepo, targetPath, commit, originName, targetName string
	}
	var all []assignment
	for rows.Next() {
		var a assignment
		if err := rows.Scan(&a.id, &a.kind, &a.originAgency, &a.originRepo, &a.originUID, &a.name, &a.originPath,
			&a.targetAgency, &a.targetRepo, &a.targetPath, &a.commit, &a.originName, &a.targetName); err != nil {
			rows.Close()
			return err
		}
		all = append(all, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	lastSHA := map[string]string{}
	shaOf := func(repo string) string {
		if v, ok := lastSHA[repo]; ok {
			return v
		}
		var sha string
		_ = r.db.QueryRowContext(ctx, `SELECT COALESCE(last_sha, '') FROM git_repos WHERE id = ?`, repo).Scan(&sha)
		lastSHA[repo] = sha
		return sha
	}

	var findings []notices.Finding
	for _, a := range all {
		subject := a.targetAgency + ":" + a.originUID
		originSHA, targetSHA := shaOf(a.originRepo), shaOf(a.targetRepo)
		originDir, targetDir := r.cloneDir(a.originRepo), r.cloneDir(a.targetRepo)
		if originSHA == "" || targetSHA == "" || !commitReaches(targetDir, targetSHA, a.commit) {
			// Not judged now: what it said stands.
			if f, ok := openNotice(ctx, r.db, subject); ok {
				findings = append(findings, f)
			}
			continue
		}
		_, targetErr := ReadCommitTree(targetDir, targetSHA, a.targetPath)
		_, originErr := ReadCommitTree(originDir, originSHA, a.originPath)
		if targetErr != nil || originErr != nil {
			// The agency has removed its copy of the definition, or the origin
			// no longer has it (there is nothing to update from: the copy is
			// the agency's own now). Nothing is followed any more.
			if _, err := r.db.ExecContext(ctx, `DELETE FROM assignments WHERE id = ?`, a.id); err != nil {
				return err
			}
			continue
		}
		files, err := r.db.QueryContext(ctx, `
			SELECT target_path, origin_path, origin_hash, committed_hash FROM assignment_files
			 WHERE assignment_id = ? ORDER BY target_path`, a.id)
		if err != nil {
			return err
		}
		var originMoved, agencyMoved []string
		for files.Next() {
			var tp, op, oh, ch string
			if err := files.Scan(&tp, &op, &oh, &ch); err != nil {
				files.Close()
				return err
			}
			moved := false
			if now, err := ReadCommitTree(originDir, originSHA, op); err != nil || len(now) != 1 {
				originMoved, moved = append(originMoved, op+" (removed)"), true
			} else if fileHash(now[0].Content) != oh {
				originMoved, moved = append(originMoved, op), true
			}
			// The agency's change matters where the origin's does too: that is
			// the file an update refuses. Its change to a file the origin left
			// alone is its own, and an update leaves it alone too.
			if !moved {
				continue
			}
			if theirs, err := ReadCommitTree(targetDir, targetSHA, tp); err != nil || len(theirs) != 1 {
				agencyMoved = append(agencyMoved, tp+" (removed)")
			} else if fileHash(theirs[0].Content) != ch {
				agencyMoved = append(agencyMoved, tp)
			}
		}
		files.Close()
		if err := files.Err(); err != nil {
			return err
		}
		if len(originMoved) == 0 {
			continue
		}
		detail := fmt.Sprintf("The %s %s was assigned to %s from %s's repository (%s, commit %.12s). It has changed there since: %s.",
			a.kind, a.name, a.targetName, a.originName, a.originPath, a.commit, strings.Join(originMoved, ", "))
		if len(agencyMoved) > 0 {
			detail += fmt.Sprintf(" %s has changed its own copy of %s: an update is refused for those files until the change is reverted.",
				a.targetName, strings.Join(agencyMoved, ", "))
		} else {
			detail += fmt.Sprintf(" %s has not changed its copy.", a.targetName)
		}
		detail += fmt.Sprintf(" To take the update, a global administrator assigns the %s to %s again.", a.kind, a.targetName)
		findings = append(findings, notices.Finding{AgencyID: a.targetAgency, Subject: subject, Detail: detail})
	}
	return notices.Reconcile(ctx, r.db, notices.KindAssignmentUpdate, findings)
}

// openNotice is the open notice of an assignment, as a finding, to keep it.
func openNotice(ctx context.Context, database *sql.DB, subject string) (notices.Finding, bool) {
	var f notices.Finding
	err := database.QueryRowContext(ctx, `
		SELECT agency_id, subject, detail FROM notices WHERE kind = ? AND subject = ? AND resolved_at IS NULL`,
		notices.KindAssignmentUpdate, subject).Scan(&f.AgencyID, &f.Subject, &f.Detail)
	return f, err == nil
}

// commitReaches reports whether commit is head or one of its ancestors in the
// clone at dir: whether the repository's last sync has the assignment's
// commit.
func commitReaches(dir, head, commit string) bool {
	if head == commit {
		return true
	}
	if !plumbing.IsHash(head) || !plumbing.IsHash(commit) {
		return false
	}
	repo, err := gogit.PlainOpen(dir)
	if err != nil {
		return false
	}
	h, err := repo.CommitObject(plumbing.NewHash(head))
	if err != nil {
		return false
	}
	c, err := repo.CommitObject(plumbing.NewHash(commit))
	if err != nil {
		return false
	}
	ok, err := c.IsAncestor(h)
	return err == nil && ok
}

// checkAssignmentsAfterSync is the update check run after a sync; it logs
// what fails, as the hook has nobody to tell.
func (r *Registry) checkAssignmentsAfterSync(ctx context.Context) {
	if err := r.CheckAssignmentUpdates(ctx); err != nil {
		r.logWarn("assignments: the update check failed", "error", err)
	}
}

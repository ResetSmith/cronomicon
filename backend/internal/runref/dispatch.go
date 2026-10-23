package runref

import (
	"context"
	"database/sql"
	"errors"

	"github.com/ResetSmith/cronomicon/internal/execspec"
)

// The dispatch decision, in one place (2.4.0 Phase R5; the owner's decision of
// 2026-10-13).
//
// A run is produced, waits, is claimed, and is then handed to an agent (the
// manifest) or executed by the server (the local runner). Every producer asks
// whether the run may be made. Things change while it waits: its scope can be
// given to another agency, its job joined to another script, its script
// removed. So the questions that such a change can turn are asked again for
// the run that has just been claimed, before anything of it is resolved: no
// command, target or secret of a refused run is read.
//
// They were asked as separate checks, by the manifest and by the local runner
// each in its own words, and added one at a time as each was found. Both
// independent reviews of this phase found a case that one of the two paths
// asked and the other did not. RefuseDispatch is the list, and both paths call
// it and nothing else.

// DispatchRefusal is why a claimed run is not handed out or executed. Code is
// the manifest's error code; Reason is the sentence the run's log carries and,
// for a run the server executes itself, the run's stored reason.
type DispatchRefusal struct {
	Code   string
	Reason string
}

// The refusals of RefuseDispatch that are not also a producer's.
const (
	// CodeScriptGone: execspec.ErrScriptGone, said before the run is resolved.
	CodeScriptGone   = "script_gone"
	ReasonScriptGone = "the script this job is built on has been removed: build the job on a script that is there"
	// CodeNoCheckoutSHA: a run of a project that was queued with no pinned
	// commit. Checkout mode is chosen by the commit being there, so until
	// 2.4.0 such a run fell through to a body run: the agent was handed the
	// project's entry file alone, without its tree, and ran it. A project
	// job's run always records its entry, and since 2.4.0 its repository; it
	// records no commit when that repository had not been synced when the run
	// was queued.
	CodeNoCheckoutSHA   = "no_checkout_sha"
	ReasonNoCheckoutSHA = "this run is of a project and was queued with no pinned commit: its repository had not been synced. " +
		"Sync the repository and run the job again"
)

// RefuseDispatch asks, of the run traceID as its row stands now, every
// question that decides whether a claimed run may go ahead, and returns the
// first refusal, or nil. In this order:
//
//  1. RepoScopeMismatch: the job, or the script a job built in the app is
//     joined to, comes from an agency's repository, and the run's scope is not
//     that agency's (GR-15; present defect 40).
//  2. StepOutsideWorkflowRepository: the run is a step of a workflow of an
//     agency's repository, and its scope is not that agency's.
//  3. PinOutsideRepository: the run is pinned to an agency's repository, which
//     is where an agent would be sent, and its scope is not that agency's.
//  4. execspec.ScriptGone: the job was built in the app on a script that is no
//     longer there (present defects 41 and 45).
//  5. A run of a project with no pinned commit.
//
// A run that is not there is not refused here: the caller has just read or
// claimed it, and says what it finds.
func RefuseDispatch(ctx context.Context, database *sql.DB, traceID string) (*DispatchRefusal, error) {
	var jobUID, jobSource, scope, checkoutRepo, checkoutSHA, checkoutEntry sql.NullString
	var jobName string
	err := database.QueryRowContext(ctx, `
		SELECT job_name, job_uid, job_source, scope, checkout_repo, checkout_sha, checkout_entry
		  FROM runs WHERE id = ?`, traceID).
		Scan(&jobName, &jobUID, &jobSource, &scope, &checkoutRepo, &checkoutSHA, &checkoutEntry)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	if stranded, err := RepoScopeMismatch(ctx, database, jobUID.String, jobSource.String, jobName, scope.String); err != nil {
		return nil, err
	} else if stranded {
		return &DispatchRefusal{Code: CodeRepoScopeMismatch, Reason: ReasonRepoScopeMismatch}, nil
	}
	if outside, err := StepOutsideWorkflowRepository(ctx, database, traceID, scope.String); err != nil {
		return nil, err
	} else if outside {
		return &DispatchRefusal{Code: CodeRepoScopeMismatch, Reason: ReasonWorkflowScopeMismatch}, nil
	}
	pinAgencies, err := RunAgencies(ctx, database, traceID)
	if err != nil {
		return nil, err
	}
	pinOwner, err := execspec.RunScopeOwner(ctx, database, pinAgencies)
	if err != nil {
		return nil, err
	}
	if outside, err := PinOutsideRepository(ctx, database, checkoutRepo.String, pinOwner, scope.String); err != nil {
		return nil, err
	} else if outside {
		return &DispatchRefusal{Code: CodeRepoScopeMismatch, Reason: ReasonRepoScopeMismatch}, nil
	}
	if gone, err := execspec.ScriptGone(ctx, database,
		execspec.JobRef{UID: jobUID.String, Name: jobName, Source: jobSource.String}); err != nil {
		return nil, err
	} else if gone {
		return &DispatchRefusal{Code: CodeScriptGone, Reason: ReasonScriptGone}, nil
	}
	if checkoutSHA.String == "" && (checkoutEntry.String != "" || checkoutRepo.String != "") {
		return &DispatchRefusal{Code: CodeNoCheckoutSHA, Reason: ReasonNoCheckoutSHA}, nil
	}
	return nil, nil
}

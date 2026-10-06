package execspec

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// The executor resolver (SB band, Phase B).
//
// Every run is frozen onto an executor when it is produced: "ssh", the in-process
// pool that connects from the control plane, or "runner", an out-of-process agent.
// This is the ONE place that decision is made. It used to be two functions with
// the same precedence written twice (api.resolveExecutor and
// scheduler.ResolveExecutor), and they had drifted in three ways this file
// settles:
//
//   - both read jobs.executor by (name, source), so two same-named cronomicon
//     jobs — which R2 allows across agencies — shared one answer. The job is now
//     read by its uid whenever the producer has one;
//   - a global default of ssh fell through to the runner for a run type ssh
//     cannot execute on the automatic paths, but was a 422 on the manual one.
//     It falls through everywhere now: a fleet-wide default is not a choice
//     anyone made about THIS job;
//   - neither knew about the scope, so nothing could say "this scope's jobs run
//     on its bound runners".
//
// Precedence, highest first:
//
//	per-run override > the job's spec.executor > SCOPE BINDING ⇒ runner
//	> the global default > the run type's default (shell ⇒ ssh, else runner)
//
// The binding sits below the two explicit choices and above the two defaults. A
// scope bound to runners (mig. 1180) says its hosts are reached from those
// runners, so a job that expresses no preference runs there; shell jobs default
// to ssh, and without this rung binding a scope would change nothing for most
// of what runs on it. An EXPLICIT ssh on a bound scope is not overridden, it is
// refused: quietly moving a run the operator asked to send from the server is
// wrong, and quietly honouring it bypasses the binding.

const (
	ExecutorSSH    = "ssh"
	ExecutorRunner = "runner"

	// CodeInvalidExecutor is the refusal code for an executor that cannot be
	// used at all: an override outside the enum, or ssh for a run type that
	// needs a local toolchain.
	CodeInvalidExecutor = "invalid_executor"
	// CodeScopeRequiresRunner is the refusal code for an explicit ssh on a scope
	// that is bound to runners.
	CodeScopeRequiresRunner = "scope_requires_runner"
)

// ReasonScopeRequiresRunner is the stored queued_reason for a fire or a step
// refused with CodeScopeRequiresRunner. A SENTENCE, per the scheduler's
// convention (History shows queued_reason verbatim; there is no token→text map
// on the frontend).
const ReasonScopeRequiresRunner = "Skipped: this job's scope is bound to runners, and this job asks for the SSH executor, " +
	"which would run it from the server instead"

// ExecutorQuery is what a producer knows about the run it is about to enqueue.
type ExecutorQuery struct {
	// JobUID is the job's permanent identity. When set it is the ONLY key used
	// to read the job; JobSource/JobName serve producers and legacy rows that
	// have none.
	JobUID    string
	JobSource string // git | cronomicon; empty ⇒ git
	JobName   string
	RunType   string
	// Scope is the run's EFFECTIVE scope name — after any per-run override — not
	// necessarily the job's own.
	Scope string
	// Override is the per-run executor choice: "", "ssh" or "runner". Only the
	// manual and token triggers have one.
	Override string
}

// ExecutorRefusal says why a run may not be produced on the executor it asked
// for. Message is written for the person who asked.
type ExecutorRefusal struct {
	Code    string
	Message string
}

// ExecutorResolution is the resolver's answer.
type ExecutorResolution struct {
	// Executor is the executor to freeze on the run. On a refusal it is what
	// was ASKED for — callers that record a skipped or failed row for the
	// refusal record it against that executor.
	Executor string
	// Refusal is non-nil when the run must not be produced as asked.
	Refusal *ExecutorRefusal
	// Err is a failure to read what the decision depends on. It is reported
	// rather than swallowed because the one wrong guess available — "the scope
	// is not bound" — sends a confined job out from the control plane.
	Err error
}

// ScopeRefused reports whether the refusal is the scope-binding one. The
// automatic producers act on that refusal alone: an ssh executor on a
// toolchain run type has always been enqueued as asked on those paths (and is
// rejected where it is authored), and this change does not alter that.
func (r ExecutorResolution) ScopeRefused() bool {
	return r.Refusal != nil && r.Refusal.Code == CodeScopeRequiresRunner
}

func validExecutor(v string) bool { return v == ExecutorSSH || v == ExecutorRunner }

// ScopeIsBound reports whether a scope (by name) is bound to at least one
// runner. An empty name, or one with no scopes row, is not.
func ScopeIsBound(ctx context.Context, q rowQueryer, scope string) (bool, error) {
	if strings.TrimSpace(scope) == "" {
		return false, nil
	}
	var bound bool
	err := q.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM scope_runners sr JOIN scopes sc ON sc.id = sr.scope_id
		                WHERE sc.name = ?)`, scope).Scan(&bound)
	return bound, err
}

// jobExecutor reads the job's own spec.executor, "" when it sets none.
func jobExecutor(ctx context.Context, q rowQueryer, eq ExecutorQuery) (string, error) {
	var v sql.NullString
	var err error
	if eq.JobUID != "" {
		err = q.QueryRowContext(ctx, `SELECT executor FROM jobs WHERE uid = ?`, eq.JobUID).Scan(&v)
	} else {
		src := eq.JobSource
		if src == "" {
			src = "git"
		}
		err = q.QueryRowContext(ctx,
			`SELECT executor FROM jobs WHERE name = ? AND source = ?`, eq.JobName, src).Scan(&v)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil // a run with no job row (ad-hoc) simply has no job-level choice
	}
	if err != nil {
		return "", err
	}
	if validExecutor(v.String) {
		return v.String, nil
	}
	return "", nil
}

// ResolveExecutor decides the executor for a run. See the file header for the
// precedence and for what a refusal means.
func ResolveExecutor(ctx context.Context, q rowQueryer, eq ExecutorQuery) ExecutorResolution {
	bound, err := ScopeIsBound(ctx, q, eq.Scope)
	if err != nil {
		return ExecutorResolution{Err: fmt.Errorf("read scope binding: %w", err)}
	}
	return ResolveExecutorGiven(ctx, q, eq, bound)
}

// ResolveExecutorGiven is ResolveExecutor with the scope's bound state supplied
// by the caller instead of read. It exists for the bind preview, which has to
// answer "what would each job on this scope resolve to if the scope WERE bound
// (or were not)" through the same rungs the real decision uses — a second copy
// of the precedence written for the preview is how the first two drifted apart.
func ResolveExecutorGiven(ctx context.Context, q rowQueryer, eq ExecutorQuery, bound bool) ExecutorResolution {
	if eq.Override != "" && !validExecutor(eq.Override) {
		return ExecutorResolution{Refusal: &ExecutorRefusal{
			Code:    CodeInvalidExecutor,
			Message: fmt.Sprintf("invalid executor %q (want ssh|runner)", eq.Override),
		}}
	}

	// The two explicit rungs.
	explicit, asked := eq.Override, "this run asks for the ssh executor"
	if explicit == "" {
		je, err := jobExecutor(ctx, q, eq)
		if err != nil {
			return ExecutorResolution{Err: fmt.Errorf("read job executor: %w", err)}
		}
		explicit, asked = je, "this job's executor is set to ssh"
	}

	if explicit != "" {
		// The binding is judged BEFORE the run type. An ssh on a toolchain run
		// type is wrong on any scope, but on a bound one the binding is the
		// reason every producer acts on: the automatic producers do not refuse
		// the run-type mismatch (they never have — it is rejected where it is
		// authored), so reporting that first would let them enqueue an ssh row
		// on a bound scope, kept off the control plane only by the ssh pool's
		// run-type filter.
		if explicit == ExecutorSSH && bound {
			return ExecutorResolution{Executor: explicit, Refusal: &ExecutorRefusal{
				Code: CodeScopeRequiresRunner,
				Message: "scope " + eq.Scope + " is bound to runners, so its jobs run on those runners; " + asked +
					", which would run it from the server instead — drop the ssh choice to run it on the bound runners",
			}}
		}
		if explicit == ExecutorSSH && !SupportedRunType(eq.RunType) {
			return ExecutorResolution{Executor: explicit, Refusal: &ExecutorRefusal{
				Code: CodeInvalidExecutor,
				Message: fmt.Sprintf("run-type %q cannot run over the SSH executor (it needs a local toolchain); "+
					"choose the runner executor", eq.RunType),
			}}
		}
		return ExecutorResolution{Executor: explicit}
	}

	// No explicit choice: the binding decides before any default does.
	if bound {
		return ExecutorResolution{Executor: ExecutorRunner}
	}

	// The global default. An ssh default cannot run a toolchain run type — fall
	// through to the run type's own default rather than enqueue a run the ssh
	// pool will never claim.
	var def sql.NullString
	if err := q.QueryRowContext(ctx,
		`SELECT value FROM settings WHERE key = 'defaultExecutor'`).Scan(&def); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return ExecutorResolution{Err: fmt.Errorf("read default executor: %w", err)}
	}
	if validExecutor(def.String) && !(def.String == ExecutorSSH && !SupportedRunType(eq.RunType)) {
		return ExecutorResolution{Executor: def.String}
	}

	if SupportedRunType(eq.RunType) {
		return ExecutorResolution{Executor: ExecutorSSH}
	}
	return ExecutorResolution{Executor: ExecutorRunner}
}

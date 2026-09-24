package runref

import (
	"context"
	"database/sql"
	"strconv"
)

// KeyBindingsOnSSH returns the declared SSH-key bindings (KindKey) when the
// run's RESOLVED executor is "ssh", and nil otherwise (KB band,
// the ssh-key-refusal plan).
//
// The in-app SSH executor connects FROM amadeus TO the target, so a bound key
// would have to be placed on the TARGET host — a different problem from the
// runner's local memfd delivery (D8), and one that was dropped rather than
// built: it needs a per-host trust flag, a cleanup path for a killed session
// and a Windows story, for a gap the runner path covers and a Secret binding
// works around. Until this check the executor warned and skipped the key, so
// a run started with an input the system already knew it would not provide.
//
// Every run producer calls this beside UnboundRunBlocked, with the executor it
// has just resolved — the executor is decided at enqueue (per-run override →
// spec.executor → global default → capability), never at compose, which is why
// the refusal lives here and not on the bindings write. The check is
// unconditional across origins and scopes: unlike the unbound probe it does
// not depend on ownership, only on where the run will execute.
func KeyBindingsOnSSH(ctx context.Context, database *sql.DB, owners []Owner, executor string) ([]Binding, error) {
	if executor != "ssh" {
		return nil, nil
	}
	var keys []Binding
	for _, o := range owners {
		bs, err := ListBindings(ctx, database, o)
		if err != nil {
			return nil, err
		}
		for _, b := range bs {
			if b.Kind == KindKey {
				keys = append(keys, b)
			}
		}
	}
	return keys, nil
}

// ReasonKeyBindingOnSSH is the stored queued_reason for a fire refused by
// KeyBindingsOnSSH. A SENTENCE, per the scheduler's convention (reasonJobPaused,
// reasonConcurrencyCap): History shows queued_reason verbatim, there is no
// token→text map on the frontend.
const ReasonKeyBindingOnSSH = "Skipped: this job binds an SSH key, which only a runner can deliver; this run resolved to the SSH executor"

// CodeKeyBindingOnSSH is the API error code (422) the manual and token triggers
// return for the same refusal.
const CodeKeyBindingOnSSH = "key_binding_requires_runner"

// KeyBindingRefusal is the message for a KeyBindingsOnSSH refusal: it names the
// first key by its REFERENCE (the alias when one was declared — RA-5 — since
// that is the name the job body reads) and says what to do instead.
func KeyBindingRefusal(keys []Binding) string {
	if len(keys) == 0 {
		return ""
	}
	more := ""
	if len(keys) > 1 {
		more = " (and " + strconv.Itoa(len(keys)-1) + " more)"
	}
	return "this job binds SSH key " + keys[0].InjectReference() + more +
		", which only a runner can deliver; this run resolved to the ssh executor — " +
		"run it on a runner, or bind the key as a Secret and write the file in the job body"
}

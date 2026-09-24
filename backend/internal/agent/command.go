package agent

import (
	"maps"

	"github.com/ResetSmith/cronomicon/internal/remotecmd"
	"github.com/ResetSmith/cronomicon/internal/runnerproto"
)

// buildRemoteCommand turns a manifest's interp[] + body + env into the remote
// invocation to run on a target shell. It delegates to the shared renderer
// (internal/remotecmd) so a run executed by the agent behaves identically to one
// executed by the in-app SSH executor — env injection, quoting, interpreter
// selection, and the H1 stdin hand-off all match, with no risk of the two paths
// drifting.
//
// The interpreter is taken from manifest.Interp (resolved server-side via the
// shared execspec path), so the agent does not re-derive it from run-type.
func buildRemoteCommand(m *runnerproto.ManifestResponse) remotecmd.Rendered {
	env := mergeInjectedEnv(m.Env, m.Secrets)
	ip := ""
	if len(m.Interp) > 0 {
		ip = m.Interp[0]
	}
	return remotecmd.Render(ip, m.Body, env)
}

// mergeInjectedEnv overlays dispatch-time resolved reference values (m.Secrets:
// AMADEUS_SECRET_*/AMADEUS_VAR_*) onto the plaintext env snapshot for injection
// (vault-integration.md P1.4). Injected values win on collision; in practice the
// key-spaces are disjoint (injected keys are all AMADEUS_*, barred from operator
// env_json by the W4 write guard). Returns env unchanged when there is nothing to
// inject.
func mergeInjectedEnv(env, injected map[string]string) map[string]string {
	if len(injected) == 0 {
		return env
	}
	out := make(map[string]string, len(env)+len(injected))
	maps.Copy(out, env)
	maps.Copy(out, injected)
	return out
}

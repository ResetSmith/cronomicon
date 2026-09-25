package sshexec

import (
	"encoding/json"
	"maps"
	"strings"

	"github.com/ResetSmith/cronomicon/internal/remotecmd"
)

// remoteCommand builds the remote invocation for the run's interpreter + body +
// env snapshot (envJSON, a JSON object string). This is the ONLY point env_json
// is parsed — the scheduler→runs→claim pipeline carries it as an opaque string,
// so jobs and workflow child steps inject uniformly. Env values are plaintext
// (schedule env lives in Git in plaintext); real secrets belong to the scope/
// secrets system and are masked in logs by the unconditional ingest-time redactor.
//
// injected carries dispatch-time reference values (resolved CRONOMICON_SECRET_*/
// CRONOMICON_VAR_* and the CRONOMICON_RUN_* run context, vault-integration.md P1.3). It
// is merged over the parsed env in-memory only — NEVER written back to env_json —
// and wins on any key collision (D5). In practice the key-spaces are disjoint:
// injected keys are all CRONOMICON_*, which the W4 write guard bars from
// operator-authored env_json.
//
// H1: the merged env (which may include injected SECRET VALUES) is delivered on
// the session's stdin, never in argv, by remotecmd.Render — see that package for
// why. The returned Rendered carries the command line and the stdin bytes; the
// caller (runTarget) attaches the stdin stream only when it is non-empty.
func remoteCommand(interp []string, body, envJSON string, injected map[string]string) remotecmd.Rendered {
	env := parseEnv(envJSON)
	if len(injected) > 0 {
		if env == nil {
			env = make(map[string]string, len(injected))
		}
		maps.Copy(env, injected)
	}
	ip := ""
	if len(interp) > 0 {
		ip = interp[0]
	}
	return remotecmd.Render(ip, body, env)
}

// parseEnv unmarshals the env_json snapshot into a map (nil when blank/invalid).
func parseEnv(envJSON string) map[string]string {
	if strings.TrimSpace(envJSON) == "" {
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(envJSON), &m); err != nil {
		return nil
	}
	return m
}

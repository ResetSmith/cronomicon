package agent

import (
	"context"
	"fmt"
	"maps"
	"time"

	"github.com/ResetSmith/cronomicon/internal/runnerproto"
)

// localRunTypes are the run-types executed by a local toolchain ON the agent
// host (ansible/terraform) rather than over SSH to remote targets.
var localRunTypes = map[string]bool{
	"ansible":   true,
	"terraform": true,
}

// executor runs a single manifest to completion: it builds the command, dials
// targets (SSH) or shells out locally (ansible/terraform), streams output into
// the log buffer, and seals the buffer with the trailing envelope. It returns
// the buffer ready to stream to the server.
type executor struct {
	ssh *sshRunner
	cfg Config
	inv localInventory // nil unless inventory == "local"
	// settings layers server-managed overrides (sandbox caps, checkout policy)
	// onto cfg per run (Phase 4). Shared with the owning Agent.
	settings *settingsStore
}

// run executes the manifest under ctx (cancelled on kill), populating buf and
// returning the final exit code. All output is streamed RAW into buf — the
// server redacts on ingest.
//
// The job's timeout_seconds rides the manifest and is enforced HERE at the
// process level (§6.1 — previously shipped but never enforced by the agent,
// mirroring the in-app SSH executor's A12 fix): the deadline cancels ctx,
// which kills the local-toolchain process / aborts the SSH fan-out.
func (e *executor) run(ctx context.Context, m *runnerproto.ManifestResponse, buf *logBuffer) int {
	start := time.Now()
	emit := func(line string) { buf.writeLine(line) }

	runCtx := ctx
	if m.TimeoutSeconds > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, time.Duration(m.TimeoutSeconds)*time.Second)
		defer cancel()
	}

	// D8: materialize any delivered SSH-key material to 0600 files off the run tree.
	// deliveredKeys (bare NAME → path) feeds the key resolver (delivered wins,
	// key-dir is the fallback); keyEnv (CRONOMICON_KEY_<name> → path) is exposed in the
	// run env. cleanup wipes the files when run returns — including on early error.
	deliveredKeys, keyEnv, cleanupKeys, kerr := materializeKeys(m.Keys)
	defer cleanupKeys()
	if kerr != nil {
		emit("amadeus: " + kerr.Error())
		buf.seal(makeEnvelope(-1, start, time.Now(), ""))
		return -1
	}
	if len(keyEnv) > 0 {
		if m.Secrets == nil {
			m.Secrets = map[string]string{}
		}
		// path only, never material — flows through both exec paths
		maps.Copy(m.Secrets, keyEnv)
	}

	// RA-12: the same treatment for file-delivered secrets. Their references land in
	// m.Secrets as PATHS, which is how exec_local resolves BecomePasswordRef to the
	// file it passes to --become-password-file.
	fileEnv, cleanupFiles, ferr := materializeSecretFiles(m.SecretFiles)
	defer cleanupFiles()
	if ferr != nil {
		emit("amadeus: " + ferr.Error())
		buf.seal(makeEnvelope(-1, start, time.Now(), ""))
		return -1
	}
	if len(fileEnv) > 0 {
		if m.Secrets == nil {
			m.Secrets = map[string]string{}
		}
		maps.Copy(m.Secrets, fileEnv)
	}

	var exitCode int
	var reason string
	if localRunTypes[m.RunType] {
		// Local toolchain (ansible/terraform) runs ON the agent host. Snapshot
		// the effective config (declared + any server-managed sandbox/checkout
		// overrides) once here, so the per-run reads downstream see a stable,
		// race-free value (Phase 4). D8: overlay delivered keys onto the key-map so
		// ansible's private-key bridge resolves a bound key to its delivered path.
		cfg := e.settings.effectiveConfig(e.cfg)
		cfg.KeyMap = mergeKeyMap(cfg.KeyMap, deliveredKeys)
		exitCode = runLocalToolchain(runCtx, m, cfg, emit)
	} else {
		// SSH run-types (bash/perl/powershell/python) fan out to targets.
		targets, err := e.resolveTargets(m)
		if err != nil {
			emit("amadeus: " + err.Error())
			exitCode = 1
		} else {
			cmd := buildRemoteCommand(m)
			// D8: a per-run runner whose key-map includes the delivered keys, so a
			// target's auth key resolves to the delivered material (delivered wins over
			// the runner's own key custody; key-dir remains the fallback).
			runner := e.ssh
			if len(deliveredKeys) > 0 {
				clone := *e.ssh
				clone.keyMap = mergeKeyMap(e.ssh.keyMap, deliveredKeys)
				runner = &clone
			}
			exitCode, reason = runner.fanOut(runCtx, targets, cmd, emit)
		}
	}

	if runCtx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
		emit(fmt.Sprintf("amadeus: run timed out after %ds; process killed", m.TimeoutSeconds))
		if exitCode == 0 {
			exitCode = -1
		}
	}

	buf.seal(makeEnvelope(exitCode, start, time.Now(), reason))
	return exitCode
}

// resolveTargets picks the host list for an SSH run. In "cronomicon" mode the
// manifest already carries fully-resolved targets; in "local" mode the agent
// resolves the scope against its OWN inventory (T-b network-isolated segments).
func (e *executor) resolveTargets(m *runnerproto.ManifestResponse) ([]runnerproto.ManifestTarget, error) {
	if m.InventoryMode == "local" || e.cfg.Inventory == "local" {
		return resolveLocalScope(e.inv, m.Scope)
	}
	return m.Targets, nil
}

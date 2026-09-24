package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/ResetSmith/cronomicon/internal/ansiblereq"
)

// galaxyInstall installs a checkout project's requirements.yml dependencies into
// an ephemeral per-run dir under workdir (ansible-update.md RX.4/RX.12). It
// returns the extra env (ANSIBLE_COLLECTIONS_PATH / ANSIBLE_ROLES_PATH, workdir
// first so project deps win) the ansible-playbook child must inherit.
//
// Skip-if-satisfied (§4): a collection already present in the runner-provisioned
// base at the EXACT pinned version is not re-downloaded — the base path already
// satisfies it — so a pre-provisioned runner runs fully offline and repeat runs
// don't hammer the galaxy source. Roles (git-SHA pins) are always installed per
// run. A download failure for an unsatisfied pin fails the run LOUDLY: there is
// deliberately NO silent fallback to a differently-versioned local copy (RX.5).
func galaxyInstall(ctx context.Context, cfg Config, workdir, reqRel string, emit func(string)) (map[string]string, error) {
	reqAbs := filepath.Join(workdir, filepath.FromSlash(reqRel))
	data, err := os.ReadFile(reqAbs)
	if err != nil {
		return nil, fmt.Errorf("read requirements %q: %w", reqRel, err)
	}
	reqs, err := ansiblereq.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("parse requirements %q: %w", reqRel, err)
	}
	if len(reqs.Collections) == 0 && len(reqs.Roles) == 0 {
		return nil, nil
	}

	depsDir := filepath.Join(workdir, ".deps")
	rolesDir := filepath.Join(depsDir, "roles")
	if err := os.MkdirAll(rolesDir, 0o700); err != nil {
		return nil, fmt.Errorf("create deps dir: %w", err)
	}

	base := galaxyBaseCollections(ctx) // best-effort; empty ⇒ nothing skipped
	for _, c := range reqs.Collections {
		if c.Name == "" || c.Version == "" {
			// Unpinned entries are rejected at authoring time (RX.5 lint); if one
			// slips through, install-by-name would fetch "latest" non-deterministically.
			return nil, fmt.Errorf("collection %q is not version-pinned; refusing a non-reproducible install", c.Name)
		}
		if base[c.Name] == c.Version {
			emit(fmt.Sprintf("amadeus: galaxy skip %s:%s (satisfied by runner base)", c.Name, c.Version))
			continue
		}
		args := []string{"collection", "install", c.Name + ":" + c.Version, "-p", depsDir}
		if src := galaxySource(cfg, c.Source); src != "" {
			args = append(args, "--server", src)
		}
		if err := runGalaxy(ctx, emit, args...); err != nil {
			return nil, fmt.Errorf("install collection %s:%s failed (no fallback to an unpinned copy): %w", c.Name, c.Version, err)
		}
		emit(fmt.Sprintf("amadeus: galaxy installed %s:%s", c.Name, c.Version))
	}

	if len(reqs.Roles) > 0 {
		// Roles are git-SHA pins (no local version introspection) → always per run.
		if err := runGalaxy(ctx, emit, "role", "install", "-r", reqAbs, "-p", rolesDir); err != nil {
			return nil, fmt.Errorf("install roles failed: %w", err)
		}
		emit(fmt.Sprintf("amadeus: galaxy installed %d role(s) from %s", len(reqs.Roles), reqRel))
	}

	// workdir-first collection path so project deps win over the base (§4); the
	// base path stays reachable so skip-if-satisfied collections still resolve.
	collectionsPath := depsDir
	if existing := os.Getenv("ANSIBLE_COLLECTIONS_PATH"); existing != "" {
		collectionsPath = depsDir + ":" + existing
	}
	return map[string]string{
		"ANSIBLE_COLLECTIONS_PATH": collectionsPath,
		"ANSIBLE_ROLES_PATH":       rolesDir,
	}, nil
}

// galaxyBaseCollections returns the collections installed in the runner base as
// name→version, from `ansible-galaxy collection list --format json`. Best-effort:
// any error yields an empty map (nothing is treated as satisfied, so everything
// installs — safe, just not offline-optimal).
func galaxyBaseCollections(ctx context.Context) map[string]string {
	out := map[string]string{}
	// Bounded probe: on an Ansible control node this listing can be very slow or
	// hang; a timeout yields an empty base (everything reinstalls per run — safe,
	// just not offline-optimal) rather than wedging startup. See runProbe.
	buf := runProbeWithTimeout(ctx, toolchainProbeTimeout, "ansible-galaxy", "collection", "list", "--format", "json")
	if len(buf) == 0 {
		return out
	}
	// Shape: { "<path>": { "namespace.name": { "version": "x.y.z" }, ... }, ... }
	var byPath map[string]map[string]struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(buf, &byPath); err != nil {
		return out
	}
	for _, colls := range byPath {
		for fqcn, info := range colls {
			if info.Version != "" {
				out[fqcn] = info.Version
			}
		}
	}
	return out
}

// galaxySource picks the galaxy server URL: the collection's own `source:`
// wins; else the runner's -galaxy-server override; else "" (ansible's default,
// public galaxy.ansible.com — P6).
func galaxySource(cfg Config, collectionSource string) string {
	if collectionSource != "" {
		return collectionSource
	}
	return cfg.GalaxyServer
}

// runGalaxy runs ansible-galaxy, streaming its output as provenance (RX.12) and
// returning a combined error on failure.
func runGalaxy(ctx context.Context, emit func(string), args ...string) error {
	cmd := exec.CommandContext(ctx, "ansible-galaxy", args...)
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	for line := range strings.SplitSeq(strings.TrimRight(string(out), "\n"), "\n") {
		if strings.TrimSpace(line) != "" {
			emit("ansible-galaxy: " + line)
		}
	}
	return err
}

// emitAnsibleCoreVersion logs the ansible-core version as a provenance line
// (RX.12). Best-effort — a missing ansible is surfaced by the run itself.
func emitAnsibleCoreVersion(ctx context.Context, emit func(string)) {
	cmd := exec.CommandContext(ctx, "ansible", "--version")
	cmd.Env = os.Environ()
	out, err := cmd.Output()
	if err != nil || len(out) == 0 {
		return
	}
	first, _, _ := strings.Cut(string(out), "\n")
	emit("amadeus: " + strings.TrimSpace(first))
}

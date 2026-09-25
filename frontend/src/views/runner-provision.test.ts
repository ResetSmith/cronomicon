import { describe, it, expect } from "vitest";
import { readFileSync } from "node:fs";
import { resolve, dirname } from "node:path";
import { fileURLToPath } from "node:url";
import {
  defaultProvisionOptions,
  generateRunnerEnv,
  provisionOneLiner,
  provisionDockerRun,
  keyMapDestSpec,
  RUN_TYPES,
  type ProvisionOptions,
} from "./runner-provision";

// The REAL template from backend/deploy — the same bytes the manuals plugin
// publishes at /cronomicon-runner.env.example. Using it here makes these tests
// the drift guard: if the example loses/renames a var the generator patches,
// generateRunnerEnv throws and this suite fails.
const example = readFileSync(
  resolve(dirname(fileURLToPath(import.meta.url)), "../../../backend/deploy/cronomicon-runner.env.example"),
  "utf8",
);

const ORIGIN = "https://cronomicon.example.com";

function fullOpts(): ProvisionOptions {
  return {
    origin: ORIGIN,
    token: "crn_reg_test123",
    name: "runner-dc2-07",
    capabilities: ["ansible", "bash"],
    inventory: "local",
    localInventorySrc: "/home/op/inventory.json",
    knownHostsSrc: "/home/op/known_hosts",
    keyMode: "key-map",
    keyMapSpec: "PROD_KEY=/home/op/prod.pem,DB_KEY=/home/op/db.pem",
    caCertSrc: "/home/op/internal-ca.pem",
    maxConcurrent: 8,
    checkout: true,
    checkoutRepos: "https://gitlab.example.com/infra/playbooks.git",
    vaultPasswordFile: "/etc/cronomicon-runner/vault-pass",
    sandboxMemoryMax: "2G",
    sandboxCpuQuota: "150%",
    sandboxTasksMax: "512",
  };
}

describe("generateRunnerEnv", () => {
  it("patches the defaults into the verbatim example (annotations survive)", () => {
    const env = generateRunnerEnv(example, { ...defaultProvisionOptions(ORIGIN) });
    expect(env).toContain(`CRONOMICON_RUNNER_SERVER=${ORIGIN}`);
    expect(env).toContain("CRONOMICON_RUNNER_REGISTRATION_TOKEN=<TOKEN>");
    expect(env).toContain("CRONOMICON_RUNNER_NAME=runner-01");
    // Default = auto-detect: the capabilities line stays present but inactive
    // (unset ⇒ the agent probes the host's toolchains at startup).
    expect(env).toMatch(/^# CRONOMICON_RUNNER_CAPABILITIES=/m);
    expect(env).not.toMatch(/^CRONOMICON_RUNNER_CAPABILITIES=/m);
    expect(env).toContain("CRONOMICON_RUNNER_INVENTORY=cronomicon");
    expect(env).toContain("CRONOMICON_RUNNER_IDENTITY_FILE=/var/lib/cronomicon-runner/identity.json");
    // Untouched optionals stay commented; the example's annotations survive.
    expect(env).toMatch(/^# CRONOMICON_RUNNER_MAX_CONCURRENT=/m);
    expect(env).toMatch(/^# CRONOMICON_RUNNER_ALLOW_CHECKOUT=/m);
    expect(env).toContain("credential model b");
    // No key custody chosen → known_hosts line deactivated, not deleted.
    expect(env).toMatch(/^# CRONOMICON_RUNNER_KNOWN_HOSTS=/m);
  });

  it("activates every optional the full form sets, with installer dest paths", () => {
    const env = generateRunnerEnv(example, fullOpts());
    expect(env).toContain("CRONOMICON_RUNNER_CAPABILITIES=ansible,bash");
    expect(env).toContain("CRONOMICON_RUNNER_INVENTORY=local");
    expect(env).toContain("CRONOMICON_RUNNER_LOCAL_INVENTORY=/etc/cronomicon-runner/inventory.json");
    expect(env).toContain("CRONOMICON_RUNNER_MAX_CONCURRENT=8");
    expect(env).toContain("CRONOMICON_RUNNER_KNOWN_HOSTS=/var/lib/cronomicon-runner/known_hosts");
    // key-map values are rewritten to the installer's keys/<NAME> destinations.
    expect(env).toContain(
      "CRONOMICON_RUNNER_KEY_MAP=PROD_KEY=/var/lib/cronomicon-runner/keys/PROD_KEY,DB_KEY=/var/lib/cronomicon-runner/keys/DB_KEY",
    );
    expect(env).toContain("CRONOMICON_RUNNER_CA_CERT=/etc/cronomicon-runner/ca.pem");
    expect(env).toContain("CRONOMICON_RUNNER_ALLOW_CHECKOUT=true");
    expect(env).toContain("CRONOMICON_RUNNER_CHECKOUT_REPOS=https://gitlab.example.com/infra/playbooks.git");
    expect(env).toContain("CRONOMICON_RUNNER_CHECKOUT_TOKEN_FILE=/etc/cronomicon-runner/checkout-token");
    expect(env).toContain("CRONOMICON_RUNNER_VAULT_PASSWORD_FILE=/etc/cronomicon-runner/vault-pass");
    expect(env).toContain("CRONOMICON_RUNNER_SANDBOX_MEMORY_MAX=2G");
    expect(env).toContain("CRONOMICON_RUNNER_SANDBOX_CPU_QUOTA=150%");
    expect(env).toContain("CRONOMICON_RUNNER_SANDBOX_TASKS_MAX=512");
    // CHECKOUT_TOKEN_FILE was set — the bare-token alternative stays commented.
    expect(env).toMatch(/^# CRONOMICON_RUNNER_CHECKOUT_TOKEN=/m);
  });

  it("key-dir mode activates KEY_DIR and leaves KEY_MAP commented", () => {
    const env = generateRunnerEnv(example, {
      ...defaultProvisionOptions(ORIGIN),
      keyMode: "key-dir",
      keyDirSrc: "/home/op/keys",
      knownHostsSrc: "/home/op/known_hosts",
    });
    expect(env).toContain("CRONOMICON_RUNNER_KEY_DIR=/var/lib/cronomicon-runner/keys");
    expect(env).toMatch(/^# CRONOMICON_RUNNER_KEY_MAP=/m);
  });

  it("does not interpret $-replacement patterns in operator values", () => {
    const env = generateRunnerEnv(example, {
      ...defaultProvisionOptions(ORIGIN),
      name: "a$&b",
      vaultPasswordFile: "/etc/keys/a$$b$'c",
    });
    expect(env).toContain("CRONOMICON_RUNNER_NAME=a$&b");
    expect(env).toContain("CRONOMICON_RUNNER_VAULT_PASSWORD_FILE=/etc/keys/a$$b$'c");
  });

  it("an explicit capability override activates the var", () => {
    const env = generateRunnerEnv(example, {
      ...defaultProvisionOptions(ORIGIN),
      capabilities: ["bash", "python"],
    });
    expect(env).toContain("CRONOMICON_RUNNER_CAPABILITIES=bash,python");
  });

  it("throws loudly when the template loses a var it patches (drift guard)", () => {
    // The capabilities line must survive in BOTH modes: detect (commentVar +
    // assertVar) and override (setVar).
    const mutilated = example.replace(/^#?\s*CRONOMICON_RUNNER_CAPABILITIES=.*$/m, "");
    expect(() => generateRunnerEnv(mutilated, defaultProvisionOptions(ORIGIN))).toThrow(/drifted/);
    expect(() =>
      generateRunnerEnv(mutilated, { ...defaultProvisionOptions(ORIGIN), capabilities: ["bash"] }),
    ).toThrow(/drifted/);
  });
});

describe("provisionOneLiner", () => {
  it("emits the base command with --download and hostname default — no -c (auto-detect)", () => {
    const cmd = provisionOneLiner(defaultProvisionOptions(ORIGIN));
    expect(cmd).toContain(`curl -fsSL ${ORIGIN}/runner-install.sh | sudo bash -s --`);
    expect(cmd).toContain(`-s ${ORIGIN}`);
    expect(cmd).toContain("-t '<TOKEN>'"); // placeholder is quoted (shell-safe)
    expect(cmd).toContain("-n $(hostname)");
    expect(cmd).not.toContain("-c "); // capabilities auto-detect on the host
    expect(cmd).toContain("--download");
    expect(cmd).not.toContain("--known-hosts");
    expect(cmd).not.toContain("--inventory");
  });

  it("carries every install flag the form sets, incl. checkout + vault (Phase 3)", () => {
    const cmd = provisionOneLiner(fullOpts());
    expect(cmd).toContain("-n runner-dc2-07");
    expect(cmd).toContain("-c ansible,bash");
    expect(cmd).toContain("--inventory local");
    expect(cmd).toContain("--local-inventory /home/op/inventory.json");
    expect(cmd).toContain("--known-hosts /home/op/known_hosts");
    expect(cmd).toContain("--key-map PROD_KEY=/home/op/prod.pem,DB_KEY=/home/op/db.pem");
    expect(cmd).toContain("--ca-cert /home/op/internal-ca.pem");
    expect(cmd).not.toContain("--key-dir"); // mutually exclusive with --key-map
    // Phase 3: checkout + vault ride the one-liner instead of an env merge.
    expect(cmd).toContain("--allow-checkout");
    expect(cmd).toContain("--checkout-repos https://gitlab.example.com/infra/playbooks.git");
    expect(cmd).toContain("--vault-pass-file /etc/cronomicon-runner/vault-pass");
  });

  it("emits --checkout-token-file when a token source path is given", () => {
    const cmd = provisionOneLiner({
      ...defaultProvisionOptions(ORIGIN),
      checkout: true,
      checkoutTokenFile: "/home/op/deploy-token",
    });
    expect(cmd).toContain("--allow-checkout");
    expect(cmd).toContain("--checkout-token-file /home/op/deploy-token");
    expect(cmd).not.toContain("--vault-pass-file");
  });

});

describe("provisionDockerRun", () => {
  it("default (auto-detect): slim image, no capabilities env, detection note", () => {
    const cmd = provisionDockerRun(defaultProvisionOptions(ORIGIN));
    expect(cmd).toContain("docker volume create cronomicon-runner-data");
    expect(cmd).toContain("--name runner-01");
    expect(cmd).toContain(`-e CRONOMICON_RUNNER_SERVER=${ORIGIN}`);
    expect(cmd).not.toContain("CRONOMICON_RUNNER_CAPABILITIES"); // agent detects in-container
    expect(cmd).toContain("auto-detect");
    expect(cmd).toContain("-v cronomicon-runner-data:/var/lib/cronomicon-runner");
    expect(cmd.trim().endsWith("cronomicon-runner:slim")).toBe(true);
  });

  it("an explicit capability override rides the env", () => {
    const cmd = provisionDockerRun({ ...defaultProvisionOptions(ORIGIN), capabilities: ["bash", "perl"] });
    expect(cmd).toContain("-e CRONOMICON_RUNNER_CAPABILITIES=bash,perl");
    expect(cmd.trim().endsWith("cronomicon-runner:slim")).toBe(true);
  });

  it("switches to the fat image when a local-toolchain capability is picked", () => {
    const cmd = provisionDockerRun(fullOpts());
    expect(cmd.trim().endsWith("cronomicon-runner:fat")).toBe(true);
    expect(cmd).toContain("-e CRONOMICON_RUNNER_INVENTORY=local");
    // Container file paths live on the volume.
    expect(cmd).toContain("-e CRONOMICON_RUNNER_LOCAL_INVENTORY=/var/lib/cronomicon-runner/inventory.json");
    expect(cmd).toContain("-e CRONOMICON_RUNNER_CA_CERT=/var/lib/cronomicon-runner/ca.pem");
    expect(cmd).toContain("-e CRONOMICON_RUNNER_CHECKOUT_TOKEN_FILE=/var/lib/cronomicon-runner/checkout-token");
    expect(cmd).toContain("-e CRONOMICON_RUNNER_VAULT_PASSWORD_FILE=/var/lib/cronomicon-runner/vault-pass");
    expect(cmd).toContain("-e CRONOMICON_RUNNER_SANDBOX_MEMORY_MAX=2G");
  });
});

describe("vocabulary", () => {
  it("RUN_TYPES matches the closed run-type set", () => {
    expect([...RUN_TYPES].sort()).toEqual(["ansible", "bash", "perl", "powershell", "python", "terraform"]);
  });

  it("keyMapDestSpec rewrites source paths to installed key paths", () => {
    expect(keyMapDestSpec("A=/x/a.pem, B=/y/b.key")).toBe(
      "A=/var/lib/cronomicon-runner/keys/A,B=/var/lib/cronomicon-runner/keys/B",
    );
  });

  it("every var the generator patches exists in the real example", () => {
    // Exercise the widest option set against the real template: any rename in
    // the example throws inside generateRunnerEnv (setVar drift guard).
    expect(() => generateRunnerEnv(example, fullOpts())).not.toThrow();
  });
});

// Generators for the "Provision a Runner" helper (runner provisioning plan
// Phase 6): from one set of choices, emit (a) a complete annotated runner.env,
// (b) the matching runner-install.sh one-liner, (c) the docker run variant.
//
// Single-source rule (the plan's drift requirement): the env artifact is NOT
// authored here — it patches values into the VERBATIM
// backend/deploy/cronomicon-runner.env.example, published by
// vite-manuals-plugin.js at ENV_EXAMPLE_PATH. setVar throws if a var it needs
// is missing from the template, so a rename in the example (or config.go)
// breaks loudly here and in the unit tests instead of drifting silently.
//
// Token-agnostic like the install-cmd builders (D6): callers pass a usable
// plaintext token or a "<TOKEN>" placeholder.

import { RUNNER_INSTALL_SCRIPT_PATH } from "./runner-install-cmd";
import { RUN_TYPES, RUNNER_ONLY_TYPES } from "../runtypes";

export const ENV_EXAMPLE_PATH = "/cronomicon-runner.env.example";

// The closed run-type vocabulary now lives in runtypes.ts (RP-5); re-exported
// under the provisioning names. ansible/terraform are runner-only local
// toolchains → the fat image; the rest are SSH-onward → slim.
export { RUN_TYPES };
export const FAT_RUN_TYPES = RUNNER_ONLY_TYPES;

// Installer-standard destination paths (runner-install.sh header constants).
// The env artifact references these so it matches what the script installs.
const STATE_DIR = "/var/lib/cronomicon-runner";
const CONF_DIR = "/etc/cronomicon-runner";
const DEST = {
  knownHosts: `${STATE_DIR}/known_hosts`,
  keysDir: `${STATE_DIR}/keys`,
  caCert: `${CONF_DIR}/ca.pem`,
  localInventory: `${CONF_DIR}/inventory.json`,
  checkoutTokenFile: `${CONF_DIR}/checkout-token`,
  vaultPasswordFile: `${CONF_DIR}/vault-pass`,
} as const;

export interface ProvisionOptions {
  origin: string;
  token: string; // plaintext or "<TOKEN>"
  name: string; // "" ⇒ $(hostname) in the one-liner, runner-01 elsewhere
  // Empty ⇒ auto-detect: the agent probes the host's toolchains at startup
  // (D1: 1B). Non-empty is an explicit narrowing override (-c).
  capabilities: string[];
  inventory: "cronomicon" | "local";
  // Source paths on the installing host (ride the one-liner's Phase-1 flags;
  // the env/docker artifacts reference the installed DEST paths).
  knownHostsSrc?: string;
  keyMode: "none" | "key-dir" | "key-map";
  keyDirSrc?: string;
  keyMapSpec?: string; // NAME=path,NAME2=path2 (source paths)
  caCertSrc?: string;
  localInventorySrc?: string;
  maxConcurrent?: number; // omitted/5 ⇒ leave the template default
  checkout: boolean;
  checkoutRepos?: string; // comma-separated clone URLs
  checkoutTokenFile?: string;
  vaultPasswordFile?: string;
  sandboxMemoryMax?: string;
  sandboxCpuQuota?: string;
  sandboxTasksMax?: string;
}

export function defaultProvisionOptions(origin: string): ProvisionOptions {
  return {
    origin,
    token: "<TOKEN>",
    name: "",
    capabilities: [], // auto-detect on the host (override to narrow)
    inventory: "cronomicon",
    keyMode: "none",
    checkout: false,
  };
}

// assertVar throws when the template has no `NAME=` line (active or
// `#`-commented) — that's the drift guard — and returns the matching regex.
function assertVar(text: string, name: string): RegExp {
  const re = new RegExp(`^#?[ \\t]*${name}=.*$`, "m");
  if (!re.test(text)) {
    throw new Error(
      `env template is missing ${name} — backend/deploy/cronomicon-runner.env.example and runner-provision.ts have drifted`,
    );
  }
  return re;
}

// setVar activates (uncomments) and sets the FIRST `NAME=` line — active or
// `#`-commented — in the template. Throws when absent: that's the drift guard.
function setVar(text: string, name: string, value: string): string {
  const re = assertVar(text, name);
  // Function replacement: a plain replacement string would interpret $-patterns
  // ($&, $', $$…) in operator-supplied values (paths, names) and silently
  // corrupt the output.
  return text.replace(re, () => `${name}=${value}`);
}

// commentVar deactivates an active `NAME=` line (no-op if already commented).
function commentVar(text: string, name: string): string {
  const re = new RegExp(`^[ \\t]*(${name}=.*)$`, "m");
  return text.replace(re, "# $1");
}

// keyMapDestSpec rewrites a NAME=path,... key-map to the installer's
// destination paths (runner-install.sh copies each key to keys/<NAME>).
export function keyMapDestSpec(spec: string): string {
  return spec
    .split(",")
    .map((pair) => pair.trim())
    .filter(Boolean)
    .map((pair) => {
      const name = pair.split("=")[0]?.trim() ?? pair;
      return `${name}=${DEST.keysDir}/${name}`;
    })
    .join(",");
}

// generateRunnerEnv patches the operator's choices into the verbatim env
// example (fetched from ENV_EXAMPLE_PATH). All annotations survive.
export function generateRunnerEnv(exampleText: string, o: ProvisionOptions): string {
  let t = exampleText;
  t = setVar(t, "CRONOMICON_RUNNER_SERVER", o.origin);
  t = setVar(t, "CRONOMICON_RUNNER_REGISTRATION_TOKEN", o.token);
  t = setVar(t, "CRONOMICON_RUNNER_NAME", o.name || "runner-01");
  if (o.capabilities.length > 0) {
    t = setVar(t, "CRONOMICON_RUNNER_CAPABILITIES", o.capabilities.join(","));
  } else {
    // Detect mode: the var stays in the artifact but inactive (unset ⇒ the
    // agent probes the host's toolchains at startup). commentVar, not removal —
    // and assertVar keeps the drift guard alive on this branch too.
    assertVar(t, "CRONOMICON_RUNNER_CAPABILITIES");
    t = commentVar(t, "CRONOMICON_RUNNER_CAPABILITIES");
  }
  t = setVar(t, "CRONOMICON_RUNNER_INVENTORY", o.inventory);
  t = setVar(t, "CRONOMICON_RUNNER_IDENTITY_FILE", `${STATE_DIR}/identity.json`);

  if (o.maxConcurrent != null && o.maxConcurrent > 0 && o.maxConcurrent !== 5) {
    t = setVar(t, "CRONOMICON_RUNNER_MAX_CONCURRENT", String(o.maxConcurrent));
  }
  if (o.inventory === "local") {
    t = setVar(t, "CRONOMICON_RUNNER_LOCAL_INVENTORY", DEST.localInventory);
  }

  // Key custody: the env references the installed destinations.
  if (o.knownHostsSrc) {
    t = setVar(t, "CRONOMICON_RUNNER_KNOWN_HOSTS", DEST.knownHosts);
  } else {
    // Leave the strict-host-key requirement visible but inactive — the loud
    // "before the first SSH run" story lives in the guide.
    t = commentVar(t, "CRONOMICON_RUNNER_KNOWN_HOSTS");
  }
  if (o.keyMode === "key-dir") {
    t = setVar(t, "CRONOMICON_RUNNER_KEY_DIR", DEST.keysDir);
  } else if (o.keyMode === "key-map" && o.keyMapSpec) {
    t = setVar(t, "CRONOMICON_RUNNER_KEY_MAP", keyMapDestSpec(o.keyMapSpec));
  }
  if (o.caCertSrc) {
    t = setVar(t, "CRONOMICON_RUNNER_CA_CERT", DEST.caCert);
  }

  if (o.checkout) {
    t = setVar(t, "CRONOMICON_RUNNER_ALLOW_CHECKOUT", "true");
    if (o.checkoutRepos) t = setVar(t, "CRONOMICON_RUNNER_CHECKOUT_REPOS", o.checkoutRepos);
    t = setVar(t, "CRONOMICON_RUNNER_CHECKOUT_TOKEN_FILE", o.checkoutTokenFile || DEST.checkoutTokenFile);
  }
  if (o.vaultPasswordFile) {
    t = setVar(t, "CRONOMICON_RUNNER_VAULT_PASSWORD_FILE", o.vaultPasswordFile);
  }
  if (o.sandboxMemoryMax) t = setVar(t, "CRONOMICON_RUNNER_SANDBOX_MEMORY_MAX", o.sandboxMemoryMax);
  if (o.sandboxCpuQuota) t = setVar(t, "CRONOMICON_RUNNER_SANDBOX_CPU_QUOTA", o.sandboxCpuQuota);
  if (o.sandboxTasksMax) t = setVar(t, "CRONOMICON_RUNNER_SANDBOX_TASKS_MAX", o.sandboxTasksMax);

  return t;
}

// shellArg quotes a value for a sh command line when it needs it.
function shellArg(v: string): string {
  return /^[A-Za-z0-9@%+=:,._/-]+$/.test(v) ? v : `'${v.replace(/'/g, `'\\''`)}'`;
}

// provisionOneLiner emits the runner-install.sh invocation matching the same
// choices, using the script's flags so the install completes in one pass. As of
// Phase 3 that includes checkout + vault: --allow-checkout / --checkout-repos
// carry the opt-in and allowlist, and --checkout-token-file / --vault-pass-file
// install the secret files the operator staged (the bytes never transit the
// server). Only sandbox caps + max jobs remain env-only (Phase 4 kills those).
export function provisionOneLiner(o: ProvisionOptions): string {
  const parts = [
    `curl -fsSL ${o.origin}${RUNNER_INSTALL_SCRIPT_PATH} | sudo bash -s --`,
    `-s ${o.origin}`,
    `-t ${shellArg(o.token)}`, // quotes the <TOKEN> placeholder; a real crn_reg_* passes verbatim
    `-n ${o.name ? shellArg(o.name) : "$(hostname)"}`,
  ];
  // No -c in detect mode: the agent probes the host's toolchains at startup.
  if (o.capabilities.length > 0) parts.push(`-c ${o.capabilities.join(",")}`);
  parts.push(`--download`);
  if (o.inventory === "local") {
    parts.push(`--inventory local`);
    if (o.localInventorySrc) parts.push(`--local-inventory ${shellArg(o.localInventorySrc)}`);
  }
  if (o.knownHostsSrc) parts.push(`--known-hosts ${shellArg(o.knownHostsSrc)}`);
  if (o.keyMode === "key-dir" && o.keyDirSrc) parts.push(`--key-dir ${shellArg(o.keyDirSrc)}`);
  if (o.keyMode === "key-map" && o.keyMapSpec) parts.push(`--key-map ${shellArg(o.keyMapSpec)}`);
  if (o.caCertSrc) parts.push(`--ca-cert ${shellArg(o.caCertSrc)}`);
  if (o.checkout) parts.push(`--allow-checkout`);
  if (o.checkoutRepos) parts.push(`--checkout-repos ${shellArg(o.checkoutRepos)}`);
  // The *-file flags take a SOURCE path on the installing host; the installer
  // copies it to the standard /etc/cronomicon-runner/{checkout-token,vault-pass}.
  if (o.checkoutTokenFile) parts.push(`--checkout-token-file ${shellArg(o.checkoutTokenFile)}`);
  if (o.vaultPasswordFile) parts.push(`--vault-pass-file ${shellArg(o.vaultPasswordFile)}`);
  return parts.join(" ");
}

// The published runner images (.github/workflows/publish-images.yml): the slim
// agent, and a separate -fat package that adds the ansible/terraform toolchains.
export const RUNNER_IMAGE = "ghcr.io/resetsmith/cronomicon-runner";

// runnerImage pins the image to the server's release when /version reports one:
// the agent must speak the server's protocol (the floor tracks it), and the
// server and its runner images publish from the same tag. A dev build has no
// release to match, so it falls back to latest.
export function runnerImage(fat: boolean, serverVersion?: string): string {
  const tag = serverVersion && /^\d+\.\d+\.\d+$/.test(serverVersion) ? serverVersion : "latest";
  return `${RUNNER_IMAGE}${fat ? "-fat" : ""}:${tag}`;
}

// provisionDockerRun emits the container variant (runner-install.html §10):
// identity + keys persist on a named volume; slim vs fat is derived from the
// selected capabilities (ansible/terraform need the fat image's toolchains).
export function provisionDockerRun(o: ProvisionOptions, serverVersion?: string): string {
  const fat = o.capabilities.some((c) => FAT_RUN_TYPES.has(c));
  const name = o.name || "runner-01";
  const env: string[] = [
    `CRONOMICON_RUNNER_SERVER=${o.origin}`,
    `CRONOMICON_RUNNER_REGISTRATION_TOKEN=${o.token}`,
    `CRONOMICON_RUNNER_NAME=${name}`,
    // Detect mode omits the var: the agent probes the image's toolchains at
    // startup (slim ⇒ the SSH-onward run-types; fat adds ansible/terraform).
    ...(o.capabilities.length > 0 ? [`CRONOMICON_RUNNER_CAPABILITIES=${o.capabilities.join(",")}`] : []),
    `CRONOMICON_RUNNER_INVENTORY=${o.inventory}`,
  ];
  if (o.maxConcurrent != null && o.maxConcurrent > 0 && o.maxConcurrent !== 5) {
    env.push(`CRONOMICON_RUNNER_MAX_CONCURRENT=${o.maxConcurrent}`);
  }
  // File-backed options live on the persistent volume in the container story.
  if (o.inventory === "local") env.push(`CRONOMICON_RUNNER_LOCAL_INVENTORY=${STATE_DIR}/inventory.json`);
  if (o.knownHostsSrc) env.push(`CRONOMICON_RUNNER_KNOWN_HOSTS=${DEST.knownHosts}`);
  if (o.keyMode === "key-dir") env.push(`CRONOMICON_RUNNER_KEY_DIR=${DEST.keysDir}`);
  if (o.keyMode === "key-map" && o.keyMapSpec) env.push(`CRONOMICON_RUNNER_KEY_MAP=${keyMapDestSpec(o.keyMapSpec)}`);
  if (o.caCertSrc) env.push(`CRONOMICON_RUNNER_CA_CERT=${STATE_DIR}/ca.pem`);
  if (o.checkout) {
    env.push(`CRONOMICON_RUNNER_ALLOW_CHECKOUT=true`);
    if (o.checkoutRepos) env.push(`CRONOMICON_RUNNER_CHECKOUT_REPOS=${o.checkoutRepos}`);
    env.push(`CRONOMICON_RUNNER_CHECKOUT_TOKEN_FILE=${STATE_DIR}/checkout-token`);
  }
  if (o.vaultPasswordFile) env.push(`CRONOMICON_RUNNER_VAULT_PASSWORD_FILE=${STATE_DIR}/vault-pass`);
  if (o.sandboxMemoryMax) env.push(`CRONOMICON_RUNNER_SANDBOX_MEMORY_MAX=${o.sandboxMemoryMax}`);
  if (o.sandboxCpuQuota) env.push(`CRONOMICON_RUNNER_SANDBOX_CPU_QUOTA=${o.sandboxCpuQuota}`);
  if (o.sandboxTasksMax) env.push(`CRONOMICON_RUNNER_SANDBOX_TASKS_MAX=${o.sandboxTasksMax}`);

  const lines = [
    `# Persist identity + keys across restarts on a named volume; place any`,
    `# referenced files (known_hosts, keys, inventory, tokens) on it first.`,
    ...(o.capabilities.length === 0
      ? [
          `# Capabilities auto-detect from the image's toolchains at startup`,
          `# (slim ⇒ SSH-onward run-types; use the -fat image for ansible/terraform).`,
        ]
      : []),
    `docker volume create cronomicon-runner-data`,
    ``,
    `docker run -d --name ${shellArg(name)} --restart unless-stopped \\`,
    ...env.map((e) => `  -e ${shellArg(e)} \\`),
    `  -v cronomicon-runner-data:${STATE_DIR} \\`,
    `  ${runnerImage(fat, serverVersion)}`,
  ];
  return lines.join("\n");
}

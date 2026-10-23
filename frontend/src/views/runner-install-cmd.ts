// Builders for the Runners panel's install command (runner provisioning plan
// D4: curl-pipe one-liner primary, download-inspect-run variant secondary).
//
// The install script itself is served by the app at RUNNER_INSTALL_SCRIPT_PATH
// (published from backend/deploy/runner-install.sh by vite-manuals-plugin.js),
// so a target host only needs to reach the Cronomicon server — not GitLab.
//
// Token-agnostic by design (D6): callers pass "a usable plaintext token" — the
// shared registration token today, a minted single-use token later — or a
// placeholder like "<TOKEN>" when no plaintext is in hand.

export const RUNNER_INSTALL_SCRIPT_PATH = "/runner-install.sh";

// A second agent on a machine that already runs one (MA-15..MA-24). The
// installer's `--instance <name>` gives it an OS user, group, unit and
// directories of its own, all named cronomicon-runner-<name>. The rule is the
// installer's, restated here so the helper refuses a name the script would
// (runner-install.sh, "Instance name"): lower-case letters, digits and hyphens,
// a letter first, 14 characters at most — it becomes part of an OS user name.
export const INSTANCE_NAME_MAX = 14;
const INSTANCE_NAME_RE = /^[a-z][a-z0-9-]{0,13}$/;

export function validInstanceName(name: string): boolean {
  return INSTANCE_NAME_RE.test(name);
}

// What every builder returns in place of a command while the Instance name
// field holds something the installer would refuse. NOT the default agent's
// command: the field is filled in precisely on a machine that already runs the
// default agent, and pasting that command there would overwrite the first
// agent's runner.env with the other agency's token. A shell comment, so that
// pasting it does nothing.
export const INVALID_INSTANCE_COMMAND = "# The instance name is not valid, so no install command is shown. Correct it above.";

// instanceInvalid: something was typed, and it is not a name the installer takes.
export function instanceInvalid(instance?: string): boolean {
  const name = (instance ?? "").trim();
  return name !== "" && !validInstanceName(name);
}

// The flag as it is appended to a command, or "" for the default agent (no
// name given). Callers return INVALID_INSTANCE_COMMAND before reaching this
// with an invalid name.
function instanceFlag(instance?: string): string {
  const name = (instance ?? "").trim();
  return name && validInstanceName(name) ? ` --instance ${name}` : "";
}

// The default runner name. Two agents on one machine must not both be called
// $(hostname): the app offers a re-enrolled runner its old placement by name.
// The installer applies the same default when -n is not given (MA-20).
function runnerName(instance?: string): string {
  const name = (instance ?? "").trim();
  return name && validInstanceName(name) ? `$(hostname)-${name}` : "$(hostname)";
}

// Resource limits on the agent's unit (2.3.1): the installer's --memory-max,
// --cpu-quota and --tasks-max, written as MemoryMax=, CPUQuota= and TasksMax=.
// They bound the agent and everything it runs, together — the only bound an
// agent that is not root has, since it cannot give each run a cgroup of its
// own. The shapes are the installer's (runner-install.sh, check_limit), restated
// here so the helper refuses a value the script would: a whole number that does
// not start with 0, then K/M/G/T for memory (optional), % for CPU (required),
// nothing for tasks. The value goes into a unit file as given, so nothing else
// is let through to the command.
export interface UnitLimits {
  memoryMax?: string;
  cpuQuota?: string;
  tasksMax?: string;
}
export type LimitField = keyof UnitLimits;
export const LIMIT_FIELDS: readonly LimitField[] = ["memoryMax", "cpuQuota", "tasksMax"];
const LIMIT_RE: Record<LimitField, RegExp> = {
  memoryMax: /^[1-9][0-9]*[KMGT]?$/,
  cpuQuota: /^[1-9][0-9]*%$/,
  tasksMax: /^[1-9][0-9]*$/,
};
const LIMIT_FLAG: Record<LimitField, string> = {
  memoryMax: "--memory-max",
  cpuQuota: "--cpu-quota",
  tasksMax: "--tasks-max",
};

export function validLimit(field: LimitField, value: string): boolean {
  return LIMIT_RE[field].test(value);
}

// The fields that hold something the installer would refuse. Empty is valid:
// no limit asked for, no flag written.
export function invalidLimits(limits?: UnitLimits): LimitField[] {
  return LIMIT_FIELDS.filter((f) => {
    const v = (limits?.[f] ?? "").trim();
    return v !== "" && !validLimit(f, v);
  });
}

export function limitsInvalid(limits?: UnitLimits): boolean {
  return invalidLimits(limits).length > 0;
}

// What every builder returns while a limit field holds something the installer
// would refuse. Not the command without the limit: an administrator who typed a
// limit and pasted a command that silently lacked it would believe the agent
// bounded. A shell comment, so that pasting it does nothing.
export const INVALID_LIMITS_COMMAND = "# A resource limit is not valid, so no install command is shown. Correct it above.";

// The flags as they are appended to a command, or "" when none is set. Callers
// return INVALID_LIMITS_COMMAND before reaching this with an invalid value.
function limitFlags(limits?: UnitLimits): string {
  return LIMIT_FIELDS.map((f) => {
    const v = (limits?.[f] ?? "").trim();
    return v && validLimit(f, v) ? ` ${LIMIT_FLAG[f]} ${v}` : "";
  }).join("");
}

// The one refusal every builder makes first, or null when there is a command
// to show.
function refusal(instance?: string, limits?: UnitLimits): string | null {
  if (instanceInvalid(instance)) return INVALID_INSTANCE_COMMAND;
  if (limitsInvalid(limits)) return INVALID_LIMITS_COMMAND;
  return null;
}

// One-click install (runner provisioning plan 2 Phase 2, D2: 2A). The server's
// GET /install/<token> endpoint bakes the server URL, token, and binary
// download into runner-install.sh, so the install is a single flagless pipe —
// no -s/-t/-c to carry. The token in the path IS the credential.
export function personalizedInstallUrl(origin: string, token: string): string {
  return `${origin}/install/${token}`;
}

// The headline copy-paste command for the Add Runner flow. The baked script
// still parses its arguments, so an instance is `bash -s -- --instance <name>`,
// and limits follow the same way.
export function personalizedInstallOneLiner(origin: string, token: string, instance?: string, limits?: UnitLimits): string {
  const no = refusal(instance, limits);
  if (no) return no;
  const flags = instanceFlag(instance) + limitFlags(limits);
  return `curl -fsSL ${personalizedInstallUrl(origin, token)} | sudo bash${flags ? ` -s --${flags}` : ""}`;
}

// The download-inspect-run variant, for orgs that ban curl-pipe-to-sudo. The
// saved file already has the server URL + token baked in, so it runs with no
// arguments too.
export function personalizedInstallTwoStep(origin: string, token: string, instance?: string, limits?: UnitLimits): string {
  const no = refusal(instance, limits);
  if (no) return no;
  const url = personalizedInstallUrl(origin, token);
  return [
    `curl -fsSL ${url} -o runner-install.sh`,
    `less runner-install.sh   # server URL + token are baked in — inspect before running`,
    `sudo bash runner-install.sh${instanceFlag(instance)}${limitFlags(limits)}`,
  ].join("\n");
}

// No -c by default: the agent auto-detects the host's run-types at startup
// (D1: 1B); pass capabilities only to narrow what the runner claims.
export function installOneLiner(origin: string, token: string, capabilities?: string, instance?: string, limits?: UnitLimits): string {
  const no = refusal(instance, limits);
  if (no) return no;
  return (
    `curl -fsSL ${origin}${RUNNER_INSTALL_SCRIPT_PATH} | ` +
    `sudo bash -s -- -s ${origin} -t ${token} -n ${runnerName(instance)}${capabilities ? ` -c ${capabilities}` : ""} --download${instanceFlag(instance)}${limitFlags(limits)}`
  );
}

// The download-inspect-run variant, for orgs that ban curl-pipe-to-sudo.
export function installTwoStep(origin: string, token: string, capabilities?: string, instance?: string, limits?: UnitLimits): string {
  const no = refusal(instance, limits);
  if (no) return no;
  return [
    `curl -fsSLO ${origin}${RUNNER_INSTALL_SCRIPT_PATH}`,
    `less runner-install.sh   # inspect before running`,
    `sudo bash runner-install.sh -s ${origin} -t ${token} -n ${runnerName(instance)}${capabilities ? ` -c ${capabilities}` : ""} --download${instanceFlag(instance)}${limitFlags(limits)}`,
  ].join("\n");
}

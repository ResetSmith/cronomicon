// Builder for the runner settings drawer's "Copy limits command" (2.3.1).
//
// A limit on an agent's systemd unit (MemoryMax, CPUQuota, TasksMax) bounds the
// agent and everything it runs, together. It is the only bound an agent that is
// not root has: it cannot create the per-run scope the sandbox caps ride on.
// The installer writes limits at install (--memory-max, --cpu-quota,
// --tasks-max); this is how they are changed afterwards.
//
// Why a copy-paste command and not a setting the server pushes: the agent
// cannot change its own unit either. `systemctl set-property` on its own unit
// is refused to it exactly as a new scope is ("Interactive authentication
// required", RHEL 8.10, 2026-10-08), and the server has no other way onto the
// machine. So, as with the upgrade command, the app prepares the command and
// root on the host runs it.
//
// The command names the runner by its ID, not by a unit name the server would
// have to guess (it is told a runner's name, never its instance): it finds the
// agent whose identity file holds that ID and acts on that agent's unit. Pasted
// on any other machine it finds nothing, says so and changes nothing.
//
// `set-property` applies at once, without a restart, and keeps the value in
// /etc/systemd/system.control/<unit>.d/, where it wins over the unit file's.

import { LIMIT_FIELDS, validLimit, type LimitField, type UnitLimits } from "./runner-install-cmd";

/** What a limit field holds to REMOVE that limit. */
export const NO_LIMIT = "none";

const PROPERTY: Record<LimitField, string> = {
  memoryMax: "MemoryMax",
  cpuQuota: "CPUQuota",
  tasksMax: "TasksMax",
};

// How each limit is removed. Not one spelling for all three: systemd 239
// (RHEL 8) refuses CPUQuota=infinity ("CPU quota 'infinity' invalid") and takes
// the empty assignment, while MemoryMax and TasksMax take "infinity".
const REMOVE: Record<LimitField, string> = {
  memoryMax: "MemoryMax=infinity",
  cpuQuota: "CPUQuota=",
  tasksMax: "TasksMax=infinity",
};

/** A value the drawer accepts: empty (leave as it is), `none`, or the installer's shape. */
export function validLimitChange(field: LimitField, value: string): boolean {
  const v = value.trim();
  return v === "" || v === NO_LIMIT || validLimit(field, v);
}

/** The fields that hold something that is none of those. */
export function invalidLimitChanges(limits: UnitLimits): LimitField[] {
  return LIMIT_FIELDS.filter((f) => !validLimitChange(f, limits[f] ?? ""));
}

/** The `Name=value` assignments the command makes, in a fixed order; none for an empty field. */
export function limitAssignments(limits: UnitLimits): string[] {
  const out: string[] = [];
  for (const f of LIMIT_FIELDS) {
    const v = (limits[f] ?? "").trim();
    if (v === "") continue;
    out.push(v === NO_LIMIT ? REMOVE[f] : `${PROPERTY[f]}=${v}`);
  }
  return out;
}

// A runner ID is a UUID the server minted. It goes into a script that root
// runs, so it is held to that shape here, not trusted because of where it
// came from.
const RUNNER_ID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;

/**
 * limitsCommand returns the paste-once block that sets (or removes) the unit
 * limits of the agent with this runner ID, or "" when there is nothing to
 * run: no field filled in, a field that is not valid, or an ID that is not one.
 *
 * Delivered as `sudo bash -s <<'SH' … SH`, a QUOTED heredoc like the upgrade
 * command's, so the pasting shell expands nothing.
 */
export function limitsCommand(runnerId: string, limits: UnitLimits): string {
  if (!RUNNER_ID_RE.test(runnerId)) return "";
  if (invalidLimitChanges(limits).length > 0) return "";
  const assignments = limitAssignments(limits);
  if (assignments.length === 0) return "";
  return [
    `sudo bash -s <<'SH'`,
    `set -euo pipefail`,
    ``,
    `RUNNER_ID="${runnerId}"`,
    ``,
    `# The agent on this machine that IS this runner: the one whose identity file`,
    `# holds the ID. A machine may run several (runner-install.sh --instance).`,
    `UNIT=""`,
    `for DIR in /var/lib/cronomicon-runner /var/lib/cronomicon-runner-*; do`,
    `  if [ -f "$DIR/identity.json" ] && grep -q "\\"$RUNNER_ID\\"" "$DIR/identity.json"; then`,
    `    UNIT="$(basename "$DIR").service"`,
    `  fi`,
    `done`,
    `if [ -z "$UNIT" ]; then`,
    `  echo "No agent on this machine is runner $RUNNER_ID. Nothing was changed." >&2`,
    `  exit 1`,
    `fi`,
    ``,
    `echo ">> Setting limits on \${UNIT} (the agent and everything it runs, together) ..."`,
    `systemctl set-property "$UNIT" ${assignments.join(" ")}`,
    `# set-property writes a drop-in, and systemd 239 then calls the unit "changed`,
    `# on disk" and warns on every later systemctl command until it is reloaded.`,
    `# The values set above are kept across the reload.`,
    `systemctl daemon-reload`,
    `echo ">> \${UNIT} now has:"`,
    `systemctl show "$UNIT" -p MemoryMax -p CPUQuotaPerSecUSec -p TasksMax`,
    `SH`,
  ].join("\n");
}

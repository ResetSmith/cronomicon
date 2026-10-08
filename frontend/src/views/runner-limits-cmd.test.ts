import { describe, expect, it } from "vitest";
import { NO_LIMIT, invalidLimitChanges, limitAssignments, limitsCommand, validLimitChange } from "./runner-limits-cmd";

const ID = "01a11cd7-2939-7abe-ae1b-8862a7262645";

// The runner settings drawer's "Unit limits" section (2.3.1). The server cannot
// set a limit on an agent's unit and neither can the agent, so the section
// builds a command for root on the machine. What the command owes:
//
//   - it changes only what was filled in, and can REMOVE a limit;
//   - it removes each one with the spelling its systemd accepts;
//   - it acts on the agent that IS this runner, found by ID, and on no other;
//   - nothing typed into a field can become anything but a limit.
describe("limitsCommand", () => {
  it("sets the limits that were filled in, on the unit of the agent with this runner ID", () => {
    const cmd = limitsCommand(ID, { memoryMax: "4G", cpuQuota: "200%", tasksMax: "1024" });
    expect(cmd.startsWith("sudo bash -s <<'SH'\n")).toBe(true);
    expect(cmd.endsWith("\nSH")).toBe(true);
    expect(cmd).toContain("set -euo pipefail");
    expect(cmd).toContain(`RUNNER_ID="${ID}"`);
    expect(cmd).toContain('systemctl set-property "$UNIT" MemoryMax=4G CPUQuota=200% TasksMax=1024');
    // It reads the values back, so the paste shows what the unit now has.
    expect(cmd).toContain('systemctl show "$UNIT" -p MemoryMax -p CPUQuotaPerSecUSec -p TasksMax');
  });

  it("names the runner by ID and finds the unit on the machine: the default agent's or an instance's", () => {
    const cmd = limitsCommand(ID, { memoryMax: "4G" });
    expect(cmd).toContain("for DIR in /var/lib/cronomicon-runner /var/lib/cronomicon-runner-*; do");
    // The ID is matched as a whole JSON string, not as a substring of another.
    expect(cmd).toContain('grep -q "\\"$RUNNER_ID\\"" "$DIR/identity.json"');
    expect(cmd).toContain('UNIT="$(basename "$DIR").service"');
    // No unit name is written into the command: the server is never told one.
    expect(cmd).not.toMatch(/set-property "?cronomicon-runner/);
  });

  it("changes nothing on a machine where this runner is not installed, and says so", () => {
    const cmd = limitsCommand(ID, { memoryMax: "4G" });
    const refuse = cmd.indexOf('if [ -z "$UNIT" ]; then');
    const act = cmd.indexOf("systemctl set-property");
    expect(refuse).toBeGreaterThan(-1);
    expect(act).toBeGreaterThan(refuse);
    expect(cmd).toContain("Nothing was changed.");
    expect(cmd.slice(refuse, act)).toContain("exit 1");
  });

  // Seen on RHEL 8.10: after set-property every later `systemctl restart` of the
  // unit warned that it had "changed on disk" until a daemon-reload. The command
  // does the reload itself, after the change and before it reads the values back.
  it("reloads systemd after the change, so the unit is not left marked as changed on disk", () => {
    const lines = limitsCommand(ID, { memoryMax: "4G" }).split("\n");
    const set = lines.findIndex((l) => l.startsWith("systemctl set-property "));
    const reload = lines.indexOf("systemctl daemon-reload");
    const show = lines.findIndex((l) => l.startsWith("systemctl show "));
    expect(set).toBeGreaterThan(-1);
    expect(reload).toBeGreaterThan(set);
    expect(show).toBeGreaterThan(reload);
  });

  it("leaves a limit alone when its field is empty", () => {
    expect(limitAssignments({ cpuQuota: "150%" })).toEqual(["CPUQuota=150%"]);
    expect(limitAssignments({ memoryMax: " 2G ", cpuQuota: "", tasksMax: "   " })).toEqual(["MemoryMax=2G"]);
    expect(limitsCommand(ID, { cpuQuota: "150%" })).toContain('systemctl set-property "$UNIT" CPUQuota=150%\n');
  });

  // systemd 239 (RHEL 8) refuses CPUQuota=infinity and takes the empty
  // assignment; MemoryMax and TasksMax take "infinity". Checked on RHEL 8.10.
  it("removes each limit with the spelling systemd accepts for it", () => {
    expect(limitAssignments({ memoryMax: NO_LIMIT, cpuQuota: NO_LIMIT, tasksMax: NO_LIMIT })).toEqual([
      "MemoryMax=infinity",
      "CPUQuota=",
      "TasksMax=infinity",
    ]);
    const cmd = limitsCommand(ID, { memoryMax: "8G", cpuQuota: NO_LIMIT });
    expect(cmd).toContain('systemctl set-property "$UNIT" MemoryMax=8G CPUQuota=\n');
    expect(cmd).not.toContain("CPUQuota=infinity");
  });

  it("builds nothing when no field is filled in", () => {
    expect(limitsCommand(ID, {})).toBe("");
    expect(limitsCommand(ID, { memoryMax: "", cpuQuota: " ", tasksMax: "" })).toBe("");
  });

  it("accepts the installer's shapes and `none`, and nothing else", () => {
    for (const ok of ["", "4G", "512M", "4096", NO_LIMIT]) expect(validLimitChange("memoryMax", ok), ok).toBe(true);
    for (const bad of ["0", "4GB", "1.5G", "4g", "50%", "infinity", "None", "4G CPUQuota=1%", "$(id)", "4G;reboot"]) {
      expect(validLimitChange("memoryMax", bad), bad).toBe(false);
    }
    expect(validLimitChange("cpuQuota", "200%")).toBe(true);
    expect(validLimitChange("cpuQuota", "200")).toBe(false);
    expect(validLimitChange("tasksMax", "1024")).toBe(true);
    expect(validLimitChange("tasksMax", "10%")).toBe(false);
    expect(invalidLimitChanges({ memoryMax: "4GB", cpuQuota: "200%", tasksMax: "x" })).toEqual(["memoryMax", "tasksMax"]);
  });

  // The block is run by root. A value that is not a limit yields no command at
  // all — never the command minus the bad field, which would read as done.
  it("builds nothing while any field is invalid, whatever else is filled in", () => {
    for (const bad of ["4G CPUQuota=1%", "4G\nreboot", "$(id)", "`id`", '4G" x', "4G;reboot", "infinity"]) {
      expect(limitsCommand(ID, { memoryMax: bad, cpuQuota: "200%" }), bad).toBe("");
    }
  });

  it("builds nothing for an ID that is not a runner ID", () => {
    for (const bad of ["", "r-fin", `${ID}"; reboot; "`, "$(id)", ID.toUpperCase(), `${ID}\n`]) {
      expect(limitsCommand(bad, { memoryMax: "4G" }), bad).toBe("");
    }
  });

  it("uses a quoted heredoc and puts nothing of the caller's outside its two checked places", () => {
    const cmd = limitsCommand(ID, { memoryMax: "4G", cpuQuota: "200%", tasksMax: "1024" });
    const body = cmd.split("\n").slice(1, -1).join("\n");
    // The terminator cannot appear inside the block.
    expect(body.split("\n")).not.toContain("SH");
    // Every $ in the block is the script's own.
    expect(body.match(/\$\(?[A-Za-z_{]+/g)?.every((v) => /^\$(\(basename|RUNNER_ID|DIR|UNIT|\{UNIT)/.test(v))).toBe(true);
  });
});

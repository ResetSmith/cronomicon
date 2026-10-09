import { describe, expect, it } from "vitest";
import {
  AGENT_BIN_PATH,
  AGENT_UNIT,
  AGENT_UNIT_PATTERNS,
  KILLMODE_DROPIN,
  RESTART_WATCH_SECONDS,
  SYSCALL_DROPIN,
  upgradeCommand,
} from "./runner-upgrade-cmd";

const ORIGIN = "https://cronomicon.example.com";

// These assertions guard the CONTRACT with runner-install.sh (the paths and the
// /agents/ endpoints it installs to and fetches from). The script's runtime
// behavior — download, checksum verification, atomic swap, rollback on a failed
// start — was verified by executing the generated body against a local agent
// server; see the RU1 notes in the CHANGELOG. What can silently rot here is the
// install-script contract, so that is what is pinned.
describe("upgradeCommand", () => {
  it("targets the same binary path and unit runner-install.sh installs", () => {
    expect(AGENT_BIN_PATH).toBe("/usr/local/bin/cronomicon-runner");
    expect(AGENT_UNIT).toBe("cronomicon-runner");
    const cmd = upgradeCommand(ORIGIN);
    expect(cmd).toContain(`BIN="${AGENT_BIN_PATH}"`);
    // With no enabled agent unit found it restarts the default one, as always.
    expect(cmd).toContain(`if [ -z "$UNITS" ]; then UNITS="${AGENT_UNIT}.service"; fi`);
  });

  // MA-22 — the command upgrades a machine. Every agent on it shares the one
  // binary, so every ENABLED agent unit restarts: the default one, and one per
  // `runner-install.sh --instance <name>` (cronomicon-runner-<name>.service).
  // Restarting only one would leave the others running the old process.
  it("restarts every enabled agent unit on the machine, and reports each", () => {
    expect([...AGENT_UNIT_PATTERNS]).toEqual(["cronomicon-runner.service", "cronomicon-runner-*.service"]);
    const cmd = upgradeCommand(ORIGIN);
    expect(cmd).toContain(`systemctl list-unit-files --no-legend 'cronomicon-runner.service' 'cronomicon-runner-*.service'`);
    // Enabled OR running. Not every unit present: an instance being removed is
    // stopped and disabled first, and an upgrade must not start it again. But a
    // unit that is running without being enabled must not stay on the old
    // process, unreported.
    expect(cmd).toContain(`awk '$2 == "enabled" { print $1 }'`);
    expect(cmd).toContain(`systemctl list-units --no-legend --plain --type=service --state=active 'cronomicon-runner.service' 'cronomicon-runner-*.service'`);
    expect(cmd).toContain("| sort -u || true)");
    expect(cmd).toContain("systemctl restart $UNITS || true");
    expect(cmd).toContain('for U in $UNITS; do');
    expect(cmd).toContain('systemctl is-active --quiet "$U"');
    expect(cmd).toContain('echo ">> OK: ${U}"');
    expect(cmd).toContain('echo ">> FAILED to start: ${U}"');
    // The restart must not abort the report under `set -e`, and a failure must
    // still fail the command.
    expect(cmd.indexOf("systemctl restart $UNITS || true")).toBeLessThan(cmd.indexOf('if [ -n "$NOT_UP" ]; then'));
    expect(cmd).toMatch(/if \[ -n "\$NOT_UP" \]; then\n.*\n {2}exit 1\nfi/);
  });

  // One look at `is-active` passes an agent that is about to exit, and passes
  // a crash-looping one whenever the look lands while systemd has it up again.
  // The unit must be running at the end of the watch as the process it was at
  // the start. Executed against a stand-in systemctl for a unit that stays up,
  // one that restarts itself and one that never starts.
  it("passes a unit only when it is still the same process after the watch", () => {
    const cmd = upgradeCommand(ORIGIN);
    expect(RESTART_WATCH_SECONDS).toBeGreaterThanOrEqual(15);
    expect(cmd).toContain(`sleep ${RESTART_WATCH_SECONDS}`);
    expect(cmd).toContain('systemctl show -p MainPID "$1"');
    const restartAt = cmd.indexOf("systemctl restart $UNITS || true");
    const startedAt = cmd.indexOf('STARTED="${STARTED} $(main_pid "$U")"');
    const sleepAt = cmd.indexOf(`sleep ${RESTART_WATCH_SECONDS}`);
    expect(startedAt).toBeGreaterThan(restartAt);
    expect(sleepAt).toBeGreaterThan(startedAt);
    // A process id of 0 at the start is a unit that did not start.
    expect(cmd).toContain('elif [ "$WAS" = 0 ] || [ "$WAS" != "$(main_pid "$U")" ]; then');
    expect(cmd).toContain('echo ">> FAILED to stay up: ${U} has restarted itself since the upgrade');
  });

  // A unit written before 2.3.0 filters system calls with no error number, and
  // systemd 239 (RHEL 8) kills the agent's PATH lookups for it. The installer
  // cannot be re-run on an enrolled agent, so the upgrade carries the line as a
  // drop-in, before the restart, to the units that lack it and to no others.
  it("adds SystemCallErrorNumber=EPERM to a unit that filters without it", () => {
    const cmd = upgradeCommand(ORIGIN);
    expect(SYSCALL_DROPIN).toBe("10-syscall-errno.conf");
    expect(cmd).toContain(`grep -q '^SystemCallFilter=' <<<"$CONF" && ! grep -q '^SystemCallErrorNumber=' <<<"$CONF"`);
    expect(cmd).toContain(
      `printf '[Service]\\nSystemCallErrorNumber=EPERM\\n' > "/etc/systemd/system/\${U}.d/${SYSCALL_DROPIN}"`,
    );
    expect(cmd).toContain('if [ "$RELOAD" = 1 ]; then systemctl daemon-reload; fi');
    expect(cmd.indexOf("systemctl daemon-reload")).toBeLessThan(cmd.indexOf("systemctl restart $UNITS || true"));
  });

  // A unit written before 2.3.2 names no KillMode, and systemd's default sends
  // the stop signal to every process of the unit: an Ansible or Terraform run,
  // which is the agent's child, is cut off by the stop its agent is draining.
  // The line goes in before the restart, so that restart is already a drain;
  // a unit that names a KillMode of its own is left alone, and so is the
  // fallback unit name on a machine where no unit was found (an empty $CONF).
  it("adds KillMode=mixed to a unit that names none, before the restart", () => {
    const cmd = upgradeCommand(ORIGIN);
    expect(KILLMODE_DROPIN).toBe("20-killmode.conf");
    expect(cmd).toContain(`if [ -n "$CONF" ] && ! grep -q '^KillMode=' <<<"$CONF"; then`);
    expect(cmd).toContain(
      `printf '[Service]\\nKillMode=mixed\\n' > "/etc/systemd/system/\${U}.d/${KILLMODE_DROPIN}"`,
    );
    expect(cmd.indexOf("KillMode=mixed")).toBeLessThan(cmd.indexOf("systemctl daemon-reload"));
    expect(cmd.indexOf("systemctl daemon-reload")).toBeLessThan(cmd.indexOf("systemctl restart $UNITS || true"));
  });

  it("fetches the binary and its checksums from the given origin's /agents/", () => {
    const cmd = upgradeCommand(ORIGIN);
    expect(cmd).toContain(`ORIGIN="${ORIGIN}"`);
    expect(cmd).toContain("${ORIGIN}/agents/${FILE}");
    expect(cmd).toContain("${ORIGIN}/agents/SHA256SUMS");
  });

  it("maps arch exactly like runner-install.sh --download", () => {
    const cmd = upgradeCommand(ORIGIN);
    expect(cmd).toContain("x86_64|amd64)  ARCH=amd64");
    expect(cmd).toContain("aarch64|arm64) ARCH=arm64");
    expect(cmd).toContain("cronomicon-runner-linux-${ARCH}");
  });

  // A mismatch must abort BEFORE anything is installed. `set -euo pipefail` plus
  // the verification running ahead of the swap is what makes that true.
  it("verifies the checksum before the swap, and aborts on failure", () => {
    const cmd = upgradeCommand(ORIGIN);
    expect(cmd).toContain("set -euo pipefail");
    const verifyAt = cmd.indexOf("sha256sum -c -");
    const swapAt = cmd.indexOf("install -m 0755");
    expect(verifyAt).toBeGreaterThan(-1);
    expect(swapAt).toBeGreaterThan(verifyAt);
  });

  // Regression: `[ -f "$BIN" ] && cp …` makes the false test the failing last
  // command of an AND-list, which under `set -e` aborts the upgrade on a host
  // that has no binary yet. Verified by executing the script with no prior
  // binary present.
  it("guards the backup with an if, never a bare AND-list", () => {
    const cmd = upgradeCommand(ORIGIN);
    expect(cmd).toContain(`if [ -f "$BIN" ]; then cp -a "$BIN" "\${BIN}.prev"; fi`);
    expect(cmd).not.toMatch(/^\[ -f "\$BIN" \] && cp/m);
  });

  it("keeps the previous binary and tells the operator how to roll back", () => {
    const cmd = upgradeCommand(ORIGIN);
    expect(cmd).toContain("${BIN}.prev");
    // The roll-back restarts the same units the upgrade did.
    expect(cmd).toContain("sudo mv -f ${BIN}.prev $BIN && sudo systemctl restart ${UNITS}");
  });

  // The quoted heredoc is what keeps the pasted-into shell from expanding the
  // script's own $VAR / $(…) before bash ever sees them.
  it("delivers as a quoted heredoc so the operator's shell expands nothing", () => {
    const cmd = upgradeCommand(ORIGIN);
    expect(cmd.startsWith("sudo bash -s <<'SH'\n")).toBe(true);
    expect(cmd.endsWith("\nSH")).toBe(true);
  });

  it("renders the origin verbatim, including a port", () => {
    expect(upgradeCommand("http://10.0.0.5:8080")).toContain(`ORIGIN="http://10.0.0.5:8080"`);
  });
});

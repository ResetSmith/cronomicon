// Builder for the runner detail's "Copy upgrade command" (RU1).
//
// Upgrading an installed agent is binary-swap + restart: the runner keeps its
// id, API key, config and unit — only /usr/local/bin/cronomicon-runner changes.
// The one exception is a drop-in for a unit that filters system calls without
// an error number (every unit written before 2.3.0): see SYSCALL_DROPIN.
//
// The command upgrades a MACHINE, not a runner (MA-22). A machine may run
// several agents (runner-install.sh --instance), each with a unit of its own,
// and they all share the one binary. Restarting only the unit whose row the
// operator happened to copy the command from would leave the others running the
// old process against a new file, so every ENABLED agent unit is restarted and
// each is reported.
// Re-running runner-install.sh would also work but is the wrong tool: it creates
// users, writes the unit, and needs a registration token the operator no longer
// has.
//
// Why a copy-paste command rather than a server-pushed self-update: a poll
// control op only reaches agents that already speak the protocol version which
// introduced it, so a push would be dark on exactly the runners most in need of
// upgrading (the ones too old to receive it). This works on ANY agent version,
// needs no new privilege model on the host, and adds no remote-code-execution
// surface to the control plane. See the runner-update discussion for the
// self-update design this defers.
//
// The download + verification steps mirror runner-install.sh --download exactly
// (arch mapping, /agents/ URLs, `grep <file> SHA256SUMS | sha256sum -c`) so the
// two can't drift in behavior. A checksum mismatch is a HARD failure here too —
// never a fallback.

/** Where runner-install.sh installs the agent binary. */
export const AGENT_BIN_PATH = "/usr/local/bin/cronomicon-runner";
/** The systemd unit runner-install.sh writes and enables for the default agent. */
export const AGENT_UNIT = "cronomicon-runner";
/**
 * Every agent unit a machine may have: the default agent's, and one per
 * `--instance <name>` (cronomicon-runner-<name>.service). systemctl patterns.
 */
export const AGENT_UNIT_PATTERNS = [`${AGENT_UNIT}.service`, `${AGENT_UNIT}-*.service`] as const;
/**
 * The drop-in the upgrade writes beside a unit that needs it. runner-install.sh
 * has written `SystemCallErrorNumber=EPERM` into the unit itself since 2.3.0;
 * a unit from an older installer has the filter without it, and systemd 239
 * (RHEL 8) then KILLS the thread that makes a call the filter does not know.
 * Go's PATH lookup makes one, so on those hosts the agent never finds ansible,
 * terraform or systemd-run. Re-running the installer is not a way to fix an
 * enrolled agent (it needs a new token and rewrites the config from flags), so
 * the upgrade is what carries the line to the units that exist.
 */
export const SYSCALL_DROPIN = "10-syscall-errno.conf";
/** How long each restarted unit is watched before it is reported, in seconds. */
export const RESTART_WATCH_SECONDS = 20;

/**
 * upgradeCommand returns the paste-once block that upgrades an installed agent
 * to the binary this server publishes at /agents/.
 *
 * Delivered as `sudo bash -s <<'SH' … SH` — a QUOTED heredoc, so the shell the
 * operator pastes into performs no expansion of its own: every `$VAR`, `$(…)`
 * and backslash reaches bash verbatim. That removes the whole class of quoting
 * bugs a nested `bash -c '…'` one-liner invites, while still being a single
 * copy-paste unit.
 *
 * Safety properties, in order of the script:
 *   - `set -euo pipefail` — any failed step aborts before the swap.
 *   - checksum verified against the server's SHA256SUMS before anything is
 *     installed; a mismatch exits non-zero and the running agent is untouched.
 *   - the previous binary is kept at <path>.prev so a bad build can be rolled
 *     back by hand — the control channel can't help once the agent won't start.
 *   - the new binary is staged beside the target and moved into place with
 *     `mv -f` (atomic rename on the same filesystem). The RUNNING process keeps
 *     its own inode, so the swap can't corrupt the in-flight agent; the restart
 *     is what picks up the new code.
 *   - a unit that filters system calls without an error number gets the
 *     SYSCALL_DROPIN drop-in before it restarts; a unit that already has the
 *     line, or no filter, is left alone.
 *   - every agent unit that is ENABLED OR RUNNING is restarted, and each is
 *     watched and reported on its own line. "Active" at one moment proves
 *     nothing: an agent that exits some seconds in is started again by systemd
 *     and is active again each time. A unit passes when it is running at the
 *     end of the watch AND is the same process it was at the start.
 *     Not every unit present is restarted: an
 *     instance being removed is stopped and disabled first, and must not be
 *     started again by an upgrade. But a unit that is running without being
 *     enabled must not be left on the old process either. A machine where none
 *     is found (an agent installed some other way) falls back to the default
 *     unit, as this command always did.
 */
export function upgradeCommand(origin: string): string {
  return [
    `sudo bash -s <<'SH'`,
    `set -euo pipefail`,
    ``,
    `ORIGIN="${origin}"`,
    `BIN="${AGENT_BIN_PATH}"`,
    ``,
    `# Same arch mapping as runner-install.sh --download.`,
    `case "$(uname -m)" in`,
    `  x86_64|amd64)  ARCH=amd64 ;;`,
    `  aarch64|arm64) ARCH=arm64 ;;`,
    `  *) echo "no published cronomicon-runner binary for $(uname -m)" >&2; exit 1 ;;`,
    `esac`,
    `FILE="cronomicon-runner-linux-\${ARCH}"`,
    ``,
    `DL=$(mktemp -d)`,
    `trap 'rm -rf "$DL"' EXIT`,
    ``,
    `echo ">> Downloading \${FILE} from \${ORIGIN}/agents/ ..."`,
    `curl -fsSL -o "\${DL}/\${FILE}"   "\${ORIGIN}/agents/\${FILE}"`,
    `curl -fsSL -o "\${DL}/SHA256SUMS" "\${ORIGIN}/agents/SHA256SUMS"`,
    ``,
    `echo ">> Verifying checksum..."`,
    `(cd "$DL" && grep " \${FILE}\\$" SHA256SUMS | sha256sum -c -)`,
    ``,
    `echo ">> Swapping binary (previous kept at \${BIN}.prev) ..."`,
    `# NOT \`[ -f "$BIN" ] && cp …\`: under \`set -e\` a false test makes the whole`,
    `# AND-list the failing last command, aborting the upgrade on a host that simply`,
    `# has no binary yet.`,
    `if [ -f "$BIN" ]; then cp -a "$BIN" "\${BIN}.prev"; fi`,
    `install -m 0755 "\${DL}/\${FILE}" "\${BIN}.new"`,
    `mv -f "\${BIN}.new" "$BIN"`,
    ``,
    `# One binary serves every agent on this machine, so every agent restarts:`,
    `# the default unit and each --instance unit, when it is enabled or running.`,
    `UNITS=$({ systemctl list-unit-files --no-legend ${AGENT_UNIT_PATTERNS.map((u) => `'${u}'`).join(" ")} 2>/dev/null | awk '$2 == "enabled" { print $1 }'; systemctl list-units --no-legend --plain --type=service --state=active ${AGENT_UNIT_PATTERNS.map((u) => `'${u}'`).join(" ")} 2>/dev/null | awk '{ print $1 }'; } | sort -u || true)`,
    `if [ -z "$UNITS" ]; then UNITS="${AGENT_UNIT}.service"; fi`,
    `UNITS=$(echo $UNITS)`,
    ``,
    `# A unit from an installer older than 2.3.0 filters system calls without`,
    `# SystemCallErrorNumber. On systemd 239 (RHEL 8) the agent's PATH lookups`,
    `# then never return and it finds no ansible, terraform or sandbox.`,
    `RELOAD=0`,
    `for U in $UNITS; do`,
    `  CONF=$(systemctl cat "$U" 2>/dev/null || true)`,
    `  if grep -q '^SystemCallFilter=' <<<"$CONF" && ! grep -q '^SystemCallErrorNumber=' <<<"$CONF"; then`,
    `    mkdir -p "/etc/systemd/system/\${U}.d"`,
    `    printf '[Service]\\nSystemCallErrorNumber=EPERM\\n' > "/etc/systemd/system/\${U}.d/${SYSCALL_DROPIN}"`,
    `    echo ">> Added SystemCallErrorNumber=EPERM to \${U} (/etc/systemd/system/\${U}.d/${SYSCALL_DROPIN})"`,
    `    RELOAD=1`,
    `  fi`,
    `done`,
    `if [ "$RELOAD" = 1 ]; then systemctl daemon-reload; fi`,
    ``,
    `main_pid() { P=$(systemctl show -p MainPID "$1" 2>/dev/null | cut -d= -f2 || true); case "$P" in ''|*[!0-9]*) echo 0 ;; *) echo "$P" ;; esac; }`,
    ``,
    `echo ">> Restarting: \${UNITS}"`,
    `systemctl restart $UNITS || true`,
    `# "active" is true of an agent that is about to fail, until it does, and`,
    `# true again each time systemd starts it afresh. So a unit passes only if`,
    `# it is running at the end of the watch as the SAME process it started as.`,
    `STARTED=""`,
    `for U in $UNITS; do STARTED="\${STARTED} $(main_pid "$U")"; done`,
    `echo ">> Watching for ${RESTART_WATCH_SECONDS}s that each agent stays up ..."`,
    `sleep ${RESTART_WATCH_SECONDS}`,
    `NOT_UP=""`,
    `set -- $STARTED`,
    `for U in $UNITS; do`,
    `  WAS="$1"; shift`,
    `  if ! systemctl is-active --quiet "$U"; then`,
    `    echo ">> FAILED to start: \${U}" >&2; NOT_UP="\${NOT_UP} \${U}"`,
    `  elif [ "$WAS" = 0 ] || [ "$WAS" != "$(main_pid "$U")" ]; then`,
    `    echo ">> FAILED to stay up: \${U} has restarted itself since the upgrade (journalctl -u \${U})" >&2; NOT_UP="\${NOT_UP} \${U}"`,
    `  else`,
    `    echo ">> OK: \${U}"`,
    `  fi`,
    `done`,
    `if [ -n "$NOT_UP" ]; then`,
    `  echo ">> Roll back with: sudo mv -f \${BIN}.prev $BIN && sudo systemctl restart \${UNITS}" >&2`,
    `  exit 1`,
    `fi`,
    `echo ">> Now running: $("$BIN" version 2>/dev/null || echo "the published build")"`,
    `SH`,
  ].join("\n");
}

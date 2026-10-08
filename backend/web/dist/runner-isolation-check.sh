#!/bin/bash
# runner-isolation-check.sh — are the agents on this machine separated?
#
# An agent serves one agency, so a machine that serves two runs two agents:
# the default one and one per `runner-install.sh --instance <name>`. What keeps
# one agency's keys, tokens and run directories from the other's jobs is that
# each agent runs as an OS user and group of its own. This script checks that
# separation as it stands on THIS machine, after the agents are installed:
#
#   sudo bash runner-isolation-check.sh
#
# It finds every agent the installer has put here (cronomicon-runner and each
# cronomicon-runner-<name>), checks each one's account, directories, unit and
# enrolment, and then, for every pair, tries as one agent's user to read and
# write the other's files. Those attempts are expected to FAIL; one that
# succeeds is the finding.
#
# It changes nothing that an agent uses. The only things it creates are a probe
# directory per agent under /dev/shm (and /tmp), each removed before it exits.
# It needs root, because it acts as each agent's user in turn.
#
# Exit 0 = every check passed. Exit 1 = at least one FAIL. Exit 2 = it could
# not run (not root, or fewer than one agent found).

set -u

FAILURES=0
WARNINGS=0
ok()   { echo "ok    $*"; }
fail() { echo "FAIL  $*" >&2; FAILURES=$((FAILURES + 1)); }
warn() { echo "warn  $*"; WARNINGS=$((WARNINGS + 1)); }
note() { echo "      $*"; }

if [ "$(id -u)" -ne 0 ]; then
  echo "This check must be run as root (sudo): it acts as each agent's user in turn." >&2
  exit 2
fi
if ! command -v runuser >/dev/null 2>&1; then
  echo "runuser (util-linux) is required." >&2
  exit 2
fi

# as USER CMD… — run a command as an agent's user. The users have no login
# shell; runuser executes the command directly, which is all that is needed.
as() { local u="$1"; shift; runuser -u "$u" -- "$@" >/dev/null 2>&1; }

# --- Find the agents -------------------------------------------------------
# An agent is a config directory the installer wrote: /etc/cronomicon-runner or
# /etc/cronomicon-runner-<name>, holding a runner.env.
AGENTS=()
for d in /etc/cronomicon-runner /etc/cronomicon-runner-*; do
  [ -f "$d/runner.env" ] || continue
  AGENTS+=("$(basename "$d")")
done
if [ "${#AGENTS[@]}" -eq 0 ]; then
  echo "No agent found: no /etc/cronomicon-runner*/runner.env on this machine." >&2
  exit 2
fi
echo "Agents on this machine: ${AGENTS[*]}"
if [ "${#AGENTS[@]}" -lt 2 ]; then
  warn "only one agent is installed, so there is nothing to separate it from; the per-agent checks still run"
fi
echo ""

HAVE_SYSTEMD=0
if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then HAVE_SYSTEMD=1; fi
[ "$HAVE_SYSTEMD" = 1 ] || warn "systemd is not running here, so unit state is not checked"

mode_of()  { stat -L -c '%a' "$1" 2>/dev/null; }
owner_of() { stat -L -c '%U:%G' "$1" 2>/dev/null; }
env_value() { sed -n "s/^$2=//p" "$1" 2>/dev/null | head -n 1; }

# --- Each agent on its own -------------------------------------------------
USABLE=""   # agents whose user can run a command: the only ones a refusal says anything about
for a in "${AGENTS[@]}"; do
  echo "== ${a}"
  state="/var/lib/${a}"; conf="/etc/${a}"; unit="/etc/systemd/system/${a}.service"

  # The account: a user and a group of the agent's own name, the group the
  # user's primary one, and nobody else in it.
  if getent passwd "$a" >/dev/null; then ok "user ${a} exists"; else fail "user ${a} does not exist"; fi
  if getent group "$a" >/dev/null; then ok "group ${a} exists"; else fail "group ${a} does not exist"; fi
  if [ "$(id -gn "$a" 2>/dev/null)" = "$a" ]; then
    ok "user ${a}'s primary group is ${a}"
  else
    fail "user ${a}'s primary group is $(id -gn "$a" 2>/dev/null || echo '?'), not ${a}"
  fi
  members="$(getent group "$a" | cut -d: -f4)"
  if [ -z "$members" ] || [ "$members" = "$a" ]; then
    ok "group ${a} has no other member"
  else
    fail "group ${a} has other members (${members}): each of them can read this agent's tokens"
  fi
  # …and the user in no other agent's group.
  for g in $(id -Gn "$a" 2>/dev/null); do
    case "$g" in
      "$a") ;;
      cronomicon-runner|cronomicon-runner-*) fail "user ${a} is in group ${g}, another agent's: it can read that agent's config" ;;
    esac
  done

  # Positive controls. Every "refused" further down is an attempt that FAILED;
  # that is only evidence if this user can run a command at all, and can read
  # what is its own. Without these, a user runuser cannot start would pass
  # every separation check.
  if as "$a" true; then
    ok "commands can be run as ${a}"
    USABLE="${USABLE} ${a} "
  else
    fail "no command can be run as ${a} (runuser failed): its separation checks are skipped, not passed"
  fi
  if as "$a" cat "$conf/runner.env"; then
    ok "${a} can read its own runner.env"
  else
    fail "${a} cannot read its own ${conf}/runner.env: the agent cannot start"
  fi

  # The directories and the file that carries the registration token.
  if [ "$(owner_of "$state")" = "${a}:${a}" ] && [ "$(mode_of "$state")" = "750" ]; then
    ok "${state} is ${a}:${a} 0750"
  else
    fail "${state} is $(owner_of "$state") $(mode_of "$state"), want ${a}:${a} 750"
    if [ "$(mode_of "$state")" = "755" ] && ! grep -qx 'StateDirectoryMode=0750' "$unit" 2>/dev/null; then
      note "systemd resets a StateDirectory to 0755 at every start unless the unit says otherwise."
      note "Re-run the installer for this agent (it now writes StateDirectoryMode=0750), or add a drop-in:"
      note "  sudo systemctl edit ${a}    →  [Service]  StateDirectoryMode=0750   then restart the unit"
    fi
  fi
  if [ "$(owner_of "$conf")" = "root:${a}" ] && [ "$(mode_of "$conf")" = "750" ]; then
    ok "${conf} is root:${a} 0750"
  else
    fail "${conf} is $(owner_of "$conf") $(mode_of "$conf"), want root:${a} 750"
  fi
  if [ "$(owner_of "$conf/runner.env")" = "root:${a}" ] && [ "$(mode_of "$conf/runner.env")" = "640" ]; then
    ok "${conf}/runner.env is root:${a} 0640"
  else
    fail "${conf}/runner.env is $(owner_of "$conf/runner.env") $(mode_of "$conf/runner.env"), want root:${a} 640"
  fi
  # The agent's own configuration points at its own directories.
  idfile="$(env_value "$conf/runner.env" CRONOMICON_RUNNER_IDENTITY_FILE)"
  if [ "$idfile" = "${state}/identity.json" ]; then
    ok "its identity file is its own (${idfile})"
  else
    fail "runner.env names the identity file ${idfile:-<unset>}, want ${state}/identity.json"
  fi
  for var in CRONOMICON_RUNNER_KEY_DIR CRONOMICON_RUNNER_KNOWN_HOSTS CRONOMICON_RUNNER_CA_CERT \
             CRONOMICON_RUNNER_CHECKOUT_TOKEN_FILE CRONOMICON_RUNNER_VAULT_PASSWORD_FILE CRONOMICON_RUNNER_LOCAL_INVENTORY; do
    val="$(env_value "$conf/runner.env" "$var")"
    [ -n "$val" ] || continue
    case "$val" in
      "${state}/"*|"${conf}/"*) ;;
      /var/lib/cronomicon-runner*|/etc/cronomicon-runner*) fail "${var}=${val} points into ANOTHER agent's directory" ;;
    esac
  done
  # A key map is NAME=path,NAME2=path2: every path is judged the same way.
  keymap="$(env_value "$conf/runner.env" CRONOMICON_RUNNER_KEY_MAP)"
  if [ -n "$keymap" ]; then
    IFS=',' read -ra entries <<< "$keymap"
    for entry in "${entries[@]}"; do
      val="${entry#*=}"
      case "$val" in
        "${state}/"*|"${conf}/"*) ;;
        /var/lib/cronomicon-runner*|/etc/cronomicon-runner*) fail "CRONOMICON_RUNNER_KEY_MAP names ${val}, in ANOTHER agent's directory" ;;
      esac
    done
  fi

  # The unit.
  if [ -f "$unit" ]; then
    if grep -qx "User=${a}" "$unit" && grep -qx "Group=${a}" "$unit"; then
      ok "${a}.service runs as ${a}:${a}"
    else
      fail "${a}.service does not run as ${a}:${a} ($(grep -E '^(User|Group)=' "$unit" | tr '\n' ' '))"
    fi
    if grep -qx "ReadWritePaths=${state}" "$unit"; then
      if grep -qx 'ProtectSystem=strict' "$unit"; then
        ok "${a}.service may write only under ${state} (ProtectSystem=strict)"
      else
        # ReadWritePaths only carves an exception out of a read-only view; with
        # no ProtectSystem the unit can write wherever its user can.
        warn "${a}.service has reduced hardening (no ProtectSystem=strict): only file ownership keeps it out of other directories"
      fi
    elif grep -q '^ReadWritePaths=' "$unit"; then
      fail "${a}.service has $(grep '^ReadWritePaths=' "$unit"), want ${state}"
    else
      warn "${a}.service has no ReadWritePaths"
    fi
    if [ "$HAVE_SYSTEMD" = 1 ]; then
      if systemctl is-active --quiet "${a}.service"; then ok "${a}.service is active"; else warn "${a}.service is not active"; fi
      if systemctl is-enabled --quiet "${a}.service" 2>/dev/null; then ok "${a}.service is enabled"; else warn "${a}.service is not enabled (the upgrade command will not restart it)"; fi
      running_as="$(systemctl show -p MainPID --value "${a}.service" 2>/dev/null)"
      if [ -n "$running_as" ] && [ "$running_as" != 0 ]; then
        # The owner straight from /proc: no dependency on ps (a minimal host may
        # not have it), and a uid, since a long user name is truncated in listings.
        puid="$(awk '/^Uid:/ { print $2; exit }' "/proc/${running_as}/status" 2>/dev/null)"
        puser="$(getent passwd "${puid:-x}" | cut -d: -f1)"
        if [ -z "$puid" ]; then
          # Gone between the two reads: a unit that keeps restarting.
          warn "${a}.service's process (pid ${running_as}) exited while it was being checked: is the unit restarting? (journalctl -u ${a})"
        elif [ "$puid" = "$(id -u "$a" 2>/dev/null)" ]; then
          ok "the running agent process (pid ${running_as}) is ${a}'s"
        else
          fail "the running agent process (pid ${running_as}) belongs to ${puser}, not ${a}"
        fi
      fi
    fi
  else
    fail "no unit at ${unit}"
  fi

  # Enrolment: an identity file means it registered and holds an API key.
  if [ -f "${state}/identity.json" ]; then
    if [ "$(mode_of "${state}/identity.json")" = "600" ]; then
      ok "it is enrolled (identity.json, 0600)"
    else
      fail "identity.json is mode $(mode_of "${state}/identity.json"), want 600: it holds the agent's API key"
    fi
  else
    warn "it has not enrolled yet (no ${state}/identity.json): check 'journalctl -u ${a}'"
  fi
  echo ""
done

# --- Every pair: can one agent reach the other's? --------------------------
# Each line is an attempt made AS the first agent's user against the second's
# files. "ok" means the attempt was refused.
refused() {
  local desc="$1" u="$2"; shift 2
  if as "$u" "$@"; then fail "${desc}"; else ok "refused: ${desc}"; fi
}
# refused_path DESC USER PATH CMD… — the same, for an attempt on one file or
# directory. An attempt on something that is not there fails for a reason that
# has nothing to do with permissions, so it is reported as skipped, not passed.
refused_path() {
  local desc="$1" u="$2" target="$3"; shift 3
  if [ ! -e "$target" ]; then echo "skip  ${desc} (${target} does not exist)"; return; fi
  refused "$desc" "$u" "$@"
}

PROBES=()
cleanup() { for p in "${PROBES[@]:-}"; do [ -n "$p" ] && rm -rf "$p"; done; }
trap cleanup EXIT

if [ "${#AGENTS[@]}" -ge 2 ]; then
  # The shared directories. /dev/shm is where an agent writes key material for
  # a run, in a directory it makes with mode 0700. Make one the same way as
  # each agent, with a file in it, and try it from the others.
  declare -A SHM TMPD
  for a in "${AGENTS[@]}"; do
    for base in /dev/shm /tmp; do
      [ -d "$base" ] && [ -w "$base" ] || continue
      p="$(runuser -u "$a" -- mktemp -d "${base}/cronomicon-isolation-check.XXXXXX" 2>/dev/null)" || p=""
      if [ -z "$p" ]; then warn "${a} could not create a directory under ${base}"; continue; fi
      PROBES+=("$p")
      runuser -u "$a" -- bash -c 'umask 077; echo secret > "$1/key"' _ "$p"
      if [ "$base" = /dev/shm ]; then SHM[$a]="$p"; else TMPD[$a]="$p"; fi
    done
  done

  for a in "${AGENTS[@]}"; do
    case "$USABLE" in
      *" ${a} "*) ;;
      *) echo "== as ${a}: skipped (no command can be run as this user; see above)"; echo ""; continue ;;
    esac
    for b in "${AGENTS[@]}"; do
      [ "$a" = "$b" ] && continue
      echo "== as ${a}, against ${b}"
      refused_path "${a} can list ${b}'s state directory"  "$a" "/var/lib/${b}" ls "/var/lib/${b}"
      refused_path "${a} can list ${b}'s config directory" "$a" "/etc/${b}" ls "/etc/${b}"
      refused_path "${a} can read ${b}'s runner.env (its registration token)" "$a" "/etc/${b}/runner.env" cat "/etc/${b}/runner.env"
      refused_path "${a} can read ${b}'s identity (its API key)" "$a" "/var/lib/${b}/identity.json" cat "/var/lib/${b}/identity.json"
      refused_path "${a} can list ${b}'s keys"             "$a" "/var/lib/${b}/keys" ls "/var/lib/${b}/keys"
      refused_path "${a} can read ${b}'s known_hosts"      "$a" "/var/lib/${b}/known_hosts" cat "/var/lib/${b}/known_hosts"
      refused_path "${a} can create a file in ${b}'s state directory" "$a" "/var/lib/${b}" touch "/var/lib/${b}/.isolation-check"
      rm -f "/var/lib/${b}/.isolation-check"
      for f in checkout-token vault-pass; do
        [ -e "/etc/${b}/${f}" ] && refused "${a} can read ${b}'s ${f}" "$a" cat "/etc/${b}/${f}"
      done
      if [ -n "${SHM[$b]:-}" ]; then
        refused "${a} can read what ${b} wrote under /dev/shm" "$a" cat "${SHM[$b]}/key"
        refused "${a} can list ${b}'s directory under /dev/shm" "$a" ls "${SHM[$b]}"
      fi
      if [ -n "${TMPD[$b]:-}" ]; then
        refused "${a} can read what ${b} wrote under /tmp" "$a" cat "${TMPD[$b]}/key"
      fi
      # Signalling: one agent must not be able to stop the other's process.
      if [ "$HAVE_SYSTEMD" = 1 ]; then
        pid="$(systemctl show -p MainPID --value "${b}.service" 2>/dev/null)"
        if [ -n "$pid" ] && [ "$pid" != 0 ]; then
          # bash's builtin kill, so a missing /usr/bin/kill cannot read as a refusal.
          refused "${a} can signal ${b}'s agent process" "$a" bash -c 'kill -0 "$1"' _ "$pid"
        fi
      fi
      echo ""
    done
  done

  # Two agents on one machine must not share a name (the app offers a
  # re-enrolled runner its old placement by name) or an identity.
  declare -A SEEN_NAME SEEN_ID
  echo "== names and identities"
  for a in "${AGENTS[@]}"; do
    name="$(env_value "/etc/${a}/runner.env" CRONOMICON_RUNNER_NAME)"
    if [ -n "$name" ] && [ -n "${SEEN_NAME[$name]:-}" ]; then
      fail "${a} and ${SEEN_NAME[$name]} are both named '${name}'"
    else
      ok "${a} is named '${name}'"
      [ -n "$name" ] && SEEN_NAME[$name]="$a"
    fi
    if [ -f "/var/lib/${a}/identity.json" ]; then
      # The identity file is JSON; take the runner id without needing jq.
      rid="$(tr -d '\n' < "/var/lib/${a}/identity.json" | sed -n 's/.*"id"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')"
      if [ -n "$rid" ] && [ -n "${SEEN_ID[$rid]:-}" ]; then
        fail "${a} and ${SEEN_ID[$rid]} hold the same runner identity (${rid})"
      elif [ -n "$rid" ]; then
        ok "${a} has its own runner identity"
        SEEN_ID[$rid]="$a"
      fi
    fi
  done
  echo ""
fi

# --- One binary ------------------------------------------------------------
BIN=/usr/local/bin/cronomicon-runner
if [ -f "$BIN" ]; then
  if [ "$(owner_of "$BIN")" = "root:root" ] && [ "$(( 0$(mode_of "$BIN") & 022 ))" -eq 0 ]; then
    ok "${BIN} is root's and no agent can change it ($("$BIN" version 2>/dev/null || echo 'version unknown'))"
  else
    fail "${BIN} is $(owner_of "$BIN") $(mode_of "$BIN"): an agent that can write it controls every agent on the machine"
  fi
else
  fail "${BIN} is missing"
fi

echo ""
if [ "$FAILURES" -gt 0 ]; then
  echo "${FAILURES} check(s) FAILED, ${WARNINGS} warning(s)." >&2
  exit 1
fi
echo "All checks passed (${WARNINGS} warning(s)). ${#AGENTS[@]} agent(s) on this machine, each under its own user."

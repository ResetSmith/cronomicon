#!/bin/bash
# runner-install-check.sh — CI-less smoke checks for runner-install.sh.
#
# The repo has no shell-test harness, so this script gives runner-install.sh a
# minimal safety net: a syntax pass (bash -n) plus flag-parse checks that all
# run BEFORE the installer's root gate, so no root (and no system mutation) is
# needed. Run it from anywhere:
#
#   bash backend/deploy/runner-install-check.sh
#
# Exit 0 = all checks pass.

set -u

SCRIPT="$(dirname "$0")/runner-install.sh"
FAILURES=0

check() {
  local desc="$1"; shift
  if "$@" >/dev/null 2>&1; then
    echo "ok   ${desc}"
  else
    echo "FAIL ${desc}" >&2
    FAILURES=$((FAILURES + 1))
  fi
}

# Inverted: passes when the command FAILS and its output matches the pattern.
check_rejects() {
  local desc="$1" pattern="$2"; shift 2
  local out
  if out=$("$@" 2>&1); then
    echo "FAIL ${desc} (expected non-zero exit)" >&2
    FAILURES=$((FAILURES + 1))
  elif ! grep -q "$pattern" <<< "$out"; then
    echo "FAIL ${desc} (missing '${pattern}' in output)" >&2
    FAILURES=$((FAILURES + 1))
  else
    echo "ok   ${desc}"
  fi
}

# --- Syntax ---
check "bash -n parses" bash -n "$SCRIPT"

# --- Help ---
check "--help exits 0" bash "$SCRIPT" --help
check "help says capabilities auto-detect" bash -c "bash '$SCRIPT' --help | grep -qi 'auto-detect'"
check "no blind capabilities default" bash -c "! grep -q '^CAPABILITIES=\"bash,ansible\"' '$SCRIPT'"
check "help lists --known-hosts" bash -c "bash '$SCRIPT' --help | grep -q -- --known-hosts"
check "help lists --key-dir" bash -c "bash '$SCRIPT' --help | grep -q -- --key-dir"
check "help lists --key-map" bash -c "bash '$SCRIPT' --help | grep -q -- --key-map"
check "help lists --generate-key" bash -c "bash '$SCRIPT' --help | grep -q -- --generate-key"
check "help lists --ca-cert" bash -c "bash '$SCRIPT' --help | grep -q -- --ca-cert"
check "help lists --inventory" bash -c "bash '$SCRIPT' --help | grep -q -- --inventory"
check "help lists --local-inventory" bash -c "bash '$SCRIPT' --help | grep -q -- --local-inventory"
check "help lists --download" bash -c "bash '$SCRIPT' --help | grep -q -- --download"
check "help lists --allow-checkout" bash -c "bash '$SCRIPT' --help | grep -q -- --allow-checkout"
check "help lists --checkout-repos" bash -c "bash '$SCRIPT' --help | grep -q -- --checkout-repos"
check "help lists --checkout-token-file" bash -c "bash '$SCRIPT' --help | grep -q -- --checkout-token-file"
check "help lists --vault-pass-file" bash -c "bash '$SCRIPT' --help | grep -q -- --vault-pass-file"
check "help lists --instance" bash -c "bash '$SCRIPT' --help | grep -q -- --instance"

# --- Required args ---
check_rejects "missing --server rejected" "Server URL" \
  bash "$SCRIPT" -t crn_reg_x
check_rejects "missing --token rejected" "Registration token" \
  bash "$SCRIPT" -s https://cronomicon.example.com

# --- Flag combinations (validated before the root gate) ---
check_rejects "--key-dir + --key-map rejected" "mutually exclusive" \
  bash "$SCRIPT" -s https://x -t crn_reg_x --key-dir /tmp --key-map K=/tmp/k
check_rejects "bad --inventory value rejected" "must be 'cronomicon' or 'local'" \
  bash "$SCRIPT" -s https://x -t crn_reg_x --inventory bogus
check_rejects "--inventory local without --local-inventory rejected" "requires --local-inventory" \
  bash "$SCRIPT" -s https://x -t crn_reg_x --inventory local
check_rejects "--local-inventory without --inventory local rejected" "only applies with" \
  bash "$SCRIPT" -s https://x -t crn_reg_x --local-inventory /tmp/inv.json
check_rejects "malformed --key-map rejected" "NAME=path" \
  bash "$SCRIPT" -s https://x -t crn_reg_x --key-map not-a-map
check_rejects "duplicate --key-map NAME rejected" "duplicate NAME" \
  bash "$SCRIPT" -s https://x -t crn_reg_x --key-map prod=/k1,prod=/k2
check_rejects "path-like --key-map NAME rejected" "plain name" \
  bash "$SCRIPT" -s https://x -t crn_reg_x --key-map ../evil=/k1
check_rejects "path-like --generate-key NAME rejected" "plain name" \
  bash "$SCRIPT" -s https://x -t crn_reg_x --generate-key ../evil
check_rejects "--generate-key with missing value rejected" "requires a value" \
  bash "$SCRIPT" -s https://x -t crn_reg_x --generate-key
check_rejects "flag with missing value rejected" "requires a value" \
  bash "$SCRIPT" -s https://x -t crn_reg_x --known-hosts
check_rejects "-c with missing value rejected" "requires a value" \
  bash "$SCRIPT" -s https://x -t crn_reg_x -c
check_rejects "unknown flag rejected" "Unknown parameter" \
  bash "$SCRIPT" -s https://x -t crn_reg_x --bogus

# --- --instance: a second agent on one machine ---
# The name becomes part of an OS user name, two paths and a unit name, so a bad
# one is refused with the other flags, before the root gate and before anything
# is created.
check_rejects "--instance with an upper-case letter rejected" "lower-case letters" \
  bash "$SCRIPT" -s https://x -t crn_reg_x --instance Tax
check_rejects "--instance starting with a digit rejected" "starting with a letter" \
  bash "$SCRIPT" -s https://x -t crn_reg_x --instance 2tax
check_rejects "--instance starting with a hyphen rejected" "starting with a letter" \
  bash "$SCRIPT" -s https://x -t crn_reg_x --instance -tax
check_rejects "--instance with a slash rejected" "lower-case letters" \
  bash "$SCRIPT" -s https://x -t crn_reg_x --instance a/b
check_rejects "--instance with a dot rejected" "lower-case letters" \
  bash "$SCRIPT" -s https://x -t crn_reg_x --instance a.b
check_rejects "--instance longer than 14 characters rejected" "at most 14 characters" \
  bash "$SCRIPT" -s https://x -t crn_reg_x --instance abcdefghijklmno
# The same refusals under a UTF-8 locale with locale-collated ranges, which is
# what bash older than 5.0 does by default: `[a-z]` would then accept "Tax".
if locale -a 2>/dev/null | grep -qix 'en_US.utf-\?8'; then
  check_rejects "--instance with an upper-case letter rejected under a collating locale" "lower-case letters" \
    env LC_ALL=en_US.UTF-8 bash -O globasciiranges -c 'shopt -u globasciiranges; . "$0" -s https://x -t crn_reg_x --instance Tax' "$SCRIPT"
  check_rejects "--instance with an accented letter rejected under a collating locale" "lower-case letters" \
    env LC_ALL=en_US.UTF-8 bash -c 'shopt -u globasciiranges; . "$0" -s https://x -t crn_reg_x --instance taé' "$SCRIPT"
fi
check_rejects "--instance with an empty value rejected" "requires a value" \
  bash "$SCRIPT" -s https://x -t crn_reg_x --instance ""
check_rejects "--instance with no value rejected" "requires a value" \
  bash "$SCRIPT" -s https://x -t crn_reg_x --instance
if [ "$EUID" -ne 0 ]; then
  check_rejects "a valid --instance (14 characters) reaches the root gate" "must be run as root" \
    bash "$SCRIPT" -s https://x -t crn_reg_x --instance tax-dept-east1
fi

# The layout. Every name an install owns comes from set_layout; evaluate that
# function alone (no root, nothing created) and compare what it derives.
layout() {
  (
    eval "$(sed -n '/^set_layout() {$/,/^}$/p' "$SCRIPT")"
    set_layout "$1"
    echo "${RUNNER_USER}|${RUNNER_GROUP}|${UNIT_FILE}|${STATE_DIR}|${CONF_DIR}|${KEYS_DEST}|${CHECKOUT_TOKEN_DEST}"
  )
}
expect_layout() {
  local desc="$1" inst="$2" want="$3" got
  got="$(layout "$inst")"
  if [ "$got" = "$want" ]; then
    echo "ok   ${desc}"
  else
    echo "FAIL ${desc}: got ${got}" >&2
    FAILURES=$((FAILURES + 1))
  fi
}
# Without --instance the installer creates exactly what it always has.
expect_layout "the default install's destinations are unchanged" "" \
  "cronomicon-runner|cronomicon-runner|/etc/systemd/system/cronomicon-runner.service|/var/lib/cronomicon-runner|/etc/cronomicon-runner|/var/lib/cronomicon-runner/keys|/etc/cronomicon-runner/checkout-token"
expect_layout "an instance has a user, group, unit and directories of its own" "tax" \
  "cronomicon-runner-tax|cronomicon-runner-tax|/etc/systemd/system/cronomicon-runner-tax.service|/var/lib/cronomicon-runner-tax|/etc/cronomicon-runner-tax|/var/lib/cronomicon-runner-tax/keys|/etc/cronomicon-runner-tax/checkout-token"

# The unit. Render the installer's own unit block for the default agent and for
# an instance (it only writes to stdout here) and check each names its own
# install and nothing of the other's.
render_unit() {
  (
    eval "$(sed -n '/^set_layout() {$/,/^}$/p' "$SCRIPT")"
    INSTANCE="$1"
    # shellcheck disable=SC2034  # read by the unit block evaluated below
    HARDENING_OK=1
    set_layout "$INSTANCE"
    eval "$(sed -n '/^UNIT_DESC="Cronomicon runner agent"$/,/^} > "\$UNIT_FILE"$/p' "$SCRIPT" \
      | sed -e '/^echo ">> Writing /d' -e 's/^} > "\$UNIT_FILE"$/}/')"
  )
}
unit_default="$(render_unit "")"
unit_tax="$(render_unit "tax")"
unit_has() { grep -qxF -- "$2" <<< "$1"; }
check "the default unit runs as cronomicon-runner" unit_has "$unit_default" "User=cronomicon-runner"
check "the default unit's state directory is unchanged" unit_has "$unit_default" "StateDirectory=cronomicon-runner"
# systemd applies StateDirectoryMode at every start; left at its default (0755)
# it undoes the installer's chmod 0750 and opens the directory to other users.
check "the default unit keeps its state directory closed to other users" unit_has "$unit_default" "StateDirectoryMode=0750"
# On systemd 239 a filtered syscall kills the calling thread; Go's PATH lookup
# makes one the old filter list does not know, and the agent then finds no
# toolchain and refuses to start. EPERM lets Go fall back.
check "the hardened unit answers EPERM for a filtered syscall" unit_has "$unit_default" "SystemCallErrorNumber=EPERM"
check "an instance's hardened unit does too" unit_has "$unit_tax" "SystemCallErrorNumber=EPERM"
# The line lives in three places, and losing it from any one is the same hang:
# the installer's unit (above), the probe that decides whether the installer
# may harden at all, and the reference unit a manual install copies.
check "the sandbox self-probe runs with the unit's filter and its EPERM answer" \
  bash -c "sed -n '/systemd-run --quiet --pipe --wait --collect/,/then\$/p' '$SCRIPT' | grep -qF -- \"-p 'SystemCallFilter=@system-service' -p 'SystemCallErrorNumber=EPERM'\""
REFERENCE_UNIT="$(dirname "$0")/cronomicon-runner.service"
check "the reference unit answers EPERM for a filtered syscall" grep -qxF "SystemCallErrorNumber=EPERM" "$REFERENCE_UNIT"
# cronomicon-runner.service is a second copy of the unit the installer writes.
# They must carry the same directives, apart from two lines that are each in
# one of them on purpose: the reference unit points at its own documentation,
# and only the installer's runs the doctor before the agent starts.
unit_directives() { grep -vE '^[[:space:]]*(#|$)' | grep -vE '^(Documentation|ExecStartPre)=' | sort; }
if drift="$(diff <(unit_directives < "$REFERENCE_UNIT") <(unit_directives <<< "$unit_default"))"; then
  echo "ok   the reference unit and the installer's unit carry the same directives"
else
  echo "FAIL the reference unit (<) and the installer's unit (>) differ:" >&2
  echo "$drift" >&2
  FAILURES=$((FAILURES + 1))
fi
check "an instance's unit keeps its state directory closed to other users" unit_has "$unit_tax" "StateDirectoryMode=0750"
check "the default unit reads /etc/cronomicon-runner/runner.env" unit_has "$unit_default" "EnvironmentFile=-/etc/cronomicon-runner/runner.env"
check "the default unit names no instance" bash -c "! grep -q 'cronomicon-runner-' <<< \"\$1\"" _ "$unit_default"
check "an instance's unit runs as its own user" unit_has "$unit_tax" "User=cronomicon-runner-tax"
check "an instance's unit runs as its own group" unit_has "$unit_tax" "Group=cronomicon-runner-tax"
check "an instance's unit has its own state directory" unit_has "$unit_tax" "StateDirectory=cronomicon-runner-tax"
check "an instance's unit confines writes to its own state" unit_has "$unit_tax" "ReadWritePaths=/var/lib/cronomicon-runner-tax"
check "an instance's unit reads its own config" unit_has "$unit_tax" "EnvironmentFile=-/etc/cronomicon-runner-tax/runner.env"
check "an instance's unit keeps its identity in its own state" unit_has "$unit_tax" "Environment=CRONOMICON_RUNNER_IDENTITY_FILE=/var/lib/cronomicon-runner-tax/identity.json"
check "an instance's unit never points at the default agent's directories" \
  bash -c "! grep -Eq '(/etc|/var/lib)/cronomicon-runner(/|\$)' <<< \"\$1\"" _ "$unit_tax"
check "both units are hardened alike" bash -c "[ \"\$(grep -c '^Protect' <<< \"\$1\")\" = \"\$(grep -c '^Protect' <<< \"\$2\")\" ] && grep -q '^NoNewPrivileges=true' <<< \"\$2\"" _ "$unit_default" "$unit_tax"

# --- Resource limits: --memory-max, --cpu-quota, --tasks-max ---
# An agent that is not root cannot cap its own runs, so the unit is where one
# agent on a shared machine is kept from starving the next. The values go into
# the unit file as given, so anything but the documented shape is refused.
check "help lists --memory-max" bash -c "bash '$SCRIPT' --help | grep -q -- --memory-max"
check "help lists --cpu-quota" bash -c "bash '$SCRIPT' --help | grep -q -- --cpu-quota"
check "help lists --tasks-max" bash -c "bash '$SCRIPT' --help | grep -q -- --tasks-max"
check_rejects "--memory-max with a second directive in it rejected" "whole number with K, M, G or T" \
  bash "$SCRIPT" -s https://x -t crn_reg_x --memory-max "4G
CPUQuota=1%"
check_rejects "--memory-max with a two-letter unit rejected" "whole number with K, M, G or T" \
  bash "$SCRIPT" -s https://x -t crn_reg_x --memory-max 4GB
check_rejects "--memory-max 0 rejected" "whole number with K, M, G or T" \
  bash "$SCRIPT" -s https://x -t crn_reg_x --memory-max 0
check_rejects "--cpu-quota without a percent sign rejected" "percentage of one CPU" \
  bash "$SCRIPT" -s https://x -t crn_reg_x --cpu-quota 200
check_rejects "--cpu-quota that is not a number rejected" "percentage of one CPU" \
  bash "$SCRIPT" -s https://x -t crn_reg_x --cpu-quota "2x%"
check_rejects "--tasks-max that is not a whole number rejected" "a whole number" \
  bash "$SCRIPT" -s https://x -t crn_reg_x --tasks-max 10%
check_rejects "--tasks-max with no value rejected" "requires a value" \
  bash "$SCRIPT" -s https://x -t crn_reg_x --tasks-max
if [ "$EUID" -ne 0 ]; then
  check_rejects "valid limits reach the root gate" "must be run as root" \
    bash "$SCRIPT" -s https://x -t crn_reg_x --memory-max 4G --cpu-quota 200% --tasks-max 1024
fi
# No limit asked for, none written: the default unit is the one this script
# always wrote (the comparison with the reference unit, above, holds that too).
check "a unit with no limits asked for carries none" \
  bash -c "! grep -Eq '^(MemoryMax|CPUQuota|TasksMax)=' <<< \"\$1\"" _ "$unit_default"
unit_limited="$(MEMORY_MAX=4G CPU_QUOTA=200% TASKS_MAX=1024 render_unit "tax")"
check "--memory-max is the unit's MemoryMax" unit_has "$unit_limited" "MemoryMax=4G"
check "--cpu-quota is the unit's CPUQuota" unit_has "$unit_limited" "CPUQuota=200%"
check "--tasks-max is the unit's TasksMax" unit_has "$unit_limited" "TasksMax=1024"
check "the limits are in the [Service] section" \
  bash -c "sed -n '/^\[Service\]\$/,/^\[Install\]\$/p' <<< \"\$1\" | grep -qx 'MemoryMax=4G'" _ "$unit_limited"
unit_one_limit="$(CPU_QUOTA=150% render_unit "")"
check "one limit alone writes only that one" \
  bash -c "grep -qx 'CPUQuota=150%' <<< \"\$1\" && ! grep -Eq '^(MemoryMax|TasksMax)=' <<< \"\$1\"" _ "$unit_one_limit"
# Apart from the limits, a limited unit is the unit it would have been.
if drift="$(diff <(grep -vE '^(MemoryMax|CPUQuota|TasksMax)=' <<< "$unit_limited" | unit_directives) <(unit_directives <<< "$unit_tax"))"; then
  echo "ok   limits add to the unit and change nothing else in it"
else
  echo "FAIL a unit with limits (<) differs from one without (>) in more than the limits:" >&2
  echo "$drift" >&2
  FAILURES=$((FAILURES + 1))
fi

# --- Secrets are never accepted as a flag VALUE (ps/history exposure) ---
check_rejects "--checkout-token with a value rejected" "refusing to read a secret" \
  bash "$SCRIPT" -s https://x -t crn_reg_x --checkout-token ghp_secretvalue
check_rejects "--vault-pass with a value rejected" "refusing to read a secret" \
  bash "$SCRIPT" -s https://x -t crn_reg_x --vault-pass hunter2
# A stdin prompt needs a terminal; piped (no TTY) must reject and point at the file flag.
check_rejects "--checkout-token - without a TTY rejected" "not a terminal" \
  bash -c "printf '' | bash '$SCRIPT' -s https://x -t crn_reg_x --checkout-token -"
check_rejects "--vault-pass - without a TTY rejected" "not a terminal" \
  bash -c "printf '' | bash '$SCRIPT' -s https://x -t crn_reg_x --vault-pass -"
check_rejects "--checkout-token-file + --checkout-token - conflict rejected" "not both" \
  bash -c "printf '' | bash '$SCRIPT' -s https://x -t crn_reg_x --checkout-token-file /tmp/tok --checkout-token -"
# A mistaken --checkout-token=SECRET (=-form) must be rejected WITHOUT echoing the secret.
_ct_out=$(bash "$SCRIPT" -s https://x -t crn_reg_x --checkout-token=SUPERSECRET 2>&1 || true)
if grep -q "SUPERSECRET" <<< "$_ct_out"; then
  echo "FAIL --checkout-token=value must not echo the secret" >&2
  FAILURES=$((FAILURES + 1))
else
  echo "ok   --checkout-token=value does not echo the secret"
fi

# --- Root gate still holds after valid args (when run unprivileged) ---
if [ "$EUID" -ne 0 ]; then
  check_rejects "valid args still hit the root gate" "must be run as root" \
    bash "$SCRIPT" -s https://x -t crn_reg_x
fi

echo ""
if [ "$FAILURES" -gt 0 ]; then
  echo "${FAILURES} check(s) FAILED" >&2
  exit 1
fi
echo "All checks passed."

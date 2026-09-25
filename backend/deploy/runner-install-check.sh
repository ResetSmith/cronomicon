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

# --- Required args ---
check_rejects "missing --server rejected" "Server URL" \
  bash "$SCRIPT" -t crn_reg_x
check_rejects "missing --token rejected" "Registration token" \
  bash "$SCRIPT" -s https://amadeus.example.com

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

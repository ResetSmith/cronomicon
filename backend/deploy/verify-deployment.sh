#!/usr/bin/env bash
# verify-deployment.sh — Phase D.1/D.3 deployment smoke + security probe.
#
# Runs the code-independent checks against a LIVE stack that a CI test can't:
# the spoofed-header rejection (the single most important control), the
# unauthenticated-surface check, and basic reachability. It does NOT log in
# through the SSO login (that's an interactive/staging step) — it verifies the app's
# own enforcement from outside the trust boundary.
#
# Usage:
#   APP_URL=https://amadeus.example.com ./verify-deployment.sh
#   # Optionally probe the app port directly (bypassing the proxy) to prove the
#   # trusted-proxy control — this is the most important check:
#   APP_DIRECT_URL=http://amadeus-host:8080 APP_URL=https://amadeus.example.com ./verify-deployment.sh
#
# Exit non-zero if any check fails.

set -uo pipefail

APP_URL="${APP_URL:?set APP_URL to the public https URL}"
APP_DIRECT_URL="${APP_DIRECT_URL:-}"   # optional: the internal app port, if reachable
pass=0 fail=0

check() { # desc, expected, actual
  if [[ "$2" == "$3" ]]; then
    echo "  PASS: $1 (got $3)"; pass=$((pass+1))
  else
    echo "  FAIL: $1 (want $2, got $3)"; fail=$((fail+1))
  fi
}

code() { curl -sk -o /dev/null -w '%{http_code}' "$@"; }

echo "== Reachability =="
check "/healthz returns 200" 200 "$(code "$APP_URL/healthz")"
check "/readyz returns 200"  200 "$(code "$APP_URL/readyz")"
check "/version returns 200" 200 "$(code "$APP_URL/version")"

echo "== Auth surface (through the proxy) =="
# Unauthenticated operator calls must be blocked (proxy redirects to the SSO login →
# typically 302, or the app returns 401). Either way: NOT 200.
me_code="$(code "$APP_URL/api/v1/me")"
if [[ "$me_code" == "200" ]]; then
  echo "  FAIL: /api/v1/me reachable unauthenticated (got 200)"; fail=$((fail+1))
else
  echo "  PASS: /api/v1/me not reachable unauthenticated (got $me_code)"; pass=$((pass+1))
fi
# dev-login must not exist in production.
check "/api/v1/auth/dev-login absent (404)" 404 "$(code "$APP_URL/api/v1/auth/dev-login")"

echo "== Trusted-proxy enforcement (THE critical control) =="
if [[ -n "$APP_DIRECT_URL" ]]; then
  # Hit the app port directly with spoofed identity headers. The app must strip
  # them (peer not in CRONOMICON_TRUSTED_PROXIES) → unauthenticated, NOT 200.
  spoof_code="$(code -H 'Remote-User: attacker' -H 'Remote-Groups: cronomicon-admins,admins' \
                    -H 'Remote-Email: attacker@evil.test' "$APP_DIRECT_URL/api/v1/me")"
  if [[ "$spoof_code" == "200" ]]; then
    echo "  FAIL: spoofed Remote-* accepted on direct app port (got 200) — TRUST BOUNDARY BROKEN"; fail=$((fail+1))
  else
    echo "  PASS: spoofed Remote-* rejected on direct app port (got $spoof_code)"; pass=$((pass+1))
  fi
  # And the app port should not be publicly reachable at all in a correct deploy.
  echo "  NOTE: app port was reachable from here ($APP_DIRECT_URL) — confirm this host is inside the trust boundary; it must NOT be public."
else
  echo "  SKIP: set APP_DIRECT_URL to the internal app port to test spoofed-header rejection directly."
  echo "        (Automated equivalent is covered by TestSpoofedHeaderRejectedAtServer.)"
fi

echo "== Metrics exposure =="
m_code="$(code "$APP_URL/metrics")"
if [[ "$m_code" == "200" ]]; then
  echo "  WARN: /metrics is reachable via the public URL — confirm this is intentional (D.1 says internal-only or gated)."
else
  echo "  PASS: /metrics not public via the proxy (got $m_code)"; pass=$((pass+1))
fi

echo
echo "== Summary: $pass passed, $fail failed =="
[[ "$fail" -eq 0 ]]

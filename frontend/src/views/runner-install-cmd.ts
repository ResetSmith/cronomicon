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

// One-click install (runner provisioning plan 2 Phase 2, D2: 2A). The server's
// GET /install/<token> endpoint bakes the server URL, token, and binary
// download into runner-install.sh, so the install is a single flagless pipe —
// no -s/-t/-c to carry. The token in the path IS the credential.
export function personalizedInstallUrl(origin: string, token: string): string {
  return `${origin}/install/${token}`;
}

// The headline copy-paste command for the Add Runner flow.
export function personalizedInstallOneLiner(origin: string, token: string): string {
  return `curl -fsSL ${personalizedInstallUrl(origin, token)} | sudo bash`;
}

// The download-inspect-run variant, for orgs that ban curl-pipe-to-sudo. The
// saved file already has the server URL + token baked in, so it runs with no
// arguments too.
export function personalizedInstallTwoStep(origin: string, token: string): string {
  const url = personalizedInstallUrl(origin, token);
  return [
    `curl -fsSL ${url} -o runner-install.sh`,
    `less runner-install.sh   # server URL + token are baked in — inspect before running`,
    `sudo bash runner-install.sh`,
  ].join("\n");
}

// No -c by default: the agent auto-detects the host's run-types at startup
// (D1: 1B); pass capabilities only to narrow what the runner claims.
export function installOneLiner(origin: string, token: string, capabilities?: string): string {
  return (
    `curl -fsSL ${origin}${RUNNER_INSTALL_SCRIPT_PATH} | ` +
    `sudo bash -s -- -s ${origin} -t ${token} -n $(hostname)${capabilities ? ` -c ${capabilities}` : ""} --download`
  );
}

// The download-inspect-run variant, for orgs that ban curl-pipe-to-sudo.
export function installTwoStep(origin: string, token: string, capabilities?: string): string {
  return [
    `curl -fsSLO ${origin}${RUNNER_INSTALL_SCRIPT_PATH}`,
    `less runner-install.sh   # inspect before running`,
    `sudo bash runner-install.sh -s ${origin} -t ${token} -n $(hostname)${capabilities ? ` -c ${capabilities}` : ""} --download`,
  ].join("\n");
}

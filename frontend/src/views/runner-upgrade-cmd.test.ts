import { describe, expect, it } from "vitest";
import { AGENT_BIN_PATH, AGENT_UNIT, upgradeCommand } from "./runner-upgrade-cmd";

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
    expect(cmd).toContain(`systemctl restart ${AGENT_UNIT}`);
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
    expect(cmd).toContain(`sudo mv -f \${BIN}.prev $BIN && sudo systemctl restart ${AGENT_UNIT}`);
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

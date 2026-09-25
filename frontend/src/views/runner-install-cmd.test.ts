import { describe, expect, it } from "vitest";
import {
  RUNNER_INSTALL_SCRIPT_PATH,
  installOneLiner,
  installTwoStep,
  personalizedInstallUrl,
  personalizedInstallOneLiner,
  personalizedInstallTwoStep,
} from "./runner-install-cmd";

const ORIGIN = "https://cronomicon.example.com";
const TOKEN = "crn_reg_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef";

describe("installOneLiner", () => {
  it("fetches the app-served install script from the given origin", () => {
    const cmd = installOneLiner(ORIGIN, "crn_reg_abc123");
    expect(cmd).toContain(`curl -fsSL ${ORIGIN}${RUNNER_INSTALL_SCRIPT_PATH}`);
    expect(RUNNER_INSTALL_SCRIPT_PATH).toBe("/runner-install.sh");
  });

  it("passes server, token, hostname, and --download — no -c: capabilities auto-detect on the host", () => {
    const cmd = installOneLiner(ORIGIN, "crn_reg_abc123");
    expect(cmd).toContain(`sudo bash -s -- -s ${ORIGIN} -t crn_reg_abc123 -n $(hostname) --download`);
    expect(cmd).not.toContain("-c ");
  });

  it("is token-agnostic: renders a placeholder verbatim", () => {
    expect(installOneLiner(ORIGIN, "<TOKEN>")).toContain("-t <TOKEN>");
  });

  it("honors an explicit capability override", () => {
    expect(installOneLiner(ORIGIN, "t", "ansible,terraform")).toContain("-c ansible,terraform");
  });
});

describe("installTwoStep", () => {
  it("downloads, inspects, then runs the same installer with the same args", () => {
    const cmd = installTwoStep(ORIGIN, "crn_reg_abc123");
    const lines = cmd.split("\n");
    expect(lines[0]).toBe(`curl -fsSLO ${ORIGIN}${RUNNER_INSTALL_SCRIPT_PATH}`);
    expect(lines[1]).toContain("inspect before running");
    expect(lines[2]).toBe(`sudo bash runner-install.sh -s ${ORIGIN} -t crn_reg_abc123 -n $(hostname) --download`);
  });

  it("honors an explicit capability override", () => {
    expect(installTwoStep(ORIGIN, "t", "bash,python")).toContain("-c bash,python --download");
  });
});

describe("personalized one-click install (Phase 2)", () => {
  it("builds the /install/<token> URL", () => {
    expect(personalizedInstallUrl(ORIGIN, TOKEN)).toBe(`${ORIGIN}/install/${TOKEN}`);
  });

  it("one-liner is a flagless curl-pipe — server+token+download are baked in", () => {
    const cmd = personalizedInstallOneLiner(ORIGIN, TOKEN);
    expect(cmd).toBe(`curl -fsSL ${ORIGIN}/install/${TOKEN} | sudo bash`);
    // No flags travel on the command line — that's the whole point.
    expect(cmd).not.toContain(" -s ");
    expect(cmd).not.toContain(" -t ");
    expect(cmd).not.toContain(" -c ");
    expect(cmd).not.toContain("--download");
  });

  it("two-step variant downloads, inspects, then runs with no arguments", () => {
    const lines = personalizedInstallTwoStep(ORIGIN, TOKEN).split("\n");
    expect(lines[0]).toBe(`curl -fsSL ${ORIGIN}/install/${TOKEN} -o runner-install.sh`);
    expect(lines[1]).toContain("inspect before running");
    expect(lines[2]).toBe("sudo bash runner-install.sh");
  });
});

import { describe, expect, it } from "vitest";
import {
  RUNNER_INSTALL_SCRIPT_PATH,
  installOneLiner,
  installTwoStep,
  personalizedInstallUrl,
  personalizedInstallOneLiner,
  personalizedInstallTwoStep,
  INVALID_INSTANCE_COMMAND,
  validInstanceName,
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

// MA-15..MA-24 — a second agent on a machine that already runs one. The
// installer's --instance gives it a user, group, unit and directories of its
// own; these builders append the flag to EVERY form of the command and name the
// runner <hostname>-<instance>, so two agents on a machine never share a name
// (the app offers a re-enrolled runner its old placement by name).
describe("the instance name", () => {
  it("accepts what runner-install.sh accepts, and nothing else", () => {
    for (const ok of ["a", "tax", "tax-dept", "t2", "tax-dept-east1"]) expect(validInstanceName(ok)).toBe(true);
    for (const bad of ["", "Tax", "2tax", "-tax", "tax_dept", "a/b", "a.b", "a b", "tax-dept-east12", "täx"]) {
      expect(validInstanceName(bad)).toBe(false);
    }
  });

  it("is appended to all four forms of the command", () => {
    expect(installOneLiner(ORIGIN, "t", undefined, "tax")).toBe(
      `curl -fsSL ${ORIGIN}/runner-install.sh | sudo bash -s -- -s ${ORIGIN} -t t -n $(hostname)-tax --download --instance tax`,
    );
    expect(installTwoStep(ORIGIN, "t", "bash", "tax").split("\n")[2]).toBe(
      `sudo bash runner-install.sh -s ${ORIGIN} -t t -n $(hostname)-tax -c bash --download --instance tax`,
    );
    // The baked script still parses its arguments: `bash -s -- <args>`.
    expect(personalizedInstallOneLiner(ORIGIN, TOKEN, "tax")).toBe(`curl -fsSL ${ORIGIN}/install/${TOKEN} | sudo bash -s -- --instance tax`);
    expect(personalizedInstallTwoStep(ORIGIN, TOKEN, "tax").split("\n")[2]).toBe("sudo bash runner-install.sh --instance tax");
  });

  it("changes nothing when it is empty: the default agent's command is what it always was", () => {
    for (const none of [undefined, "", "   "]) {
      expect(installOneLiner(ORIGIN, "t", undefined, none)).toBe(installOneLiner(ORIGIN, "t"));
      expect(installTwoStep(ORIGIN, "t", undefined, none)).toBe(installTwoStep(ORIGIN, "t"));
      expect(personalizedInstallOneLiner(ORIGIN, TOKEN, none)).toBe(`curl -fsSL ${ORIGIN}/install/${TOKEN} | sudo bash`);
      expect(personalizedInstallTwoStep(ORIGIN, TOKEN, none)).toBe(personalizedInstallTwoStep(ORIGIN, TOKEN));
    }
    expect(installOneLiner(ORIGIN, "t")).toContain("-n $(hostname) --download");
  });

  // A name the installer would refuse yields NO command — not the default
  // agent's. The field is filled in on a machine that already runs the default
  // agent, and that command, pasted there, would overwrite the first agent's
  // runner.env with the other agency's token and restart it. What is returned
  // is a shell comment, so pasting it does nothing.
  it("offers no command at all while the name is invalid", () => {
    for (const bad of ["Tax", "x; rm -rf /", "$(id)", "a b", "tax-dept-east12"]) {
      for (const cmd of [
        installOneLiner(ORIGIN, "t", undefined, bad),
        installTwoStep(ORIGIN, "t", undefined, bad),
        personalizedInstallOneLiner(ORIGIN, TOKEN, bad),
        personalizedInstallTwoStep(ORIGIN, TOKEN, bad),
      ]) {
        expect(cmd).toBe(INVALID_INSTANCE_COMMAND);
        expect(cmd.startsWith("#")).toBe(true);
        expect(cmd).not.toContain("\n");
        expect(cmd).not.toContain("curl");
        expect(cmd).not.toContain(bad);
      }
    }
  });
});

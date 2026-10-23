import { spawnSync } from "node:child_process";
import { existsSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";
import {
  INVALID_LIMITS_COMMAND,
  invalidLimits,
  validLimit,
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

// 2.3.1 — limits for the unit the installer writes: --memory-max, --cpu-quota,
// --tasks-max. The helper must offer exactly what the installer takes, and a
// command that carries every limit that was typed or no command at all.
describe("resource limits", () => {
  // One table for both tests below: what each flag takes, and what it refuses.
  const SHAPES = {
    memoryMax: { flag: "--memory-max", ok: ["4G", "512M", "64K", "2T", "4096"], bad: ["0", "0G", "04G", "4GB", "1.5G", "G", "4g", "50%", "infinity", "none", "4G;reboot", "4G\nCPUQuota=1%", "$(id)", " 4G"] },
    cpuQuota: { flag: "--cpu-quota", ok: ["200%", "50%", "1%"], bad: ["200", "0%", "%", "2x%", "1.5%", "200%%", "infinity"] },
    tasksMax: { flag: "--tasks-max", ok: ["1024", "1"], bad: ["0", "10%", "x", "1k", "-1", "1 2", "infinity"] },
  } as const;

  it("accepts the shapes runner-install.sh accepts, and nothing else", () => {
    for (const [field, { ok, bad }] of Object.entries(SHAPES) as [keyof typeof SHAPES, (typeof SHAPES)[keyof typeof SHAPES]][]) {
      for (const v of ok) expect(validLimit(field, v), `${field} ${v}`).toBe(true);
      for (const v of bad) expect(validLimit(field, v), `${field} ${JSON.stringify(v)}`).toBe(false);
    }
  });

  // The rule is written twice, here and in the installer's check_limit, and a
  // value the helper lets through goes into a unit file. So ask the installer
  // itself: it validates its flags before its root gate, which means that,
  // run without root, it refuses a bad value by name and stops at the gate for
  // a good one. NEVER as root — there the gate is open and it would install.
  const INSTALLER = fileURLToPath(new URL("../../../backend/deploy/runner-install.sh", import.meta.url));
  const asRoot = typeof process.getuid === "function" && process.getuid() === 0;
  const canAsk = !asRoot && process.platform !== "win32" && existsSync(INSTALLER) && spawnSync("bash", ["-c", "true"]).status === 0;
  it.skipIf(!canAsk)("agrees with runner-install.sh on every value in that table", () => {
    const ask = (flag: string, value: string) => {
      const r = spawnSync("bash", [INSTALLER, "-s", "https://x", "-t", "crn_reg_x", flag, value], { encoding: "utf8" });
      const out = `${r.stdout}${r.stderr}`;
      if (out.includes("must be run as root")) return true;
      if (out.includes(`Error: ${flag} must be`)) return false;
      throw new Error(`unexpected answer for ${flag} ${JSON.stringify(value)}: ${out.slice(0, 200)}`);
    };
    for (const [field, { flag, ok, bad }] of Object.entries(SHAPES) as [keyof typeof SHAPES, (typeof SHAPES)[keyof typeof SHAPES]][]) {
      for (const v of [...ok, ...bad]) expect(ask(flag, v), `${flag} ${JSON.stringify(v)}`).toBe(validLimit(field, v));
    }
  });

  const LIMITS = { memoryMax: "4G", cpuQuota: "200%", tasksMax: "1024" };
  const FLAGS = "--memory-max 4G --cpu-quota 200% --tasks-max 1024";

  it("are appended to all four forms of the command, after the instance", () => {
    expect(installOneLiner(ORIGIN, "t", undefined, "tax", LIMITS)).toBe(
      `curl -fsSL ${ORIGIN}/runner-install.sh | sudo bash -s -- -s ${ORIGIN} -t t -n $(hostname)-tax --download --instance tax ${FLAGS}`,
    );
    expect(installTwoStep(ORIGIN, "t", "bash", undefined, LIMITS).split("\n")[2]).toBe(
      `sudo bash runner-install.sh -s ${ORIGIN} -t t -n $(hostname) -c bash --download ${FLAGS}`,
    );
    expect(personalizedInstallOneLiner(ORIGIN, TOKEN, "tax", LIMITS)).toBe(
      `curl -fsSL ${ORIGIN}/install/${TOKEN} | sudo bash -s -- --instance tax ${FLAGS}`,
    );
    // No instance: the limits alone still need `-s --` to reach the baked script.
    expect(personalizedInstallOneLiner(ORIGIN, TOKEN, undefined, LIMITS)).toBe(`curl -fsSL ${ORIGIN}/install/${TOKEN} | sudo bash -s -- ${FLAGS}`);
    expect(personalizedInstallTwoStep(ORIGIN, TOKEN, "", LIMITS).split("\n")[2]).toBe(`sudo bash runner-install.sh ${FLAGS}`);
  });

  it("write only the limits that were given", () => {
    expect(personalizedInstallOneLiner(ORIGIN, TOKEN, undefined, { cpuQuota: " 150% " })).toBe(
      `curl -fsSL ${ORIGIN}/install/${TOKEN} | sudo bash -s -- --cpu-quota 150%`,
    );
  });

  it("change nothing when none is given: every command is what it was", () => {
    for (const none of [undefined, {}, { memoryMax: "", cpuQuota: "  ", tasksMax: "" }]) {
      expect(installOneLiner(ORIGIN, "t", undefined, "tax", none)).toBe(installOneLiner(ORIGIN, "t", undefined, "tax"));
      expect(installTwoStep(ORIGIN, "t", undefined, undefined, none)).toBe(installTwoStep(ORIGIN, "t"));
      expect(personalizedInstallOneLiner(ORIGIN, TOKEN, undefined, none)).toBe(`curl -fsSL ${ORIGIN}/install/${TOKEN} | sudo bash`);
      expect(personalizedInstallTwoStep(ORIGIN, TOKEN, "tax", none)).toBe(personalizedInstallTwoStep(ORIGIN, TOKEN, "tax"));
    }
  });

  // Not the command minus the bad limit: an administrator who typed a limit and
  // pasted a command that silently lacked it would believe the agent bounded.
  it("offer no command at all while one is invalid", () => {
    for (const bad of [{ memoryMax: "4GB" }, { ...LIMITS, cpuQuota: "200" }, { tasksMax: "$(id)" }, { memoryMax: "4G; reboot" }]) {
      expect(invalidLimits(bad).length).toBe(1);
      for (const cmd of [
        installOneLiner(ORIGIN, "t", undefined, undefined, bad),
        installTwoStep(ORIGIN, "t", undefined, "tax", bad),
        personalizedInstallOneLiner(ORIGIN, TOKEN, undefined, bad),
        personalizedInstallTwoStep(ORIGIN, TOKEN, "tax", bad),
      ]) {
        expect(cmd).toBe(INVALID_LIMITS_COMMAND);
        expect(cmd.startsWith("#")).toBe(true);
        expect(cmd).not.toContain("\n");
      }
    }
    // A bad instance name is still the first thing said.
    expect(personalizedInstallOneLiner(ORIGIN, TOKEN, "Tax", { memoryMax: "4GB" })).toBe(INVALID_INSTANCE_COMMAND);
  });
});

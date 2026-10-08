// @vitest-environment jsdom
import { readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

// v2.3.1 — the advanced install helper ("Advanced: pre-authored install") turns
// one set of choices into three artifacts: the install one-liner, a runner.env
// and a docker run. Resource limits belong in two of them and in two different
// spellings, and in the third not at all:
//
//   - the install command: the installer's flags, which it writes into the unit;
//   - the container command: the runtime's own limits, with swap off;
//   - the env file: nothing. Neither a unit nor a container reads a limit from it.
//
// The builders have their own tests (runner-provision.test.ts). This one is the
// screen: that the fields are there, feed all three artifacts live, and that a
// value the installer would refuse leaves no command to copy.

// The REAL template, the same bytes the app serves at /cronomicon-runner.env.example.
const EXAMPLE = readFileSync(
  resolve(dirname(fileURLToPath(import.meta.url)), "../../../backend/deploy/cronomicon-runner.env.example"),
  "utf8",
);

vi.mock("../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/client")>();
  return {
    ...actual,
    api: {
      GET: vi.fn(async (path: string) => {
        if (path === "/agencies") return { data: [{ id: "global", name: "Global" }] };
        if (path === "/runners/registration-tokens" || path === "/runners/host-keys/pending" || path === "/scopes") return { data: [] };
        return { data: { items: [] } };
      }),
      POST: vi.fn(async () => ({ data: {} })),
      PUT: vi.fn(async () => ({ data: [] })),
    } as unknown as typeof actual.api,
    fetchCapabilities: vi.fn(async () => ({ configureApp: true, configureAppGlobal: true })),
    fetchVersion: vi.fn(async () => ({ version: "2.3.1" })),
  };
});

import { Runners } from "./Runners";

beforeEach(() => {
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => ({ ok: true, status: 200, text: async () => EXAMPLE })),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

// The three artifacts, each found by its own label: the <pre> under it. (Not by
// content: while a limit is invalid two of them hold the same one-line comment.)
const artifactUnder = (label: RegExp) => {
  const block = screen.getByText(label).parentElement?.parentElement;
  const pre = block?.querySelector("pre");
  if (!pre) throw new Error(`no artifact under ${label}`);
  return pre.textContent ?? "";
};
const artifacts = () => ({
  install: artifactUnder(/^Install one-liner — /),
  env: artifactUnder(/^runner\.env — /),
  docker: artifactUnder(/^docker run — /),
});

const openHelper = async () => {
  render(
    <MemoryRouter>
      <Runners />
    </MemoryRouter>,
  );
  fireEvent.click(await screen.findByRole("button", { name: "Open the helper" }));
  // The env artifact appears once the template has loaded.
  await waitFor(() => expect(artifacts().env).toContain("CRONOMICON_RUNNER_SERVER="));
};

// The helper's own limit fields: the last on the page (the token install helper
// above it shows the same group once a token has been minted).
const limitField = (placeholder: string) => screen.getAllByPlaceholderText(placeholder).at(-1) as HTMLInputElement;
const type = (placeholder: string, value: string) => fireEvent.change(limitField(placeholder), { target: { value } });

describe("Runners — resource limits in the advanced install helper", () => {
  it("starts with no limit in any artifact", async () => {
    await openHelper();
    const a = artifacts();
    expect(a.install).not.toMatch(/--(memory-max|cpu-quota|tasks-max)/);
    expect(a.docker).not.toMatch(/--(memory|memory-swap|cpus|pids-limit) /);
    expect(a.docker).toContain("docker run -d");
  });

  it("writes them into the install command as the installer's flags and into the container command as the runtime's", async () => {
    await openHelper();
    const before = artifacts();
    type("e.g. 4G", "4G");
    type("e.g. 200%", "150%");
    type("e.g. 1024", "256");

    await waitFor(() => expect(artifacts().install).toContain("--memory-max 4G --cpu-quota 150% --tasks-max 256"));
    const a = artifacts();
    // A count of CPUs, and no swap: a run that goes over the memory limit is stopped.
    expect(a.docker).toContain("--memory 4g --memory-swap 4g --cpus 1.5 --pids-limit 256 \\\n");
    expect(a.docker).not.toMatch(/--(memory-max|cpu-quota|tasks-max)/);
    // The env file carries none of it: not as the installer's flags, not as docker's.
    expect(a.env).toBe(before.env);
    // And the rest of each command is what it was.
    expect(a.install.replace(" --memory-max 4G --cpu-quota 150% --tasks-max 256", "")).toBe(before.install);
  });

  it("goes with an instance name, after it", async () => {
    await openHelper();
    fireEvent.change(screen.getAllByPlaceholderText("e.g. tax").at(-1) as HTMLInputElement, { target: { value: "tax" } });
    type("e.g. 4G", "2G");
    await waitFor(() => expect(artifacts().install).toContain("--instance tax --memory-max 2G"));
    // A container is one agent: the instance does not reach it, the limit does.
    expect(artifacts().docker).not.toContain("--instance");
    expect(artifacts().docker).toContain("--memory 2g --memory-swap 2g");
  });

  // Not the command minus the limit: an administrator who typed one and copied
  // a command that silently lacked it would believe the agent bounded.
  it("shows no install or container command while a limit is not one the installer takes", async () => {
    await openHelper();
    const before = artifacts();
    type("e.g. 4G", "4GB");
    await waitFor(() => expect(artifacts().install.startsWith("# A resource limit is not valid")).toBe(true));
    const a = artifacts();
    expect(a.docker.startsWith("# A resource limit is not valid")).toBe(true);
    expect(a.install).not.toContain("curl");
    expect(a.docker).not.toContain("docker run");
    expect(screen.getAllByText(/No command is shown until every field is valid, or empty\./).length).toBeGreaterThan(0);
    // The env file never held a limit, so it is still offered, unchanged.
    expect(a.env).toBe(before.env);

    // Corrected, all of it is back.
    type("e.g. 4G", "");
    await waitFor(() => expect(artifacts().install).toBe(before.install));
    expect(artifacts().docker).toBe(before.docker);
  });

  it("says where the limits go, beside the fields", async () => {
    await openHelper();
    expect(screen.getByText(/In the install command and the container command, not the env file\./)).toBeTruthy();
  });
});

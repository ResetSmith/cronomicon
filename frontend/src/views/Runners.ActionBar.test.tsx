// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

const RUNNER = {
  id: "019f0000-1111-2222-3333-aaaaaaaaaaaa",
  name: "runner-a",
  status: "online",
  protocolVersion: 13,
  version: "0.52.28",
  capabilities: ["bash"],
};
// FX-7 — degraded (still heartbeating, still holding work).
const DEGRADED = {
  id: "019f0000-1111-2222-3333-bbbbbbbbbbbb",
  name: "runner-b",
  status: "degraded",
  protocolVersion: 13,
  version: "0.52.28",
  capabilities: ["bash"],
};
const OFFLINE = {
  id: "019f0000-1111-2222-3333-cccccccccccc",
  name: "runner-c",
  status: "offline",
  protocolVersion: 13,
  version: "0.52.28",
  capabilities: ["bash"],
};

vi.mock("../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/client")>();
  return {
    ...actual,
    api: {
      GET: vi.fn(async (path: string) => {
        if (path === "/runners") return { data: { items: [RUNNER, DEGRADED, OFFLINE] } };
        return { data: { items: [] } };
      }),
      POST: vi.fn(async () => ({ data: {} })),
    } as unknown as typeof actual.api,
    fetchCapabilities: vi.fn(async () => ({ manageEnvVars: false, configureApp: false })),
    fetchVersion: vi.fn(async () => ({ version: "0.52.28" })),
  };
});

import { Runners } from "./Runners";
import { upgradeCommand } from "./runner-upgrade-cmd";

const writeText = vi.fn(async () => {});

beforeEach(() => {
  writeText.mockClear();
  Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
});

afterEach(cleanup);

// FX-4 — the upgrade command used to sit below the fold in the detail's "Agent
// version" section. It is a per-runner action, so it belongs with the other
// per-runner actions, and it is offered whatever the version badge says.
describe("Runners — expanded row action bar", () => {
  const expandRunner = async (name = "runner-a") => {
    const row = await screen.findByRole("button", { name: new RegExp(`Runner ${name} — expand details`) });
    fireEvent.click(row);
  };
  const renderRunners = () =>
    render(
      <MemoryRouter>
        <Runners />
      </MemoryRouter>,
    );

  it("offers Copy upgrade command in the action bar and copies the command", async () => {
    renderRunners();
    await expandRunner();

    // The accessible name is the visible label plus the runner it targets — the
    // page can show several of these at once, and "Copy upgrade command" three
    // times over tells a screen-reader user nothing about which host.
    const btn = await screen.findByRole("button", { name: "Copy upgrade command for runner-a" });
    fireEvent.click(btn);

    await waitFor(() => expect(writeText).toHaveBeenCalledWith(upgradeCommand(window.location.origin)));
    await waitFor(() => expect(btn.textContent).toContain("Copied"));
  });

  // FX-14 — Registered/Protocol were rendered twice under different labels. The
  // Registration section is the surviving copy.
  it("states the registration facts once", async () => {
    renderRunners();
    await expandRunner();

    await screen.findByText("Protocol version");
    expect(screen.queryByText("Protocol")).toBeNull();
    expect(screen.queryByText("Registered")).toBeNull();
    expect(screen.getByText("Registered at")).toBeTruthy();
  });
});

// FX-7 — a degraded runner still heartbeats and still holds work, so it is
// exactly the runner an operator needs to Resync or Drain. Gating those on
// `online` took away every recovery action and left only Test + Deregister.
describe("Runners — recovery actions by status (FX-7)", () => {
  const expandRunner = async (name: string) => {
    fireEvent.click(await screen.findByRole("button", { name: new RegExp(`Runner ${name} — expand details`) }));
  };
  const renderRunners = () =>
    render(
      <MemoryRouter>
        <Runners />
      </MemoryRouter>,
    );

  it("keeps Resync, Scan keys and Drain on a degraded runner", async () => {
    renderRunners();
    await expandRunner("runner-b");

    await waitFor(() => expect(screen.getByRole("button", { name: "Resync" })).toBeTruthy());
    expect(screen.getByRole("button", { name: "Drain" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Scan keys" })).toBeTruthy();
  });

  // Offline is not "unhealthy", it is "nobody there to ask" — irrelevance still hides.
  it("still hides them on an offline runner", async () => {
    renderRunners();
    await expandRunner("runner-c");

    await waitFor(() => expect(screen.getByRole("button", { name: /Copy upgrade command/ })).toBeTruthy());
    expect(screen.queryByRole("button", { name: "Resync" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Scan keys" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Drain" })).toBeNull();
  });
});

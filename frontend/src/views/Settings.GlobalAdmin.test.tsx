// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

// GC (v2.2.2, gate closing) — Settings as an administrator of ONE agency sees it.
//
// Before 2.2.2 every settings write asked "configureApp on any agency". They are
// install-wide, so they now ask for a GLOBAL administrator (one grant covering
// every agency AND carrying the permission), which GET /capabilities reports as
// `configureAppGlobal` / `manageRolesGlobal` / `composeAdmin`. The flat flags
// still decide which sections are listed — the caller holds the permission, so
// by FX-7 the sections stay and say why they are read-only.
//
// Three shapes are pinned here, one per kind of section:
//   · read open, write global  → the form loads, a note above it, Save disabled
//     with the reason (General, Observability, …);
//   · read global too          → NO fetch, an explanation in place of the data
//     (GitLab, Vault, Recycle Bin) — a card that loads and prints "forbidden" is
//     the regression;
//   · a global administrator   → none of it.

let caps: Record<string, boolean> = {};
const gets: string[] = [];

const GENERAL = { appName: "Cronomicon", timezone: "UTC", appTimezone: "UTC", maxConcurrent: 4 };

vi.mock("../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/client")>();
  return {
    ...actual,
    fetchCapabilities: vi.fn(async () => ({
      vault: false, apprise: false, compose: false, composeUnbound: false, manageRoles: false,
      configureApp: false, manageEnvVars: false, publishSchedule: false, triggerJobs: false,
      killJobs: false, unrestricted: false,
      ...caps,
    })),
    api: {
      GET: vi.fn(async (path: string) => {
        gets.push(path);
        if (path === "/settings/general") return { data: GENERAL };
        if (path === "/settings/observability") return { data: { enabled: true, path: "/metrics", authType: "none" } };
        if (path === "/settings/gitlab") return { data: { repoUrl: "https://gitlab.example/defs.git", writeBranch: "main" } };
        if (path === "/settings/vault") return { data: { addr: "https://vault.internal:8200", status: "ok", authMethod: "approle" } };
        if (path === "/recycle-bin") return { data: { items: [], retentionDays: 30 } };
        return { data: [] };
      }),
      PUT: vi.fn(async () => ({ data: {} })),
      POST: vi.fn(async () => ({ data: {} })),
      DELETE: vi.fn(async () => ({ data: {} })),
    } as unknown as typeof actual.api,
  };
});

import { Settings } from "./Settings";

const WHY = "Only a global administrator (a role on every agency) can change this.";

beforeEach(() => {
  gets.length = 0;
  caps = {};
});
afterEach(cleanup);

const open = async () => {
  render(
    <MemoryRouter>
      <Settings />
    </MemoryRouter>,
  );
  await waitFor(() => expect(screen.getByText("General Settings")).toBeTruthy());
};
const go = (label: string) => fireEvent.click(screen.getByText(label));
const saveBtn = () => screen.getByRole("button", { name: "Save Changes" }) as HTMLButtonElement;

describe("Settings — an administrator of one agency (GC)", () => {
  beforeEach(() => {
    caps = { configureApp: true, manageRoles: true };
  });

  it("keeps every section it had — the flat permission still lists them", async () => {
    await open();
    for (const label of ["General", "Notifications", "GitLab Connection", "Vault", "Observability", "SSH Targets", "Log Storage", "Audit & Compliance", "Recycle Bin"]) {
      expect(screen.getAllByText(label).length, label).toBeGreaterThan(0);
    }
  });

  it("shows General read-only: the values load, a note says why, Save is disabled with the reason", async () => {
    await open();
    await waitFor(() => expect(screen.getByDisplayValue("Cronomicon")).toBeTruthy());
    expect(screen.getByText(`Read-only. ${WHY}`)).toBeTruthy();
    expect(saveBtn().disabled).toBe(true);
    expect(saveBtn().title).toBe(WHY);
    expect((screen.getByDisplayValue("Cronomicon") as HTMLInputElement).closest("fieldset")!.disabled).toBe(true);
  });

  it("shows Observability read-only the same way", async () => {
    await open();
    go("Observability");
    await waitFor(() => expect(screen.getByDisplayValue("/metrics")).toBeTruthy());
    expect(screen.getByText(`Read-only. ${WHY}`)).toBeTruthy();
    expect(saveBtn().disabled).toBe(true);
    expect(saveBtn().title).toBe(WHY);
  });

  it("does not fetch the GitLab or Vault connection — it explains instead of loading a 403", async () => {
    await open();
    go("GitLab Connection");
    expect(screen.getByRole("note").textContent).toMatch(
      /Only a global administrator \(a role on every agency\) can view or change this\./,
    );
    expect(screen.queryByRole("button", { name: "Save Changes" })).toBeNull();

    go("Vault");
    expect(screen.getByRole("note").textContent).toMatch(/can view or change this\./);

    await new Promise((r) => setTimeout(r, 20));
    expect(gets).not.toContain("/settings/gitlab");
    expect(gets).not.toContain("/settings/vault");
    expect(screen.queryByText(/Error:/)).toBeNull();
  });

  it("does not fetch the Recycle Bin without composeAdmin", async () => {
    await open();
    go("Recycle Bin");
    expect(screen.getByRole("note").textContent).toMatch(/shared by every agency/);
    await new Promise((r) => setTimeout(r, 20));
    expect(gets).not.toContain("/recycle-bin");
  });
});

describe("Settings — a global administrator (GC)", () => {
  beforeEach(() => {
    caps = { configureApp: true, configureAppGlobal: true, manageRoles: true, manageRolesGlobal: true, compose: true, composeAdmin: true, unrestricted: true };
  });

  it("gets General with a live Save and no read-only note", async () => {
    await open();
    await waitFor(() => expect(screen.getByDisplayValue("Cronomicon")).toBeTruthy());
    expect(screen.queryByText(/Read-only\./)).toBeNull();
    expect(saveBtn().disabled).toBe(false);
    expect(saveBtn().title).toBe("");
    expect((screen.getByDisplayValue("Cronomicon") as HTMLInputElement).closest("fieldset")!.disabled).toBe(false);
  });

  it("loads the GitLab and Vault connections and the Recycle Bin", async () => {
    await open();
    go("GitLab Connection");
    await waitFor(() => expect(screen.getByDisplayValue("https://gitlab.example/defs.git")).toBeTruthy());
    expect(saveBtn().disabled).toBe(false);
    expect(screen.queryByRole("note")).toBeNull();

    go("Vault");
    await waitFor(() => expect(screen.getByDisplayValue("https://vault.internal:8200")).toBeTruthy());

    go("Recycle Bin");
    await waitFor(() => expect(screen.getByText(/Nothing has been deleted/)).toBeTruthy());
    expect(gets).toContain("/settings/gitlab");
    expect(gets).toContain("/settings/vault");
    expect(gets).toContain("/recycle-bin");
  });

  it("an older server that omits the global flags reads as NOT global — fail closed", async () => {
    // `undefined` must never be mistaken for "allowed": withholding is the safe
    // way to be wrong, because the server refuses anyway.
    caps = { configureApp: true };
    await open();
    await waitFor(() => expect(screen.getByDisplayValue("Cronomicon")).toBeTruthy());
    expect(saveBtn().disabled).toBe(true);
    expect(saveBtn().title).toBe(WHY);
  });
});

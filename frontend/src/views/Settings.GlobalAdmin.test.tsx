// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

// Settings, by who is reading it (GC in v2.2.2; LR-86 in v2.3.0).
//
// Settings is two groups. INSTALLATION is what only a global administrator
// changes, and it is not shown to anyone else: in 2.2.2 an administrator of one
// agency saw each of those sections read-only with a note, which is a whole
// section that can never apply to its reader. AGENCY is what an agency's
// administrators change for their own agency, and it is where they land.
//
// Pinned here:
//   · an administrator of one agency → the Agency group and nothing of
//     Installation, no fetch of an install-wide setting, and an address that
//     names an Installation section falls back to their first;
//   · the Recycle Bin (still `composeAdmin`) explains instead of loading a 403;
//   · a global administrator → both groups, live, and addressable by ?tab=.

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

beforeEach(() => {
  gets.length = 0;
  caps = {};
});
afterEach(cleanup);

const INSTALLATION = ["General", "Notifications", "GitLab Connection", "Vault", "Observability", "Log Storage", "Audit & Compliance", "Local runner"];
const AGENCY = ["Users & Access", "Service Accounts", "SSH Targets", "Recycle Bin"];

const open = async (at = "/settings") => {
  render(
    <MemoryRouter initialEntries={[at]}>
      <Settings />
    </MemoryRouter>,
  );
  await waitFor(() => expect(screen.getByText("Agency")).toBeTruthy());
};
const go = (label: string) => fireEvent.click(screen.getByText(label));
const saveBtn = () => screen.getByRole("button", { name: "Save Changes" }) as HTMLButtonElement;
const rail = () => Array.from(document.querySelectorAll("div")).map((d) => d.textContent);

describe("Settings — an administrator of one agency (LR-86)", () => {
  beforeEach(() => {
    caps = { configureApp: true, manageRoles: true };
  });

  it("shows the Agency group and nothing of Installation", async () => {
    await open();
    for (const label of AGENCY) expect(screen.getAllByText(label).length, label).toBeGreaterThan(0);
    for (const label of INSTALLATION) expect(rail(), label).not.toContain(label);
    expect(screen.queryByText("Installation")).toBeNull();
    // And none of the install-wide settings was read on the way in.
    await new Promise((r) => setTimeout(r, 20));
    // (General is read by the timezone banner on every Settings page, so it is not in this list.)
    for (const path of ["/settings/gitlab", "/settings/vault", "/settings/observability"]) {
      expect(gets, path).not.toContain(path);
    }
  });

  it("falls back to their first section when the address names one that is not theirs", async () => {
    await open("/settings?tab=vault");
    await new Promise((r) => setTimeout(r, 20));
    expect(gets).not.toContain("/settings/vault");
    expect(screen.queryByText(/can view or change this/)).toBeNull();
  });

  it("opens the section its address names", async () => {
    await open("/settings?tab=recyclebin");
    // Still a global administrator's to use: it explains, and does not load a 403.
    expect(screen.getByRole("note").textContent).toMatch(/shared by every agency/);
    await new Promise((r) => setTimeout(r, 20));
    expect(gets).not.toContain("/recycle-bin");
  });
});

describe("Settings — someone who administers nothing", () => {
  it("says so, and where to look", async () => {
    caps = {};
    render(
      <MemoryRouter>
        <Settings />
      </MemoryRouter>,
    );
    expect(await screen.findByText(/You don't administer anything here/)).toBeTruthy();
  });
});

describe("Settings — a global administrator", () => {
  beforeEach(() => {
    caps = { configureApp: true, configureAppGlobal: true, manageRoles: true, manageRolesGlobal: true, compose: true, composeAdmin: true, unrestricted: true };
  });

  it("gets both groups, Installation first, with General live", async () => {
    await open();
    expect(screen.getByText("Installation")).toBeTruthy();
    for (const label of [...INSTALLATION, ...AGENCY]) expect(screen.getAllByText(label).length, label).toBeGreaterThan(0);
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

  it("opens straight on the section its address names", async () => {
    await open("/settings?tab=vault");
    await waitFor(() => expect(screen.getByDisplayValue("https://vault.internal:8200")).toBeTruthy());
    expect(screen.queryByText("General Settings")).toBeNull();
  });

  it("an older server that omits the global flags reads as NOT global — Installation is withheld", async () => {
    // `undefined` must never be mistaken for "allowed": withholding is the safe
    // way to be wrong, because the server refuses anyway.
    caps = { configureApp: true };
    await open();
    expect(screen.queryByText("Installation")).toBeNull();
    expect(screen.queryByText("General Settings")).toBeNull();
  });
});

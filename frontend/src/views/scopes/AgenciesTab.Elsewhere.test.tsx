// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

// 2.5.0 C7.4 (questions 96, 97) — an agency's panel shows, read-only, what is
// edited elsewhere: its repository, the grants that name it, its alerts, each
// with the way to the page that edits it in that agency. And the tab says that
// its counts are of what the reader may read.

let grantsRefused = false;
const getHeaders: Record<string, unknown> = {};

vi.mock("../../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/client")>();
  return {
    ...actual,
    api: {
      GET: vi.fn(async (path: string, opts?: { headers?: unknown }) => {
        getHeaders[path] = opts?.headers;
        if (path === "/agencies") return { data: [{ id: "ag-tax", name: "Tax", maxConcurrent: null }] };
        if (path === "/agency-matrix") return { data: { rows: [] } };
        if (path === "/agencies/{agencyId}") return { data: { members: [], onlineRunners: 0, queuedRuns: 0 } };
        if (path === "/agencies/{agencyId}/vault-prefixes") return { data: { agencyId: "ag-tax", prefixes: [] } };
        if (path === "/agencies/{agencyId}/groups") return { data: { agencyId: "ag-tax", groups: [] } };
        if (path === "/agencies/{agencyId}/git-repo")
          return { data: { id: "repo-tax", agencyId: "ag-tax", url: "https://git.example/tax.git", branch: "main", lastStatus: "success", lastSyncedAt: "2026-10-18T10:00:00Z" } };
        if (path === "/access-grants")
          return grantsRefused
            ? { error: { code: "forbidden", message: "you do not administer access" } }
            : { data: [
                { adGroup: "SG-Tax-Ops", role: "operator", agencyId: "ag-tax" },
                { adGroup: "SG-Fin-Ops", role: "operator", agencyId: "ag-fin" },
              ] };
        if (path === "/alerts") return { data: [{ ownerAgency: "ag-tax" }, { ownerAgency: "ag-tax" }, { ownerAgency: "global" }] };
        if (path === "/alert-targets") return { data: [{ ownerAgency: "ag-tax" }, { ownerAgency: "ag-fin" }] };
        return { data: [] };
      }),
      PUT: vi.fn(async () => ({ data: {} })),
      POST: vi.fn(async () => ({ data: {} })),
      DELETE: vi.fn(async () => ({ data: {} })),
    } as unknown as typeof actual.api,
  };
});

import { AgenciesTab } from "./AgenciesTab";

beforeEach(() => {
  grantsRefused = false;
});
afterEach(cleanup);

const openAgency = async (globalAdmin: boolean) => {
  render(
    <MemoryRouter>
      <AgenciesTab canEdit globalAdmin={globalAdmin} />
    </MemoryRouter>,
  );
  await waitFor(() => expect(screen.getByText("Tax")).toBeTruthy());
  fireEvent.click(screen.getByText("Tax").closest("tr")!);
  await waitFor(() => expect(screen.getByText("Repository, access and alerts")).toBeTruthy());
  return within(screen.getByText("Repository, access and alerts").parentElement!);
};

describe("an agency's panel: what is edited elsewhere (C7.4)", () => {
  it("shows the repository, the grants naming the agency and its alerts, with the way to each", async () => {
    const q = await openAgency(true);
    await waitFor(() => expect(q.getByText("https://git.example/tax.git")).toBeTruthy());
    expect(q.getByText("SG-Tax-Ops")).toBeTruthy();
    expect(q.queryByText("SG-Fin-Ops")).toBeNull();
    expect(q.getByText("2 alert rules, 1 push target")).toBeTruthy();
    // Alerts are read with no agency named, and kept to this agency's.
    expect(getHeaders["/alerts"]).toEqual({ "X-Cronomicon-Agency": "none" });
    const href = (name: RegExp) => (q.getByRole("link", { name }) as HTMLAnchorElement).getAttribute("href");
    expect(href(/Repositories/)).toBe("/settings?tab=repositories&agency=ag-tax");
    expect(href(/Users & Access/)).toBe("/settings?tab=users&agency=ag-tax");
    expect(href(/Alerts/)).toBe("/settings?tab=alerts&agency=ag-tax");
    // Read-only: no editor of its own.
    expect(q.queryByRole("button")).toBeNull();
  });

  it("says a read it may not make is not the reader's, rather than failing", async () => {
    grantsRefused = true;
    const q = await openAgency(false);
    await waitFor(() => expect(q.getByText("not yours to read")).toBeTruthy());
    expect(q.getByText("https://git.example/tax.git")).toBeTruthy();
  });
});

describe("the tab's counts (question 97)", () => {
  it("say they are of what the reader may read, to anyone but a global administrator", async () => {
    render(
      <MemoryRouter>
        <AgenciesTab canEdit globalAdmin={false} />
      </MemoryRouter>,
    );
    await waitFor(() => expect(screen.getByText("Tax")).toBeTruthy());
    expect(screen.getByRole("note").textContent).toMatch(/of what you may read/);
    cleanup();
    render(
      <MemoryRouter>
        <AgenciesTab canEdit globalAdmin />
      </MemoryRouter>,
    );
    await waitFor(() => expect(screen.getByText("Tax")).toBeTruthy());
    expect(screen.queryByText(/of what you may read/)).toBeNull();
  });
});

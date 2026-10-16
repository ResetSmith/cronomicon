// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";

// The Operations overview page (2.4.4 F3).

let CONTEXT: Record<string, unknown> = {};
let OVERVIEW: unknown = null;
let HITS: unknown[] = [];

vi.mock("../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/client")>();
  return {
    ...actual,
    setRequestAgency: vi.fn(),
    api: {
      GET: vi.fn(async (path: string) => {
        if (path === "/me/context") return { data: CONTEXT };
        if (path === "/overview") return { data: OVERVIEW };
        if (path === "/overview/search") return { data: HITS };
        return { data: [] };
      }),
    } as unknown as typeof actual.api,
  };
});

import { AgencyProvider, useAgency } from "../agency";
import { Overview, attentionHref, hitHref } from "./Overview";

const choices = [
  { id: "overview", name: "Overview: all agencies", kind: "overview" },
  { id: "global", name: "Global", kind: "agency" },
  { id: "ag-a", name: "agency-alpha", kind: "agency" },
  { id: "ag-b", name: "agency-beta", kind: "agency" },
];

function Where() {
  const ag = useAgency();
  const loc = useLocation();
  return (
    <span data-testid="where">
      {loc.pathname}|{ag.agency}
    </span>
  );
}

function renderAt(url: string) {
  return render(
    <MemoryRouter initialEntries={[url]}>
      <AgencyProvider>
        <Where />
        <Routes>
          <Route path="/overview" element={<Overview />} />
          <Route path="*" element={null} />
        </Routes>
      </AgencyProvider>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  CONTEXT = { choices, storedDefault: null, effectiveDefault: "overview", workingAgency: "global", canOverview: true };
  OVERVIEW = {
    agencies: [
      { id: "ag-a", name: "agency-alpha", jobs: 5, pausedJobs: 1, workflows: 2, pausedWorkflows: 0, runs24h: { success: 4, lost: 1 }, nextRunAt: null, repository: { status: "success", syncedAt: "2026-10-16T10:00:00Z" }, openNotices: 0 },
      { id: "ag-b", name: "agency-beta", jobs: 4, pausedJobs: 0, workflows: 0, pausedWorkflows: 0, runs24h: { failure: 1 }, nextRunAt: null, repository: { status: "failed", syncedAt: null }, openNotices: 2 },
    ],
    attention: [
      { kind: "run_lost", agencyId: "ag-a", agencyName: "agency-alpha", name: "e2e-alpha-slow", traceId: "t-1", at: "2026-10-16T11:00:00Z" },
      { kind: "sync_failed", agencyId: "ag-b", agencyName: "agency-beta", name: "agency-beta's repository", at: "2026-10-16T10:30:00Z" },
    ],
    attentionTotal: 2,
    generatedAt: "2026-10-16T12:00:00Z",
  };
  HITS = [];
});
afterEach(cleanup);

describe("the Operations overview (2.4.4 F3)", () => {
  it("shows a row per agency and what needs attention, and nothing to change", async () => {
    renderAt("/overview");
    const page = await screen.findByTestId("overview");
    expect(within(screen.getByTestId("overview-row-ag-a")).getByText("agency-alpha")).toBeTruthy();
    expect(screen.getByTestId("overview-row-ag-b").textContent).toContain("failed");
    expect(screen.getAllByTestId("attention-item")).toHaveLength(2);
    // Read-only: links to open things in their agency, no button to change one.
    expect(page.querySelectorAll("button")).toHaveLength(0);
  });

  it("opens an item in its own agency, which switches and says so", async () => {
    renderAt("/overview");
    await screen.findByTestId("overview");
    expect(screen.getByTestId("where").textContent).toBe("/overview|global");
    fireEvent.click(screen.getByRole("link", { name: "e2e-alpha-slow" }));
    await waitFor(() => expect(screen.getByTestId("where").textContent).toBe("/runs|ag-a"));
    expect(screen.getByRole("status").textContent).toBe("Switched to agency-alpha");
  });

  it("finds a job or workflow by name, saying whose", async () => {
    HITS = [{ kind: "job", id: "u-1", name: "nightly-backup", agencyId: "ag-b", agencyName: "agency-beta" }];
    renderAt("/overview");
    fireEvent.change(await screen.findByRole("textbox", { name: "Find a job or workflow" }), { target: { value: "night" } });
    const hit = await screen.findByRole("link", { name: "nightly-backup" });
    expect(hit.parentElement?.textContent).toContain("in agency-beta");
    expect(hit.getAttribute("href")).toBe("/runs?jobUid=u-1&agency=ag-b");
  });

  it("is not for someone whose access does not reach every agency", async () => {
    CONTEXT = { choices: choices.slice(1), storedDefault: null, effectiveDefault: "ag-a", workingAgency: "ag-a", canOverview: false };
    renderAt("/overview");
    expect(await screen.findByText("The overview is for those whose access reaches every agency.")).toBeTruthy();
  });

  it("does not land a global administrator on it", async () => {
    CONTEXT = { ...CONTEXT, effectiveDefault: "global", workingAgency: "global" };
    renderAt("/");
    await waitFor(() => expect(screen.getByTestId("where").textContent).toBe("/|global"));
  });

  it("links each kind of item where it is worked on", () => {
    const base = { agencyId: "ag-a", agencyName: "a", name: "n", at: "" };
    expect(attentionHref({ ...base, kind: "job_failed", id: "u" })).toBe("/runs?jobUid=u&agency=ag-a");
    expect(attentionHref({ ...base, kind: "run_overdue", traceId: "t" })).toBe("/runs?trace=t&agency=ag-a");
    expect(attentionHref({ ...base, kind: "workflow_failed" })).toBe("/runs?tab=workflow-runs&agency=ag-a");
    expect(attentionHref({ ...base, kind: "sync_failed" })).toBe("/runs?tab=git-sync&agency=ag-a");
    expect(hitHref({ kind: "workflow", id: "w", name: "n", agencyId: "global", agencyName: "Global" })).toBe("/workflows?agency=global");
  });
});

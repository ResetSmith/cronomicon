// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";

// The Operations overview page (2.4.4 F3).

let CONTEXT: Record<string, unknown> = {};
let OVERVIEW: unknown = null;
let HITS: unknown[] = [];
let RUNS: unknown[] = [];
// Set: /runs answers page by page, one run a page, of this many runs in all.
let RUNS_TOTAL: number | null = null;
const asked: { path: string; headers?: Record<string, string> }[] = [];

vi.mock("../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/client")>();
  return {
    ...actual,
    setRequestAgency: vi.fn(),
    api: {
      GET: vi.fn(async (path: string, opts?: { headers?: Record<string, string>; params?: { query?: { page?: number } } }) => {
        asked.push({ path, headers: opts?.headers });
        if (path === "/runs" && RUNS_TOTAL != null) {
          const page = opts?.params?.query?.page ?? 1;
          const at = new Date(Date.now() - page * 3600 * 1000).toISOString();
          return {
            data: {
              items: [{ traceId: "p" + page, jobName: "job-" + page, ownerAgency: "ag-a", status: "success", kind: "job", startedAt: at }],
              totalItems: RUNS_TOTAL,
              totalPages: RUNS_TOTAL,
              page,
            },
          };
        }
        if (path === "/runs") return { data: { items: RUNS } };
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
import { SCORE_MAX_PAGES, resetScoreRunsCache } from "../components/score-runs";

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
  RUNS = [];
  RUNS_TOTAL = null;
  resetScoreRunsCache();
  asked.length = 0;
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

  it("says who to call about each item, and marks a critical one (2.5.2 OP-1)", async () => {
    OVERVIEW = {
      ...(OVERVIEW as Record<string, unknown>),
      attention: [
        { kind: "job_failed", agencyId: "ag-a", agencyName: "agency-alpha", name: "payroll-export", id: "u-1", traceId: "t-1", contact: "Payroll on-call", critical: true, at: "2026-10-16T11:00:00Z" },
        { kind: "run_lost", agencyId: "ag-a", agencyName: "agency-alpha", name: "e2e-alpha-slow", id: "u-2", traceId: "t-2", at: "2026-10-16T10:50:00Z" },
        { kind: "sync_failed", agencyId: "ag-b", agencyName: "agency-beta", name: "agency-beta's repository", at: "2026-10-16T10:30:00Z" },
      ],
      attentionTotal: 3,
    };
    renderAt("/overview");
    await screen.findByTestId("overview");
    expect(screen.getByRole("columnheader", { name: "Contact" })).toBeTruthy();
    const [annotated, bare, sync] = screen.getAllByTestId("attention-item");
    expect(annotated.textContent).toContain("Payroll on-call");
    expect(within(annotated).getByText("Critical")).toBeTruthy();
    expect(bare.textContent).toContain("None set");
    expect(within(bare).queryByText("Critical")).toBeNull();
    // A repository has no contact to set.
    expect(sync.textContent).not.toContain("None set");
  });

  it("falls back to the agency's contact, marked as the agency's (2.5.2 OP-28)", async () => {
    OVERVIEW = {
      ...(OVERVIEW as Record<string, unknown>),
      attention: [
        { kind: "job_failed", agencyId: "ag-a", agencyName: "agency-alpha", name: "payroll-export", id: "u-1", traceId: "t-1", agencyContact: "Alpha on-call", at: "2026-10-16T11:00:00Z" },
        { kind: "sync_failed", agencyId: "ag-b", agencyName: "agency-beta", name: "agency-beta's repository", agencyContact: "Beta desk", at: "2026-10-16T10:30:00Z" },
      ],
      attentionTotal: 2,
    };
    renderAt("/overview");
    await screen.findByTestId("overview");
    const [job, sync] = screen.getAllByTestId("attention-item");
    expect(job.textContent).toContain("Alpha on-call (agency)");
    expect(job.textContent).not.toContain("None set");
    // A repository has no contact of its own; its agency's is who to call.
    expect(sync.textContent).toContain("Beta desk (agency)");
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

  it("draws the Score across every agency, its readout always showing, with an Agency column (F4)", async () => {
    const recent = new Date(Date.now() - 3600 * 1000).toISOString();
    RUNS = [
      { traceId: "t-a", jobName: "backup", ownerAgency: "ag-a", status: "success", kind: "job", startedAt: recent },
      { traceId: "t-b", jobName: "report", ownerAgency: "ag-b", status: "danger", kind: "job", startedAt: new Date(Date.now() - 3 * 3600 * 1000).toISOString() },
    ];
    renderAt("/overview");
    await screen.findByTestId("overview-score");
    await waitFor(() => expect(screen.getAllByRole("option").length).toBeGreaterThan(0));
    const names = screen.getAllByRole("option").map((o) => o.getAttribute("aria-label") ?? "").join(" | ");
    expect(names).toContain("backup, agency-alpha");
    expect(names).toContain("report, agency-beta");
    // The readout shows at rest (the most recent run) and has an Agency column.
    const agencyHead = await within(screen.getByTestId("overview-score")).findByRole("columnheader", { name: "Agency" });
    const table = agencyHead.closest("table") as HTMLElement;
    expect(within(table).getByText("backup")).toBeTruthy();
    expect(within(table).getByText("agency-alpha")).toBeTruthy();
    // A fixed size, room for ten jobs; the rest scroll (it no longer resizes as the pointer moves).
    const slot = screen.getByTestId("score-readout");
    expect(slot.style.height).toMatch(/^\d+px$/);
    expect(screen.getByTestId("score-readout-rows").style.overflowY).toBe("auto");
    // Its feeds ask for every agency the viewer reads, not the one in use.
    for (const p of ["/runs", "/schedules/upcoming", "/runners"]) {
      const a = asked.find((x) => x.path === p);
      expect(a?.headers?.["X-Cronomicon-Agency"], p).toBe("none");
    }
  });

  it("reads the Score's runs page after page, and says when the day held more", async () => {
    RUNS_TOTAL = 1350;
    renderAt("/overview");
    const note = await screen.findByTestId("score-cut");
    expect(asked.filter((a) => a.path === "/runs")).toHaveLength(SCORE_MAX_PAGES);
    expect(note.textContent).toContain("Showing the newest 5 of 1,350 runs");
  });

  it("reads no further than the last page, and says nothing then", async () => {
    RUNS_TOTAL = 2;
    renderAt("/overview");
    await screen.findByTestId("overview-score");
    await waitFor(() => expect(asked.filter((a) => a.path === "/runs")).toHaveLength(2));
    expect(screen.queryByTestId("score-cut")).toBeNull();
  });

  it("links each kind of item to the run it names (2.5.2 OP-4)", () => {
    const base = { agencyId: "ag-a", agencyName: "a", name: "n", at: "" };
    expect(attentionHref({ ...base, kind: "job_failed", id: "u", traceId: "t" })).toBe("/runs?trace=t&agency=ag-a");
    expect(attentionHref({ ...base, kind: "run_lost", id: "u", traceId: "t" })).toBe("/runs?trace=t&agency=ag-a");
    expect(attentionHref({ ...base, kind: "run_overdue", traceId: "t" })).toBe("/runs?trace=t&agency=ag-a");
    expect(attentionHref({ ...base, kind: "workflow_failed", id: "w", traceId: "wt" })).toBe("/runs?tab=workflow-runs&trace=wt&agency=ag-a");
    expect(attentionHref({ ...base, kind: "sync_failed" })).toBe("/runs?tab=git-sync&agency=ag-a");
    // An item without a run falls back to the agency's list, as before.
    expect(attentionHref({ ...base, kind: "job_failed", id: "u" })).toBe("/runs?agency=ag-a");
    expect(hitHref({ kind: "workflow", id: "w", name: "n", agencyId: "global", agencyName: "Global" })).toBe("/workflows?agency=global");
  });
});

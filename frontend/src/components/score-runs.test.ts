// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

// The Score's read (2.4.4): every page of its day, the older pages at most
// once a minute, kept per agency.

const calls: { page: number; agency: string; all: boolean }[] = [];
let TOTAL = 3;
let PAGE1: { traceId: string }[] = [{ traceId: "p1" }];
let agency = "ag-a";

vi.mock("../api/client", () => ({
  allReadable: { "X-Cronomicon-Agency": "none" },
  getRequestAgency: () => agency,
  api: {
    GET: vi.fn(async (_path: string, opts: { params: { query: { page: number } }; headers?: Record<string, string> }) => {
      const page = opts.params.query.page;
      calls.push({ page, agency, all: !!opts.headers });
      const items = page === 1 ? PAGE1 : [{ traceId: `${agency}-p${page}` }];
      return { data: { items, totalItems: TOTAL, totalPages: TOTAL, page } };
    }),
  },
}));

import { OLDER_PAGES_MS, fetchScoreRuns, resetScoreRunsCache } from "./score-runs";

const ids = (r: { data?: unknown }) => ((r.data as { items: { traceId: string }[] }).items ?? []).map((x) => x.traceId);

beforeEach(() => {
  calls.length = 0;
  TOTAL = 3;
  PAGE1 = [{ traceId: "p1" }];
  agency = "ag-a";
  resetScoreRunsCache();
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date("2026-10-16T12:00:00Z"));
});
afterEach(() => vi.useRealTimers());

describe("fetchScoreRuns", () => {
  it("reads every page the first time, then only page 1 until a minute has passed", async () => {
    expect(ids(await fetchScoreRuns())).toEqual(["p1", "ag-a-p2", "ag-a-p3"]);
    expect(calls.map((c) => c.page)).toEqual([1, 2, 3]);

    calls.length = 0;
    vi.setSystemTime(new Date(Date.now() + OLDER_PAGES_MS - 1000));
    expect(ids(await fetchScoreRuns())).toEqual(["p1", "ag-a-p2", "ag-a-p3"]);
    expect(calls.map((c) => c.page)).toEqual([1]);

    calls.length = 0;
    vi.setSystemTime(new Date(Date.now() + 2000));
    await fetchScoreRuns();
    expect(calls.map((c) => c.page)).toEqual([1, 2, 3]);
  });

  it("keeps each agency's older pages apart", async () => {
    await fetchScoreRuns();
    agency = "ag-b";
    calls.length = 0;
    expect(ids(await fetchScoreRuns())).toEqual(["p1", "ag-b-p2", "ag-b-p3"]);
    expect(calls.map((c) => c.page)).toEqual([1, 2, 3]);
    // The overview's every-agency read is its own too.
    calls.length = 0;
    await fetchScoreRuns({ allAgencies: true });
    expect(calls.map((c) => [c.page, c.all])).toEqual([[1, true], [2, true], [3, true]]);
  });

  it("draws a run once when page 1 has moved onto the kept pages", async () => {
    await fetchScoreRuns();
    PAGE1 = [{ traceId: "new" }, { traceId: "ag-a-p2" }];
    expect(ids(await fetchScoreRuns())).toEqual(["new", "ag-a-p2", "ag-a-p3"]);
  });

  it("asks once for a quiet day", async () => {
    TOTAL = 1;
    await fetchScoreRuns();
    expect(calls.map((c) => c.page)).toEqual([1]);
  });
});

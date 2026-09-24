// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import type { components } from "../api/schema";

// Expanding a row mounts RunLog, which fetches the run's log text.
vi.mock("../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/client")>();
  return {
    ...actual,
    api: { GET: vi.fn(async () => ({ data: "log line" })) } as unknown as typeof actual.api,
  };
});

import { RecentRuns } from "./RecentRuns";
import type { PagerState } from "./ui";

type Run = components["schemas"]["Run"];

afterEach(cleanup);

const RUNS = [
  { traceId: "019f0000-1111-2222-3333-aaaaaaaaaaaa", status: "success", startedAt: "2026-07-14T10:00:00Z", durationMs: 4200, exitCode: 0, manual: true, triggeredBy: "alice@ex.com", jobName: "deploy-web" },
  { traceId: "019f0000-1111-2222-3333-bbbbbbbbbbbb", status: "danger", startedAt: "2026-07-14T09:00:00Z", durationMs: 2100, exitCode: 1, manual: false, jobName: "db-migrate" },
] as unknown as Run[];

describe("RecentRuns", () => {
  it("shows the loading state while runs are in flight", () => {
    render(<RecentRuns runs={[]} loading={true} error={null} />);
    expect(screen.getByText("Loading runs…")).toBeTruthy();
  });

  it("surfaces a fetch error", () => {
    render(<RecentRuns runs={[]} loading={false} error="boom" />);
    expect(screen.getByText("boom")).toBeTruthy();
  });

  it("renders a custom empty state when there are no runs, and owns no caption (EV-1)", () => {
    render(<RecentRuns runs={[]} loading={false} error={null} emptyText="This runner hasn't executed any jobs yet." />);
    expect(screen.getByText("This runner hasn't executed any jobs yet.")).toBeTruthy();
    // The caption is the CALLER's now (it wraps this in a Section), so the
    // component must not render one of its own.
    expect(screen.queryByText("Recent runs")).toBeNull();
  });

  it("renders one row per run with mapped result + By (the caption is the caller's, EV-1)", () => {
    render(<RecentRuns runs={RUNS} loading={false} error={null} />);
    // Result badges go through statusLabel: success→Success, danger→Failed.
    expect(screen.getByText("Success")).toBeTruthy();
    expect(screen.getByText("Failed")).toBeTruthy();
    // "By" column: manual → triggeredBy; scheduler-triggered → "Cronomicon".
    expect(screen.getByText("alice@ex.com")).toBeTruthy();
    expect(screen.getByText("Cronomicon")).toBeTruthy();
    // One data row per run (header row lives in <thead>).
    const bodyRows = document.querySelectorAll("tbody > tr");
    expect(bodyRows.length).toBe(RUNS.length);
  });

  // FX-11 — RunLog's <pre> is 400px tall, so keeping the 340px cap while a row is
  // expanded nested one scrollport inside a shorter one: double scrollbars, and the
  // sticky header painting over the log the operator just opened.
  it("drops the list height cap while a run is expanded", () => {
    const { container } = render(<RecentRuns runs={RUNS} loading={false} error={null} />);
    const scrollport = container.querySelector("table")!.parentElement as HTMLElement;
    expect(scrollport.style.maxHeight).toBe("340px");
    expect(container.querySelector("th")!.style.position).toBe("sticky");

    fireEvent.click(screen.getAllByRole("row")[1]);

    expect(scrollport.style.maxHeight).toBe("");
    expect(container.querySelector("th")!.style.position).toBe("");
  });

  // Server-side paging — the footer renders only once the history outgrows one
  // page (or the operator is already off page 1), and its controls drive the
  // caller-owned pager state.
  describe("pager footer", () => {
    const mkPager = (over: Partial<PagerState> = {}): PagerState => ({
      page: 0,
      setPage: vi.fn(),
      pageSize: 25,
      setPageSize: vi.fn(),
      ...over,
    });

    it("is absent when the runs fit one page", () => {
      render(<RecentRuns runs={RUNS} loading={false} error={null} pager={mkPager()} page={0} total={2} />);
      expect(screen.queryByText("Next →")).toBeNull();
    });

    it("renders and pages forward when the history outgrows a page", () => {
      const pager = mkPager();
      render(<RecentRuns runs={RUNS} loading={false} error={null} pager={pager} page={0} total={60} />);
      fireEvent.click(screen.getByText("Next →"));
      expect(pager.setPage).toHaveBeenCalledWith(1);
    });

    it("stays visible off page 1 even when the tail page is short", () => {
      render(<RecentRuns runs={RUNS} loading={false} error={null} pager={mkPager({ page: 2 })} page={2} total={10} />);
      expect(screen.getByText("← Prev")).toBeTruthy();
    });
  });
});

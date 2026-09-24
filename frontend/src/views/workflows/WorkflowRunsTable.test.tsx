// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

// F-1/F-2 — the Status filter's round trip. The defect was that the dropdown
// offered raw wire statuses: the operator picked "danger" and got rows whose
// own Status column said Failed, and a soft-cancelled run — which this very
// table can create — had no option at all. The filter now offers the canonical
// labels and sends the wire value behind each one, so what you pick and what
// the rows say are the same word.

const QUERIES: Record<string, unknown>[] = [];

const RUNS = [
  { traceId: "wr-1", workflowName: "nightly", status: "danger", jobTraceIds: [] },
  { traceId: "wr-2", workflowName: "hourly", status: "cancelled", cancelled: true, jobTraceIds: [] },
];

vi.mock("../../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/client")>();
  return {
    ...actual,
    api: {
      GET: vi.fn(async (path: string, opts?: { params?: { query?: Record<string, unknown> } }) => {
        if (path === "/workflow-runs") {
          QUERIES.push(opts?.params?.query ?? {});
          return { data: { items: RUNS, page: 1, pageSize: 25, totalItems: RUNS.length } };
        }
        return { data: [] };
      }),
      POST: vi.fn(async () => ({ data: {} })),
    } as unknown as typeof actual.api,
  };
});

import { WorkflowRunsTable } from "./WorkflowRunsTable";

const renderTable = () =>
  render(
    <MemoryRouter>
      <WorkflowRunsTable storageKey="test-workflow-runs" />
    </MemoryRouter>,
  );

const statusSelect = () => screen.getByRole("combobox") as HTMLSelectElement;
const lastQuery = () => QUERIES[QUERIES.length - 1];

beforeEach(() => {
  QUERIES.length = 0;
  localStorage.clear();
});
afterEach(cleanup);

describe("WorkflowRunsTable status filter (F-1/F-2)", () => {
  it("offers the canonical labels, never the raw wire statuses", async () => {
    renderTable();
    await waitFor(() => expect(QUERIES.length).toBeGreaterThan(0));
    const options = [...statusSelect().options].map((o) => o.value);
    expect(options).toEqual(["All", "Running", "Success", "Warn", "Failed", "Skipped", "Cancelled"]);
    // The regression, spelled out: no option is a wire token.
    for (const raw of ["danger", "queued", "warning", "running", "success", "skipped"]) {
      expect(options).not.toContain(raw);
    }
    // Running appears once, not twice — statusLabel folds queued into it.
    expect(options.filter((o) => o === "Running").length).toBe(1);
  });

  it("sends the wire value behind the chosen label", async () => {
    renderTable();
    await waitFor(() => expect(QUERIES.length).toBeGreaterThan(0));
    expect(lastQuery().status).toBeUndefined(); // "All" narrows nothing

    for (const [label, wire] of [
      ["Failed", "danger"],
      ["Warn", "warning"],
      ["Running", "running"],
      ["Cancelled", "cancelled"],
      ["Success", "success"],
    ] as const) {
      fireEvent.change(statusSelect(), { target: { value: label } });
      await waitFor(() => expect(lastQuery().status).toBe(wire));
    }

    fireEvent.change(statusSelect(), { target: { value: "All" } });
    await waitFor(() => expect(lastQuery().status).toBeUndefined());
  });

  it("labels a soft-cancelled run the same word its filter option uses", async () => {
    renderTable();
    await waitFor(() => expect(QUERIES.length).toBeGreaterThan(0));
    // Two matches is the assertion, not a nuisance: the badge on the row and the
    // option in the filter are the same word. Before F-1 only the badge existed.
    const body = document.querySelector("tbody") as HTMLElement;
    await waitFor(() => expect(within(body).getByText("Cancelled")).toBeTruthy());
    expect([...statusSelect().options].map((o) => o.value)).toContain("Cancelled");
    expect(screen.getAllByText("Cancelled").length).toBe(2);
  });
});

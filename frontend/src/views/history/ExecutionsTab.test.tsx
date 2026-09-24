// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

// CS-2 — the `?job=` deep link the Dashboard's Current Status rows arrive on.
//
// The property under test is that the filter is applied SERVER-side. That is
// not a style preference: the free-text search box next to it refines the
// CURRENT PAGE only, so a job whose runs sit past page 1 would render a
// confident "No matching runs" while the pager still counted every run in the
// system. Asserting on the query the client sends is the only way to tell the
// two implementations apart from the outside — both look identical on page 1.

// vi.hoisted, because the vi.mock factory below is hoisted above every const in
// this file and would otherwise reference GET before initialisation.
const { GET } = vi.hoisted(() => ({
  GET: vi.fn(async () => ({ data: { items: [], totalItems: 0, page: 1, pageSize: 25 } })),
}));

vi.mock("../../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/client")>();
  return { ...actual, api: { GET } as unknown as typeof actual.api };
});

import { ExecutionsTab } from "./ExecutionsTab";

/** The query object of the most recent GET /runs call. */
const lastQuery = () => {
  const call = GET.mock.calls[GET.mock.calls.length - 1] as unknown as [string, { params?: { query?: Record<string, unknown> } }];
  return call?.[1]?.params?.query ?? {};
};

const renderAt = (url: string) =>
  render(
    <MemoryRouter initialEntries={[url]}>
      <ExecutionsTab />
    </MemoryRouter>,
  );

beforeEach(() => GET.mockClear());
afterEach(cleanup);

describe("ExecutionsTab — job deep link (CS-2)", () => {
  it("sends ?job= to the server rather than filtering the fetched page", async () => {
    renderAt("/runs?job=k8s-node-drain");
    await waitFor(() => expect(GET).toHaveBeenCalled());
    expect(lastQuery().job).toBe("k8s-node-drain");
  });

  it("names the filter on screen — an unexplained short table is the failure mode", async () => {
    renderAt("/runs?job=k8s-node-drain");
    const chip = await screen.findByTestId("job-filter-chip");
    expect(chip.textContent).toContain("k8s-node-drain");
  });

  it("sends no job param when the URL carries none", async () => {
    renderAt("/runs");
    await waitFor(() => expect(GET).toHaveBeenCalled());
    expect(lastQuery().job).toBeUndefined();
    expect(screen.queryByTestId("job-filter-chip")).toBeNull();
  });

  it("clearing the chip refetches unfiltered", async () => {
    renderAt("/runs?job=k8s-node-drain");
    const clear = await screen.findByRole("button", { name: "Clear the k8s-node-drain job filter" });

    fireEvent.click(clear);

    await waitFor(() => expect(lastQuery().job).toBeUndefined());
    expect(screen.queryByTestId("job-filter-chip")).toBeNull();
  });

  it("offers the filters back when a job filter excludes everything (VU-14)", async () => {
    // The distinction VU-14 exists for: this is "the filter excluded
    // everything", not "nothing has ever run" — so it must not offer the
    // go-create-a-job empty state.
    renderAt("/runs?job=never-ran");
    expect(await screen.findByText("No matching runs")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Clear filters" })).toBeTruthy();
    expect(screen.queryByText("No runs recorded yet")).toBeNull();
  });
});

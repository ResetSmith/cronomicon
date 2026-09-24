// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

// RX-22 — the "stopped by a human" filter, and specifically the two ways its URL
// round-trip can lie.
//
// The filter is server-side (killedBy IS NOT NULL/IS NULL), so what the client
// SENDS is the whole behaviour — a wrong query silently returns a different set
// of runs with a pager that agrees with it, which is indistinguishable from
// "there just aren't many" from the outside.

const { GET } = vi.hoisted(() => ({
  GET: vi.fn(async () => ({ data: { items: [], totalItems: 0, page: 1, pageSize: 25 } })),
}));

vi.mock("../../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/client")>();
  return { ...actual, api: { GET } as unknown as typeof actual.api };
});

import { ExecutionsTab } from "./ExecutionsTab";

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

describe("ExecutionsTab — stopped filter (RX-22)", () => {
  it("sends ?stopped=true to the server for the human-stopped deep link", async () => {
    renderAt("/runs?stopped=true");
    await waitFor(() => expect(GET).toHaveBeenCalled());
    expect(lastQuery().stopped).toBe(true);
  });

  it("sends ?stopped=false for the inverse", async () => {
    renderAt("/runs?stopped=false");
    await waitFor(() => expect(GET).toHaveBeenCalled());
    expect(lastQuery().stopped).toBe(false);
  });

  it("does not filter at all when the param is absent", async () => {
    renderAt("/runs");
    await waitFor(() => expect(GET).toHaveBeenCalled());
    expect(lastQuery().stopped).toBeUndefined();
  });

  // The regression this exists for: the request builder sends
  // `stopped: value === "true"`, so ANY non-empty value that is not exactly
  // "true" would send stopped=false — hiding every operator-stopped run behind a
  // filter the control still renders as "All". A hand-edited or stale link
  // (?stopped=1, ?stopped=True) must mean unfiltered, not inverted.
  it.each(["1", "True", "yes", "null"])("treats a malformed ?stopped=%s as unfiltered", async (raw) => {
    renderAt(`/runs?stopped=${raw}`);
    await waitFor(() => expect(GET).toHaveBeenCalled());
    expect(lastQuery().stopped).toBeUndefined();
  });

  // Clearing filters must clear the URL too, or a refresh or the Back button
  // resurrects a filter the operator just dismissed. This is one assertion for
  // three params because they share one navigation: react-router hands the
  // functional updater the CURRENT render's searchParams, so three back-to-back
  // setSearchParams calls each compute from the same base and only the last
  // survives — which used to leave ?stopped= and ?calendar= behind.
  it("drops stopped, calendar and job from the URL together when filters are cleared", async () => {
    renderAt("/runs?stopped=true&calendar=federal-holidays&job=nightly");
    await waitFor(() => expect(GET).toHaveBeenCalled());
    expect(lastQuery().stopped).toBe(true);

    fireEvent.click(await screen.findByText(/Clear filters/i));

    await waitFor(() => {
      const q = lastQuery();
      expect(q.stopped).toBeUndefined();
      expect(q.calendar).toBeUndefined();
      expect(q.job).toBeUndefined();
    });
  });
});

// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

// RX-17 — History answering "why did this run?".
//
// Reactions make the run graph implicit: nothing on a run's own row says what
// caused it, and nothing on the causing run says what it set off. These are
// server-side filters, so what the client SENDS is the whole behaviour — a wrong
// query silently returns a different set with a pager that agrees with it.

const { GET } = vi.hoisted(() => ({
  GET: vi.fn(async (_p?: string) => ({ data: { items: [], totalItems: 0, page: 1, pageSize: 25 } })),
}));

vi.mock("../../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/client")>();
  return { ...actual, api: { GET } as unknown as typeof actual.api };
});

import { ExecutionsTab, ReactionCauseLink, TriggeredRunsLink } from "./ExecutionsTab";

const lastQuery = () => {
  const call = GET.mock.calls[GET.mock.calls.length - 1] as unknown as [
    string,
    { params?: { query?: Record<string, unknown> } },
  ];
  return call?.[1]?.params?.query ?? {};
};

const renderAt = (url: string) =>
  render(
    <MemoryRouter initialEntries={[url]}>
      <ExecutionsTab />
    </MemoryRouter>,
  );

beforeEach(() => {
  GET.mockReset();
  GET.mockResolvedValue({ data: { items: [], totalItems: 0, page: 1, pageSize: 25 } });
});
afterEach(cleanup);

// The links themselves, not just the query strings they carry. The first version
// of this file mounted <ExecutionsTab /> directly and asserted only the query —
// which passed while BOTH links pointed at /history, a route this app does not
// have (History is mounted at /runs), so every because-of pivot rendered a blank
// page. Asserting the href is what makes a path regression fail here.
describe("History — because-of links target a route that exists", () => {
  // The links themselves, not just the query strings they carry. The first
  // version of this file asserted only the query, which passed while BOTH links
  // pointed at /history — a route this app does not mount (History is at
  // /runs), so every because-of pivot rendered a blank page. Testing the
  // rendered href is what makes a path regression fail here.
  it.each([
    ["forward (triggered by)", <ReactionCauseLink runId="run-cause" depth={1} />],
    ["reverse (set off)", <TriggeredRunsLink runId="run-effect" />],
  ])("%s points at a route the app actually mounts", (_label, el) => {
    const { container } = render(<MemoryRouter>{el}</MemoryRouter>);
    const hrefs = Array.from(container.querySelectorAll("a")).map((a) => a.getAttribute("href")!);
    expect(hrefs.length).toBeGreaterThan(0);
    for (const href of hrefs) {
      expect(href.startsWith("/runs?")).toBe(true);
      // Guard the specific mistake, by name, so the next person sees why.
      expect(href.startsWith("/history")).toBe(false);
    }
  });

  // A reaction fires jobs OR workflows, and History keeps them on different
  // tabs. One link would answer half the graph while looking complete: a
  // workflow this run started would simply not appear, with nothing saying so.
  it("reaches BOTH run tables from the reverse pivot", () => {
    const { container } = render(<MemoryRouter><TriggeredRunsLink runId="r" /></MemoryRouter>);
    const hrefs = Array.from(container.querySelectorAll("a")).map((a) => a.getAttribute("href")!);
    expect(hrefs.some((h) => !h.includes("tab="))).toBe(true);
    expect(hrefs.some((h) => h.includes("tab=workflow-runs"))).toBe(true);
  });

  it("shows the chain hop only when there is a chain to describe", () => {
    const one = render(<MemoryRouter><ReactionCauseLink runId="r" depth={1} /></MemoryRouter>);
    expect(one.container.textContent).not.toMatch(/hop/);
    cleanup();
    const deep = render(<MemoryRouter><ReactionCauseLink runId="r" depth={3} /></MemoryRouter>);
    expect(deep.container.textContent).toMatch(/hop 3/);
  });
});

describe("History — reaction provenance (RX-17)", () => {
  it("sends ?triggerKind=reaction for the Trigger filter deep link", async () => {
    renderAt("/runs?triggerKind=reaction");
    await waitFor(() => expect(GET).toHaveBeenCalled());
    expect(lastQuery().triggerKind).toBe("reaction");
  });

  // A hand-edited or stale value must read as unfiltered rather than returning
  // an empty table while the control claims "All" — the same normalisation the
  // stopped filter needed.
  it.each(["nonsense", "REACTION", "1"])("treats ?triggerKind=%s as unfiltered", async (raw) => {
    renderAt(`/runs?triggerKind=${raw}`);
    await waitFor(() => expect(GET).toHaveBeenCalled());
    expect(lastQuery().triggerKind).toBeUndefined();
  });

  it("sends ?reactedTo= for the reverse because-of pivot", async () => {
    renderAt("/runs?reactedTo=run-abc");
    await waitFor(() => expect(GET).toHaveBeenCalled());
    expect(lastQuery().reactedTo).toBe("run-abc");
  });

  it("sends ?trace= for the forward because-of pivot", async () => {
    renderAt("/runs?trace=run-cause");
    await waitFor(() => expect(GET).toHaveBeenCalled());
    expect(lastQuery().trace).toBe("run-cause");
  });

  // A server-side filter the operator did not set on this page has to be
  // visible and reversible, or a short table has no stated reason — the CS-2
  // rule the job chip already follows.
  it("shows a reversible chip for a pivot arrived at by link", async () => {
    renderAt("/runs?reactedTo=run-abcdef123");
    expect(await screen.findByTestId("reacted-to-chip")).toBeTruthy();
    expect(screen.getByText(/Triggered by run:/)).toBeTruthy();
  });

  it("shows the same chip for the trace pivot, labelled for what it is", async () => {
    renderAt("/runs?trace=run-abcdef123");
    expect(await screen.findByTestId("reacted-to-chip")).toBeTruthy();
    expect(screen.getByText(/^Run:$/)).toBeTruthy();
  });

  it("does not filter at all without the params", async () => {
    renderAt("/runs");
    await waitFor(() => expect(GET).toHaveBeenCalled());
    const q = lastQuery();
    expect(q.triggerKind).toBeUndefined();
    expect(q.reactedTo).toBeUndefined();
    expect(q.trace).toBeUndefined();
    expect(screen.queryByTestId("reacted-to-chip")).toBeNull();
  });
});

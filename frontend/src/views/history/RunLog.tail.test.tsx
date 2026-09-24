// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen, waitFor } from "@testing-library/react";

import { RunLog } from "./RunLog";
import { api } from "../../api/client";

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.restoreAllMocks();
});

// EP-8b (the expanded-panels plan) — live log tailing.
//
// The transport is an INCREMENTAL poll: each request carries ?offset= and the
// response's X-Log-Offset says where to resume. These pin the four rules that
// make that honest — it appends rather than re-downloads, it does one final
// read on the terminal flip, a failed tick keeps the buffer, and an inactive
// mount does not poll at all.

type Reply = { data?: unknown; error?: unknown; response?: { headers: { get: (k: string) => string | null } } };

const reply = (body: string, endOffset: number): Reply => ({
  data: body,
  response: { headers: { get: (k: string) => (k === "X-Log-Offset" ? String(endOffset) : null) } },
});

function mockLog(replies: Reply[]) {
  const calls: number[] = [];
  let i = 0;
  vi.spyOn(api, "GET").mockImplementation((async (_path: string, opts: { params?: { query?: { offset?: number } } }) => {
    calls.push(opts?.params?.query?.offset ?? -1);
    const r = replies[Math.min(i, replies.length - 1)];
    i += 1;
    return r;
  }) as unknown as typeof api.GET);
  return calls;
}

describe("RunLog tailing (EP-8b)", () => {
  beforeEach(() => vi.useFakeTimers({ shouldAdvanceTime: true }));

  it("an inactive mount fetches exactly once — pre-EP-8 behaviour is untouched", async () => {
    const calls = mockLog([reply("done\n", 5)]);
    render(<RunLog traceId="t1" />);
    await waitFor(() => expect(screen.getByText(/done/)).toBeTruthy());

    await act(async () => { vi.advanceTimersByTime(10_000); });
    expect(calls.length).toBe(1);
    expect(calls[0]).toBe(0); // the first read always starts at 0
  });

  it("an active mount polls incrementally and APPENDS rather than re-downloading", async () => {
    const calls = mockLog([reply("line one\n", 9), reply("line two\n", 18), reply("", 18)]);
    render(<RunLog traceId="t2" active />);
    await waitFor(() => expect(screen.getByText(/line one/)).toBeTruthy());

    await act(async () => { vi.advanceTimersByTime(2600); });
    await waitFor(() => expect(screen.getByText(/line two/)).toBeTruthy());

    // The buffer holds BOTH chunks — the second response carried only the new
    // bytes, so an implementation that replaced instead of appending would have
    // dropped "line one".
    expect(screen.getByText(/line one/).textContent).toContain("line two");
    // …and the second request resumed from the offset the first one reported.
    expect(calls[0]).toBe(0);
    expect(calls[1]).toBe(9);
  });

  it("does ONE final read when the run goes terminal, so the last lines are not lost", async () => {
    const calls = mockLog([reply("running…\n", 9), reply("finished\n", 18)]);
    const { rerender } = render(<RunLog traceId="t3" active />);
    await waitFor(() => expect(screen.getByText(/running…/)).toBeTruthy());
    const before = calls.length;

    rerender(<RunLog traceId="t3" active={false} />);
    await waitFor(() => expect(calls.length).toBe(before + 1));
    await waitFor(() => expect(screen.getByText(/finished/)).toBeTruthy());

    // …and then it stops. No polling on a finished run.
    await act(async () => { vi.advanceTimersByTime(10_000); });
    expect(calls.length).toBe(before + 1);
  });

  it("a failed tick keeps the buffer — a blip must not blank a log being read", async () => {
    const calls = mockLog([reply("kept\n", 5), { error: { message: "boom" } }]);
    render(<RunLog traceId="t4" active />);
    await waitFor(() => expect(screen.getByText(/kept/)).toBeTruthy());

    await act(async () => { vi.advanceTimersByTime(2600); });
    await waitFor(() => expect(calls.length).toBeGreaterThan(1));
    expect(screen.getByText(/kept/)).toBeTruthy();
  });

  it("restarts from zero if the server's offset goes backwards (a reaped or rotated log)", async () => {
    // Second response reports an END offset below our bookmark: the file was
    // replaced. Appending its bytes to the stale buffer would render a log that
    // never existed, so the buffer is replaced instead.
    const calls = mockLog([reply("old long content\n", 17), reply("fresh\n", 6)]);
    render(<RunLog traceId="t5" active />);
    await waitFor(() => expect(screen.getByText(/old long content/)).toBeTruthy());

    await act(async () => { vi.advanceTimersByTime(2600); });
    await waitFor(() => expect(screen.getByText(/fresh/)).toBeTruthy());
    expect(screen.getByText(/fresh/).textContent).not.toContain("old long content");
    expect(calls[1]).toBe(17);
  });
});

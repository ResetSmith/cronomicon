// @vitest-environment jsdom
import { describe, expect, it } from "vitest";
import { act, renderHook, waitFor } from "@testing-library/react";

import { useGet } from "./hooks";

// EP-2 (the expanded-panels plan) — useGet's third fetch mode.
// A manual refresh must be neither of the two modes that existed: "initial"
// flashes the skeleton over data the operator is reading, and "poll" swallows
// its errors, which would let a failed refresh look identical to a successful
// one. These tests pin the three rules the plan states.

// A fetcher whose promises the test resolves by hand, so the in-flight window
// is observable instead of raced.
function manualFetcher() {
  const pending: Array<{
    resolve: (v: { data?: unknown; error?: unknown }) => void;
    reject: (e: unknown) => void;
  }> = [];
  const fetcher = () =>
    new Promise<{ data?: unknown; error?: unknown }>((resolve, reject) => {
      pending.push({ resolve, reject });
    });
  return { fetcher, pending };
}

describe("useGet — refetch (EP-2)", () => {
  it("refetch() re-runs the thunk in refresh mode: data survives the in-flight window, no loading flash", async () => {
    const { fetcher, pending } = manualFetcher();
    const { result } = renderHook(() => useGet<string>(fetcher, []));

    await act(async () => pending[0].resolve({ data: "v1" }));
    await waitFor(() => expect(result.current.data).toBe("v1"));
    expect(result.current.loading).toBe(false);

    act(() => result.current.refetch());
    await waitFor(() => expect(pending.length).toBe(2));
    // The in-flight window: old data still shown, skeleton NOT re-shown,
    // refreshing on for the button.
    expect(result.current.data).toBe("v1");
    expect(result.current.loading).toBe(false);
    expect(result.current.refreshing).toBe(true);

    await act(async () => pending[1].resolve({ data: "v2" }));
    await waitFor(() => expect(result.current.data).toBe("v2"));
    expect(result.current.refreshing).toBe(false);
    expect(result.current.error).toBeNull();
  });

  it("a failed refresh surfaces the error, keeps the last good data, and clears refreshing (error-response path)", async () => {
    const { fetcher, pending } = manualFetcher();
    const { result } = renderHook(() => useGet<string>(fetcher, []));
    await act(async () => pending[0].resolve({ data: "v1" }));
    await waitFor(() => expect(result.current.data).toBe("v1"));

    act(() => result.current.refetch());
    await waitFor(() => expect(pending.length).toBe(2));
    await act(async () => pending[1].resolve({ error: { message: "boom" } }));

    await waitFor(() => expect(result.current.error).toBe("boom"));
    expect(result.current.data).toBe("v1"); // stale data stays under the error
    expect(result.current.refreshing).toBe(false);
  });

  it("a failed refresh clears refreshing on the rejected-promise path too", async () => {
    const { fetcher, pending } = manualFetcher();
    const { result } = renderHook(() => useGet<string>(fetcher, []));
    await act(async () => pending[0].resolve({ data: "v1" }));
    await waitFor(() => expect(result.current.data).toBe("v1"));

    act(() => result.current.refetch());
    await waitFor(() => expect(pending.length).toBe(2));
    await act(async () => pending[1].reject(new Error("net down")));

    await waitFor(() => expect(result.current.refreshing).toBe(false));
    expect(result.current.error).toContain("net down");
    expect(result.current.data).toBe("v1");
  });

  it("a deps change stays an initial load (skeleton), not a refresh — a new identity must not show the old one's data", async () => {
    const { fetcher, pending } = manualFetcher();
    const { result, rerender } = renderHook(({ id }) => useGet<string>(fetcher, [id]), {
      initialProps: { id: 1 },
    });
    await act(async () => pending[0].resolve({ data: "job-1" }));
    await waitFor(() => expect(result.current.data).toBe("job-1"));

    rerender({ id: 2 });
    await waitFor(() => expect(pending.length).toBe(2));
    expect(result.current.loading).toBe(true);
    expect(result.current.refreshing).toBe(false);

    await act(async () => pending[1].resolve({ data: "job-2" }));
    await waitFor(() => expect(result.current.data).toBe("job-2"));
  });

  it("an initial failure still clears to null (pre-EP-2 behaviour unchanged)", async () => {
    const { fetcher, pending } = manualFetcher();
    const { result } = renderHook(() => useGet<string>(fetcher, []));
    await act(async () => pending[0].reject(new Error("down")));
    await waitFor(() => expect(result.current.error).toContain("down"));
    expect(result.current.data).toBeNull();
    expect(result.current.loading).toBe(false);
  });
});

// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";

import { RefreshScope } from "./RefreshScope";
import { RefreshButton } from "./ui";
import { useGet } from "../hooks";

afterEach(cleanup);

// EP-3 (the expanded-panels plan) — one button, every fetch in the
// panel. The point of the ambient nonce is that fetches the button holds no
// reference to still participate, so the interesting test is a useGet nested
// two components deep, not one beside the button.

function manualFetcher() {
  const pending: Array<(v: { data?: unknown; error?: unknown }) => void> = [];
  const calls = { n: 0 };
  const fetcher = () => {
    calls.n += 1;
    return new Promise<{ data?: unknown; error?: unknown }>((resolve) => pending.push(resolve));
  };
  return { fetcher, pending, calls };
}

describe("RefreshScope (EP-3)", () => {
  it("a bump re-fetches a useGet nested two components deep", async () => {
    const { fetcher, pending, calls } = manualFetcher();
    const Leaf = () => {
      const q = useGet<string>(fetcher, []);
      return <span data-testid="leaf">{q.data ?? "…"}</span>;
    };
    const Middle = () => <div><Leaf /></div>;

    render(
      <RefreshScope>
        <RefreshButton />
        <Middle />
      </RefreshScope>,
    );
    await act(async () => pending[0]({ data: "first" }));
    await waitFor(() => expect(screen.getByTestId("leaf").textContent).toBe("first"));
    expect(calls.n).toBe(1);

    fireEvent.click(screen.getByRole("button", { name: /refresh/i }));
    await waitFor(() => expect(calls.n).toBe(2));
    // The old value stays visible while the refresh is in flight — a refresh
    // must not blank a panel someone is reading.
    expect(screen.getByTestId("leaf").textContent).toBe("first");

    await act(async () => pending[1]({ data: "second" }));
    await waitFor(() => expect(screen.getByTestId("leaf").textContent).toBe("second"));
  });

  it("drives every fetch in the subtree, not just the first", async () => {
    const a = manualFetcher();
    const b = manualFetcher();
    const Two = () => {
      useGet<string>(a.fetcher, []);
      useGet<string>(b.fetcher, []);
      return null;
    };
    render(
      <RefreshScope>
        <RefreshButton />
        <Two />
      </RefreshScope>,
    );
    await waitFor(() => expect(a.calls.n).toBe(1));
    await act(async () => {
      a.pending[0]({ data: "a" });
      b.pending[0]({ data: "b" });
    });

    fireEvent.click(screen.getByRole("button", { name: /refresh/i }));
    await waitFor(() => {
      expect(a.calls.n).toBe(2);
      expect(b.calls.n).toBe(2);
    });
  });

  it("the button reports real in-flight work and returns to idle", async () => {
    const { fetcher, pending } = manualFetcher();
    const Leaf = () => {
      useGet<string>(fetcher, []);
      return null;
    };
    render(
      <RefreshScope>
        <RefreshButton />
        <Leaf />
      </RefreshScope>,
    );
    await act(async () => pending[0]({ data: "x" }));
    const btn = () => screen.getByRole("button", { name: /refresh/i });
    expect(btn().getAttribute("aria-busy")).toBe("false");

    fireEvent.click(btn());
    await waitFor(() => expect(btn().getAttribute("aria-busy")).toBe("true"));
    expect((btn() as HTMLButtonElement).disabled).toBe(true);

    await act(async () => pending[1]({ data: "y" }));
    await waitFor(() => expect(btn().getAttribute("aria-busy")).toBe("false"));
    expect((btn() as HTMLButtonElement).disabled).toBe(false);
  });

  it("`also` drives a Shape-B list refetch alongside the ambient bump", async () => {
    const { fetcher, pending } = manualFetcher();
    const also = vi.fn();
    const Leaf = () => {
      useGet<string>(fetcher, []);
      return null;
    };
    render(
      <RefreshScope>
        <RefreshButton also={also} />
        <Leaf />
      </RefreshScope>,
    );
    await act(async () => pending[0]({ data: "x" }));
    fireEvent.click(screen.getByRole("button", { name: /refresh/i }));
    await waitFor(() => expect(also).toHaveBeenCalledTimes(1));
  });

  it("a useGet with no provider is completely unaffected (the additive guarantee)", async () => {
    const { fetcher, pending, calls } = manualFetcher();
    const Leaf = () => {
      const q = useGet<string>(fetcher, []);
      return <span data-testid="bare">{q.data ?? "…"}</span>;
    };
    render(<Leaf />);
    await act(async () => pending[0]({ data: "only" }));
    await waitFor(() => expect(screen.getByTestId("bare").textContent).toBe("only"));
    expect(calls.n).toBe(1);
  });
});

// ── Placement (the EP-4 bug) ────────────────────────────────────────────────
// React context reaches DESCENDANTS. A provider rendered inside a detail
// component's own `return` therefore does NOT cover that component's own useGet
// calls — they run in the component function, which sits ABOVE the provider in
// the tree. EP-4 shipped that mistake into seven panels: tsc passed, 928 unit
// tests passed, and a Jobs panel refresh re-fired 2 of its 5 requests. A browser
// network trace found it. These two tests are the guard, and the first one
// deliberately asserts the BROKEN shape so the trap is documented as behaviour
// rather than as a comment someone can skim past.
describe("RefreshScope placement (EP-4 regression)", () => {
  const mk = () => {
    const calls = { n: 0 };
    return { calls, fetcher: () => { calls.n += 1; return Promise.resolve({ data: "x" }); } };
  };

  it("WRONG: a provider inside a component's own return does NOT cover that component's fetch", async () => {
    const own = mk();
    const child = mk();
    const Child = () => { useGet<string>(child.fetcher, []); return null; };
    const Detail = () => {
      useGet<string>(own.fetcher, []); // outside its own provider below
      return (
        <RefreshScope>
          <RefreshButton />
          <Child />
        </RefreshScope>
      );
    };
    render(<Detail />);
    await waitFor(() => expect(own.calls.n).toBe(1));
    fireEvent.click(screen.getByRole("button", { name: /refresh/i }));
    await waitFor(() => expect(child.calls.n).toBe(2)); // the descendant refetches…
    expect(own.calls.n).toBe(1); // …the component's own fetch does not.
  });

  it("RIGHT: the provider at the CALL SITE covers the component's own fetch and its children's", async () => {
    const own = mk();
    const child = mk();
    const Child = () => { useGet<string>(child.fetcher, []); return null; };
    const Detail = () => {
      useGet<string>(own.fetcher, []);
      return (<><RefreshButton /><Child /></>);
    };
    render(
      <RefreshScope>
        <Detail />
      </RefreshScope>,
    );
    await waitFor(() => expect(own.calls.n).toBe(1));
    fireEvent.click(screen.getByRole("button", { name: /refresh/i }));
    await waitFor(() => {
      expect(own.calls.n).toBe(2);
      expect(child.calls.n).toBe(2);
    });
  });
});

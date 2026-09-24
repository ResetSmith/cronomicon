// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";

import { DetailPanel } from "../../components/ui";
import { useGet } from "../../hooks";
import { RefreshScope } from "../../components/RefreshScope";

// EP-4 Shape B (the expanded-panels plan) — the hazard this phase had
// to design around: five panels render LIST-ROW data, so their Refresh drives
// the view's list reload. The Env Vars tabs' existing refetch() ALSO collapsed
// the expansion, which would have closed the very panel the button refreshed.
// EP-4 split that into reloadList (data only) and refetch (data + collapse);
// these tests pin the contract the split has to satisfy.

afterEach(cleanup);

describe("Shape-B panel refresh (EP-4)", () => {
  it("drives the list reload without collapsing the expanded row", async () => {
    const reloadList = vi.fn();
    let expandedClosed = false;

    // A miniature of the Shape-B shape: a row that is expanded, whose panel
    // content comes from the list, and whose Refresh gets `reloadList`.
    function Tab() {
      return (
        <DetailPanel also={reloadList}>
          <div data-testid="panel">value from the list row</div>
        </DetailPanel>
      );
    }
    render(<Tab />);
    fireEvent.click(screen.getByRole("button", { name: /refresh/i }));

    await waitFor(() => expect(reloadList).toHaveBeenCalledTimes(1));
    // The panel is still mounted: refreshing must not close what it refreshed.
    expect(screen.getByTestId("panel")).toBeTruthy();
    expect(expandedClosed).toBe(false);
  });

  it("Shape A needs no `also` — the scope at the call site reaches the panel's own fetch", async () => {
    const calls = { n: 0 };
    const fetcher = () => {
      calls.n += 1;
      return Promise.resolve({ data: "detail" });
    };
    // The detail's own useGet and its DetailPanel are BOTH inside the provider,
    // because the provider is at the call site. DetailPanel deliberately does
    // not provide the scope itself — see its comment, and the placement
    // regression tests in RefreshScope.test.tsx.
    function Panel() {
      const q = useGet<string>(fetcher, []);
      return (
        <DetailPanel>
          <span data-testid="v">{q.data ?? "…"}</span>
        </DetailPanel>
      );
    }
    render(
      <RefreshScope>
        <Panel />
      </RefreshScope>,
    );
    await waitFor(() => expect(screen.getByTestId("v").textContent).toBe("detail"));
    expect(calls.n).toBe(1);

    fireEvent.click(screen.getByRole("button", { name: /refresh/i }));
    await waitFor(() => expect(calls.n).toBe(2));
  });

  it("Refresh leads the action strip — it must not sit beside a destructive action", () => {
    render(
      <DetailPanel actions={<button>Delete</button>}>
        <div />
      </DetailPanel>,
    );
    const buttons = screen.getAllByRole("button");
    expect(buttons[0].textContent).toMatch(/refresh/i);
    expect(buttons[1].textContent).toBe("Delete");
  });
});

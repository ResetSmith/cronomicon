// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

// SB — what a runner's row owes the operator about scope bindings.
//
// A binding is edited on the SCOPE; the runner's row carries the consequence.
// Two moments matter, and both are about a scope sitting closed because the one
// runner bound to it is not there:
//
//   - before it happens: "Scopes served" says which scopes stop if this runner
//     goes away, and offers the one-step hand-over;
//   - after it happens: a re-enrolled runner has a new id, so its predecessor's
//     bindings name nobody. The restore offer says which scopes are waiting —
//     including for a general-pool runner, which has no agency to restore and
//     used to get no offer at all.

const SERVING = {
  id: "019f0000-0000-0000-0000-00000000aaaa",
  name: "runner-dmz-01",
  status: "online",
  protocolVersion: 14,
  capabilities: ["bash"],
  agencies: [{ id: "ag-fin", name: "Finance" }],
};
const IDLE = { ...SERVING, id: "019f0000-0000-0000-0000-00000000bbbb", name: "runner-fin-02" };
// Re-enrolled into the general pool: no agencies, and nothing to restore but
// the scopes its old id still holds.
const REENROLLED = {
  id: "019f0000-0000-0000-0000-00000000cccc",
  name: "runner-pool-01",
  status: "online",
  protocolVersion: 14,
  capabilities: ["bash"],
  agencies: [],
  placementSuggestion: {
    historyId: 9,
    previousRunnerId: "019f0000-0000-0000-0000-00000000dead",
    agencies: [],
    tags: [],
    scopes: ["shared-hosts"],
    deregisteredAt: "2026-10-01T08:00:00Z",
    deregisteredVia: "reaper",
    previousClientIp: "10.0.0.9",
    currentClientIp: "10.0.0.9",
    clientIpMatches: true,
  },
};

const bound = (runnerId: string, name: string) => ({ runnerId, name, registered: true, status: "online", eligible: true });
const SCOPES = [
  { id: "s1", scope: "dmz-web", boundRunners: [bound(SERVING.id, SERVING.name)] },
  { id: "s2", scope: "dmz-db", boundRunners: [bound(SERVING.id, SERVING.name), bound("someone-else", "runner-x")] },
  { id: "s3", scope: "open", boundRunners: [] },
];

vi.mock("../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/client")>();
  return {
    ...actual,
    api: {
      GET: vi.fn(async (path: string) => {
        if (path === "/runners") {
          // A real request takes a turn of the event loop, and the registry
          // shows its skeleton (unmounting every row) for that long. An
          // instantly-resolved mock would batch the two renders into one and
          // hide exactly the remount the hand-over test is about.
          await new Promise((r) => setTimeout(r, 5));
          return { data: { items: [SERVING, IDLE, REENROLLED] } };
        }
        if (path === "/scopes") return { data: SCOPES };
        if (path === "/runners/host-keys/pending") return { data: [] };
        return { data: { items: [] } };
      }),
      POST: vi.fn(async () => ({ data: {} })),
    } as unknown as typeof actual.api,
    fetchCapabilities: vi.fn(async () => ({ configureApp: true })),
    fetchVersion: vi.fn(async () => ({ version: "2.2.0" })),
  };
});

import { Runners } from "./Runners";

afterEach(cleanup);

const renderRunners = () =>
  render(
    <MemoryRouter>
      <Runners />
    </MemoryRouter>,
  );

const expand = async (name: string) => {
  const row = await screen.findByRole("button", { name: new RegExp(`Runner ${name} — expand details`) });
  fireEvent.click(row);
};

describe("Runners — scopes served (SB)", () => {
  it("lists the scopes bound to the runner, says what happens if it goes away, and offers the hand-over", async () => {
    renderRunners();
    await expand("runner-dmz-01");

    const heading = await screen.findByText("Scopes served (2)");
    const section = within(heading.closest("section") ?? heading.parentElement!.parentElement!);
    // Sorted, and only the scopes that actually name this runner.
    expect(section.getByText("dmz-db")).toBeTruthy();
    expect(section.getByText("dmz-web")).toBeTruthy();
    expect(section.queryByText("open")).toBeNull();
    expect(screen.getByText(/these scopes' runs wait/)).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "Replace this runner…" }));
    expect(await screen.findByText("Replace runner-dmz-01")).toBeTruthy();
  });

  // A hand-over ends by reloading the runner list, and the registry unmounts its
  // rows while it loads. The offer to copy host keys was state inside the row,
  // so it was dropped before it was ever shown: the scopes moved and nothing
  // said the replacement trusts none of their hosts.
  it("after a hand-over, still offers to copy the host keys once the list has reloaded", async () => {
    renderRunners();
    await expand("runner-dmz-01");

    fireEvent.click(await screen.findByRole("button", { name: "Replace this runner…" }));
    const dialog = within((await screen.findByText("Replace runner-dmz-01")).closest("[role=dialog]") ?? document.body);
    fireEvent.change(await dialog.findByRole("combobox"), { target: { value: IDLE.id } });
    fireEvent.click(dialog.getByRole("button", { name: "Replace" }));

    expect(await screen.findByText(/Host keys approved for runner-dmz-01 are not handed over with its scopes/)).toBeTruthy();
    expect(screen.getByRole("button", { name: "Copy host keys to runner-fin-02…" })).toBeTruthy();
  });

  it("says so, and points at the Scopes page, for a runner bound to nothing", async () => {
    renderRunners();
    await expand("runner-fin-02");

    expect(await screen.findByText("Scopes served")).toBeTruthy();
    expect(screen.getByText(/this runner serves whatever its groups allow/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Replace this runner…" })).toBeNull();
  });
});

describe("Runners — restore offer for a general-pool runner (SB)", () => {
  it("offers a runner with no agencies when its old id still holds scopes, and names them", async () => {
    renderRunners();
    await expand("runner-pool-01");

    expect(await screen.findByText(/Previous placement found/i)).toBeTruthy();
    // No agency to restore — said as what it is, not as an empty list.
    expect(screen.getByText(/while in the general pool/)).toBeTruthy();
    const waiting = screen.getByText(/still bound to it/);
    expect(waiting.textContent).toMatch(/Scope shared-hosts is still bound to it, so its runs are waiting\. Restoring re-points it at this runner\./);
  });
});

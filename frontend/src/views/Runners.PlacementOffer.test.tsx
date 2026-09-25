// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

// A runner that re-registered after losing its identity: new id, same
// self-declared name, no agencies — and a snapshot of what it used to be.
const MATCHED = {
  id: "019f0000-1111-2222-3333-aaaaaaaaaaaa",
  name: "ansible-rh8",
  status: "online",
  protocolVersion: 13,
  capabilities: ["ansible", "bash"],
  agencies: [],
  placementSuggestion: {
    historyId: 7,
    previousRunnerId: "019f6163-20a8-79e5-9a45-902a00ec1e28",
    agencies: [{ id: "ag-carson", name: "Carson" }],
    tags: ["rh8"],
    deregisteredAt: "2026-08-26T08:36:03Z",
    deregisteredVia: "reaper",
    previousClientIp: "10.142.11.7",
    currentClientIp: "10.142.11.7",
    clientIpMatches: true,
  },
};
// Same, but rebuilt on a new lease — the weaker match DR-Q7 says must stay
// visible rather than be suppressed.
const MOVED = {
  ...MATCHED,
  id: "019f0000-1111-2222-3333-bbbbbbbbbbbb",
  name: "ansible-reno",
  placementSuggestion: {
    ...MATCHED.placementSuggestion,
    historyId: 8,
    currentClientIp: "10.231.86.4",
    clientIpMatches: false,
  },
};

// vi.mock is hoisted above module-level consts, so the spy has to be too.
const { POST } = vi.hoisted(() => ({ POST: vi.fn(async () => ({ data: {} })) }));

vi.mock("../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/client")>();
  return {
    ...actual,
    api: {
      GET: vi.fn(async (path: string) => {
        if (path === "/runners") return { data: { items: [MATCHED, MOVED] } };
        // configureApp reveals the pending host-keys panel, which consumes a
        // BARE array rather than an { items } envelope.
        if (path === "/runners/host-keys/pending") return { data: [] };
        return { data: { items: [] } };
      }),
      POST,
    } as unknown as typeof actual.api,
    fetchCapabilities: vi.fn(async () => ({ configureApp: true })),
    fetchVersion: vi.fn(async () => ({ version: "1.5.28" })),
  };
});

import { Runners } from "./Runners";

afterEach(() => {
  cleanup();
  POST.mockClear();
});

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

describe("Runners — placement suggestion (DR-7)", () => {
  it("offers the previous placement without applying it", async () => {
    renderRunners();
    await expand("ansible-rh8");

    expect(await screen.findByText(/Previous placement found/i)).toBeTruthy();
    // The agency it WOULD restore is named, so the operator knows what they are
    // authorising rather than clicking a bare "restore".
    expect(screen.getByText(/Carson/)).toBeTruthy();

    // DR-Q2: nothing has been applied merely by looking.
    expect(POST).not.toHaveBeenCalled();
  });

  it("labels the name as self-declared and the address as observed", async () => {
    renderRunners();
    await expand("ansible-rh8");

    // The distinction is the whole basis for asking a human: a name a rogue
    // agent can assert, versus an address it cannot.
    expect(await screen.findByText(/self-declared by the agent/i)).toBeTruthy();
    expect(screen.getAllByText(/observed/i).length).toBeGreaterThan(0);
  });

  it("warns when only the self-declared name matches", async () => {
    renderRunners();
    await expand("ansible-reno");

    expect(await screen.findByText(/Only the self-declared name matches/i)).toBeTruthy();
    // ...but the offer is still made — a host rebuilt during recovery lands on a
    // new lease, and suppressing it would hurt the case this exists for.
    expect(screen.getByRole("button", { name: /Restore placement/i })).toBeTruthy();
  });

  it("restores only when the operator asks", async () => {
    renderRunners();
    await expand("ansible-rh8");

    fireEvent.click(await screen.findByRole("button", { name: /Restore placement/i }));
    expect(POST).toHaveBeenCalledWith(
      "/runners/{runnerId}/placement",
      expect.objectContaining({ body: { historyId: 7 } }),
    );
  });

  it("dismisses through the server, never by applying", async () => {
    renderRunners();
    await expand("ansible-rh8");

    fireEvent.click(await screen.findByRole("button", { name: /^Dismiss$/i }));
    // DRF-3: a dismissal is a durable decision on the snapshot, so it is a
    // request — and specifically the dismiss endpoint, not the accept one.
    await waitFor(() =>
      expect(POST).toHaveBeenCalledWith(
        "/runners/{runnerId}/placement/dismiss",
        expect.objectContaining({ body: { historyId: 7 } }),
      ),
    );
    expect(POST).not.toHaveBeenCalledWith("/runners/{runnerId}/placement", expect.anything());
  });
});

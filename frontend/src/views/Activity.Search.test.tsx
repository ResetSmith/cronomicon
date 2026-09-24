// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

// AS-1 — the Activity page's search/filter/window are SERVER params. Before this
// the page fetched bare `/activity` (page 1, 50 rows) and filtered that array in
// the browser, so "Search activity…" silently covered the newest 50 events and
// nothing older, with no pager and no count to say so. These tests pin the
// wire: what gets SENT, not what the page happens to render from a fixture.

type Query = Record<string, unknown>;
const calls: Query[] = [];
let totalItems = 0;

vi.mock("../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/client")>();
  return {
    ...actual,
    api: {
      GET: vi.fn(async (path: string, opts?: { params?: { query?: Query } }) => {
        if (path === "/activity") {
          calls.push(opts?.params?.query ?? {});
          return {
            data: {
              items: totalItems === 0 ? [] : [{ id: 1, kind: "run-end", outcome: "success", actor: "op@ex.com", jobName: "nightly", at: "2026-08-25T10:00:00Z" }],
              totalItems,
              totalPages: Math.max(1, Math.ceil(totalItems / 25)),
              page: 1,
              pageSize: 25,
            },
          };
        }
        if (path === "/activity/actors") {
          return { data: { users: ["alice@corp.example", "op@ex.com"], runners: ["runner-east"], system: ["system"] } };
        }
        if (path === "/me") return { data: { email: "op@ex.com", roles: ["operator"] } };
        return { data: [] };
      }),
    } as unknown as typeof actual.api,
  };
});

import { Activity } from "./Activity";

beforeEach(() => {
  calls.length = 0;
  totalItems = 1;
});
afterEach(cleanup);

const renderActivity = async () => {
  const { container } = render(
    <MemoryRouter>
      <Activity />
    </MemoryRouter>,
  );
  const q = within(container);
  await waitFor(() => expect(q.getByText("nightly")).toBeTruthy());
  return q;
};
const last = () => calls[calls.length - 1];
const hoursAgo = (iso: unknown) => (Date.now() - Date.parse(String(iso))) / 3600e3;

describe("Activity — server-side search & window (AS-1)", () => {
  it("opens on a 72h server window with page 1 and no filters", async () => {
    await renderActivity();
    const q = last();
    expect(q.page).toBe(1);
    expect(q.pageSize).toBe(25);
    expect(hoursAgo(q.from)).toBeCloseTo(72, 0);
    expect(q.q).toBeUndefined();
    expect(q.kind).toBeUndefined();
    expect(q.actor).toBeUndefined();
  });

  it("sends the search box as ?q= (debounced) — the search spans the window, not the page", async () => {
    const q = await renderActivity();
    fireEvent.change(q.getByLabelText("Search activity"), { target: { value: "vault" } });
    await waitFor(() => expect(last().q).toBe("vault"));
    expect(last().page).toBe(1);
  });

  it("sends the Kind dropdown as ?kind=", async () => {
    const q = await renderActivity();
    fireEvent.change(q.getByLabelText("Filter by kind"), { target: { value: "config" } });
    await waitFor(() => expect(last().kind).toBe("config"));
  });

  it("backs 'My triggers' with ?actor=<me>, not a client-side compare", async () => {
    const q = await renderActivity();
    fireEvent.click(q.getByRole("button", { name: "My triggers" }));
    await waitFor(() => expect(last().actor).toBe("op@ex.com"));
    expect(q.getByRole("button", { name: "My triggers" }).getAttribute("aria-pressed")).toBe("true");
    fireEvent.click(q.getByRole("button", { name: "All" }));
    await waitFor(() => expect(last().actor).toBeUndefined());
  });

  // The Range select replaced a one-way "+ Show older" ratchet that could widen
  // but never narrow — the only way back to 72 hours was reloading the page.
  // Reversibility is the property that fix exists for, so it is what is pinned.
  it("moves ?from= to any window, in both directions", async () => {
    const q = await renderActivity();
    const range = () => q.getByLabelText("Time range") as HTMLSelectElement;

    fireEvent.change(range(), { target: { value: "90d" } });
    await waitFor(() => expect(hoursAgo(last().from)).toBeCloseTo(90 * 24, 0));

    // Back to the narrowest without a reload — impossible with the ratchet.
    fireEvent.change(range(), { target: { value: "72h" } });
    await waitFor(() => expect(hoursAgo(last().from)).toBeCloseTo(72, 0));

    fireEvent.change(range(), { target: { value: "7d" } });
    await waitFor(() => expect(hoursAgo(last().from)).toBeCloseTo(7 * 24, 0));
    expect(last().page).toBe(1);
  });

  it("offers every window plus a custom range, with the ceiling visible up front", async () => {
    const q = await renderActivity();
    const opts = Array.from((q.getByLabelText("Time range") as HTMLSelectElement).querySelectorAll("option"));
    expect(opts.map((o) => o.value)).toEqual(["72h", "7d", "30d", "90d", "custom"]);
    // The ratchet hid the ceiling until you hit it; the select states it.
    expect(opts.find((o) => o.value === "90d")!.textContent).toMatch(/everything retained/i);
  });

  it("a relative window never sends ?to= — it means 'until now', and a frozen upper edge would hide new events", async () => {
    const q = await renderActivity();
    fireEvent.change(q.getByLabelText("Time range"), { target: { value: "30d" } });
    await waitFor(() => expect(hoursAgo(last().from)).toBeCloseTo(30 * 24, 0));
    expect(last().to).toBeUndefined();
  });

  it("pages server-side and reports the FILTERED total", async () => {
    totalItems = 312;
    const q = await renderActivity();
    expect(q.getByText(/of 312 events/)).toBeTruthy();
    fireEvent.click(q.getByRole("button", { name: /Next/ }));
    await waitFor(() => expect(last().page).toBe(2));
  });

  it("names the window in the empty state and offers the full retention", async () => {
    totalItems = 0;
    const { container } = render(
      <MemoryRouter>
        <Activity />
      </MemoryRouter>,
    );
    const q = within(container);
    await waitFor(() => expect(q.getByText(/No activity in the last 72 hours/)).toBeTruthy());
    // One jump to everything, not a step — the escape hatch is for the operator
    // who found nothing and wants to stop guessing at window sizes.
    fireEvent.click(q.getByRole("button", { name: /Search all 90 days/ }));
    await waitFor(() => expect(hoursAgo(last().from)).toBeCloseTo(90 * 24, 0));
  });
});

// ── AA-3: the actor picker ───────────────────────────────────────────────────
//
// One choice across three groups, so one piece of state and one query param at
// a time. These pin which param a choice sends — the group decides it, and a
// runner sends ?runner= rather than ?actor= because the actor column holds
// `runner:<id>`, not the name.

describe("Activity — actor picker (AA-3)", () => {
  it("sends ?actor= for a user and never ?runner=", async () => {
    const q = await renderActivity();
    fireEvent.change(q.getByLabelText("Filter by actor"), { target: { value: "u:alice@corp.example" } });
    await waitFor(() => expect(last().actor).toBe("alice@corp.example"));
    expect(last().runner).toBeUndefined();
    expect(last().page).toBe(1);
  });

  it("sends ?runner= for a runner and never ?actor=", async () => {
    const q = await renderActivity();
    fireEvent.change(q.getByLabelText("Filter by actor"), { target: { value: "r:runner-east" } });
    await waitFor(() => expect(last().runner).toBe("runner-east"));
    expect(last().actor).toBeUndefined();
  });

  it("sends ?actor= for a system actor", async () => {
    const q = await renderActivity();
    fireEvent.change(q.getByLabelText("Filter by actor"), { target: { value: "s:system" } });
    await waitFor(() => expect(last().actor).toBe("system"));
    expect(last().runner).toBeUndefined();
  });

  // The picker and the chip are two affordances for ONE filter. Both set would
  // send actor=<me> AND runner=<x>, which the API honours as an AND — a query
  // no click asked for.
  it("clears the picker when My triggers is chosen, and vice versa", async () => {
    const q = await renderActivity();
    fireEvent.change(q.getByLabelText("Filter by actor"), { target: { value: "r:runner-east" } });
    await waitFor(() => expect(last().runner).toBe("runner-east"));

    fireEvent.click(q.getByRole("button", { name: "My triggers" }));
    await waitFor(() => expect(last().actor).toBe("op@ex.com"));
    expect(last().runner).toBeUndefined();
    expect((q.getByLabelText("Filter by actor") as HTMLSelectElement).value).toBe("");

    fireEvent.change(q.getByLabelText("Filter by actor"), { target: { value: "u:alice@corp.example" } });
    await waitFor(() => expect(last().actor).toBe("alice@corp.example"));
    expect(q.getByRole("button", { name: "My triggers" }).getAttribute("aria-pressed")).toBe("false");
  });

  it("returns to unfiltered on Anyone", async () => {
    const q = await renderActivity();
    fireEvent.change(q.getByLabelText("Filter by actor"), { target: { value: "r:runner-east" } });
    await waitFor(() => expect(last().runner).toBe("runner-east"));
    fireEvent.change(q.getByLabelText("Filter by actor"), { target: { value: "" } });
    await waitFor(() => expect(last().runner).toBeUndefined());
    expect(last().actor).toBeUndefined();
  });

  it("groups the options and omits empty groups", async () => {
    const q = await renderActivity();
    const sel = q.getByLabelText("Filter by actor");
    const groups = Array.from(sel.querySelectorAll("optgroup")).map((g) => g.getAttribute("label"));
    expect(groups).toEqual(["Users", "Runners", "System"]);
    // Values carry their group so a runner and a user sharing a name can never
    // produce the wrong query.
    expect(Array.from(sel.querySelectorAll("option")).map((o) => (o as HTMLOptionElement).value)).toEqual([
      "",
      "u:alice@corp.example",
      "u:op@ex.com",
      "r:runner-east",
      "s:system",
    ]);
  });
});

// ── The custom range (AS-2) ──────────────────────────────────────────────────
//
// The relative windows answer "what happened recently"; an absolute range
// answers "what happened on the 14th", which no relative window can express.
// The API had always accepted from AND to — `to` had simply never been sent by
// anything.

describe("Activity — custom range (AS-2)", () => {
  const pick = async (q: ReturnType<typeof within>) => {
    fireEvent.change(q.getByLabelText("Time range"), { target: { value: "custom" } });
    await waitFor(() => expect(q.getByLabelText("Range start")).toBeTruthy());
  };

  it("reveals the two inputs only when the custom range is chosen", async () => {
    const q = await renderActivity();
    expect(q.queryByLabelText("Range start")).toBeNull();
    await pick(q);
    expect(q.getByLabelText("Range end")).toBeTruthy();
  });

  it("sends both bounds as absolute instants", async () => {
    const q = await renderActivity();
    await pick(q);
    fireEvent.change(q.getByLabelText("Range start"), { target: { value: "2026-08-14T09:00" } });
    fireEvent.change(q.getByLabelText("Range end"), { target: { value: "2026-08-14T17:30" } });
    await waitFor(() => expect(last().to).toBeTruthy());
    // Absolute instants, whatever the browser's offset happens to be.
    expect(new Date(String(last().from)).getTime()).toBe(new Date("2026-08-14T09:00").getTime());
    expect(new Date(String(last().to)).getTime()).toBe(new Date("2026-08-14T17:30").getTime());
  });

  // Either end may be blank: the server treats from/to independently, so a
  // half-open range is a real query, not an incomplete form.
  it("supports an open-ended range in both directions", async () => {
    const q = await renderActivity();
    await pick(q);

    fireEvent.change(q.getByLabelText("Range start"), { target: { value: "2026-08-14T09:00" } });
    await waitFor(() => expect(last().from).toBeTruthy());
    expect(last().to).toBeUndefined();

    fireEvent.change(q.getByLabelText("Range start"), { target: { value: "" } });
    fireEvent.change(q.getByLabelText("Range end"), { target: { value: "2026-08-14T17:30" } });
    await waitFor(() => expect(last().to).toBeTruthy());
    expect(last().from).toBeUndefined();
  });

  it("switching back to a relative window drops ?to= entirely", async () => {
    const q = await renderActivity();
    await pick(q);
    fireEvent.change(q.getByLabelText("Range end"), { target: { value: "2026-08-14T17:30" } });
    await waitFor(() => expect(last().to).toBeTruthy());

    fireEvent.change(q.getByLabelText("Time range"), { target: { value: "72h" } });
    await waitFor(() => expect(hoursAgo(last().from)).toBeCloseTo(72, 0));
    expect(last().to).toBeUndefined();
    expect(q.queryByLabelText("Range start")).toBeNull();
  });
});

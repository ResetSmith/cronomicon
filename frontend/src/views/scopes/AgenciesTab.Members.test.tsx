// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";

// RB-22 — the per-agency member editor that replaced the Membership matrix.
//
// The matrix had a test; deleting it without replacing the coverage would mean the
// surface that took over its job ships untested. What matters here is what the
// matrix could not do: keep the row's identity on screen while you edit it, and
// write through the agency axis so a save names ONE agency.

const AGENCIES = [{ id: "ag-tax", name: "Tax", description: "Tax dept" }];

let DETAIL: Record<string, unknown> = {};
const MATRIX = {
  rows: [
    { kind: "secret", id: "s1", name: "DB_PASS", scope: "prod", agencyIds: ["ag-tax"] },
    { kind: "secret", id: "s2", name: "API_KEY", scope: "", agencyIds: [] },
    { kind: "runner", id: "r1", name: "runner-one", scope: "", agencyIds: [] },
  ],
};

const puts: Array<{ path: string; body: unknown }> = [];

vi.mock("../../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/client")>();
  return {
    ...actual,
    api: {
      GET: vi.fn(async (path: string) => {
        if (path === "/agencies") return { data: AGENCIES };
        if (path === "/agency-matrix") return { data: MATRIX };
        if (path === "/agencies/{agencyId}") return { data: DETAIL };
        return { data: [] };
      }),
      PUT: vi.fn(async (path: string, opts?: { body?: unknown }) => {
        puts.push({ path, body: opts?.body });
        // The endpoint answers with the resulting AgencyDetail; mirror that so the
        // panel re-renders from the response rather than refetching.
        const members = (opts?.body as { members: { kind: string; id: string }[] }).members.map((m) => {
          const row = MATRIX.rows.find((r) => r.kind === m.kind && r.id === m.id)!;
          return { kind: m.kind, id: m.id, name: row.name, scope: row.scope, status: "" };
        });
        DETAIL = { members, onlineRunners: 0, queuedRuns: 0 };
        return { data: DETAIL };
      }),
      POST: vi.fn(async () => ({ data: {} })),
      DELETE: vi.fn(async () => ({ data: {} })),
    } as unknown as typeof actual.api,
  };
});

import { AgenciesTab } from "./AgenciesTab";

beforeEach(() => {
  puts.length = 0;
  DETAIL = {
    members: [{ kind: "secret", id: "s1", name: "DB_PASS", scope: "prod", status: "" }],
    onlineRunners: 0,
    queuedRuns: 0,
  };
});
afterEach(cleanup);

const openAgency = async (canEdit = true) => {
  render(<AgenciesTab canEdit={canEdit} />);
  await waitFor(() => expect(screen.getByText("Tax")).toBeTruthy());
  fireEvent.click(screen.getByText("Tax").closest("tr")!);
  await waitFor(() => expect(screen.getByText("DB_PASS")).toBeTruthy());
};

describe("per-agency member editor (RB-22)", () => {
  it("lists the agency's members grouped by kind", async () => {
    await openAgency();
    // The member and its scope both render — the matrix showed the name only, and
    // an operator with DB_PASS in two scopes could not tell which row they held.
    expect(screen.getByText("DB_PASS")).toBeTruthy();
    expect(screen.getByText("@prod")).toBeTruthy();
  });

  it("adds a member through the agency-scoped setter", async () => {
    await openAgency();
    // The picker offers only what is NOT already in this agency.
    const add = screen.getByLabelText(/Add a secret to this agency/i) as HTMLSelectElement;
    expect(within(add).queryByText("DB_PASS @prod")).toBeNull();
    fireEvent.change(add, { target: { value: "s2" } });

    await waitFor(() => expect(puts.length).toBe(1));
    // ONE agency named in the path; the body is that agency's complete member list.
    // This is the write isolation the endpoint exists for — an entity-centric save
    // would have sent the SECRET's full agency list instead, and clobbered a
    // concurrent edit to another department.
    expect(puts[0].path).toBe("/agencies/{agencyId}/members");
    const sent = (puts[0].body as { members: { kind: string; id: string }[] }).members;
    expect(sent).toEqual([
      { kind: "secret", id: "s1" },
      { kind: "secret", id: "s2" },
    ]);
  });

  it("removes a member without touching the others", async () => {
    DETAIL = {
      members: [
        { kind: "secret", id: "s1", name: "DB_PASS", scope: "prod", status: "" },
        { kind: "runner", id: "r1", name: "runner-one", scope: "", status: "online" },
      ],
      onlineRunners: 1,
      queuedRuns: 0,
    };
    await openAgency();
    fireEvent.click(screen.getByLabelText(/Remove DB_PASS from this agency/i));

    await waitFor(() => expect(puts.length).toBe(1));
    const sent = (puts[0].body as { members: { kind: string; id: string }[] }).members;
    expect(sent).toEqual([{ kind: "runner", id: "r1" }]);
  });

  it("is read-only without ConfigureApp", async () => {
    await openAgency(false);
    // No remove affordance and no picker — a control that always 403s is worse
    // than no control.
    expect(screen.queryByLabelText(/Remove DB_PASS/i)).toBeNull();
    expect(screen.queryByLabelText(/Add a secret/i)).toBeNull();
  });

  it("says what an agency with no online runner means", async () => {
    await openAgency();
    // The trap the whole agencies plan came from: no online runner means every run
    // targeting this agency queues forever, and nothing alerts.
    expect(screen.getByText(/No online runner serves this agency/i)).toBeTruthy();
  });
});

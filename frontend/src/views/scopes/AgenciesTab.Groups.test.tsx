// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

// 2.5.0 C5 (GA-1) — a global administrator lists, on the agency's row, the
// directory groups its grants may name. The agency's delegates read the list
// (question 82) and cannot change it; anyone else is refused the read and sees
// nothing, as for the Vault paths.

let GROUPS: string[] = [];
let groupsForbidden = false;
const puts: Array<{ path: string; body: unknown }> = [];

vi.mock("../../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/client")>();
  return {
    ...actual,
    api: {
      GET: vi.fn(async (path: string) => {
        if (path === "/agencies") return { data: [{ id: "ag-tax", name: "Tax" }] };
        if (path === "/agency-matrix") return { data: { rows: [] } };
        if (path === "/agencies/{agencyId}") return { data: { members: [], onlineRunners: 0, queuedRuns: 0 } };
        if (path === "/agencies/{agencyId}/vault-prefixes") return { data: { agencyId: "ag-tax", prefixes: [] } };
        if (path === "/agencies/{agencyId}/groups") {
          return groupsForbidden ? { error: { code: "forbidden", message: "no" } } : { data: { agencyId: "ag-tax", groups: GROUPS } };
        }
        return { data: [] };
      }),
      PUT: vi.fn(async (path: string, opts?: { body?: unknown }) => {
        puts.push({ path, body: opts?.body });
        GROUPS = (opts?.body as { groups: string[] }).groups;
        return { data: { agencyId: "ag-tax", groups: GROUPS } };
      }),
      POST: vi.fn(async () => ({ data: {} })),
      DELETE: vi.fn(async () => ({ data: {} })),
    } as unknown as typeof actual.api,
  };
});

import { AgenciesTab } from "./AgenciesTab";

beforeEach(() => {
  puts.length = 0;
  GROUPS = [];
  groupsForbidden = false;
});
afterEach(cleanup);

const openAgency = async (rolesAdmin: boolean) => {
  render(
    <MemoryRouter>
      <AgenciesTab canEdit globalAdmin rolesAdmin={rolesAdmin} />
    </MemoryRouter>,
  );
  await waitFor(() => expect(screen.getByText("Tax")).toBeTruthy());
  fireEvent.click(screen.getByText("Tax").closest("tr")!);
  await waitFor(() => expect(screen.getByText(/Vault paths/)).toBeTruthy());
};

describe("an agency's group list (GA-1)", () => {
  it("says what an empty list means", async () => {
    await openAgency(true);
    await waitFor(() => expect(screen.getByText("Groups (0)")).toBeTruthy());
    expect(screen.getByText(/cannot grant access to any group/)).toBeTruthy();
  });

  it("is edited by a global administrator, one group per line, as typed", async () => {
    GROUPS = ["SG-Tax"];
    await openAgency(true);
    await waitFor(() => expect(screen.getByText("SG-Tax")).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "Edit groups" }));
    fireEvent.change(screen.getByLabelText("Directory groups, one per line"), { target: { value: "SG-Tax\n  SG-Tax-Ops \n\n" } });
    fireEvent.click(screen.getByRole("button", { name: "Save groups" }));
    await waitFor(() => expect(puts).toEqual([{ path: "/agencies/{agencyId}/groups", body: { groups: ["SG-Tax", "SG-Tax-Ops"] } }]));
    await waitFor(() => expect(screen.getByText("Groups (2)")).toBeTruthy());
  });

  it("is read, not edited, by the agency's delegate, with the reason", async () => {
    GROUPS = ["SG-Tax"];
    await openAgency(false);
    await waitFor(() => expect(screen.getByText("SG-Tax")).toBeTruthy());
    const edit = screen.getByRole("button", { name: "Edit groups" }) as HTMLButtonElement;
    expect(edit.disabled).toBe(true);
    expect(edit.title).toMatch(/Only a global administrator .* can list the groups/);
  });

  it("is not shown to someone refused the read", async () => {
    groupsForbidden = true;
    await openAgency(false);
    // Long enough for the refused read to land, so the absence is not just "not yet".
    await new Promise((r) => setTimeout(r, 30));
    expect(screen.queryByText(/^Groups \(/)).toBeNull();
  });
});

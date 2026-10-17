// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

// 2.5.0 C5 — the group list in the grant form (GA-1, GA-3; question 84).
//
// A delegate (manageRoles on an agency, not everywhere) grants only to a group
// a global administrator listed for the agency, so their group field is a
// picker of that list. A global administrator is not bound and types the
// group; the form says when it is not listed, since such a grant is noticed.

let caps: Record<string, boolean> = {};
const LISTS: Record<string, string[]> = { "ag-tax": ["SG-Tax", "SG-Tax-Ops"], "ag-fin": [] };
const posts: Record<string, unknown>[] = [];

vi.mock("../../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/client")>();
  return {
    ...actual,
    api: {
      GET: vi.fn(async (path: string, opts?: { params?: { path?: { agencyId?: string } } }) => {
        if (path === "/roles") return { data: [{ name: "operator", builtin: true, rank: 2, permissions: { triggerJobs: true } }] };
        if (path === "/agencies")
          return { data: [{ id: "ag-tax", name: "Tax" }, { id: "ag-fin", name: "Fin" }] };
        if (path === "/agencies/{agencyId}/groups") {
          const id = opts?.params?.path?.agencyId ?? "";
          return { data: { agencyId: id, groups: LISTS[id] ?? [] } };
        }
        return { data: [] };
      }),
      POST: vi.fn(async (_path: string, opts: { body: Record<string, unknown> }) => {
        posts.push(opts.body);
        return { data: {} };
      }),
      PUT: vi.fn(async () => ({ data: {} })),
      DELETE: vi.fn(async () => ({ data: {} })),
    } as unknown as typeof actual.api,
    fetchCapabilities: vi.fn(async () => ({
      manageRoles: true,
      configureApp: false,
      unrestricted: false,
      manageRolesGlobal: false,
      ...caps,
    })),
  };
});

import { UsersAccessSection } from "./UsersAccess";

const open = async () => {
  render(
    <MemoryRouter>
      <UsersAccessSection />
    </MemoryRouter>,
  );
  await waitFor(() => expect(screen.getByRole("option", { name: "Tax" })).toBeTruthy());
};
const whereSelect = () => screen.getByRole("option", { name: "Where…" }).closest("select") as HTMLSelectElement;
const pickWhere = (id: string) => fireEvent.change(whereSelect(), { target: { value: id } });

beforeEach(() => {
  caps = {};
  posts.length = 0;
});
afterEach(cleanup);

describe("the grant form for a delegate (GA-3)", () => {
  it("picks the group from the chosen agency's list, and sends it", async () => {
    await open();
    const group = screen.getByLabelText("AD group") as HTMLSelectElement;
    expect(group.disabled).toBe(true);
    expect(group.textContent).toContain("Pick where first");

    pickWhere("ag-tax");
    await waitFor(() => expect(screen.getByRole("option", { name: "SG-Tax-Ops" })).toBeTruthy());
    expect([...group.options].map((o) => o.value)).toEqual(["", "SG-Tax", "SG-Tax-Ops"]);
    fireEvent.change(group, { target: { value: "SG-Tax-Ops" } });
    fireEvent.click(screen.getByRole("button", { name: "+ Add" }));
    await waitFor(() => expect(posts).toEqual([{ adGroup: "SG-Tax-Ops", role: "operator", agencyId: "ag-tax" }]));
  });

  it("says when an agency lists no group, and offers none", async () => {
    await open();
    pickWhere("ag-fin");
    await waitFor(() => expect(screen.getByText(/No groups are listed for Fin/)).toBeTruthy());
    expect((screen.getByLabelText("AD group") as HTMLSelectElement).disabled).toBe(true);
  });

  it("clears the group when the agency changes: one agency's group is not another's", async () => {
    await open();
    pickWhere("ag-tax");
    await waitFor(() => expect(screen.getByRole("option", { name: "SG-Tax" })).toBeTruthy());
    fireEvent.change(screen.getByLabelText("AD group"), { target: { value: "SG-Tax" } });
    pickWhere("ag-fin");
    expect((screen.getByLabelText("AD group") as HTMLSelectElement).value).toBe("");
  });
});

describe("the grant form for a global administrator (question 84)", () => {
  beforeEach(() => {
    caps = { manageRolesGlobal: true };
  });

  it("types the group, and is told when it is not on the agency's list", async () => {
    await open();
    await waitFor(() => expect(screen.getByPlaceholderText("SG-Cronomicon-Tax")).toBeTruthy());
    pickWhere("ag-tax");
    const input = screen.getByPlaceholderText("SG-Cronomicon-Tax");
    fireEvent.change(input, { target: { value: "sg-tax" } });
    await waitFor(() => expect(screen.getByText(/sg-tax is not on Tax's group list/)).toBeTruthy());
    expect(screen.getByText(/listed in Notices until the group is listed/)).toBeTruthy();

    fireEvent.change(input, { target: { value: "SG-Tax" } });
    expect(screen.queryByText(/is not on Tax's group list/)).toBeNull();
  });
});

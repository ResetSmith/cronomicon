// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

// RF-14 (the RBAC-fixes plan) — the two cards that ACTUALLY control
// access had no tests at all, while the only test on this page exercised the
// legacy card that no longer does.
//
// That gap is not hypothetical: the "No longer controls access" banner was
// written for v0.56.5, never landed, and spent three releases letting operators
// edit a dead surface believing it was live. Nothing caught it because nothing
// asserted it.

const ROLES = [
  { name: "admin", description: "Full access", builtin: true, rank: 3, permissions: { manageRoles: true, configureApp: true } },
  { name: "viewer", description: "Read-only", builtin: true, rank: 1, permissions: {} },
  { name: "tax-operator", description: "Tax dept", builtin: false, rank: 2, permissions: { triggerJobs: true } },
];
const AGENCIES = [
  { id: "ag-tax", name: "Tax" },
  { id: "ag-fin", name: "Finance" },
];
let GRANTS: Array<Record<string, unknown>> = [];

const posts: Array<{ path: string; body: unknown }> = [];
const deletes: string[] = [];
const gets: string[] = [];

vi.mock("../../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/client")>();
  return {
    ...actual,
    api: {
      GET: vi.fn(async (path: string) => {
        gets.push(path);
        if (path === "/roles") return { data: ROLES };
        if (path === "/agencies") return { data: AGENCIES };
        if (path === "/access-grants") return { data: GRANTS };
        if (path === "/me") return { data: { email: "admin@ex.com", roles: ["admin"], groups: [] } };
        if (path === "/recent-logins") return { data: [] };
        return { data: [] };
      }),
      POST: vi.fn(async (path: string, opts?: { body?: unknown }) => {
        posts.push({ path, body: opts?.body });
        return { data: {} };
      }),
      PUT: vi.fn(async (path: string, opts?: { body?: unknown }) => {
        posts.push({ path, body: opts?.body });
        return { data: {} };
      }),
      DELETE: vi.fn(async (path: string) => {
        deletes.push(path);
        return { data: {} };
      }),
    } as unknown as typeof actual.api,
    // AF-3 — role TEMPLATE writes are unrestricted-only (a role is shared by
    // every agency), so these cards render their write affordances only for an
    // unrestricted administrator. This suite is that administrator; the
    // delegate's view is covered by UsersAccess.Delegate.test.tsx.
    fetchCapabilities: vi.fn(async () => ({
      vault: false,
      apprise: false,
      compose: true,
      composeUnbound: true,
      manageRoles: true,
      configureApp: true,
      manageEnvVars: true,
      publishSchedule: true,
      triggerJobs: true,
      killJobs: true,
      unrestricted: true,
    })),
  };
});

import { UsersAccessSection } from "./UsersAccess";
import { AuthProvider } from "../../auth";

beforeEach(() => {
  posts.length = 0;
  deletes.length = 0;
  gets.length = 0;
  GRANTS = [
    { id: "g1", adGroup: "SG-Tax", role: "tax-operator", agencyId: "ag-tax", agencyName: "Tax", allScopes: false },
    { id: "g2", adGroup: "SG-Admins", role: "admin", agencyId: null, agencyName: null, allScopes: true },
  ];
});
afterEach(cleanup);

const renderSection = async () => {
  render(
    <MemoryRouter>
      <AuthProvider>
        <UsersAccessSection />
      </AuthProvider>
    </MemoryRouter>,
  );
  await waitFor(() => expect(screen.getByText("SG-Tax")).toBeTruthy());
};

describe("Access Grants card (RB-20)", () => {
  it("renders each grant's where as an agency or All scopes", async () => {
    await renderSection();
    // A grant's "where" is the whole point of the model: same role, different
    // reach. Rendering both shapes identically would hide the distinction the
    // cross-product fix exists to make.
    const taxRow = screen.getByText("SG-Tax").closest("tr")!;
    expect(within(taxRow).getByText("Tax")).toBeTruthy();
    expect(within(taxRow).getByText("tax-operator")).toBeTruthy();
    const adminRow = screen.getByText("SG-Admins").closest("tr")!;
    expect(within(adminRow).getByText("All scopes")).toBeTruthy();
  });

  it("says plainly that no grants means no access", async () => {
    GRANTS = [];
    render(
      <MemoryRouter>
        <AuthProvider>
          <UsersAccessSection />
        </AuthProvider>
      </MemoryRouter>,
    );
    // "Empty means nobody has access" is the opposite of the old matrix's
    // "empty row = no restriction", and getting it backwards is how an operator
    // locks out their whole organisation.
    await waitFor(() => expect(screen.getByText(/Nobody has any access/i)).toBeTruthy());
  });

  it("warns that removing a grant revokes access and signs everyone out", async () => {
    await renderSection();
    const row = screen.getByText("SG-Tax").closest("tr")!;
    fireEvent.click(within(row).getByText("Remove"));
    // The confirm must state BOTH consequences — losing the role, and the epoch
    // bump that signs every other operator out — because neither is guessable
    // from a button labelled "Remove".
    await waitFor(() => expect(screen.getByText(/Remove this grant\?/i)).toBeTruthy());
    expect(screen.getByText(/signs out every other operator/i)).toBeTruthy();
    // Nothing is deleted until it is confirmed.
    expect(deletes.length).toBe(0);
    fireEvent.click(screen.getByText(/Remove grant/i));
    await waitFor(() => expect(deletes.length).toBe(1));
  });

  it("finds and filters once the list is long enough to need it (RF-18)", async () => {
    // The controls appear only past a handful of rows: on a single-department
    // install they would be noise, and this table is the one that grows fastest —
    // roughly a row per group per department.
    GRANTS = [
      { id: "a", adGroup: "SG-Tax-Ops", role: "tax-operator", agencyId: "ag-tax", agencyName: "Tax", allScopes: false },
      { id: "b", adGroup: "SG-Tax-View", role: "viewer", agencyId: "ag-tax", agencyName: "Tax", allScopes: false },
      { id: "c", adGroup: "SG-Fin-Ops", role: "tax-operator", agencyId: "ag-fin", agencyName: "Finance", allScopes: false },
      { id: "d", adGroup: "SG-Fin-View", role: "viewer", agencyId: "ag-fin", agencyName: "Finance", allScopes: false },
      { id: "e", adGroup: "SG-Admins", role: "admin", agencyId: null, agencyName: null, allScopes: true },
      { id: "f", adGroup: "SG-Extra", role: "viewer", agencyId: "ag-tax", agencyName: "Tax", allScopes: false },
    ];
    render(
      <MemoryRouter>
        <AuthProvider>
          <UsersAccessSection />
        </AuthProvider>
      </MemoryRouter>,
    );
    await waitFor(() => expect(screen.getByPlaceholderText(/Search group or where/i)).toBeTruthy());

    // Search matches the WHERE as well as the group: "who can touch Finance?" is
    // as common a question as "what does SG-Ops hold?".
    fireEvent.change(screen.getByPlaceholderText(/Search group or where/i), { target: { value: "finance" } });
    await waitFor(() => expect(screen.queryByText("SG-Tax-Ops")).toBeNull());
    expect(screen.getByText("SG-Fin-Ops")).toBeTruthy();
    expect(screen.getByText(/2 of 6/)).toBeTruthy();

    // Filtering to the unrestricted grants is its own option, because those are
    // the rows worth auditing first — they reach every department.
    fireEvent.change(screen.getByPlaceholderText(/Search group or where/i), { target: { value: "" } });
    const selects = screen.getAllByRole("combobox");
    const agencySelect = selects.find((sel) =>
      within(sel as HTMLElement).queryByText(/All scopes \(unrestricted\)/) != null &&
      within(sel as HTMLElement).queryByText(/All agencies/) != null,
    ) as HTMLSelectElement;
    fireEvent.change(agencySelect, { target: { value: "*" } });
    await waitFor(() => expect(screen.getByText("SG-Admins")).toBeTruthy());
    expect(screen.queryByText("SG-Tax-Ops")).toBeNull();
  });

  it("requires a where before a grant can be added", async () => {
    await renderSection();
    const group = screen.getByPlaceholderText("SG-Cronomicon-Tax");
    fireEvent.change(group, { target: { value: "SG-New" } });
    // Role defaults, but "Where…" does not: a grant with no reach is not a
    // narrower grant, it is a meaningless row. (Scoped to the grants card's own
    // add row — the legacy mappings card has its own "+ Add".)
    const addRow = group.parentElement as HTMLElement;
    const add = within(addRow).getByText("+ Add").closest("button") as HTMLButtonElement;
    expect(add.disabled).toBe(true);

    // Choosing a where enables it, so the assertion above is about the rule and
    // not about some unrelated disabled state.
    const wheres = within(addRow).getAllByRole("combobox");
    fireEvent.change(wheres[wheres.length - 1], { target: { value: "ag-tax" } });
    expect((within(addRow).getByText("+ Add").closest("button") as HTMLButtonElement).disabled).toBe(false);
  });
});

describe("Roles card (RB-11)", () => {
  // The role NAME appears in both cards (a grant names its role), so every
  // lookup here is scoped to the row carrying the role-table's own markup.
  const roleRow = (name: string) =>
    screen.getAllByText(name).map((el) => el.closest("tr")).find(
      (tr) => tr != null && within(tr as HTMLElement).queryByText("Edit") != null,
    ) as HTMLElement;

  it("lists built-in and custom roles, and offers Delete only on custom ones", async () => {
    await renderSection();
    // Built-ins are undeletable, and the button is ABSENT rather than disabled:
    // at that size a disabled red Delete beside `admin` reads as a bug.
    expect(within(roleRow("admin")).queryByText("Delete")).toBeNull();
    expect(within(roleRow("admin")).queryByText("built-in")).toBeTruthy();
    expect(within(roleRow("tax-operator")).queryByText("Delete")).toBeTruthy();
  });

  it("shows a viewer holding no permissions as a statement, not a blank", async () => {
    await renderSection();
    // viewer's only entry was viewDashboard, deleted in v0.56.1 — so it now holds
    // zero permissions, which is correct and worth saying out loud. An empty cell
    // reads as a rendering failure.
    expect(roleRow("viewer").textContent).toMatch(/No permissions/i);
  });

  it("warns that deleting a role signs everyone out", async () => {
    await renderSection();
    fireEvent.click(within(roleRow("tax-operator")).getByText("Delete"));
    await waitFor(() => expect(screen.getByText(/Delete role tax-operator\?/i)).toBeTruthy());
    expect(screen.getByText(/signed out/i)).toBeTruthy();
    expect(deletes.length).toBe(0);
  });
});

describe("legacy AD-group card", () => {
  it("is gone from the page entirely", async () => {
    await renderSection();
    // Removed in v0.57.7. It stopped deciding access at v0.56.5 and spent the
    // releases since wearing a "No longer controls access" banner — a form that
    // accepts edits and changes nothing, which is worse than no form. The banner
    // was the apology; this is the fix.
    //
    // Asserted by ABSENCE of the card, not of the banner: a re-added card that
    // forgot its banner would pass a banner-only check while being the exact
    // regression this fences.
    expect(screen.queryByText(/AD Group . Role Mappings/i)).toBeNull();
    expect(screen.queryByText(/No longer controls access/i)).toBeNull();
  });

  it("stops calling the retired endpoint", async () => {
    await renderSection();
    // The endpoint outlived the card by exactly one release: v0.57.7 removed the
    // card, v0.57.8 (RB-19 step 4) dropped the route and the table. Removing the
    // SPA's dependency first is what made that drop a cleanup rather than a
    // breaking change, and this assertion is what keeps the dependency from
    // creeping back in ahead of a route that no longer exists.
    expect(gets).not.toContain("/ad-group-mappings");
  });
});

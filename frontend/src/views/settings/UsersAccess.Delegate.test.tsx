// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

// AF-3 — the Users & Access screen as a DELEGATE sees it: a departmental holder
// of Manage roles & access, not a global administrator.
//
// Two rules are visible here, and both follow the principle the AF bands have
// used throughout: do not offer what the server will refuse.
//   · role TEMPLATES are shared by every agency, so their writes are
//     unrestricted-only (AF3-D1a) — the delegate reads the list and is told why
//     it is read-only, rather than meeting a 403 on click;
//   · "All scopes (unrestricted)" is not theirs to grant, so it is not listed.
// Everything else stays: they must still be able to grant and revoke within
// their own agencies, which is the entire point of delegation.

const ROLES = [
  { name: "admin", description: "Full access", builtin: true, rank: 3, permissions: { manageRoles: true } },
  { name: "operator", description: "Runs jobs", builtin: true, rank: 2, permissions: { triggerJobs: true } },
  { name: "tax-operator", description: "Tax dept", builtin: false, rank: 2, permissions: { triggerJobs: true } },
];

vi.mock("../../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/client")>();
  return {
    ...actual,
    api: {
      GET: vi.fn(async (path: string) => {
        if (path === "/roles") return { data: ROLES };
        if (path === "/agencies") return { data: [{ id: "ag-tax", name: "Tax" }] };
        if (path === "/access-grants") return { data: [] };
        if (path === "/recent-logins") return { data: [] };
        return { data: [] };
      }),
      POST: vi.fn(async () => ({ data: {} })),
      PUT: vi.fn(async () => ({ data: {} })),
      DELETE: vi.fn(async () => ({ data: {} })),
    } as unknown as typeof actual.api,
    fetchCapabilities: vi.fn(async () => ({
      vault: false,
      apprise: false,
      compose: false,
      composeUnbound: false,
      manageRoles: true, // administers access…
      configureApp: false,
      manageEnvVars: false,
      publishSchedule: false,
      triggerJobs: true,
      killJobs: true,
      unrestricted: false, // …but only for its own agencies
    })),
  };
});

import { UsersAccessSection } from "./UsersAccess";

afterEach(cleanup);

const renderSection = () => {
  const { container } = render(
    <MemoryRouter>
      <UsersAccessSection />
    </MemoryRouter>,
  );
  return within(container);
};

describe("Users & Access — the delegate's view (AF-3)", () => {
  it("shows the roles but offers no template writes, and says why", async () => {
    const q = renderSection();
    await waitFor(() => expect(q.getByText("Tax dept")).toBeTruthy());

    // Reading stays: knowing what a role carries is how a delegate chooses
    // which one to grant.
    expect(q.getByText("Runs jobs")).toBeTruthy();
    // Writing does not.
    expect(q.queryByRole("button", { name: "+ New role" })).toBeNull();
    expect(q.queryByRole("button", { name: "Edit" })).toBeNull();
    expect(q.queryByRole("button", { name: "Delete" })).toBeNull();
    // And the absence is explained rather than mysterious.
    expect(q.getByText(/only an unrestricted administrator changes them/i)).toBeTruthy();
  });

  it("withholds the All-scopes option when granting", async () => {
    const q = renderSection();
    await waitFor(() => expect(q.getByPlaceholderText("SG-Cronomicon-Tax")).toBeTruthy());

    // The named agencies are still offered — a delegate grants within them.
    expect(q.getByRole("option", { name: "Tax" })).toBeTruthy();
    // "Everywhere" is not theirs to give.
    expect(q.queryByRole("option", { name: /All scopes \(unrestricted\)/ })).toBeNull();
  });

  it("still offers the grant controls themselves", async () => {
    // The delegation must remain usable: hiding the whole card would make the
    // permission decorative, which is the failure mode this codebase treats as
    // a disease (see permissions.go).
    const q = renderSection();
    await waitFor(() => expect(q.getByPlaceholderText("SG-Cronomicon-Tax")).toBeTruthy());
    expect(q.getByRole("button", { name: "+ Add" })).toBeTruthy();
  });
});

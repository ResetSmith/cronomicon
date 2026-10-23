// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

// LR-86 / LR-87 — the user menu, "My access", and the Notices control.

let ACCESS: unknown = null;
let CAPS: Record<string, boolean> = {};
let NOTICES: unknown[] = [];
const posts: string[] = [];
const { logout } = vi.hoisted(() => ({ logout: vi.fn(async () => {}) }));

vi.mock("../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/client")>();
  return {
    ...actual,
    logout,
    fetchCapabilities: vi.fn(async () => ({ configureApp: false, manageRolesGlobal: false, ...CAPS })),
    api: {
      GET: vi.fn(async (path: string) => (path === "/notices" ? { data: NOTICES } : { data: [] })),
      POST: vi.fn(async (path: string) => {
        posts.push(path);
        return { data: {} };
      }),
    } as unknown as typeof actual.api,
  };
});
vi.mock("../api/access", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/access")>();
  return { ...actual, fetchMyAccess: vi.fn(async () => ACCESS) };
});

import { UserMenu, accessChip } from "./UserMenu";
import { NoticesButton } from "./Shell";

const grant = (over: Record<string, unknown>) => ({
  role: "admin", allScopes: false, agencyId: "ag-tax", agencyName: "Tax", scopes: ["tax-hosts"], permissions: ["configureApp", "triggerJobs"], groups: ["tax-admins"], origin: "group", ...over,
});
const access = (grants: unknown[], extra: Record<string, unknown> = {}) => ({
  email: "pat@example.com", name: "Pat", groups: ["tax-admins", "Tax-Viewers"], grants, unmatchedGroups: ["Tax-Viewers"], ...extra,
});

beforeEach(() => {
  ACCESS = access([grant({})]);
  CAPS = {};
  NOTICES = [];
  posts.length = 0;
  logout.mockClear();
});
afterEach(cleanup);

describe("accessChip — who am I here (LR-86)", () => {
  it("says Global admin, the role and agency, or a count", () => {
    expect(accessChip(null)).toBe("");
    expect(accessChip(access([]) as never)).toBe("No access");
    expect(accessChip(access([grant({})]) as never)).toBe("admin · Tax");
    expect(accessChip(access([grant({ allScopes: true, agencyId: undefined, agencyName: undefined })]) as never)).toBe("Global admin");
    // Reach is not authority: a viewer on every agency is not a global admin.
    expect(accessChip(access([grant({ role: "viewer", allScopes: true, permissions: [] })]) as never)).toBe("viewer · every agency");
    expect(accessChip(access([grant({}), grant({ agencyId: "ag-fin", agencyName: "Finance" })]) as never)).toBe("2 grants");
  });
});

describe("UserMenu (LR-87)", () => {
  const openMenu = async () => {
    render(<UserMenu email="pat@example.com" collapsed={false} icon={<span />} />);
    const trigger = await screen.findByRole("button", { name: /pat@example\.com/ });
    await waitFor(() => expect(trigger.textContent).toMatch(/admin · Tax/));
    fireEvent.click(trigger);
    return within(await screen.findByRole("dialog", { name: "Account" }));
  };

  it("shows who the caller is here, and signs out", async () => {
    const menu = await openMenu();
    expect(menu.getByText("admin · Tax")).toBeTruthy();
    // Not a global administrator for manageRoles: no "sign out everyone".
    expect(menu.queryByRole("button", { name: /Sign out everyone else/ })).toBeNull();
    fireEvent.click(menu.getByRole("button", { name: "Sign out" }));
    expect(logout).toHaveBeenCalled();
  });

  it("opens My access: each grant's role, agency, scopes and permissions, and the group that matched nothing", async () => {
    const menu = await openMenu();
    fireEvent.click(menu.getByRole("button", { name: "My access…" }));
    expect(await screen.findByText("My access")).toBeTruthy();
    const row = (await screen.findByText("1 scope: tax-hosts")).closest("tr")!;
    expect(within(row).getByText("admin")).toBeTruthy();
    expect(within(row).getByText("Configure, Run jobs")).toBeTruthy();
    expect(within(row).getByText("tax-admins")).toBeTruthy();
    // The usual reason for "I was added to the group and nothing changed".
    expect(screen.getByText(/Not named by any access grant:/).textContent).toMatch(/Tax-Viewers/);
    expect(screen.getByText(/must match exactly, letter case included/)).toBeTruthy();
  });

  it("says what a grant on an agency with no scopes is worth, and what no grant at all means", async () => {
    ACCESS = access([grant({ scopes: [] })]);
    let menu = await openMenu();
    fireEvent.click(menu.getByRole("button", { name: "My access…" }));
    expect(await screen.findByText("This agency has no scopes yet, so the grant reaches nothing.")).toBeTruthy();
    cleanup();

    ACCESS = access([], { unmatchedGroups: ["tax-admins", "Tax-Viewers"] });
    render(<UserMenu email="pat@example.com" collapsed={false} icon={<span />} />);
    const trigger = await screen.findByRole("button", { name: /pat@example\.com/ });
    await waitFor(() => expect(trigger.textContent).toMatch(/No access/));
    fireEvent.click(trigger);
    menu = within(await screen.findByRole("dialog", { name: "Account" }));
    fireEvent.click(menu.getByRole("button", { name: "My access…" }));
    expect(await screen.findByText(/None of your groups is named by an access grant/)).toBeTruthy();
  });

  it("offers a global administrator 'Sign out everyone else', behind a confirmation that says when it is needed", async () => {
    CAPS = { manageRolesGlobal: true };
    ACCESS = access([grant({ allScopes: true, permissions: ["configureApp", "manageRoles"] })]);
    render(<UserMenu email="root@example.com" collapsed={false} icon={<span />} />);
    const trigger = await screen.findByRole("button", { name: /root@example\.com/ });
    await waitFor(() => expect(trigger.textContent).toMatch(/Global admin/));
    fireEvent.click(trigger);
    const menu = within(await screen.findByRole("dialog", { name: "Account" }));
    fireEvent.click(await waitFor(() => menu.getByRole("button", { name: "Sign out everyone else…" })));
    expect(posts).toHaveLength(0);
    expect(await screen.findByText(/No change to roles, grants or scopes needs this/)).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Sign out everyone else" }));
    await waitFor(() => expect(posts).toEqual(["/auth/sessions/revoke"]));
    // Says what was done, and is true behind a sign-on proxy too, where there
    // was nothing of this kind to end.
    expect(await screen.findByText(/Every other Cronomicon session has been signed out\..*access already follows the proxy/)).toBeTruthy();
  });

  it("keeps a name on the trigger when the rail is collapsed", async () => {
    render(<UserMenu email="pat@example.com" collapsed icon={<span data-testid="ic" />} />);
    expect((await screen.findByRole("button", { name: "Account (pat@example.com)" })).querySelector("[data-testid=ic]")).toBeTruthy();
  });
});

describe("NoticesButton (LR-85)", () => {
  const show = () =>
    render(
      <MemoryRouter>
        <NoticesButton btnStyle={{}} pathname="/" />
      </MemoryRouter>,
    );

  it("is absent for someone who administers nothing", async () => {
    CAPS = { configureApp: false };
    NOTICES = [{ id: "n1" }];
    show();
    await new Promise((r) => setTimeout(r, 20));
    expect(screen.queryByRole("link")).toBeNull();
  });

  it("shows the number open, and links to the inbox", async () => {
    CAPS = { configureApp: true };
    NOTICES = [{ id: "n1" }, { id: "n2" }];
    show();
    const link = (await screen.findByRole("link", { name: "Notices, 2 open" })) as HTMLAnchorElement;
    expect(link.getAttribute("href")).toBe("/notices");
    expect(link.title).toBe("2 notices need attention");
  });

  it("carries no count when nothing is open", async () => {
    CAPS = { configureApp: true };
    show();
    const link = await screen.findByRole("link", { name: "Notices" });
    expect(link.textContent).toBe("Notices");
  });
});

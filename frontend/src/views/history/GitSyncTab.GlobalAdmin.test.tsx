// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

// History → Git Sync, one repository at a time (2.4.0 I3; R8).
//
// Every agency has a repository. The log is read per repository (`?repo=`,
// Global's when none is named), and "Sync now" syncs the one chosen: Global's
// is a global administrator's, an agency's is its administrators' too (GR-9).
// The button stays for everyone and is disabled with the reason for a caller
// the server would refuse (FX-7), in the toolbar and in the empty log's call
// to action.

let global = false;
let ACCESS: unknown = null;
const REPOS = [
  { id: "global", agencyId: "global", agencyName: "Global" },
  { id: "repo-fin", agencyId: "ag-fin", agencyName: "Finance" },
];
const { GET, POST } = vi.hoisted(() => ({
  GET: vi.fn(async (path: string) =>
    path === "/git-repo-names"
      ? { data: { items: REPOS } }
      : // The empty envelope carries `items: []` alongside `totalItems: 0`.
        { data: { items: [], totalItems: 0, totalPages: 1, page: 1, pageSize: 25 } },
  ),
  POST: vi.fn(async () => ({ data: {} })),
}));

vi.mock("../../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/client")>();
  return {
    ...actual,
    api: { GET, POST } as unknown as typeof actual.api,
    fetchCapabilities: vi.fn(async () => ({
      vault: false, apprise: false, compose: false, composeUnbound: false, manageRoles: false,
      // The flat flag is deliberately TRUE in both cases: an administrator of
      // one agency holds configureApp and is still refused Global's.
      configureApp: true, manageEnvVars: false, publishSchedule: false, triggerJobs: false,
      killJobs: false, unrestricted: false,
      configureAppGlobal: global,
    })),
  };
});
vi.mock("../../api/access", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/access")>();
  return { ...actual, useMyAccess: () => ACCESS };
});

import { GitSyncTab } from "./GitSyncTab";

const WHY = "Only a global administrator (a role on every agency) can sync Global's repository.";

beforeEach(() => {
  global = false;
  ACCESS = { grants: [{ role: "admin", allScopes: false, agencyId: "ag-fin", agencyName: "Finance", scopes: [], permissions: ["configureApp"], groups: [], origin: "group" }] };
  GET.mockClear();
  POST.mockClear();
  localStorage.clear();
});
afterEach(cleanup);

const open = async () => {
  render(
    <MemoryRouter>
      <GitSyncTab />
    </MemoryRouter>,
  );
  await waitFor(() => expect(screen.getByText("No Git sync events yet")).toBeTruthy());
  await waitFor(() => expect(screen.getByLabelText("Repository")).toBeTruthy());
  // Let the capabilities read land before judging what is disabled.
  await new Promise((r) => setTimeout(r, 20));
  return () => screen.getAllByRole("button", { name: /Sync now/ }) as HTMLButtonElement[];
};
const historyCalls = () =>
  (GET.mock.calls as unknown as [string, { params?: { query?: { repo?: string } } }][]).filter(([p]) => p === "/git/history");

describe("Git Sync — per repository (R8)", () => {
  it("reads Global's log first, and disables Global's sync for an agency's administrator, with the reason", async () => {
    const buttons = (await open())();
    expect(historyCalls().at(-1)![1].params!.query!.repo).toBe("global");
    expect(buttons.length).toBe(2);
    for (const b of buttons) {
      expect(b.disabled).toBe(true);
      expect(b.title).toBe(WHY);
    }
    for (const b of buttons) fireEvent.click(b);
    expect(POST).not.toHaveBeenCalled();
  });

  it("lets an agency's administrator read and sync their own repository", async () => {
    const buttons = await open();
    fireEvent.change(screen.getByLabelText("Repository"), { target: { value: "repo-fin" } });
    await waitFor(() => expect(historyCalls().at(-1)![1].params!.query!.repo).toBe("repo-fin"));
    await waitFor(() => expect(buttons()[0].disabled).toBe(false));
    fireEvent.click(buttons()[0]);
    await waitFor(() => expect(POST).toHaveBeenCalledTimes(1));
    expect(POST.mock.calls[0] as unknown).toEqual(["/git/sync", expect.objectContaining({ params: expect.objectContaining({ query: { repo: "repo-fin" } }) })]);
  });

  it("enables both for a global administrator, and the click reaches POST /git/sync", async () => {
    global = true;
    const buttons = (await open())();
    expect(buttons.length).toBe(2);
    for (const b of buttons) {
      expect(b.disabled).toBe(false);
      expect(b.title).toBe("");
    }
    fireEvent.click(buttons[0]);
    await waitFor(() => expect(POST).toHaveBeenCalledTimes(1));
    expect((POST.mock.calls[0] as unknown as [string])[0]).toBe("/git/sync");
  });
});

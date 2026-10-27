// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

// 2.5.3 CA-16: a new installation starts with one working agency, Operations,
// made at its first start. Its Dashboard offers a global administrator a name
// for it and, optionally, its administrators' AD group (question 210), until it
// has a name of its own or a grant names it. With every agency deleted, the
// All-agencies Dashboard offers to create one, with the same optional group.

type Ctx = { agency: string; agencyName: string; onOverview: boolean; globalName: string; choices: unknown[] };
const { inUse } = vi.hoisted(() => ({ inUse: { current: null as null | Ctx } }));
vi.mock("../../agency", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../agency")>();
  return { ...actual, useAgencyOptional: () => inUse.current };
});

let CAPS: Record<string, boolean> = {};
let AGENCIES: Record<string, unknown>[] = [];
let GRANTS: Record<string, unknown>[] = [];
let grantError: unknown = null;
const writes: { method: string; path: string; body: unknown; headers?: unknown }[] = [];

vi.mock("../../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/client")>();
  return {
    ...actual,
    fetchCapabilities: vi.fn(async () => CAPS),
    api: {
      GET: vi.fn(async (path: string) => {
        if (path === "/agencies") return { data: AGENCIES };
        if (path === "/access-grants") return { data: GRANTS };
        return { data: [] };
      }),
      PUT: vi.fn(async (path: string, init: { body: unknown; params?: { path?: { agencyId?: string } } }) => {
        writes.push({ method: "PUT", path, body: init.body });
        // As the server: the rename is the agency's new name on the next read.
        const id = init.params?.path?.agencyId;
        const name = (init.body as { name?: string }).name;
        if (path === "/agencies/{agencyId}" && id && name) AGENCIES = AGENCIES.map((a) => (a.id === id ? { ...a, name } : a));
        return { data: {} };
      }),
      POST: vi.fn(async (path: string, init: { body: unknown; headers?: unknown }) => {
        writes.push({ method: "POST", path, body: init.body, headers: init.headers });
        if (path === "/access-grants" && grantError) return { error: grantError };
        if (path === "/agencies") return { data: { id: "ag-new", name: (init.body as { name: string }).name } };
        return { data: {} };
      }),
    } as unknown as typeof actual.api,
  };
});

import { CreateFirstAgencyCard, NameYourAgencyCard } from "./FirstAgency";
import { AGENCY_HEADER, NO_AGENCY } from "../../api/client";

const OPS = { id: "ag-ops", name: "Operations", description: null, createdBy: "system (first start)" };
const inOps = (): Ctx => ({ agency: "ag-ops", agencyName: "Operations", onOverview: false, globalName: "Central", choices: [] });

beforeEach(() => {
  CAPS = { configureAppGlobal: true, manageRolesGlobal: true };
  AGENCIES = [{ id: "global", name: "Central" }, OPS];
  GRANTS = [];
  grantError = null;
  writes.length = 0;
  inUse.current = inOps();
  window.localStorage.clear();
});
afterEach(cleanup);

const showName = () =>
  render(
    <MemoryRouter>
      <NameYourAgencyCard />
    </MemoryRouter>,
  );

describe("Name your agency", () => {
  it("names the first agency and grants its administrators' group, the grant naming no agency in the request", async () => {
    showName();
    fireEvent.change(await screen.findByLabelText("Agency name"), { target: { value: "Platform" } });
    fireEvent.change(screen.getByLabelText("Administrators' AD group"), { target: { value: " platform-admins " } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(writes).toHaveLength(2));
    expect(writes[0]).toMatchObject({ method: "PUT", path: "/agencies/{agencyId}", body: { name: "Platform", description: null } });
    expect(writes[1]).toMatchObject({
      method: "POST",
      path: "/access-grants",
      body: { adGroup: "platform-admins", role: "admin", agencyId: "ag-ops" },
      headers: { [AGENCY_HEADER]: NO_AGENCY },
    });
    expect(await screen.findByText(/It is called Platform now\. platform-admins administers it\./)).toBeTruthy();
  });

  it("keeps the name and grants the group alone", async () => {
    showName();
    fireEvent.change(await screen.findByLabelText("Administrators' AD group"), { target: { value: "ops-admins" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(writes).toHaveLength(1));
    expect(writes[0]).toMatchObject({ method: "POST", path: "/access-grants" });
  });

  it("says the group was not added when the grant is refused, and keeps the rename", async () => {
    grantError = { code: "validation_failed", message: "adGroup is required" };
    showName();
    fireEvent.change(await screen.findByLabelText("Agency name"), { target: { value: "Platform" } });
    fireEvent.change(screen.getByLabelText("Administrators' AD group"), { target: { value: "x" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(await screen.findByRole("alert")).toBeTruthy();
    expect(screen.getByRole("alert").textContent).toMatch(/It is called Platform now\. The administrators' group was not added: adGroup is required/);
  });

  it.each([
    ["renamed", () => (AGENCIES = [{ ...OPS, name: "Platform" }])],
    ["named by a grant", () => (GRANTS = [{ agencyId: "ag-ops", role: "viewer" }])],
    ["made by a person", () => (AGENCIES = [{ ...OPS, createdBy: "alice@example.org" }])],
    ["shown to someone who is not a global administrator", () => (CAPS = { configureApp: true })],
    ["in Central", () => (inUse.current = { ...inOps(), agency: "global" })],
  ])("is not offered once %s", async (_what, arrange) => {
    arrange();
    showName();
    await new Promise((r) => setTimeout(r, 30));
    expect(screen.queryByText("Name your agency")).toBeNull();
  });

  it("stays dismissed for this viewer", async () => {
    showName();
    fireEvent.click(await screen.findByRole("button", { name: "Dismiss" }));
    expect(screen.queryByText("Name your agency")).toBeNull();
    cleanup();
    showName();
    await new Promise((r) => setTimeout(r, 30));
    expect(screen.queryByText("Name your agency")).toBeNull();
  });
});

describe("Create your first agency", () => {
  const assign = vi.fn();
  beforeEach(() => {
    inUse.current = { ...inOps(), agency: "global", onOverview: true };
    AGENCIES = [{ id: "global", name: "Central" }];
    assign.mockClear();
    Object.defineProperty(window, "location", { value: { ...window.location, assign }, writable: true });
  });
  const showCreate = () =>
    render(
      <MemoryRouter>
        <CreateFirstAgencyCard />
      </MemoryRouter>,
    );

  it("creates the agency with its administrators' group, then opens it", async () => {
    showCreate();
    fireEvent.click(await screen.findByRole("button", { name: "Create your first agency" }));
    fireEvent.change(screen.getByPlaceholderText("e.g. agency-alpha"), { target: { value: "Platform" } });
    fireEvent.change(screen.getByLabelText("Administrators' AD group"), { target: { value: "platform-admins" } });
    fireEvent.click(screen.getByRole("button", { name: "Save Agency" }));
    await waitFor(() => expect(assign).toHaveBeenCalledWith("/?agency=ag-new"));
    expect(writes.map((w) => `${w.method} ${w.path}`)).toEqual(["POST /agencies", "POST /access-grants"]);
    expect(writes[1]).toMatchObject({ body: { adGroup: "platform-admins", role: "admin", agencyId: "ag-new" }, headers: { [AGENCY_HEADER]: NO_AGENCY } });
  });

  it("keeps the agency and says so when the group is refused", async () => {
    grantError = { code: "validation_failed", message: "unknown group" };
    showCreate();
    fireEvent.click(await screen.findByRole("button", { name: "Create your first agency" }));
    fireEvent.change(screen.getByPlaceholderText("e.g. agency-alpha"), { target: { value: "Platform" } });
    fireEvent.change(screen.getByLabelText("Administrators' AD group"), { target: { value: "nope" } });
    fireEvent.click(screen.getByRole("button", { name: "Save Agency" }));
    expect(await screen.findByText(/Platform was created, but the administrators' group was not added: unknown group/)).toBeTruthy();
    expect(assign).not.toHaveBeenCalled();
    // Created: no second Save (it would create it again), only the way in.
    expect(screen.queryByRole("button", { name: "Save Agency" })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Open Platform" }));
    expect(assign).toHaveBeenCalledWith("/?agency=ag-new");
    expect(writes.filter((w) => w.path === "/agencies")).toHaveLength(1);
  });

  it("is not offered while an agency exists", async () => {
    AGENCIES = [{ id: "global", name: "Central" }, OPS];
    showCreate();
    await new Promise((r) => setTimeout(r, 30));
    expect(screen.queryByText("Create your first agency")).toBeNull();
  });
});

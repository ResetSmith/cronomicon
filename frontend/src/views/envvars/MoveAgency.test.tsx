// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";

// Moving a secret, variable or SSH key to another agency (v2.3.0).
//
// Each belongs to exactly one agency. The server moves one when the caller
// holds the kind's permission on the agency that has it AND on the one it goes
// to; the control offers what that rule would accept and no more, says whose
// the row is when it is not the caller's to move, and is absent when there is
// nowhere to move it to. It was missing altogether: the server could move a
// row and nothing in the UI reached the route.

let CAPS: Record<string, boolean> = {};
let ACCESS: unknown = null;
const puts: { path: string; body: unknown }[] = [];
let putError: unknown = null;

const CATALOG = [
  { id: "global", name: "Global" },
  { id: "ag-fin", name: "Finance" },
  { id: "ag-tax", name: "Tax" },
];

vi.mock("../../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/client")>();
  return {
    ...actual,
    fetchCapabilities: vi.fn(async () => ({ manageEnvVars: true, configureApp: true, ...CAPS })),
    api: {
      GET: vi.fn(async (path: string) => (path === "/agencies" ? { data: CATALOG } : { data: [] })),
      PUT: vi.fn(async (path: string, init?: { body?: unknown }) => {
        puts.push({ path, body: init?.body });
        return putError ? { error: putError } : { data: [] };
      }),
    } as unknown as typeof actual.api,
  };
});
vi.mock("../../api/access", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/access")>();
  return { ...actual, useMyAccess: () => ACCESS };
});

import { MoveAgency } from "./MoveAgency";

const grant = (id: string, name: string, permissions = ["manageEnvVars", "configureApp"]) => ({
  role: "admin", allScopes: false, agencyId: id, agencyName: name, scopes: [], permissions, groups: [], origin: "group",
});
const moved = vi.fn();
const failed = vi.fn();
const show = (over: Partial<Parameters<typeof MoveAgency>[0]> = {}) =>
  render(<MoveAgency kind="secret" id="s1" name="DB_PASS" ownerName="Finance" permission="manageEnvVars" onMoved={moved} onError={failed} {...over} />);
const select = () => screen.findByRole("combobox", { name: "Move DB_PASS to another agency" }) as Promise<HTMLSelectElement>;
const options = (el: HTMLSelectElement) => Array.from(el.options).map((o) => o.textContent);

beforeEach(() => {
  CAPS = {};
  ACCESS = { grants: [grant("ag-fin", "Finance"), grant("ag-tax", "Tax")] };
  puts.length = 0;
  putError = null;
  moved.mockClear();
  failed.mockClear();
});
afterEach(cleanup);

describe("MoveAgency", () => {
  it("offers an administrator of both agencies the other one, never Global, and moves after a confirmation", async () => {
    show();
    const el = await select();
    await waitFor(() => expect(options(el)).toEqual(["Move to another agency…", "Tax"]));
    expect(el.disabled).toBe(false);

    fireEvent.change(el, { target: { value: "ag-tax" } });
    expect(puts).toHaveLength(0);
    // The confirmation says what stops working.
    expect(await screen.findByText(/Finance's runs that bind this secret will stop resolving it/)).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Move to Tax" }));
    await waitFor(() => expect(puts).toHaveLength(1));
    // One agency, on the kind's own route.
    expect(puts[0]).toEqual({ path: "/secret-agencies", body: [{ id: "s1", agencyIds: ["ag-tax"] }] });
    await waitFor(() => expect(moved).toHaveBeenCalledWith("DB_PASS now belongs to Tax"));
  });

  it("uses each kind's own route", async () => {
    show({ kind: "ssh-credential", permission: "configureApp" });
    fireEvent.change(await select(), { target: { value: "ag-tax" } });
    fireEvent.click(await screen.findByRole("button", { name: "Move to Tax" }));
    await waitFor(() => expect(puts[0]?.path).toBe("/ssh-credential-agencies"));
    cleanup();
    puts.length = 0;
    show({ kind: "env-var" });
    fireEvent.change(await select(), { target: { value: "ag-tax" } });
    fireEvent.click(await screen.findByRole("button", { name: "Move to Tax" }));
    await waitFor(() => expect(puts[0]?.path).toBe("/env-var-agencies"));
  });

  it("offers a global administrator Global and every other agency, and says what Global means", async () => {
    CAPS = { manageEnvVarsGlobal: true };
    ACCESS = { grants: [] };
    show();
    const el = await select();
    await waitFor(() => expect(options(el)).toEqual(["Move to another agency…", "Global", "Tax"]));
    fireEvent.change(el, { target: { value: "global" } });
    expect(await screen.findByText(/It becomes Global's: every agency's runs may use it, and only global administrators change it\./)).toBeTruthy();
  });

  it("is disabled, with whose row it is, for someone who does not administer its owner", async () => {
    ACCESS = { grants: [grant("ag-tax", "Tax")] };
    show();
    const el = await select();
    expect(el.disabled).toBe(true);
    expect(el.title).toBe("This secret belongs to Finance — moving it takes authority over that agency too.");
    cleanup();
    show({ ownerName: "Global" });
    expect((await select()).title).toMatch(/This secret is Global's — only a global administrator/);
  });

  it("is absent when there is nowhere to move the row to", async () => {
    ACCESS = { grants: [grant("ag-fin", "Finance")] };
    show();
    await new Promise((r) => setTimeout(r, 20));
    expect(screen.queryByRole("combobox")).toBeNull();
  });

  it("counts only the agencies the caller holds THIS permission on", async () => {
    // configureApp on Tax is not manageEnvVars on Tax: not somewhere they may put a secret.
    ACCESS = { grants: [grant("ag-fin", "Finance"), grant("ag-tax", "Tax", ["configureApp"])] };
    show();
    await new Promise((r) => setTimeout(r, 20));
    expect(screen.queryByRole("combobox")).toBeNull();
  });

  it("reports the server's refusal and moves nothing", async () => {
    putError = { code: "vault_path_not_allowed", message: "the path is outside the Vault paths assigned to Tax" };
    show();
    fireEvent.change(await select(), { target: { value: "ag-tax" } });
    fireEvent.click(await screen.findByRole("button", { name: "Move to Tax" }));
    await waitFor(() => expect(failed).toHaveBeenCalledWith("Move failed: the path is outside the Vault paths assigned to Tax"));
    expect(moved).not.toHaveBeenCalled();
  });
});

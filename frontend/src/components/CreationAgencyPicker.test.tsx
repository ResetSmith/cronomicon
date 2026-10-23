// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";

// The creation agency picker (RF-14; one agency per row since v2.3.0, LR-7/LR-54).
//
// A new scope, secret, variable or SSH key belongs to exactly one agency. The
// properties worth fencing are about WHO IS OFFERED WHAT:
//   - an administrator of several agencies must choose, from their own and no
//     others, and Save says so until they do;
//   - an administrator of one is not asked, and is shown where the row will go;
//   - a global administrator may name any agency and defaults to Global, which
//     nobody else is ever offered;
//   - a capabilities FAILURE treats the caller as restricted rather than
//     offering Global to someone the server will refuse.

let CAPS: Record<string, boolean> = {};
let capsThrows = false;
let ACCESS: unknown = null;
let CATALOG_OVERRIDE: unknown[] | null = null;

const CAPS_OFF = {
  vault: false, apprise: false, compose: false, manageRoles: false,
  configureApp: false, manageEnvVars: false, publishSchedule: false,
  triggerJobs: false, killJobs: false, unrestricted: false,
};

const CATALOG = [
  { id: "global", name: "Global" },
  { id: "ag-tax", name: "Tax" },
  { id: "ag-fin", name: "Finance" },
  { id: "ag-hr", name: "HR" },
];

const grant = (agencyId: string, agencyName: string, permissions: string[]) => ({
  role: "admin", allScopes: false, agencyId, agencyName, scopes: [], permissions, groups: [], origin: "group",
});

vi.mock("../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/client")>();
  return {
    ...actual,
    api: {
      GET: vi.fn(async (path: string) => {
        if (path === "/agencies") return { data: CATALOG_OVERRIDE ?? CATALOG };
        return { data: [] };
      }),
    } as unknown as typeof actual.api,
    fetchCapabilities: vi.fn(async () => (capsThrows ? { ...CAPS_OFF } : { ...CAPS_OFF, ...CAPS })),
  };
});

vi.mock("../api/access", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/access")>();
  return { ...actual, fetchMyAccess: vi.fn(async () => ACCESS) };
});

import { CreationAgencyPicker, useCreationAgencies } from "./CreationAgencyPicker";

let lastBody: unknown = null;

function Host({ isEdit, permission }: { isEdit: boolean; permission?: "configureApp" | "manageEnvVars" }) {
  const pick = useCreationAgencies(isEdit, permission);
  lastBody = pick.body;
  return (
    <div>
      <button disabled={!!pick.blockedReason}>{pick.blockedReason || "Save"}</button>
      <CreationAgencyPicker label="secret" pick={pick} />
    </div>
  );
}

const select = () => screen.getByRole("combobox", { name: "Agency" }) as HTMLSelectElement;
const options = () => Array.from(select().options).map((o) => o.textContent);
const save = () => screen.getByRole("button") as HTMLButtonElement;

beforeEach(() => {
  CAPS = {};
  capsThrows = false;
  ACCESS = null;
  CATALOG_OVERRIDE = null;
  lastBody = null;
});
afterEach(cleanup);

describe("creation agency picker", () => {
  it("makes an administrator of several agencies choose, from their own agencies only", async () => {
    CAPS = { manageEnvVars: true };
    ACCESS = {
      grants: [
        grant("ag-tax", "Tax", ["manageEnvVars"]),
        grant("ag-fin", "Finance", ["manageEnvVars", "configureApp"]),
        // Held, but not for this permission: not an agency they may create a secret in.
        grant("ag-hr", "HR", ["triggerJobs"]),
      ],
    };
    render(<Host isEdit={false} permission="manageEnvVars" />);
    // The reason is ON the button, not hidden in a tooltip.
    await waitFor(() => expect(save().textContent).toBe("Choose an agency"));
    expect(save().disabled).toBe(true);
    expect(options()).toEqual(["Choose an agency…", "Finance", "Tax"]);
    expect(lastBody).toEqual({});

    fireEvent.change(select(), { target: { value: "ag-fin" } });
    await waitFor(() => expect(save().disabled).toBe(false));
    // One agency, as the list the API takes.
    expect(lastBody).toEqual({ agencyIds: ["ag-fin"] });
  });

  it("does not ask an administrator of one agency, and shows where the row will go", async () => {
    CAPS = { manageEnvVars: true };
    ACCESS = { grants: [grant("ag-tax", "Tax", ["manageEnvVars"])] };
    render(<Host isEdit={false} permission="manageEnvVars" />);
    await waitFor(() => expect(select().value).toBe("ag-tax"));
    expect(select().disabled).toBe(true);
    expect(save().disabled).toBe(false);
    expect(screen.getByText(/will belong to your agency/)).toBeTruthy();
    expect(lastBody).toEqual({ agencyIds: ["ag-tax"] });
  });

  it("offers a global administrator Global, by default, and every agency", async () => {
    CAPS = { manageEnvVars: true, manageEnvVarsGlobal: true, unrestricted: true };
    render(<Host isEdit={false} permission="manageEnvVars" />);
    await waitFor(() => expect(select().value).toBe("global"));
    expect(options()).toEqual(["Global", "Tax", "Finance", "HR"]);
    expect(save().disabled).toBe(false);
    expect(lastBody).toEqual({ agencyIds: ["global"] });
    fireEvent.change(select(), { target: { value: "ag-hr" } });
    await waitFor(() => expect(lastBody).toEqual({ agencyIds: ["ag-hr"] }));
  });

  it("never offers Global to anyone else", async () => {
    // Reaches every scope as a viewer and administers one agency: not a global
    // administrator for this permission (GC-6), so no Global, and no catalog.
    CAPS = { unrestricted: true, configureApp: true, configureAppGlobal: false };
    ACCESS = { grants: [grant("ag-fin", "Finance", ["configureApp"])] };
    render(<Host isEdit={false} permission="configureApp" />);
    await waitFor(() => expect(select().value).toBe("ag-fin"));
    expect(options()).toEqual(["Finance"]);
  });

  it("never appears on an edit", async () => {
    CAPS = { manageEnvVars: true };
    ACCESS = { grants: [grant("ag-tax", "Tax", ["manageEnvVars"]), grant("ag-fin", "Finance", ["manageEnvVars"])] };
    render(<Host isEdit={true} permission="manageEnvVars" />);
    await new Promise((r) => setTimeout(r, 20));
    // An agency is changed by moving the row, on the Agencies tab; re-asking
    // here would imply this form could change it.
    expect(screen.queryByRole("combobox", { name: "Agency" })).toBeNull();
    expect(save().disabled).toBe(false);
    expect(lastBody).toEqual({});
  });

  it("fails CLOSED when capabilities cannot be read: restricted, and no Global", async () => {
    capsThrows = true;
    render(<Host isEdit={false} permission="manageEnvVars" />);
    // With nothing known about the caller they must choose, and what they are
    // offered is the catalog without Global; the server refuses a wrong pick.
    await waitFor(() => expect(save().textContent).toBe("Choose an agency"));
    expect(save().disabled).toBe(true);
    expect(options()).toEqual(["Choose an agency…", "Tax", "Finance", "HR"]);
  });

  it("does not block the form when nothing could be read to choose from", async () => {
    // No grants readable and an empty catalog: with nothing to offer, a blocked
    // Save would be a dead end. The server places the row in the caller's one
    // agency, or answers which to name.
    CAPS = { manageEnvVars: true };
    ACCESS = null;
    CATALOG_OVERRIDE = [];
    render(<Host isEdit={false} permission="manageEnvVars" />);
    await waitFor(() => expect(screen.getByRole("combobox", { name: "Agency" })).toBeTruthy());
    await new Promise((r) => setTimeout(r, 20));
    expect(save().disabled).toBe(false);
    expect(lastBody).toEqual({});
  });

  it("keeps a caller that names no permission on `unrestricted`", async () => {
    CAPS = { unrestricted: true };
    render(<Host isEdit={false} />);
    await waitFor(() => expect(select().value).toBe("global"));
    expect(save().disabled).toBe(false);
  });
});

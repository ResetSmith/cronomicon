// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";

// SSH Targets, by owner (GC in v2.2.2; LR-69 in v2.3.0).
//
// A bastion and a hand-written host record belong to ONE agency. In 2.2.2 both
// were a global administrator's alone, because neither had an owner. Now:
//   · "+ Add" asks whose the record is, offering the agencies the caller
//     administers — Global too, for a global administrator and nobody else;
//   · a record another agency owns keeps its actions, disabled, with whose it
//     is (FX-7);
//   · a host IMPORTED for a scope follows that scope's gate, which the server
//     judges: its actions stay live, and a refusal shows the server's sentence.

const FIN = { id: "ag-fin", name: "Finance" };
const TAX = { id: "ag-tax", name: "Tax" };
const HOSTS = [
  // Imported for a scope: the scope's gate, judged by the server.
  { id: "h1", hostname: "web-01.internal", address: "10.0.0.1", port: 22, user: "deploy", source: "cronomicon", status: "verified", scopeId: "sc1", ownerAgency: "global", ownerAgencyName: "Global" },
  // Hand-written, by owner.
  { id: "h2", hostname: "fin-db.internal", address: "10.0.0.2", port: 22, user: "deploy", source: "cronomicon", status: "verified", scopeId: null, ownerAgency: "ag-fin", ownerAgencyName: "Finance" },
  { id: "h3", hostname: "tax-db.internal", address: "10.0.0.3", port: 22, user: "deploy", source: "cronomicon", status: "verified", scopeId: null, ownerAgency: "ag-tax", ownerAgencyName: "Tax" },
];
const BASTIONS = [
  { id: "b1", name: "jump-dmz", address: "10.9.0.1", port: 22, user: "jump", status: "verified", hostKeyPinned: true, ownerAgency: "global", ownerAgencyName: "Global" },
  { id: "b2", name: "jump-fin", address: "10.9.0.2", port: 22, user: "jump", status: "verified", hostKeyPinned: true, ownerAgency: "ag-fin", ownerAgencyName: "Finance" },
];
const CREDS = [
  { id: "k-global", label: "shared_key", ownerAgency: "Global" },
  { id: "k-fin", label: "fin_key", ownerAgency: "Finance" },
  { id: "k-tax", label: "tax_key", ownerAgency: "Tax" },
];
let ACCESS: unknown = null;

const writes: { method: string; path: string; body?: unknown }[] = [];
let putHostError: unknown = null;

vi.mock("../../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/client")>();
  const record = (method: string) =>
    vi.fn(async (path: string, init?: { body?: unknown }) => {
      writes.push({ method, path, body: init?.body });
      if (method === "PUT" && path === "/ssh/hosts/{hostId}" && putHostError) return { error: putHostError };
      return { data: {} };
    });
  return {
    ...actual,
    api: {
      GET: vi.fn(async (path: string) => {
        if (path === "/ssh/hosts") return { data: HOSTS };
        if (path === "/ssh/bastions") return { data: BASTIONS };
        if (path === "/ssh/credentials") return { data: CREDS };
        if (path === "/agencies") return { data: [{ id: "global", name: "Global" }, FIN, TAX] };
        return { data: [] };
      }),
      POST: record("POST"),
      PUT: record("PUT"),
      DELETE: record("DELETE"),
    } as unknown as typeof actual.api,
  };
});

vi.mock("../../api/access", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/access")>();
  return { ...actual, useMyAccess: () => ACCESS };
});

import { SshTargetsSection } from "./SshTargets";

const grant = (a: { id: string; name: string }) => ({
  role: "admin", allScopes: false, agencyId: a.id, agencyName: a.name, scopes: [], permissions: ["configureApp"], groups: [], origin: "group",
});

beforeEach(() => {
  ACCESS = null;
  writes.length = 0;
  putHostError = null;
  localStorage.clear();
});
afterEach(cleanup);

const open = async (canWrite: boolean) => {
  render(<SshTargetsSection canWrite={canWrite} />);
  await waitFor(() => expect(screen.getByText("web-01.internal")).toBeTruthy());
  await waitFor(() => expect(screen.getByText("jump-dmz")).toBeTruthy());
};
const rowOf = (text: string) => within(screen.getByText(text).closest("tr") as HTMLElement);
const button = (q: { getByRole: typeof screen.getByRole }, name: string) => q.getByRole("button", { name }) as HTMLButtonElement;

describe("SSH Targets — an administrator of one agency (LR-69)", () => {
  beforeEach(() => {
    ACCESS = { grants: [grant(FIN)] };
  });

  it("lets them add a host record and a bastion, for their own agency and no other", async () => {
    await open(false);
    for (const name of ["+ Add Host", "+ Add Bastion"]) expect(button(screen, name).disabled, name).toBe(false);

    fireEvent.click(button(screen, "+ Add Bastion"));
    const owner = (await waitFor(() => screen.getByLabelText("Agency that owns this bastion"))) as HTMLSelectElement;
    // One agency is not a choice, and Global is never offered to them.
    expect(Array.from(owner.options).map((o) => o.textContent)).toEqual(["Finance"]);
    expect(owner.disabled).toBe(true);
    fireEvent.change(screen.getByPlaceholderText("Name (e.g. bastion-prod)"), { target: { value: "jump-new" } });
    fireEvent.change(screen.getByPlaceholderText("Address"), { target: { value: "10.9.0.9" } });
    fireEvent.click(button(screen, "Add"));
    await waitFor(() => expect(writes.some((w) => w.method === "POST" && w.path === "/ssh/bastions")).toBe(true));
    expect(writes.find((w) => w.path === "/ssh/bastions")!.body).toMatchObject({ name: "jump-new", ownerAgency: "ag-fin" });
  });

  it("offers a new record only its owner's keys and Global's", async () => {
    await open(false);
    fireEvent.click(button(screen, "+ Add Host"));
    await waitFor(() => screen.getByLabelText("Agency that owns this host record"));
    const keys = screen.getByTitle(/SSH key: a credential/) as HTMLSelectElement;
    const offered = Array.from(keys.options).map((o) => o.textContent);
    expect(offered).toContain("shared_key");
    expect(offered).toContain("fin_key");
    expect(offered).not.toContain("tax_key");
  });

  it("leaves their own records' actions live, and disables another owner's with whose it is", async () => {
    await open(false);
    for (const name of ["Test", "Edit", "Remove"]) expect(button(rowOf("jump-fin"), name).disabled, name).toBe(false);
    for (const name of ["Test", "Edit", "Remove"]) expect(button(rowOf("fin-db.internal"), name).disabled, name).toBe(false);

    for (const name of ["Test", "Edit", "Remove"]) {
      const b = button(rowOf("jump-dmz"), name);
      expect(b.disabled, name).toBe(true);
      expect(b.title, name).toBe("This bastion is Global's — only a global administrator (a role on every agency) can change it.");
    }
    for (const name of ["Test", "Edit", "Remove"]) {
      const b = button(rowOf("tax-db.internal"), name);
      expect(b.disabled, name).toBe(true);
      expect(b.title, name).toBe("This host record belongs to Tax — only that agency's administrators can change it.");
    }
    fireEvent.click(button(rowOf("jump-dmz"), "Test"));
    expect(writes.length).toBe(0);
  });

  it("shows who owns each record", async () => {
    await open(false);
    expect(within(screen.getByText("fin-db.internal").closest("tr") as HTMLElement).getByText("Finance")).toBeTruthy();
    expect(within(screen.getByText("jump-dmz").closest("tr") as HTMLElement).getByText("Global")).toBeTruthy();
    // An imported host belongs to its scope, and says so instead of naming an owner.
    expect(within(screen.getByText("web-01.internal").closest("tr") as HTMLElement).getByText("its scope's")).toBeTruthy();
  });

  it("leaves an imported host's actions live, and shows the server's refusal when one is refused", async () => {
    putHostError = { code: "forbidden", message: "you do not have configureApp on the agency this scope belongs to" };
    await open(false);
    const row = rowOf("web-01.internal");
    for (const name of ["Test", "Edit", "Remove"]) expect(button(row, name).disabled, name).toBe(false);

    fireEvent.click(button(row, "Edit"));
    fireEvent.click(await waitFor(() => button(rowOf("web-01.internal"), "Save")));
    await waitFor(() => expect(screen.getByText(/you do not have configureApp on the agency this scope belongs to/)).toBeTruthy());
  });

  it("disables + Add for someone who administers no agency, with the reason", async () => {
    ACCESS = { grants: [] };
    await open(false);
    for (const name of ["+ Add Host", "+ Add Bastion"]) {
      const b = button(screen, name);
      expect(b.disabled, name).toBe(true);
      expect(b.title, name).toBe("You do not administer an agency, so there is none to create this for.");
    }
  });
});

// While GET /me/access is loading, or when it cannot be read, the caller's
// agencies are unknown. Nothing may then claim a record is somebody else's or
// that there is no agency to create one for: the controls stay live and the
// server decides, as on the Runners page.
describe("SSH Targets — the caller's agencies are unknown", () => {
  it("leaves + Add and every record's actions live, and sends no owner for the server to refuse", async () => {
    ACCESS = null;
    await open(false);
    for (const name of ["+ Add Host", "+ Add Bastion"]) {
      const b = button(screen, name);
      expect(b.disabled, name).toBe(false);
      expect(b.title, name).toBe("");
    }
    for (const r of ["jump-dmz", "jump-fin"]) {
      for (const name of ["Test", "Edit", "Remove"]) expect(button(rowOf(r), name).disabled, `${r} ${name}`).toBe(false);
    }
    for (const name of ["Test", "Edit", "Remove"]) expect(button(rowOf("tax-db.internal"), name).disabled, name).toBe(false);

    fireEvent.click(button(screen, "+ Add Bastion"));
    fireEvent.change(await waitFor(() => screen.getByPlaceholderText("Name (e.g. bastion-prod)")), { target: { value: "jump-x" } });
    fireEvent.change(screen.getByPlaceholderText("Address"), { target: { value: "10.9.0.8" } });
    fireEvent.click(button(screen, "Add"));
    await waitFor(() => expect(writes.some((w) => w.method === "POST" && w.path === "/ssh/bastions")).toBe(true));
    // No owner named: the server places it in the caller's one agency, or says which to name.
    expect("ownerAgency" in (writes.find((w) => w.path === "/ssh/bastions")!.body as object)).toBe(false);
  });
});

describe("SSH Targets — a global administrator", () => {
  it("has every control enabled, and may create a record for Global or any agency", async () => {
    await open(true);
    for (const name of ["+ Add Host", "+ Add Bastion"]) {
      const b = button(screen, name);
      expect(b.disabled, name).toBe(false);
      expect(b.title, name).toBe("");
    }
    for (const r of ["jump-dmz", "jump-fin"]) {
      for (const name of ["Test", "Edit", "Remove"]) expect(button(rowOf(r), name).disabled, `${r} ${name}`).toBe(false);
    }
    for (const name of ["Test", "Edit", "Remove"]) expect(button(rowOf("tax-db.internal"), name).disabled, name).toBe(false);

    fireEvent.click(button(screen, "+ Add Host"));
    const owner = (await waitFor(() => screen.getByLabelText("Agency that owns this host record"))) as HTMLSelectElement;
    expect(Array.from(owner.options).map((o) => o.textContent)).toEqual(["Global", "Finance", "Tax"]);
    expect(owner.value).toBe("global");
    fireEvent.change(owner, { target: { value: "ag-tax" } });
    fireEvent.change(screen.getByPlaceholderText("Hostname"), { target: { value: "new-host" } });
    fireEvent.click(button(screen, "Add"));
    await waitFor(() => expect(writes.some((w) => w.method === "POST" && w.path === "/ssh/hosts")).toBe(true));
    expect(writes.find((w) => w.path === "/ssh/hosts")!.body).toMatchObject({ hostname: "new-host", ownerAgency: "ag-tax" });
  });

  // The host key on a record is the one the LOCAL RUNNER trusts for its address
  // (2.3.0). The server connects only to a host that has one, and captures
  // nothing on first connect: a record without one says so, and where the key
  // is approved. There is no "clear" here any more — a key is replaced or
  // removed where it is approved.
  it("shows whether the local runner has an approved key, and offers nothing that would capture or clear one", async () => {
    await open(true);
    // The fixture's hosts have no approved key; its bastions do.
    const badge = rowOf("web-01.internal").getByText("⚠ no approved key");
    expect(badge.getAttribute("title")).toMatch(/the server will not connect to it/);
    expect(badge.getAttribute("title")).toMatch(/Runners → Local runner → Host keys/);
    expect(badge.getAttribute("title")).toMatch(/Nothing is captured on first connect/);
    expect(rowOf("jump-dmz").getByText("approved")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Clear pin" })).toBeNull();
  });
});

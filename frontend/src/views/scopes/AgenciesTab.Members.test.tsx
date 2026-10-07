// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

// RB-22 — the per-agency member editor that replaced the Membership matrix.
//
// The matrix had a test; deleting it without replacing the coverage would mean the
// surface that took over its job ships untested. What matters here is what the
// matrix could not do: keep the row's identity on screen while you edit it, and
// write through the agency axis so a save names ONE agency.

const AGENCIES = [{ id: "ag-tax", name: "Tax", description: "Tax dept" }];

let DETAIL: Record<string, unknown> = {};
const MATRIX = {
  rows: [
    { kind: "secret", id: "s1", name: "DB_PASS", scope: "prod", agencyIds: ["ag-tax"] },
    // Global's: the only kind of row this list can ADD (one agency per row).
    { kind: "secret", id: "s2", name: "API_KEY", scope: "", agencyIds: ["global"] },
    // Another agency's: moved on the row itself, never offered here.
    { kind: "secret", id: "s3", name: "FIN_KEY", scope: "", agencyIds: ["ag-fin"] },
    { kind: "runner", id: "r1", name: "runner-one", scope: "", agencyIds: ["ag-tax"] },
    { kind: "runner", id: "r2", name: "runner-global", scope: "", agencyIds: ["global"] },
  ],
};
let PREFIXES: string[] = [];
let prefixesForbidden = false;

const puts: Array<{ path: string; body: unknown }> = [];

vi.mock("../../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/client")>();
  return {
    ...actual,
    api: {
      GET: vi.fn(async (path: string) => {
        if (path === "/agencies") return { data: AGENCIES };
        if (path === "/agency-matrix") return { data: MATRIX };
        if (path === "/agencies/{agencyId}") return { data: DETAIL };
        if (path === "/agencies/{agencyId}/vault-prefixes") {
          return prefixesForbidden ? { error: { code: "forbidden", message: "no" } } : { data: { agencyId: "ag-tax", prefixes: PREFIXES } };
        }
        return { data: [] };
      }),
      PUT: vi.fn(async (path: string, opts?: { body?: unknown }) => {
        puts.push({ path, body: opts?.body });
        if (path === "/agencies/{agencyId}/vault-prefixes") {
          PREFIXES = (opts?.body as { prefixes: string[] }).prefixes;
          return { data: { agencyId: "ag-tax", prefixes: PREFIXES } };
        }
        // The endpoint answers with the resulting AgencyDetail; mirror that so the
        // panel re-renders from the response rather than refetching.
        const members = (opts?.body as { members: { kind: string; id: string }[] }).members.map((m) => {
          const row = MATRIX.rows.find((r) => r.kind === m.kind && r.id === m.id)!;
          return { kind: m.kind, id: m.id, name: row.name, scope: row.scope, status: "" };
        });
        DETAIL = { members, onlineRunners: 0, queuedRuns: 0 };
        return { data: DETAIL };
      }),
      POST: vi.fn(async () => ({ data: {} })),
      DELETE: vi.fn(async () => ({ data: {} })),
    } as unknown as typeof actual.api,
  };
});

import { AgenciesTab } from "./AgenciesTab";

beforeEach(() => {
  puts.length = 0;
  PREFIXES = [];
  prefixesForbidden = false;
  DETAIL = {
    members: [{ kind: "secret", id: "s1", name: "DB_PASS", scope: "prod", status: "" }],
    onlineRunners: 0,
    queuedRuns: 0,
  };
});
afterEach(cleanup);

// `globalAdmin` defaults to canEdit: the pre-GC tests mean "an administrator" as
// one fact. The GC block below passes the two apart.
const openAgency = async (canEdit = true, globalAdmin = canEdit, envGlobal = globalAdmin) => {
  render(
    <MemoryRouter>
      <AgenciesTab canEdit={canEdit} globalAdmin={globalAdmin} envGlobal={envGlobal} />
    </MemoryRouter>,
  );
  await waitFor(() => expect(screen.getByText("Tax")).toBeTruthy());
  fireEvent.click(screen.getByText("Tax").closest("tr")!);
  await waitFor(() => expect(screen.getByText("DB_PASS")).toBeTruthy());
};

describe("per-agency member editor (RB-22)", () => {
  it("lists the agency's members grouped by kind", async () => {
    await openAgency();
    // The member and its scope both render — the matrix showed the name only, and
    // an operator with DB_PASS in two scopes could not tell which row they held.
    expect(screen.getByText("DB_PASS")).toBeTruthy();
    expect(screen.getByText("@prod")).toBeTruthy();
  });

  it("adds a member through the agency-scoped setter", async () => {
    await openAgency();
    // The picker offers what is Global's: not what this agency already has, and
    // not another agency's (that is a move, made on the row itself).
    const add = screen.getByLabelText(/Add a secret to this agency/i) as HTMLSelectElement;
    expect(within(add).queryByText("DB_PASS @prod")).toBeNull();
    expect(within(add).queryByText("FIN_KEY")).toBeNull();
    expect(within(add).getByText("API_KEY")).toBeTruthy();
    fireEvent.change(add, { target: { value: "s2" } });

    await waitFor(() => expect(puts.length).toBe(1));
    // ONE agency named in the path; the body is that agency's complete member list.
    // This is the write isolation the endpoint exists for — an entity-centric save
    // would have sent the SECRET's full agency list instead, and clobbered a
    // concurrent edit to another department.
    expect(puts[0].path).toBe("/agencies/{agencyId}/members");
    const sent = (puts[0].body as { members: { kind: string; id: string }[] }).members;
    expect(sent).toEqual([
      { kind: "secret", id: "s1" },
      { kind: "secret", id: "s2" },
    ]);
  });

  // A row in ONE agency cannot be removed from it: it would be left in none,
  // and one the agency owns is moved, not removed. So there is no control to
  // press. A row still in SEVERAL agencies (from before 2.3.0) can leave one.
  it("offers Remove only on a row still in several agencies, and removes it without touching the others", async () => {
    const SHARED = { kind: "secret", id: "s9", name: "OLD_SHARED", scope: "", agencyIds: ["ag-tax", "ag-fin"] };
    MATRIX.rows.push(SHARED);
    DETAIL = {
      members: [
        { kind: "secret", id: "s1", name: "DB_PASS", scope: "prod", status: "" },
        { kind: "secret", id: "s9", name: "OLD_SHARED", scope: "", status: "" },
        { kind: "runner", id: "r1", name: "runner-one", scope: "", status: "online" },
      ],
      onlineRunners: 1,
      queuedRuns: 0,
    };
    try {
      await openAgency();
      expect(screen.queryByLabelText(/Remove DB_PASS from this agency/i)).toBeNull();
      fireEvent.click(screen.getByLabelText(/Remove OLD_SHARED from this agency/i));

      await waitFor(() => expect(puts.length).toBe(1));
      const sent = (puts[0].body as { members: { kind: string; id: string }[] }).members;
      expect(sent).toEqual([
        { kind: "secret", id: "s1" },
        { kind: "runner", id: "r1" },
      ]);
    } finally {
      MATRIX.rows.splice(MATRIX.rows.indexOf(SHARED), 1);
    }
  });

  it("is read-only without ConfigureApp", async () => {
    await openAgency(false);
    // No remove affordance and no picker — a control that always 403s is worse
    // than no control.
    expect(screen.queryByLabelText(/Remove /i)).toBeNull();
    expect(screen.queryByLabelText(/Add a secret/i)).toBeNull();
  });

  it("says what an agency with no online runner means", async () => {
    await openAgency();
    // The trap the whole agencies plan came from: no online runner means every run
    // targeting this agency queues forever, and nothing alerts.
    expect(screen.getByText(/No online runner serves this agency/i)).toBeTruthy();
    // And what to do about it: enrol an agent FOR this agency (MA-27).
    expect((screen.getByRole("link", { name: "Enrol a runner for this agency" }) as HTMLAnchorElement).getAttribute("href")).toBe("/runners");
  });

  it("lists the runners that serve the agency and offers no way to add or remove one", async () => {
    DETAIL = {
      members: [
        { kind: "secret", id: "s1", name: "DB_PASS", scope: "prod", status: "" },
        { kind: "runner", id: "r1", name: "runner-one", scope: "", status: "online" },
      ],
      onlineRunners: 1,
      queuedRuns: 0,
    };
    await openAgency();
    expect(screen.getByText("runner-one")).toBeTruthy();
    // An agent serves the agency whose token it enrolled with (MA-11): this
    // list cannot place one, and taking one away is done on the runner.
    expect(screen.queryByLabelText(/Remove runner-one from this agency/i)).toBeNull();
    expect(screen.queryByLabelText(/Add a runner to this agency/i)).toBeNull();
    expect(screen.getByRole("link", { name: "Enrol or remove one on Runners" })).toBeTruthy();
  });
});

// LR-80 — the Vault paths assigned to an agency. A global administrator edits
// the list; the agency's own administrators read it; anyone the read is refused
// to sees nothing.
describe("agency Vault paths (LR-80)", () => {
  it("shows a global administrator the list and saves a replacement, one prefix per line", async () => {
    PREFIXES = ["secret/data/tax"];
    await openAgency(true, true);
    expect(await screen.findByText("Vault paths (1)")).toBeTruthy();
    expect(screen.getByText("secret/data/tax")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Edit Vault paths" }));
    const box = screen.getByLabelText("Vault path prefixes, one per line") as HTMLTextAreaElement;
    expect(box.value).toBe("secret/data/tax");
    fireEvent.change(box, { target: { value: "secret/data/tax\n\n  kv/tax  \n" } });
    fireEvent.click(screen.getByRole("button", { name: "Save Vault paths" }));
    await waitFor(() => expect(puts.some((p) => p.path === "/agencies/{agencyId}/vault-prefixes")).toBe(true));
    // Blank lines and surrounding spaces are not prefixes; the field is always sent.
    expect(puts.find((p) => p.path === "/agencies/{agencyId}/vault-prefixes")!.body).toEqual({ prefixes: ["secret/data/tax", "kv/tax"] });
    expect(await screen.findByText("Vault paths (2)")).toBeTruthy();
  });

  it("says what no prefixes means", async () => {
    await openAgency(true, true);
    expect(await screen.findByText(/None assigned\. This agency cannot create a Vault-backed secret or key/)).toBeTruthy();
  });

  it("shows the agency's own administrator the list, with Edit disabled and the reason", async () => {
    PREFIXES = ["secret/data/tax"];
    await openAgency(true, false);
    expect(await screen.findByText("secret/data/tax")).toBeTruthy();
    const edit = screen.getByRole("button", { name: "Edit Vault paths" }) as HTMLButtonElement;
    expect(edit.disabled).toBe(true);
    expect(edit.title).toBe("Only a global administrator (a role on every agency) can assign an agency its Vault paths.");
  });

  it("shows nothing to someone the read is refused to", async () => {
    prefixesForbidden = true;
    await openAgency(true, false);
    await new Promise((r) => setTimeout(r, 20));
    expect(screen.queryByText(/Vault paths/)).toBeNull();
  });
});

// GC (v2.2.2, gate closing) — the agency CATALOG is install-wide: creating,
// renaming or deleting an agency needs a global administrator (one grant
// covering every agency AND carrying configureApp). An administrator of one
// agency holds configureApp, so by FX-7 these are preconditions, not
// irrelevance: the controls stay and are disabled with the reason.
describe("agency catalog — global-administrator controls (GC)", () => {
  const WHY = "Only a global administrator (a role on every agency) can change this.";
  // Both scopes are in the matrix (the PUT mock resolves every member through
  // it): sc0 is already a member of Tax, sc1 is the candidate.
  const MEMBER_ROW = { kind: "scope", id: "sc0", name: "prod-web", scope: "", agencyIds: ["ag-tax"] };
  const SCOPE_ROW = { kind: "scope", id: "sc1", name: "edge-lab", scope: "", agencyIds: ["global"] as string[] };
  const withScopes = () => {
    DETAIL = {
      members: [
        { kind: "secret", id: "s1", name: "DB_PASS", scope: "prod", status: "" },
        { kind: "scope", id: "sc0", name: "prod-web", scope: "", status: "" },
      ],
      onlineRunners: 0,
      queuedRuns: 0,
    };
    MATRIX.rows.push(MEMBER_ROW, SCOPE_ROW);
  };
  afterEach(() => {
    for (const row of [MEMBER_ROW, SCOPE_ROW]) {
      const i = MATRIX.rows.indexOf(row);
      if (i >= 0) MATRIX.rows.splice(i, 1);
    }
  });

  it("disables Add, Edit and Delete Agency for an administrator of one agency, with the reason", async () => {
    await openAgency(true, false);
    for (const name of ["+ Add Agency", "Edit", "Delete"]) {
      const b = screen.getByRole("button", { name }) as HTMLButtonElement;
      expect(b.disabled, name).toBe(true);
      expect(b.title, name).toBe(WHY);
    }
  });

  // ADDING here takes one of Global's rows out of Global, and Global is a
  // global administrator's on either side of a move: for anyone else the server
  // always refuses the add. So it is disabled with the reason, per kind — a
  // scope or key by the configureApp flag, a secret or variable by the
  // manageEnvVars one.
  it("disables every add for an administrator of one agency, with the reason", async () => {
    withScopes();
    await openAgency(true, false, false);
    for (const label of [/Add a scope to this agency/i, /Add a secret to this agency/i]) {
      const add = screen.getByLabelText(label) as HTMLSelectElement;
      expect(add.disabled).toBe(true);
      expect(add.title).toMatch(/takes it out of Global — only a global administrator/);
    }
    // Nothing on a single-agency row to press, either.
    expect(screen.queryByLabelText(/Remove prod-web from this agency/i)).toBeNull();
    expect(screen.queryByLabelText(/Remove DB_PASS from this agency/i)).toBeNull();
    expect(puts.length).toBe(0);
  });

  it("judges each kind by its own permission's global flag", async () => {
    withScopes();
    // A global administrator for manageEnvVars who is not one for configureApp.
    await openAgency(true, false, true);
    expect((screen.getByLabelText(/Add a scope to this agency/i) as HTMLSelectElement).disabled).toBe(true);
    expect((screen.getByLabelText(/Add a secret to this agency/i) as HTMLSelectElement).disabled).toBe(false);
  });

  it("leaves all of it enabled for a global administrator", async () => {
    withScopes();
    await openAgency(true, true);
    for (const name of ["+ Add Agency", "Edit", "Delete"]) {
      const b = screen.getByRole("button", { name }) as HTMLButtonElement;
      expect(b.disabled, name).toBe(false);
      expect(b.title, name).toBe("");
    }
    const addScope = screen.getByLabelText(/Add a scope to this agency/i) as HTMLSelectElement;
    expect(addScope.disabled).toBe(false);
    expect(addScope.title).toBe("");

    fireEvent.change(addScope, { target: { value: "sc1" } });
    await waitFor(() => expect(puts.length).toBe(1));
    const sent = (puts[0].body as { members: { kind: string; id: string }[] }).members;
    expect(sent).toContainEqual({ kind: "scope", id: "sc1" });
  });
});

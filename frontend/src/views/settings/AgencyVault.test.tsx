// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

// 2.5.1 SG3 — Settings → Secret store (Agency Vault until 2.5.5). An agency connects a Vault of its own;
// a save whose references would not all resolve asks first (SG-25), and the
// credentials are never shown back.

let CONNECTED: Record<string, unknown> | null = null;
let EXTERNAL_ROWS = 0;
let PUT_ANSWERS: { error?: unknown; response: { status: number } }[] = [];
const writes: { method: string; path: string; body?: Record<string, unknown> }[] = [];

vi.mock("../../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/client")>();
  type Opts = { body?: Record<string, unknown> };
  return {
    ...actual,
    api: {
      GET: vi.fn(async (path: string) => {
        if (path === "/agencies/{agencyId}/vault") return { data: { agencyId: "ag-tax", connected: !!CONNECTED, vault: CONNECTED, externalRows: EXTERNAL_ROWS } };
        if (path === "/agencies/{agencyId}/vault-prefixes") return { data: { agencyId: "ag-tax", prefixes: [], ownVault: !!CONNECTED } };
        return { data: {} };
      }),
      POST: vi.fn(async (path: string, opts: Opts) => {
        writes.push({ method: "POST", path, body: opts.body });
        if (path.endsWith("/preview"))
          return { data: { references: [{ kind: "secret", id: "s1", name: "DB", path: "kv/tax/db#pw", resolves: false, problem: "status 404" }] } };
        return { data: { ok: true } };
      }),
      PUT: vi.fn(async (path: string, opts: Opts) => {
        writes.push({ method: "PUT", path, body: opts.body });
        return PUT_ANSWERS.shift() ?? { data: {}, response: { status: 200 } };
      }),
      DELETE: vi.fn(async (path: string) => {
        writes.push({ method: "DELETE", path });
        return { data: {} };
      }),
    } as unknown as typeof actual.api,
  };
});

let AG: { agency: string; agencyName: string } | null = { agency: "ag-tax", agencyName: "Tax" };
vi.mock("../../agency", () => ({ useAgencyOptional: () => AG }));
type Access = import("../../api/access").MyAccess;
let ACCESS: Access | null | undefined = undefined;
vi.mock("../../api/access", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/access")>();
  return { ...actual, useMyAccess: () => ACCESS };
});
const adminOf = (id: string, name: string) =>
  ({
    grants: [{ role: "admin", allScopes: false, agencyId: id, agencyName: name, scopes: [], permissions: ["configureApp"], groups: [], origin: "group" }],
  }) as unknown as Access;

import { AgencyVaultSection } from "./AgencyVault";

beforeEach(() => {
  CONNECTED = null;
  EXTERNAL_ROWS = 0;
  PUT_ANSWERS = [];
  writes.length = 0;
  AG = { agency: "ag-tax", agencyName: "Tax" };
  ACCESS = adminOf("ag-tax", "Tax");
});
afterEach(cleanup);

const type = (label: string, value: string) => fireEvent.change(screen.getByLabelText(label), { target: { value } });

describe("Settings → Agency Vault (2.5.1 SG3)", () => {
  it("connects, asking first when a reference would not resolve", async () => {
    PUT_ANSWERS = [
      {
        error: { code: "references_unresolved", references: [{ kind: "secret", id: "s1", name: "DB", path: "kv/tax/db#pw", resolves: false }] },
        response: { status: 409 },
      },
      { response: { status: 200 } },
    ];
    render(<AgencyVaultSection isGlobal={false} />);
    await waitFor(() => expect(screen.getByText("Connect a secret store of its own")).toBeTruthy());
    expect(screen.getByText(/uses the installation's Vault/)).toBeTruthy();
    const save = screen.getByRole("button", { name: "Save" }) as HTMLButtonElement;
    expect(save.disabled).toBe(true);

    type("Vault address", "https://vault.tax.internal:8200");
    type("Role id", "r");
    type("Secret id", "s");
    expect(save.disabled).toBe(false);

    fireEvent.click(screen.getByRole("button", { name: "Preview references" }));
    await waitFor(() => expect(screen.getByText("No: status 404")).toBeTruthy());

    fireEvent.click(save);
    await waitFor(() => expect(screen.getByText("Some references would not resolve")).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "Save anyway" }));
    await waitFor(() => expect(writes.filter((w) => w.method === "PUT")).toHaveLength(2));
    const [first, second] = writes.filter((w) => w.method === "PUT");
    expect(first.body).toEqual({ provider: "hashicorp", addr: "https://vault.tax.internal:8200", namespace: "", roleId: "r", secretId: "s" });
    expect(second.body).toMatchObject({ confirmUnresolved: true });
  });

  it("keeps the stored credentials, and offers its own prefixes and a disconnect, once connected", async () => {
    CONNECTED = {
      agencyId: "ag-tax", addr: "https://vault.tax.internal:8200", namespace: "", caPresent: true,
      roleIdSet: true, secretIdSet: true, lastModifiedBy: "a@x", lastModifiedAt: "2026-10-21T10:00:00Z",
    };
    render(<AgencyVaultSection isGlobal={false} />);
    await waitFor(() => expect(screen.getByText("Its own secret store")).toBeTruthy());
    expect((screen.getByLabelText("Role id") as HTMLInputElement).value).toBe("");
    expect((screen.getByLabelText("Role id") as HTMLInputElement).placeholder).toMatch(/set/);
    expect(screen.getByText("Path prefixes")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Disconnect…" })).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(writes.some((w) => w.method === "PUT")).toBe(true));
    // Blank credentials and an untouched CA bundle: the stored ones are kept.
    expect(writes.find((w) => w.method === "PUT")!.body).toEqual({
      provider: "hashicorp", addr: "https://vault.tax.internal:8200", namespace: "", roleId: "", secretId: "",
    });
  });

  // 2.5.5 SM5: AWS Secrets Manager — its own fields, an example reference to
  // test with, and nothing of the server's own AWS identity.
  it("connects AWS Secrets Manager with its own fields", async () => {
    render(<AgencyVaultSection isGlobal={false} />);
    await waitFor(() => expect(screen.getByText("Connect a secret store of its own")).toBeTruthy());
    fireEvent.change(screen.getByLabelText("Kind of store"), { target: { value: "aws" } });
    expect(screen.queryByLabelText("Vault address")).toBeNull();
    expect(screen.getByText(/never the server's own AWS identity/)).toBeTruthy();
    const save = screen.getByRole("button", { name: "Save" }) as HTMLButtonElement;
    expect(save.disabled).toBe(true);
    expect(screen.queryByLabelText("CA bundle")).toBeNull(); // only with an endpoint of one's own
    type("Region", "eu-west-1");
    type("Access key id", "AKID");
    type("Secret access key", "shh");
    type("Example reference", "cronomicon/db#password");
    expect(save.disabled).toBe(false);
    fireEvent.click(screen.getByRole("button", { name: "Test" }));
    await waitFor(() => expect(screen.getByText("AWS accepted the credential.")).toBeTruthy());
    fireEvent.click(save);
    await waitFor(() => expect(writes.some((w) => w.method === "PUT")).toBe(true));
    expect(writes.find((w) => w.method === "PUT")!.body).toEqual({
      provider: "aws",
      settings: { region: "eu-west-1", endpoint: "" },
      credential: { accessKeyId: "AKID", secretAccessKey: "shh", sessionToken: "" },
      exampleRef: "cronomicon/db#password",
    });
  });

  // SM-9: the kind of store does not change while rows name it.
  it("locks the kind of store while the agency's rows use it, and says why", async () => {
    CONNECTED = {
      agencyId: "ag-tax", provider: "hashicorp", providerLabel: "HashiCorp Vault / OpenBao", addr: "https://v", namespace: "",
      caPresent: false, roleIdSet: true, secretIdSet: true, lastModifiedBy: "a@x", lastModifiedAt: "t",
    };
    EXTERNAL_ROWS = 3;
    render(<MemoryRouter><AgencyVaultSection isGlobal={false} /></MemoryRouter>);
    const kind = (await waitFor(() => screen.getByLabelText("Kind of store"))) as HTMLSelectElement;
    expect(kind.disabled).toBe(true);
    expect(kind.title).toMatch(/3 of Tax's external secrets and SSH keys use this store/);
    expect(screen.getByRole("link", { name: "Open Env Vars → Secrets" }).getAttribute("href")).toBe("/env-vars?tab=secrets");
  });

  it("points Global at the installation's Vault, and a reader of another agency at its administrators", async () => {
    AG = { agency: "global", agencyName: "Global" };
    render(<AgencyVaultSection isGlobal />);
    expect(screen.getByText(/Global's Vault is the installation's/)).toBeTruthy();
    expect(screen.getByText("Settings → Installation secret store")).toBeTruthy();
    cleanup();

    AG = { agency: "ag-fin", agencyName: "Fin" };
    render(<AgencyVaultSection isGlobal={false} />);
    expect(screen.getByText(/Only Fin's administrators/)).toBeTruthy();
    expect(writes).toHaveLength(0);
  });
});

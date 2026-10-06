// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, waitFor, within } from "@testing-library/react";

// "Delete Scope" in an expanded scope row is gated on TWO things, and both carry
// meaning: the scope must be cronomicon-authored (a git-source scope is owned by its
// repository, deleting it here would be a lie), and the caller must hold
// ConfigureApp. Before the second half of the gate a read-only viewer was offered
// a button whose only possible outcome was a 403 — the regression pinned below.

const deletes: { path: string; params: unknown }[] = [];
const puts: { path: string; params: unknown; body: unknown }[] = [];
const posts: { path: string; body: unknown }[] = [];
// SB — the notice list the Scopes tab reads for callers who may configure the app.
let bindingNotices: unknown[] = [];
// GC (v2.2.2) — what GET /capabilities answers. The Add Scope dialog reads it to
// decide whether the creator must name an agency (useCreationAgencies), so the
// default here is the global administrator every pre-GC test was written as.
let caps: Record<string, boolean> = {};
let agencies: { id: string; name: string }[] = [];

vi.mock("../../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/client")>();
  return {
    ...actual,
    fetchCapabilities: vi.fn(async () => ({ configureApp: true, configureAppGlobal: true, unrestricted: true, ...caps })),
    api: {
      // The tab fetches /agencies for the binding selector, and each expanded row
      // lazily fetches its inventory. Neither is under test — resolve both to
      // something inert so the panels render nothing of consequence.
      GET: vi.fn(async (path: string) =>
        path === "/agencies"
          ? { data: agencies }
          : path === "/scope-binding-notices"
            ? { data: bindingNotices }
            : { data: { editable: false, hasInventory: false } },
      ),
      DELETE: vi.fn(async (path: string, opts: { params?: unknown }) => {
        deletes.push({ path, params: opts.params });
        return { data: {} };
      }),
      POST: vi.fn(async (path: string, opts: { body?: unknown }) => {
        posts.push({ path, body: opts.body });
        return { data: {} };
      }),
      // The scope-tags write answers with the scope carrying its normalized tags.
      PUT: vi.fn(async (path: string, opts: { params?: unknown; body?: { tags?: string[] } }) => {
        puts.push({ path, params: opts.params, body: opts.body });
        return { data: { tags: opts.body?.tags ?? [] } };
      }),
    } as unknown as typeof actual.api,
  };
});

import { ScopesTab, type ScopeRow } from "./ScopesTab";

const LOCAL: ScopeRow = {
  id: "019fa189-0001-7000-8000-000000000001",
  scope: "edge-lab",
  source: "cronomicon",
  description: "operator-authored",
  hosts: ["host1.internal"],
};
const GIT: ScopeRow = {
  id: "019fa189-0002-7000-8000-000000000002",
  scope: "prod-web",
  source: "git",
  description: "from GitLab",
  gitlabUrl: "https://gitlab.example/inventories/prod-web",
};

// `globalAdmin` defaults to canEdit: before GC "may configure the app" and "may
// configure all of it" were one fact, and every test above the GC block means it
// that way. The GC block passes the two apart.
const renderTab = (scopes: ScopeRow[], canEdit: boolean, globalAdmin: boolean = canEdit) =>
  render(<ScopesTab scopes={scopes} loading={false} error={null} refetch={vi.fn()} canEdit={canEdit} globalAdmin={globalAdmin} />);

// Expanding is a click on the row itself (the actions cell stops propagation).
const expand = (q: ReturnType<typeof within>, scope: string) => fireEvent.click(q.getByText(scope));

const deleteButtons = (q: ReturnType<typeof within>) => q.queryAllByRole("button", { name: "Delete Scope" });

beforeEach(() => {
  deletes.length = 0;
  puts.length = 0;
  posts.length = 0;
  bindingNotices = [];
  caps = {};
  agencies = [];
});
afterEach(cleanup);

describe("ScopesTab — Delete Scope gating", () => {
  it("offers Delete on an expanded Cronomicon scope when the caller may configure the app", async () => {
    const { container } = renderTab([LOCAL, GIT], true);
    const q = within(container);
    expand(q, "edge-lab");
    await waitFor(() => expect(deleteButtons(q)).toHaveLength(1));
    // Sanity: the row really is the Cronomicon-source one that was expanded.
    expect(q.getByText("Cronomicon")).toBeTruthy();
    expect(q.getByText("host1.internal")).toBeTruthy();
  });

  it("hides Delete from a read-only viewer (it could only ever 403)", async () => {
    const { container } = renderTab([LOCAL, GIT], false);
    const q = within(container);
    expand(q, "edge-lab");
    // The row still expands — a viewer may inspect it; only the mutation is gone.
    await waitFor(() => expect(q.getByText("host1.internal")).toBeTruthy());
    expect(deleteButtons(q)).toHaveLength(0);
    // The other mutation affordances stay gone too, so Delete is not an outlier.
    expect(q.queryByRole("button", { name: "Edit" })).toBeNull();
    expect(q.queryByRole("button", { name: "+ Add Scope" })).toBeNull();
  });

  it("never offers Delete on a git-source scope, even with ConfigureApp", async () => {
    const { container } = renderTab([LOCAL, GIT], true);
    const q = within(container);
    expand(q, "prod-web");
    await waitFor(() => expect(q.getByText("Git")).toBeTruthy());
    expect(deleteButtons(q)).toHaveLength(0);
    // Its destructive path is upstream, in the repository that owns it.
    expect(q.getByRole("link", { name: /Edit in GitLab/ })).toBeTruthy();
  });

  it("confirms first, then DELETEs the expanded scope by id", async () => {
    const { container } = renderTab([LOCAL, GIT], true);
    const q = within(container);
    expand(q, "edge-lab");
    await waitFor(() => expect(deleteButtons(q)).toHaveLength(1));
    fireEvent.click(deleteButtons(q)[0]);

    // The confirm dialog names the scope and adds its own "Delete Scope" button,
    // so the row button and the confirm are now both present; the dialog's is last.
    await waitFor(() => expect(q.getByText(/cannot be undone/)).toBeTruthy());
    expect(deletes).toHaveLength(0);

    const buttons = deleteButtons(q);
    expect(buttons).toHaveLength(2);
    fireEvent.click(buttons[buttons.length - 1]);
    await waitFor(() => expect(deletes).toHaveLength(1));
    expect(deletes[0].path).toBe("/scopes/{scopeId}");
    // K-3 — a scope id is a UUIDv7 string, not a rowid. The spec claimed
    // `integer` until v0.52.24 and this fixture faithfully reproduced the lie.
    expect((deletes[0].params as { path: { scopeId: string } }).path.scopeId).toBe(LOCAL.id);
  });
});

// ST band — scope tags. Operator-owned labels (migration 1160): shown to anyone
// who can see the scope, edited only with ConfigureApp, and available on a
// git-source scope too, because the tags never come from Git.
describe("ScopesTab — tags", () => {
  const TAGGED_LOCAL: ScopeRow = { ...LOCAL, tags: ["prod", "linux"] };
  const TAGGED_GIT: ScopeRow = { ...GIT, tags: ["staging"] };
  const tagInput = (q: ReturnType<typeof within>) => q.queryByPlaceholderText("Add a tag…");

  it("shows each scope's tags in its row", () => {
    const { container } = renderTab([TAGGED_LOCAL, TAGGED_GIT], true);
    const q = within(container);
    expect(q.getByText("prod")).toBeTruthy();
    expect(q.getByText("linux")).toBeTruthy();
    expect(q.getByText("staging")).toBeTruthy();
  });

  it("narrows the list to the scopes carrying a selected tag, and offers the filter back", async () => {
    const { container } = renderTab([TAGGED_LOCAL, TAGGED_GIT], true);
    const q = within(container);
    fireEvent.click(q.getByRole("button", { name: /All/ }));
    fireEvent.click(within(q.getByRole("listbox")).getByLabelText("staging"));
    await waitFor(() => expect(q.queryByText("edge-lab")).toBeNull());
    expect(q.getByText("prod-web")).toBeTruthy();

    // "All" mode with a second tag no scope shares leaves nothing — the empty
    // state must name filters (not a search that was never typed) and reset them.
    fireEvent.click(within(q.getByRole("listbox")).getByRole("button", { name: "All" }));
    fireEvent.click(within(q.getByRole("listbox")).getByLabelText("prod"));
    await waitFor(() => expect(q.getByText("No scopes match the current filters.")).toBeTruthy());
    fireEvent.click(q.getByRole("button", { name: "Clear filters" }));
    await waitFor(() => expect(q.getByText("edge-lab")).toBeTruthy());
    expect(q.getByText("prod-web")).toBeTruthy();
  });

  it("saves an added tag through PUT /scope-tags/{scopeId} and repaints the row", async () => {
    const { container } = renderTab([TAGGED_LOCAL], true);
    const q = within(container);
    expand(q, "edge-lab");
    await waitFor(() => expect(tagInput(q)).toBeTruthy());
    fireEvent.change(tagInput(q)!, { target: { value: "dmz" } });
    fireEvent.keyDown(tagInput(q)!, { key: "Enter" });

    await waitFor(() => expect(puts).toHaveLength(1));
    expect(puts[0].path).toBe("/scope-tags/{scopeId}");
    expect((puts[0].params as { path: { scopeId: string } }).path.scopeId).toBe(LOCAL.id);
    expect(puts[0].body).toEqual({ tags: ["prod", "linux", "dmz"] });
    // The editor chip appears at once; the row cell caps at two chips plus "+1".
    await waitFor(() => expect(q.getAllByText("dmz").length).toBeGreaterThan(0));
  });

  it("lets a git-source scope be tagged — the tags are not part of the repository", async () => {
    const { container } = renderTab([TAGGED_GIT], true);
    const q = within(container);
    expand(q, "prod-web");
    await waitFor(() => expect(tagInput(q)).toBeTruthy());
    fireEvent.click(q.getByRole("button", { name: "Remove tag staging" }));
    await waitFor(() => expect(puts).toHaveLength(1));
    expect((puts[0].params as { path: { scopeId: string } }).path.scopeId).toBe(GIT.id);
    expect(puts[0].body).toEqual({ tags: [] });
  });

  it("shows the tags read-only to a viewer without ConfigureApp", async () => {
    const { container } = renderTab([TAGGED_LOCAL], false);
    const q = within(container);
    expand(q, "edge-lab");
    await waitFor(() => expect(q.getByText("host1.internal")).toBeTruthy());
    // Still visible (row cell + the read-only editor chips)…
    expect(q.getAllByText("prod").length).toBeGreaterThan(1);
    // …but with nothing to type into and nothing to remove.
    expect(tagInput(q)).toBeNull();
    expect(q.queryByRole("button", { name: /Remove tag/ })).toBeNull();
    expect(puts).toHaveLength(0);
  });
});

// ST band — "supported run types" are gone from a scope. They were advisory and
// nothing acted on them; what a scope shows now is its tags and, for a git-source
// scope, the Git metadata that used to ride along with the capability.
describe("ScopesTab — no supported run types", () => {
  const GIT_META: ScopeRow = {
    ...GIT,
    name: "prod-web.ini",
    owner: "infra-platform",
    sidecarPath: "inventory/prod-web.cronomicon.yaml",
    pragmaErrors: [{ line: 2, message: 'unknown directive: "ownr"' }],
  };

  it("has no Supported Types column and no capability origin", async () => {
    const { container } = renderTab([LOCAL, GIT_META], true);
    const q = within(container);
    expect(q.queryByText("Supported Types")).toBeNull();
    expect(q.getByText("Tags")).toBeTruthy();
    expand(q, "prod-web");
    await waitFor(() => expect(q.getByText("Owner")).toBeTruthy());
    expect(q.queryByText("Capability origin")).toBeNull();
    expect(q.queryByText(/inferred types/i)).toBeNull();
  });

  it("keeps the Git metadata: owner, inventory file, sidecar and the pragma errors", async () => {
    const { container } = renderTab([LOCAL, GIT_META], true);
    const q = within(container);
    // The row flags the unparsed pragma beside the scope name…
    expect(q.getByLabelText("1 pragma parse error")).toBeTruthy();
    expand(q, "prod-web");
    // …and the expanded row says what and where.
    await waitFor(() => expect(q.getByText("infra-platform")).toBeTruthy());
    expect(q.getByText("prod-web.ini")).toBeTruthy();
    expect(q.getByText("inventory/prod-web.cronomicon.yaml")).toBeTruthy();
    expect(q.getByText(/unknown directive/)).toBeTruthy();
    expect(q.getByText("line 2")).toBeTruthy();
  });

  it("creates a scope without asking for, or sending, run types", async () => {
    const { container } = renderTab([LOCAL], true);
    const q = within(container);
    fireEvent.click(q.getByRole("button", { name: "+ Add Scope" }));
    // The modal renders in a portal, so query the document.
    const body = within(document.body);
    await waitFor(() => expect(body.getByPlaceholderText("e.g. Edge-Lab")).toBeTruthy());
    expect(body.queryByText(/Supported run types/)).toBeNull();
    expect(body.queryByText(/bash floor/i)).toBeNull();

    fireEvent.change(body.getByPlaceholderText("e.g. Edge-Lab"), { target: { value: "New-Scope" } });
    fireEvent.change(body.getByPlaceholderText(/host1\.internal/), { target: { value: "h1.internal" } });
    fireEvent.click(body.getByRole("button", { name: "Create Scope" }));
    await waitFor(() => expect(posts.some((p) => p.path === "/scopes")).toBe(true));
    const sent = posts.find((p) => p.path === "/scopes")!.body as Record<string, unknown>;
    expect(sent.scope).toBe("New-Scope");
    expect(sent.hosts).toEqual(["h1.internal"]);
    expect("supportedTypes" in sent).toBe(false);
  });
});

// SB — where a scope's runner binding shows up in this view. The components are
// tested on their own (ScopeRunners.test.tsx); what is pinned here is the wiring:
// the column, the expanded-row section, the notice banner and who sees it, and
// the Agencies column no longer calling a general-pool scope "unrestricted".
describe("ScopesTab — runner bindings (SB)", () => {
  const BOUND: ScopeRow = {
    ...GIT,
    boundRunners: [{ runnerId: "r1", name: "runner-dmz-01", registered: false, status: "", eligible: false }],
  };

  it("shows the binding in the Runners column and the expanded row, on a Git scope too", async () => {
    const { container } = renderTab([LOCAL, BOUND], true);
    const q = within(container);
    // Column: the unbound scope says so, the bound one names its runner and warns.
    expect(q.getByText("any eligible")).toBeTruthy();
    expect(q.getByLabelText("No bound runner can claim work")).toBeTruthy();

    expand(q, "prod-web");
    await waitFor(() => expect(container.textContent).toMatch(/No bound runner can claim work right now/));
    // The binding is an operator overlay, so a Git-source scope is editable here.
    expect(q.getByRole("button", { name: "Change…" })).toBeTruthy();
    expect(q.getByRole("button", { name: "replace" })).toBeTruthy();
  });

  it("calls a scope in no agency the general pool — it is not open to every runner", () => {
    const { container } = renderTab([LOCAL], true);
    const q = within(container);
    const cell = q.getByText("general pool");
    expect(cell.getAttribute("title")).toMatch(/only a general-pool runner/);
    expect(q.queryByText("unrestricted")).toBeNull();
  });

  it("shows the retired-pin notices to a caller who may configure the app, and to nobody else", async () => {
    bindingNotices = [
      { id: 1, jobUid: "u1", jobName: "deploy", jobSource: "git", scope: "prod-web", runnerTag: "vlan-dmz", reason: "partial_pins", recordedAt: "t" },
    ];
    const admin = renderTab([LOCAL, GIT], true);
    await waitFor(() => expect(admin.container.textContent).toMatch(/1 job used to be confined to particular runners/));
    cleanup();

    const viewer = renderTab([LOCAL, GIT], false);
    // Give a fetch the chance to land; there must be none to land.
    await new Promise((r) => setTimeout(r, 20));
    expect(viewer.container.textContent).not.toMatch(/used to be confined/);
  });
});

// GC (v2.2.2, gate closing) — an administrator of ONE agency holds configureApp
// and is still refused by the install-wide routes: moving a scope between
// agencies (PUT /scopes/{id}/agency) and the GitLab re-sync. FX-7: those controls
// stay and are disabled with the reason; they are not hidden, because the caller
// holds the permission and the precondition is one they can read about.
describe("ScopesTab — global-administrator controls (GC)", () => {
  const WHY = /Only a global administrator \(a role on every agency\) can/;

  it("disables Re-sync and the per-scope agency select for an administrator of one agency, with the reason", async () => {
    const { container } = renderTab([LOCAL, GIT], true, false);
    const q = within(container);

    const resync = q.getByRole("button", { name: /Re-sync from GitLab/ }) as HTMLButtonElement;
    expect(resync.disabled).toBe(true);
    expect(resync.title).toMatch(WHY);

    expand(q, "edge-lab");
    const select = (await waitFor(() => q.getByLabelText("Agency for edge-lab"))) as HTMLSelectElement;
    expect(select.disabled).toBe(true);
    expect(select.title).toMatch(WHY);

    // The per-scope writes are NOT global: they are judged against the scope's
    // agency by the server, and this view does not guess at that — they stay.
    expect((q.getByRole("button", { name: "+ Add Scope" }) as HTMLButtonElement).disabled).toBe(false);
    expect((q.getByRole("button", { name: "Delete Scope" }) as HTMLButtonElement).disabled).toBe(false);
  });

  it("leaves both enabled for a global administrator", async () => {
    const { container } = renderTab([LOCAL, GIT], true, true);
    const q = within(container);
    const resync = q.getByRole("button", { name: /Re-sync from GitLab/ }) as HTMLButtonElement;
    expect(resync.disabled).toBe(false);
    expect(resync.title).toBe("");

    expand(q, "edge-lab");
    const select = (await waitFor(() => q.getByLabelText("Agency for edge-lab"))) as HTMLSelectElement;
    expect(select.disabled).toBe(false);
  });
});

// GC-6 — POST /scopes takes `agencyIds`. A scope created by a departmental
// administrator must land in an agency they hold (it used to land in none, where
// its own creator could not see it), and a caller holding configureApp on
// several gets 422 `agency_required` unless they name one. The dialog reuses the
// creation picker from secrets, variables and SSH keys, keyed on the GLOBAL flag
// for configureApp rather than on `unrestricted`.
describe("ScopesTab — Add Scope names its agency (GC-6)", () => {
  const openAdd = async () => {
    const { container } = renderTab([LOCAL], true, false);
    fireEvent.click(within(container).getByRole("button", { name: "+ Add Scope" }));
    const body = within(document.body);
    await waitFor(() => expect(body.getByPlaceholderText("e.g. Edge-Lab")).toBeTruthy());
    return body;
  };

  it("asks a non-global administrator for an agency, blocks until one is chosen, and sends it", async () => {
    caps = { configureAppGlobal: false, unrestricted: false };
    agencies = [{ id: "ag-tax", name: "Tax" }, { id: "ag-fin", name: "Finance" }];
    const body = await openAdd();

    // The picker, and a submit button that says what is missing.
    await waitFor(() => expect(body.getByText(/this scope must belong to one of your\s+agencies/)).toBeTruthy());
    const blocked = body.getByRole("button", { name: "Choose at least one agency" }) as HTMLButtonElement;
    expect(blocked.disabled).toBe(true);

    fireEvent.change(body.getByPlaceholderText("e.g. Edge-Lab"), { target: { value: "Tax-Lab" } });
    const list = body.getByRole("listbox") as HTMLSelectElement;
    await waitFor(() => expect(list.options.length).toBe(2));
    list.options[0].selected = true;
    fireEvent.change(list);

    fireEvent.click(await waitFor(() => body.getByRole("button", { name: "Create Scope" })));
    await waitFor(() => expect(posts.some((p) => p.path === "/scopes")).toBe(true));
    const sent = posts.find((p) => p.path === "/scopes")!.body as Record<string, unknown>;
    expect(sent.scope).toBe("Tax-Lab");
    expect(sent.agencyIds).toEqual(["ag-tax"]);
  });

  it("keys on the global flag, not on `unrestricted` — a viewer of everything who administers one agency is asked", async () => {
    // The permission-blind trap: `unrestricted` is true for this caller, and the
    // server still requires an agency because no all-agencies grant carries
    // configureApp.
    caps = { configureAppGlobal: false, unrestricted: true };
    agencies = [{ id: "ag-tax", name: "Tax" }];
    const body = await openAdd();
    await waitFor(() => expect(body.getByRole("button", { name: "Choose at least one agency" })).toBeTruthy());
  });

  it("asks a global administrator for nothing and sends no agencyIds", async () => {
    caps = { configureAppGlobal: true };
    agencies = [{ id: "ag-tax", name: "Tax" }];
    const { container } = renderTab([LOCAL], true, true);
    fireEvent.click(within(container).getByRole("button", { name: "+ Add Scope" }));
    const body = within(document.body);
    await waitFor(() => expect(body.getByPlaceholderText("e.g. Edge-Lab")).toBeTruthy());
    // Let the capabilities read land; there must still be no picker.
    await new Promise((r) => setTimeout(r, 20));
    expect(body.queryByRole("listbox")).toBeNull();

    fireEvent.change(body.getByPlaceholderText("e.g. Edge-Lab"), { target: { value: "Shared" } });
    fireEvent.click(body.getByRole("button", { name: "Create Scope" }));
    await waitFor(() => expect(posts.some((p) => p.path === "/scopes")).toBe(true));
    const sent = posts.find((p) => p.path === "/scopes")!.body as Record<string, unknown>;
    expect("agencyIds" in sent).toBe(false);
  });
});

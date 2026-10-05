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

vi.mock("../../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/client")>();
  return {
    ...actual,
    api: {
      // The tab fetches /agencies for the binding selector, and each expanded row
      // lazily fetches its inventory. Neither is under test — resolve both to
      // something inert so the panels render nothing of consequence.
      GET: vi.fn(async (path: string) =>
        path === "/agencies" ? { data: [] } : { data: { editable: false, hasInventory: false } },
      ),
      DELETE: vi.fn(async (path: string, opts: { params?: unknown }) => {
        deletes.push({ path, params: opts.params });
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
  capability: { types: ["bash"], origin: "local" },
};
const GIT: ScopeRow = {
  id: "019fa189-0002-7000-8000-000000000002",
  scope: "prod-web",
  source: "git",
  description: "from GitLab",
  gitlabUrl: "https://gitlab.example/inventories/prod-web",
  capability: { types: ["ansible"], origin: "pragma" },
};

const renderTab = (scopes: ScopeRow[], canEdit: boolean) =>
  render(<ScopesTab scopes={scopes} loading={false} error={null} refetch={vi.fn()} canEdit={canEdit} />);

// Expanding is a click on the row itself (the actions cell stops propagation).
const expand = (q: ReturnType<typeof within>, scope: string) => fireEvent.click(q.getByText(scope));

const deleteButtons = (q: ReturnType<typeof within>) => q.queryAllByRole("button", { name: "Delete Scope" });

beforeEach(() => {
  deletes.length = 0;
  puts.length = 0;
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

// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";

// Scopes → a scope → Clone to an agency (2.4.0; R8). Another agency gets a
// scope by a clone, not a move: the targets are the agencies the caller
// administers, without the scope's own; the clone has a name of its own; and
// afterwards the dialog says what was copied and what was not.

const posts: { path: string; params: unknown; body: unknown }[] = [];
let answer: unknown = {};

vi.mock("../../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/client")>();
  return {
    ...actual,
    api: {
      GET: vi.fn(async () => ({ data: [{ id: "global", name: "Global" }, { id: "ag-fin", name: "Finance" }, { id: "ag-tax", name: "Tax" }] })),
      POST: vi.fn(async (path: string, init: { params?: unknown; body?: unknown }) => {
        posts.push({ path, params: init.params, body: init.body });
        return answer;
      }),
    } as unknown as typeof actual.api,
  };
});
const grant = (id: string, name: string) => ({ role: "admin", allScopes: false, agencyId: id, agencyName: name, scopes: [], permissions: ["configureApp"], groups: [], origin: "group" });
vi.mock("../../api/access", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/access")>();
  return { ...actual, useMyAccess: () => ({ grants: [grant("ag-fin", "Finance"), grant("ag-tax", "Tax")] }) };
});

import { CloneScopeDialog } from "./CloneScope";

beforeEach(() => {
  posts.length = 0;
  answer = { data: { scope: { id: "s2" }, copied: { hosts: 3, groups: 1, hostVars: 2, groupVars: 0, hostRecords: 3 }, note: "The host records have no SSH key." } };
});
afterEach(cleanup);

describe("Clone to an agency", () => {
  it("offers the other agency the caller administers, sends the clone, and says what was not copied", async () => {
    const cloned = vi.fn();
    render(<CloneScopeDialog scopeId="s1" scopeName="edge-lab" currentAgency="ag-fin" globalAdmin={false} onClose={vi.fn()} onCloned={cloned} />);
    const select = (await screen.findByLabelText("Clone to agency")) as HTMLSelectElement;
    // Its own agency is not a target, and Global is a global administrator's.
    expect(Array.from(select.options).map((o) => o.textContent)).toEqual(["Tax"]);
    fireEvent.change(screen.getByLabelText("Clone name"), { target: { value: "edge-lab-tax" } });
    fireEvent.click(screen.getByRole("button", { name: "Clone" }));
    await waitFor(() => expect(posts).toHaveLength(1));
    expect(posts[0].path).toBe("/scopes/{scopeId}/clone");
    expect(posts[0].body).toEqual({ agencyId: "ag-tax", name: "edge-lab-tax" });
    expect(await screen.findByText(/Copied: 3 hosts, 1 groups/)).toBeTruthy();
    expect(screen.getByText(/Not copied: the runner bindings, the approved host keys, the jobs/)).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Done" }));
    expect(cloned).toHaveBeenCalledWith("Cloned edge-lab to Tax as edge-lab-tax");
  });

  it("offers a global administrator Global and every other agency, and says why a clone is refused", async () => {
    answer = { error: { code: "scope_name_in_use", message: "jobs of another agency name a scope edge-lab" } };
    render(<CloneScopeDialog scopeId="s1" scopeName="edge-lab" currentAgency="ag-fin" globalAdmin onClose={vi.fn()} onCloned={vi.fn()} />);
    const select = (await screen.findByLabelText("Clone to agency")) as HTMLSelectElement;
    await waitFor(() => expect(Array.from(select.options).map((o) => o.textContent)).toEqual(["Choose an agency…", "Global", "Tax"]));
    fireEvent.change(select, { target: { value: "global" } });
    fireEvent.click(screen.getByRole("button", { name: "Clone" }));
    expect((await screen.findByRole("alert")).textContent).toMatch(/jobs of another agency name a scope/);
  });
});

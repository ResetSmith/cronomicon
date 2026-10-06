// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

// The Scripts detail pane's "Create Job" hand-off into JobComposer. What is worth
// pinning: the link carries the selected script's NAME (URL-encoded — script names
// contain slashes) as ?script=, and it is compose-gated like every other create
// affordance (never offer a button that dead-ends on the composer's notice).

let composeOn = true;
// GC (v2.2.2) — POST /git/sync needs a global administrator for configureApp.
let syncOn = true;
let scripts: unknown = null;

const SCRIPTS = {
  items: [
    {
      name: "tools/backup.sh",
      runType: "bash",
      sourceKind: "file",
      sourcePath: "scripts/tools/backup.sh",
      usedByCount: 0,
      tags: [],
    },
  ],
  totalItems: 1,
  totalPages: 1,
  page: 1,
  pageSize: 200,
};

const laterTick = () => new Promise((resolve) => setTimeout(resolve, 0));

vi.mock("../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/client")>();
  return {
    ...actual,
    api: {
      GET: vi.fn(async (path: string) => {
        if (path === "/scripts") {
          await laterTick();
          return { data: scripts ?? SCRIPTS };
        }
        if (path === "/scripts/{name}") return { data: SCRIPTS.items[0] };
        if (path === "/script-content/{name}") return { data: { content: "echo hi", encoding: "utf-8" } };
        if (path === "/job-reference-bindings/{jobId}" || path === "/script-reference-bindings/{name}") {
          return { data: { bindings: [] } };
        }
        return { data: [] };
      }),
      POST: vi.fn(async () => ({ data: {} })),
      PUT: vi.fn(async () => ({ data: {} })),
      DELETE: vi.fn(async () => ({ data: {} })),
    } as unknown as typeof actual.api,
    fetchCapabilities: vi.fn(async () => ({
      vault: false,
      apprise: false,
      compose: composeOn,
      manageRoles: false,
      configureApp: false,
      configureAppGlobal: syncOn,
      manageEnvVars: false,
      publishSchedule: false,
    })),
  };
});

import { Scripts } from "./Scripts";

beforeEach(() => {
  composeOn = true;
  syncOn = true;
  scripts = null;
});
afterEach(cleanup);

// Search to leave browse mode (flat rows, no folder tree), then expand the row.
const renderAndExpand = async () => {
  const { container } = render(
    <MemoryRouter>
      <Scripts />
    </MemoryRouter>,
  );
  const q = within(container);
  fireEvent.change(q.getByPlaceholderText("Search scripts…"), { target: { value: "backup" } });
  await waitFor(() => expect(q.getByText("tools/backup.sh")).toBeTruthy());
  fireEvent.click(q.getByText("tools/backup.sh").closest("tr")!);
  // The detail pane is up once its field row renders.
  await waitFor(() => expect(q.getByText("Run type")).toBeTruthy());
  return q;
};

describe("Scripts — detail-pane Create Job", () => {
  it("links to the composer with the script preset (URL-encoded name)", async () => {
    const q = await renderAndExpand();
    const link = await waitFor(() => {
      const el = q.getByRole("link", { name: "+ Create Job" });
      expect(el).toBeTruthy();
      return el;
    });
    expect(link.getAttribute("href")).toBe("/compose?script=tools%2Fbackup.sh");
  });

  it("hides Create Job without the compose capability", async () => {
    composeOn = false;
    const q = await renderAndExpand();
    // The detail pane rendered (Run type is on screen), so the absence is the
    // gate, not a loading race.
    expect(q.queryByRole("link", { name: "+ Create Job" })).toBeNull();
  });
});

// GC (v2.2.2, gate closing) — the Git pull re-reads the WHOLE definitions repo,
// every agency's files included, so POST /git/sync is a global administrator's
// (configureApp on every agency). The button sits on a catalog everyone reads and
// used to be live for all of them, 403ing on click. It stays — disabled, with
// the reason (FX-7) — in the toolbar and in the empty catalog's call to action.
describe("Scripts — Git Pull needs a global administrator (GC)", () => {
  const WHY = "Only a global administrator (a role on every agency) can start a sync from GitLab.";
  const renderScripts = () =>
    within(
      render(
        <MemoryRouter>
          <Scripts />
        </MemoryRouter>,
      ).container,
    );

  it("disables the toolbar pull with the reason for anyone who is not one", async () => {
    syncOn = false;
    const q = renderScripts();
    const pull = q.getByRole("button", { name: /Git Pull/ }) as HTMLButtonElement;
    await waitFor(() => expect(pull.title).toBe(WHY));
    expect(pull.disabled).toBe(true);

    const { api } = await import("../api/client");
    vi.mocked(api.POST).mockClear();
    fireEvent.click(pull);
    expect(vi.mocked(api.POST)).not.toHaveBeenCalled();
  });

  it("disables the empty catalog's Pull from GitLab the same way", async () => {
    syncOn = false;
    scripts = { items: [], totalItems: 0, totalPages: 1, page: 1, pageSize: 200 };
    const q = renderScripts();
    const pull = (await waitFor(() => q.getByRole("button", { name: "Pull from GitLab" }))) as HTMLButtonElement;
    await waitFor(() => expect(pull.title).toBe(WHY));
    expect(pull.disabled).toBe(true);
  });

  it("enables both for a global administrator", async () => {
    scripts = { items: [], totalItems: 0, totalPages: 1, page: 1, pageSize: 200 };
    const q = renderScripts();
    const empty = (await waitFor(() => q.getByRole("button", { name: "Pull from GitLab" }))) as HTMLButtonElement;
    const toolbar = q.getByRole("button", { name: /Git Pull/ }) as HTMLButtonElement;
    await waitFor(() => expect(toolbar.disabled).toBe(false));
    expect(toolbar.title).toMatch(/Pulls the whole definitions repo/);
    expect(empty.disabled).toBe(false);
    expect(empty.title).toBe("");
  });
});

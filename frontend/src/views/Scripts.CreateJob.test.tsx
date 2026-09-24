// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

// The Scripts detail pane's "Create Job" hand-off into JobComposer. What is worth
// pinning: the link carries the selected script's NAME (URL-encoded — script names
// contain slashes) as ?script=, and it is compose-gated like every other create
// affordance (never offer a button that dead-ends on the composer's notice).

let composeOn = true;

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
          return { data: SCRIPTS };
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
      manageEnvVars: false,
      publishSchedule: false,
    })),
  };
});

import { Scripts } from "./Scripts";

beforeEach(() => {
  composeOn = true;
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

// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

// JP-4b — the expanded view's Secrets & variables section is READ-ONLY and shows
// the EFFECTIVE set (the job's own bindings ∪ the referenced script's), hidden
// entirely when that set is empty. What is worth pinning: no add/remove controls
// survive here, script-declared references are shown and labelled, the section
// disappears for a job that uses none, and the "Edit in Composer" affordance
// follows the same gate as the row's Edit button.

let composeOn = true;
// Per-job binding fixtures, keyed by the id the detail fetch asks for.
const JOB_BINDINGS: Record<number, { kind: string; name: string; reference?: string }[]> = {
  1: [{ kind: "secret", name: "DB_PASSWORD", reference: "CRONOMICON_SECRET_DB_PASSWORD" }],
  2: [],
  3: [],
};
const SCRIPT_BINDINGS: Record<string, { kind: string; name: string; reference?: string }[]> = {
  "tools/deploy.sh": [{ kind: "var", name: "REGION", reference: "CRONOMICON_VAR_REGION" }],
};

const JOBS = {
  items: [
    { id: 1, name: "job-with-refs", type: "bash", scope: "Prod", source: "amadeus", status: "success" },
    { id: 2, name: "job-no-refs", type: "bash", scope: "Prod", source: "amadeus", status: "success" },
    { id: 3, name: "job-script-refs", type: "bash", scope: "Prod", source: "git", status: "success" },
  ],
  totalItems: 3,
  totalPages: 1,
  page: 1,
  pageSize: 50,
};

// Job 3 references a script that declares its own binding — the case only this
// section surfaces (the script half was never visible on this page before).
const DETAILS: Record<number, object> = {
  1: JOBS.items[0],
  2: JOBS.items[1],
  3: { ...JOBS.items[2], scriptRef: "tools/deploy.sh" },
};

const laterTick = () => new Promise((resolve) => setTimeout(resolve, 0));

vi.mock("../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/client")>();
  return {
    ...actual,
    api: {
      GET: vi.fn(async (path: string, opts?: { params?: { path?: { jobId?: number; name?: string } } }) => {
        if (path === "/jobs") {
          await laterTick();
          return { data: JOBS };
        }
        if (path === "/jobs/{jobId}") return { data: DETAILS[opts?.params?.path?.jobId ?? 0] };
        if (path === "/runs") return { data: { items: [] } };
        if (path === "/job-reference-bindings/{jobId}")
          return { data: { bindings: JOB_BINDINGS[opts?.params?.path?.jobId ?? 0] ?? [] } };
        if (path === "/script-reference-bindings/{name}")
          return { data: { bindings: SCRIPT_BINDINGS[opts?.params?.path?.name ?? ""] ?? [] } };
        return { data: [] };
      }),
      POST: vi.fn(async () => ({ data: { results: [] } })),
      PUT: vi.fn(async () => ({ data: {} })),
      DELETE: vi.fn(async () => ({ data: {} })),
    } as unknown as typeof actual.api,
    fetchCapabilities: vi.fn(async () => ({
      vault: false,
      apprise: false,
      compose: composeOn,
      // AF-2 — an unrestricted composer, so the All scope option is offered.
      composeUnbound: true,
      manageRoles: false,
      configureApp: false,
      manageEnvVars: true,
      publishSchedule: true,
    })),
  };
});

import { Jobs } from "./Jobs";

beforeEach(() => {
  composeOn = true;
});
afterEach(cleanup);

const renderJobs = async (search: string) => {
  const { container } = render(
    <MemoryRouter>
      <Jobs />
    </MemoryRouter>,
  );
  const q = within(container);
  fireEvent.change(q.getByPlaceholderText("Search jobs…"), { target: { value: search } });
  await waitFor(() => expect(q.getAllByText(/^job-/).length).toBeGreaterThan(0));
  return q;
};

const expandRow = (q: ReturnType<typeof within>, name: string) => {
  fireEvent.click(q.getByText(name).closest("tr")!);
};

describe("Jobs — effective references (JP-4b)", () => {
  it("shows the job's declared references read-only, with no add or remove controls", async () => {
    const q = await renderJobs("job-with-refs");
    expandRow(q, "job-with-refs");
    await waitFor(() => expect(q.getByText("CRONOMICON_SECRET_DB_PASSWORD")).toBeTruthy());
    // The editor's controls are gone: no Add button, no name field, no remove ×.
    expect(q.queryByRole("button", { name: "Add" })).toBeNull();
    expect(q.queryByPlaceholderText("reference name…")).toBeNull();
    expect(q.queryByRole("button", { name: /^Remove reference/ })).toBeNull();
  });

  it("omits the section entirely when the job receives no references", async () => {
    const q = await renderJobs("job-no-refs");
    expandRow(q, "job-no-refs");
    // "Recent runs" is a detail-only sibling section that always renders —
    // waiting on it proves the detail is open before asserting this one's
    // absence. (Tags would be ambiguous: the table has a Tags column header.)
    await waitFor(() => expect(q.getByText("Recent runs")).toBeTruthy());
    expect(q.queryByText(/Secrets & variables/)).toBeNull();
    expect(q.queryByText("No secrets or variables declared.")).toBeNull();
  });

  it("surfaces a script-declared reference and labels its provenance", async () => {
    const q = await renderJobs("job-script-refs");
    expandRow(q, "job-script-refs");
    await waitFor(() => expect(q.getByText("CRONOMICON_VAR_REGION")).toBeTruthy());
    expect(q.getByText("from script")).toBeTruthy();
  });

  it("offers Edit in Composer only for an amadeus row the caller may compose", async () => {
    const q = await renderJobs("job-with-refs");
    expandRow(q, "job-with-refs");
    await waitFor(() => expect(q.getByText("CRONOMICON_SECRET_DB_PASSWORD")).toBeTruthy());
    expect(q.getByRole("button", { name: "Edit in Composer" })).toBeTruthy();
  });

  it("withholds Edit in Composer on a git-source row", async () => {
    const q = await renderJobs("job-script-refs");
    expandRow(q, "job-script-refs");
    await waitFor(() => expect(q.getByText("CRONOMICON_VAR_REGION")).toBeTruthy());
    expect(q.queryByRole("button", { name: "Edit in Composer" })).toBeNull();
  });

  it("withholds Edit in Composer when the caller cannot compose", async () => {
    composeOn = false;
    const q = await renderJobs("job-with-refs");
    expandRow(q, "job-with-refs");
    await waitFor(() => expect(q.getByText("CRONOMICON_SECRET_DB_PASSWORD")).toBeTruthy());
    expect(q.queryByRole("button", { name: "Edit in Composer" })).toBeNull();
  });
});

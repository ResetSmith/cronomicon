// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

// AF-1 — a job's scope decides which agencies can see it, so the composer must
// make it a CHOICE. What is pinned: an unanswered scope blocks the create with a
// sentence (not a silent global job, and not a server round-trip), the explicit
// All option sends "" so the server's required-field check passes, a named scope
// still works, and editing an existing global job prefills All rather than
// re-asking (the value was already decided, even if by a legacy default).

const SCOPES = [
  { id: "sc1", scope: "Prod", hosts: ["db-01"], capability: { types: ["bash"] }, agencies: ["FIN"] },
  // Deliberately unmapped: drives the AF-Q3 advisory warning.
  { id: "sc2", scope: "Orphan", hosts: [], capability: { types: [] }, agencies: [] },
];

const GLOBAL_JOB = {
  id: 8,
  name: "legacy-global",
  source: "cronomicon",
  scriptRef: "tools/backup.sh",
  scope: null,
  prompts: [],
  schedules: [],
  enabled: true,
};

const posts: { path: string; body: Record<string, unknown> }[] = [];

vi.mock("../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/client")>();
  return {
    ...actual,
    api: {
      GET: vi.fn(async (path: string, opts?: { params?: { path?: { jobId?: number } } }) => {
        if (path === "/jobs/{jobId}") {
          return opts?.params?.path?.jobId === 8 ? { data: GLOBAL_JOB } : { data: undefined, error: {} };
        }
        if (path === "/scopes") return { data: SCOPES };
        if (path === "/job-reference-bindings/{jobId}") return { data: { bindings: [] } };
        if (path === "/scripts/{name}") {
          return { data: { name: "tools/backup.sh", runType: "bash", variables: [], prompts: [] } };
        }
        return { data: [] };
      }),
      POST: vi.fn(async (path: string, opts: { body: Record<string, unknown> }) => {
        posts.push({ path, body: opts.body });
        return { data: { id: 99 }, response: { ok: true, status: 200 } };
      }),
      PUT: vi.fn(async () => ({ data: {}, response: { ok: true, status: 200 } })),
      DELETE: vi.fn(async () => ({ data: {}, response: { ok: true, status: 200 } })),
    } as unknown as typeof actual.api,
    fetchCapabilities: vi.fn(async () => ({
      vault: false,
      apprise: false,
      compose: true,
      // AF-2 — an unrestricted composer, so the All scope option is offered.
      composeUnbound: true,
      manageRoles: false,
      configureApp: false,
      manageEnvVars: true,
      publishSchedule: false,
    })),
  };
});

import { JobComposer } from "./JobComposer";

beforeEach(() => {
  posts.length = 0;
});
afterEach(cleanup);

const renderComposer = (url: string) => {
  const { container } = render(
    <MemoryRouter initialEntries={[url]}>
      <JobComposer />
    </MemoryRouter>,
  );
  return within(container);
};

const pickScope = (q: ReturnType<typeof within>, optionName: RegExp) => {
  fireEvent.click(q.getByRole("combobox", { name: "Scope" }));
  fireEvent.click(q.getByRole("option", { name: optionName }));
};

describe("JobComposer — scope is a required choice (AF-1)", () => {
  it("refuses to create until a scope is chosen, and says why", async () => {
    const q = renderComposer("/compose?script=tools%2Fbackup.sh");
    await waitFor(() => expect(q.getAllByText("tools/backup.sh").length).toBeGreaterThan(0));
    fireEvent.change(q.getByPlaceholderText("nightly-backup"), { target: { value: "unscoped" } });
    fireEvent.click(q.getByRole("button", { name: "Create job" }));

    await waitFor(() => expect(q.getByText(/Pick a scope/)).toBeTruthy());
    // Nothing was sent: the accident is stopped in the form, not by a 422.
    expect(posts.filter((p) => p.path === "/jobs")).toHaveLength(0);
  });

  it("sends an explicit empty scope when All is chosen", async () => {
    const q = renderComposer("/compose?script=tools%2Fbackup.sh");
    await waitFor(() => expect(q.getAllByText("tools/backup.sh").length).toBeGreaterThan(0));
    fireEvent.change(q.getByPlaceholderText("nightly-backup"), { target: { value: "global-job" } });
    pickScope(q, /All agencies \(global\)/);
    fireEvent.click(q.getByRole("button", { name: "Create job" }));

    await waitFor(() => expect(posts.filter((p) => p.path === "/jobs")).toHaveLength(1));
    expect(posts[0].body).toMatchObject({ name: "global-job", scope: "" });
    // The consequence is stated, so "All" is never chosen blind.
    expect(q.getByText(/Visible to every agency/)).toBeTruthy();
  });

  it("sends a named scope unchanged", async () => {
    const q = renderComposer("/compose?script=tools%2Fbackup.sh");
    await waitFor(() => expect(q.getAllByText("tools/backup.sh").length).toBeGreaterThan(0));
    fireEvent.change(q.getByPlaceholderText("nightly-backup"), { target: { value: "prod-job" } });
    pickScope(q, /^Prod/);
    fireEvent.click(q.getByRole("button", { name: "Create job" }));

    await waitFor(() => expect(posts.filter((p) => p.path === "/jobs")).toHaveLength(1));
    expect(posts[0].body).toMatchObject({ name: "prod-job", scope: "Prod" });
  });

  it("warns when the chosen scope maps to no agency, without blocking (AF-Q3)", async () => {
    const q = renderComposer("/compose?script=tools%2Fbackup.sh");
    await waitFor(() => expect(q.getAllByText("tools/backup.sh").length).toBeGreaterThan(0));
    fireEvent.change(q.getByPlaceholderText("nightly-backup"), { target: { value: "orphan-job" } });
    pickScope(q, /^Orphan/);

    await waitFor(() => expect(q.getByText(/not mapped to any agency/)).toBeTruthy());
    // Advisory only — the save still goes through.
    fireEvent.click(q.getByRole("button", { name: "Create job" }));
    await waitFor(() => expect(posts.filter((p) => p.path === "/jobs")).toHaveLength(1));
    expect(posts[0].body).toMatchObject({ scope: "Orphan" });
  });

  it("prefills All when editing a job that is already global", async () => {
    const q = renderComposer("/compose?id=8");
    // The picker states the existing choice rather than sitting empty: the value
    // was decided (a legacy default counts), so re-asking would be noise.
    await waitFor(() => expect(q.getAllByDisplayValue("All agencies (global)").length).toBeGreaterThan(0));
  });
});

// AF-2 — a DEPARTMENTAL composer (compose granted on an agency, not everywhere)
// may not author an All-scoped job: a scheduled fire of an unbound job runs
// scope-unchecked, so All is reserved for an unrestricted grant (RB-30). The
// server refuses it; the composer's job is not to offer it in the first place.
describe("JobComposer — All is withheld without composeUnbound (AF-2)", () => {
  it("offers named scopes only, and says so", async () => {
    const { fetchCapabilities } = await import("../api/client");
    vi.mocked(fetchCapabilities).mockResolvedValueOnce({
      vault: false,
      apprise: false,
      compose: true,
      composeUnbound: false, // departmental: agency-bound compose
      manageRoles: false,
      configureApp: false,
      manageEnvVars: true,
      publishSchedule: false,
      triggerJobs: false,
      killJobs: false,
      unrestricted: false,
    });
    const q = renderComposer("/compose?script=tools%2Fbackup.sh");
    await waitFor(() => expect(q.getAllByText("tools/backup.sh").length).toBeGreaterThan(0));

    fireEvent.click(q.getByRole("combobox", { name: "Scope" }));
    // The named scopes are still offered; only the global option is absent.
    await waitFor(() => expect(q.getByRole("option", { name: /^Prod/ })).toBeTruthy());
    expect(q.queryByRole("option", { name: /All agencies \(global\)/ })).toBeNull();
    expect(q.getByText(/Pick one of the scopes your role is granted/)).toBeTruthy();
  });
});

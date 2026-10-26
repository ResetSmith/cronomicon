// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

// AF-1, as it stands since 2.5.3 (CA7) — every job names one of its agency's
// scopes. What is pinned: an unanswered scope blocks the create with a sentence
// (not a server round-trip), there is no "No scope" option even for an
// unrestricted composer, a named scope still works, and a job with no scope
// from before is shown as such and saved only with a scope.

const SCOPES = [
  { id: "sc1", scope: "Prod", hosts: ["db-01"], agencies: ["FIN"] },
  // Deliberately unmapped: drives the AF-Q3 advisory warning.
  { id: "sc2", scope: "Orphan", hosts: [], agencies: [] },
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
      // An unrestricted composer: until 2.5.3 it was offered "No scope".
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

  it("offers no 'No scope' option, even to an unrestricted composer", async () => {
    const q = renderComposer("/compose?script=tools%2Fbackup.sh");
    await waitFor(() => expect(q.getAllByText("tools/backup.sh").length).toBeGreaterThan(0));
    fireEvent.click(q.getByRole("combobox", { name: "Scope" }));
    expect(q.queryByRole("option", { name: /^No scope/ })).toBeNull();
    expect(q.getByRole("option", { name: /^Prod/ })).toBeTruthy();
    // The hint names the shapes a scopeless job was used for.
    expect(q.getAllByText(/a scope whose one host is the runner/).length).toBeGreaterThan(0);
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

    await waitFor(() => expect(q.getByText(/is in no agency, so only global administrators will\s+see this job/)).toBeTruthy());
    // Advisory only — the save still goes through.
    fireEvent.click(q.getByRole("button", { name: "Create job" }));
    await waitFor(() => expect(posts.filter((p) => p.path === "/jobs")).toHaveLength(1));
    expect(posts[0].body).toMatchObject({ scope: "Orphan" });
  });

  it("shows a job with no scope from before as such, and saves it only with a scope", async () => {
    const q = renderComposer("/compose?id=8");
    await waitFor(() => expect(q.getByText(/This job has no scope \(it was made before 2\.5\.3\)/)).toBeTruthy());
    fireEvent.click(q.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(q.getByText(/Pick a scope/)).toBeTruthy());
    const { api } = await import("../api/client");
    expect(api.PUT).not.toHaveBeenCalled();
  });
});

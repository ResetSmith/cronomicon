// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

// The composer's query-param entry points for creating a job from something that
// already exists: ?cloneFrom=<id> (Jobs row "Clone" — prefill everything, stay in
// create mode) and ?script=<name> (Scripts detail "Create Job" — preseed just the
// script). What is worth pinning: a clone POSTs a NEW job (never PUTs the
// original), the name is editable and pre-suffixed (name is the identity, it must
// differ), and the carried-over SSH-key bindings land on the NEW job's id.

const SOURCE_JOB = {
  id: 7,
  name: "nightly-backup",
  source: "cronomicon",
  scriptRef: "tools/backup.sh",
  scope: "Prod",
  host: "db-01",
  env: { RETENTION: "30" },
  prompts: [],
  schedules: [],
  enabled: true,
  description: "nightly dump",
  tags: ["db"],
};

const posts: { path: string; body: Record<string, unknown> }[] = [];
const puts: { path: string; jobId?: unknown; body?: unknown }[] = [];

vi.mock("../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/client")>();
  return {
    ...actual,
    api: {
      GET: vi.fn(async (path: string, opts?: { params?: { path?: { jobId?: number; name?: string } } }) => {
        if (path === "/jobs/{jobId}") {
          if (opts?.params?.path?.jobId === 7) return { data: SOURCE_JOB };
          // id 5 is the git-sourced control for the clone guard.
          if (opts?.params?.path?.jobId === 5) return { data: { ...SOURCE_JOB, id: 5, source: "git" } };
          return { data: undefined, error: {} };
        }
        if (path === "/job-reference-bindings/{jobId}") {
          // The source job carries one key binding; the freshly created job (99)
          // has none yet — putBindings re-reads it before the merge-write.
          return opts?.params?.path?.jobId === 7
            ? { data: { bindings: [{ kind: "key", name: "deploy-key" }] } }
            : { data: { bindings: [] } };
        }
        if (path === "/scripts/{name}") {
          return { data: { name: "tools/backup.sh", runType: "bash", variables: [], prompts: [] } };
        }
        return { data: [] };
      }),
      POST: vi.fn(async (path: string, opts: { body: Record<string, unknown> }) => {
        posts.push({ path, body: opts.body });
        return { data: { id: 99 }, response: { ok: true, status: 200 } };
      }),
      PUT: vi.fn(async (path: string, opts?: { params?: { path?: { jobId?: number } }; body?: unknown }) => {
        puts.push({ path, jobId: opts?.params?.path?.jobId, body: opts?.body });
        return { data: {}, response: { ok: true, status: 200 } };
      }),
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
  puts.length = 0;
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

describe("JobComposer — ?cloneFrom= (clone into create mode)", () => {
  it("prefills from the source job with an editable, suffixed name and stays in create mode", async () => {
    const q = renderComposer("/compose?cloneFrom=7");
    const nameInput = await waitFor(() => {
      const el = q.getByDisplayValue("nightly-backup-copy") as HTMLInputElement;
      expect(el).toBeTruthy();
      return el;
    });
    // Create mode, not edit: the name is the one field a clone must change, so
    // it stays editable (edit mode disables it as the identity).
    expect(nameInput.disabled).toBe(false);
    expect(q.getByRole("button", { name: "Create job" })).toBeTruthy();
    // No edit-mode affordances against the ORIGINAL job.
    expect(q.queryByRole("button", { name: "Delete job" })).toBeNull();
    expect(q.queryByRole("button", { name: "Save" })).toBeNull();
  });

  it("POSTs a new job with the copied settings and never PUTs the original", async () => {
    const q = renderComposer("/compose?cloneFrom=7");
    await waitFor(() => expect(q.getByDisplayValue("nightly-backup-copy")).toBeTruthy());

    fireEvent.click(q.getByRole("button", { name: "Create job" }));

    const jobPosts = () => posts.filter((p) => p.path === "/jobs");
    await waitFor(() => expect(jobPosts()).toHaveLength(1));
    expect(jobPosts()[0].body).toMatchObject({
      name: "nightly-backup-copy",
      scriptRef: "tools/backup.sh",
      scope: "Prod",
      targetHost: "db-01",
      env: { RETENTION: "30" },
      description: "nightly dump",
      tags: ["db"],
    });
    // The original row must be untouched.
    expect(puts.filter((p) => p.path === "/jobs/{jobId}")).toHaveLength(0);
    await waitFor(() => expect(q.getByText(/created \(cronomicon-source\)/)).toBeTruthy());
  });

  it("carries the source job's SSH-key bindings onto the NEW job's id", async () => {
    const q = renderComposer("/compose?cloneFrom=7");
    await waitFor(() => expect(q.getByDisplayValue("nightly-backup-copy")).toBeTruthy());

    fireEvent.click(q.getByRole("button", { name: "Create job" }));

    await waitFor(() => {
      const bindingPuts = puts.filter((p) => p.path === "/job-reference-bindings/{jobId}");
      expect(bindingPuts).toHaveLength(1);
      expect(bindingPuts[0].jobId).toBe(99);
      expect(bindingPuts[0].body).toMatchObject({ bindings: [{ kind: "key", name: "deploy-key" }] });
    });
  });

  it("refuses to clone a git-sourced job (read-only notice, no form)", async () => {
    const q = renderComposer("/compose?cloneFrom=5");
    await waitFor(() => expect(q.getByText(/Git-authored/)).toBeTruthy());
    expect(q.queryByRole("button", { name: "Create job" })).toBeNull();
  });
});

describe("JobComposer — ?script= (Scripts → Create Job hand-off)", () => {
  it("preseeds the script ref so the created job binds the selected script", async () => {
    const q = renderComposer("/compose?script=tools%2Fbackup.sh");
    // The picker resolves the preset by name; the effective-binding recap shows it.
    await waitFor(() => expect(q.getAllByText("tools/backup.sh").length).toBeGreaterThan(0));

    fireEvent.change(q.getByPlaceholderText("nightly-backup"), { target: { value: "from-script" } });
    // AF-1 — the scope is no longer defaulted, so a create must state one. This
    // hand-off starts from a script and carries no scope, so the author picks:
    // here, the explicit global.
    fireEvent.click(q.getByRole("combobox", { name: "Scope" }));
    fireEvent.click(q.getByRole("option", { name: /All agencies \(global\)/ }));
    fireEvent.click(q.getByRole("button", { name: "Create job" }));

    await waitFor(() => expect(posts.filter((p) => p.path === "/jobs")).toHaveLength(1));
    // scope "" is the deliberate All, and it must reach the wire: an absent field
    // is what the server 422s.
    expect(posts.find((p) => p.path === "/jobs")!.body).toMatchObject({
      name: "from-script",
      scriptRef: "tools/backup.sh",
      scope: "",
    });
  });
});

// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

// JP-4a — the composer now authors a job's SECRET/VARIABLE bindings (they used to
// be editable only from the Jobs expanded view, which is read-only as of JP-4b).
// What is worth pinning: edit mode prefills them (they used to be dropped on load
// and preserved blind at save), a declared reference reaches the bindings PUT in
// ONE call alongside the keys, that call is skipped when nothing changed (so a
// plain save never needs ManageEnvVars), and the picker is withheld without the
// permission.

const EDIT_JOB = {
  id: 7,
  name: "nightly-backup",
  source: "cronomicon",
  scriptRef: "tools/backup.sh",
  scope: "Prod",
  prompts: [],
  schedules: [],
  enabled: true,
};

// The job's saved binding set: one key + one secret. The secret is the half the
// composer used to discard on load.
const SAVED_BINDINGS = [
  { kind: "key", name: "deploy-key" },
  { kind: "secret", name: "DB_PASSWORD", reference: "CRONOMICON_SECRET_DB_PASSWORD" },
];

let manageEnvVars = true;
const puts: { path: string; body?: unknown }[] = [];

vi.mock("../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/client")>();
  return {
    ...actual,
    api: {
      GET: vi.fn(async (path: string, opts?: { params?: { path?: { jobId?: number; name?: string } } }) => {
        if (path === "/jobs/{jobId}") {
          return opts?.params?.path?.jobId === 7 ? { data: EDIT_JOB } : { data: undefined, error: {} };
        }
        if (path === "/job-reference-bindings/{jobId}") return { data: { bindings: SAVED_BINDINGS } };
        if (path === "/scripts/{name}") {
          return { data: { name: "tools/backup.sh", runType: "bash", variables: [], prompts: [] } };
        }
        if (path === "/env-secrets") return { data: [{ id: "s1", key: "DB_PASSWORD" }, { id: "s2", key: "API_TOKEN" }] };
        if (path === "/env-vars") return { data: [{ id: "v1", key: "REGION" }] };
        return { data: [] };
      }),
      POST: vi.fn(async () => ({ data: { id: 99, results: [] }, response: { ok: true, status: 200 } })),
      PUT: vi.fn(async (path: string, opts?: { body?: unknown }) => {
        puts.push({ path, body: opts?.body });
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
      manageEnvVars,
      publishSchedule: false,
    })),
  };
});

import { JobComposer } from "./JobComposer";

beforeEach(() => {
  manageEnvVars = true;
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

const bindingsPut = () => puts.find((p) => p.path === "/job-reference-bindings/{jobId}");
const sentBindings = () =>
  ((bindingsPut()?.body as { bindings?: { kind: string; name: string; as?: string }[] } | undefined)?.bindings ?? []);

describe("JobComposer — secrets & variables (JP-4a)", () => {
  it("prefills the job's declared secret/variable bindings in edit mode", async () => {
    const q = renderComposer("/compose?id=7");
    await waitFor(() => expect(q.getByText("CRONOMICON_SECRET_DB_PASSWORD")).toBeTruthy());
    // The key binding still prefills into its own section, unchanged.
    expect(q.getByText("CRONOMICON_KEY_deploy-key")).toBeTruthy();
  });

  it("sends a newly declared reference with the keys in ONE bindings write", async () => {
    const q = renderComposer("/compose?id=7");
    await waitFor(() => expect(q.getByText("CRONOMICON_SECRET_DB_PASSWORD")).toBeTruthy());

    fireEvent.change(q.getByLabelText("Reference name"), { target: { value: "API_TOKEN" } });
    fireEvent.click(q.getAllByRole("button", { name: "Add" })[0]);
    await waitFor(() => expect(q.getByText("CRONOMICON_SECRET_API_TOKEN")).toBeTruthy());

    fireEvent.click(q.getByRole("button", { name: /Save changes|Save|Update/ }));
    await waitFor(() => expect(bindingsPut()).toBeTruthy());

    // One write, carrying every kind the composer owns — not one PUT per kind.
    expect(puts.filter((p) => p.path === "/job-reference-bindings/{jobId}")).toHaveLength(1);
    const names = sentBindings().map((b) => `${b.kind}:${b.name}`);
    expect(names).toContain("secret:API_TOKEN");
    expect(names).toContain("secret:DB_PASSWORD");
    expect(names).toContain("key:deploy-key");
  });

  it("carries the inject-as alias through to the write", async () => {
    const q = renderComposer("/compose?id=7");
    await waitFor(() => expect(q.getByText("CRONOMICON_SECRET_DB_PASSWORD")).toBeTruthy());

    fireEvent.change(q.getByLabelText("Reference name"), { target: { value: "API_TOKEN" } });
    fireEvent.change(q.getByLabelText(/^Alias/), { target: { value: "TOKEN" } });
    fireEvent.click(q.getAllByRole("button", { name: "Add" })[0]);
    fireEvent.click(q.getByRole("button", { name: /Save changes|Save|Update/ }));
    await waitFor(() => expect(bindingsPut()).toBeTruthy());

    expect(sentBindings().find((b) => b.name === "API_TOKEN")?.as).toBe("TOKEN");
  });

  it("writes no bindings at all when they were not touched", async () => {
    const q = renderComposer("/compose?id=7");
    await waitFor(() => expect(q.getByText("CRONOMICON_SECRET_DB_PASSWORD")).toBeTruthy());
    fireEvent.click(q.getByRole("button", { name: /Save changes|Save|Update/ }));
    // The job PUT lands; the bindings PUT must not (an untouched save should not
    // require ManageEnvVars).
    await waitFor(() => expect(puts.some((p) => p.path === "/jobs/{jobId}")).toBe(true));
    expect(bindingsPut()).toBeUndefined();
  });

  it("withholds the picker's controls without the Manage Env Vars permission", async () => {
    manageEnvVars = false;
    const q = renderComposer("/compose?id=7");
    await waitFor(() => expect(q.getByText("CRONOMICON_SECRET_DB_PASSWORD")).toBeTruthy());
    // Rows still render (they are the job's declared set) but nothing can edit them.
    expect(q.queryByLabelText("Reference name")).toBeNull();
    expect(q.queryByRole("button", { name: /^Remove reference/ })).toBeNull();
  });
});

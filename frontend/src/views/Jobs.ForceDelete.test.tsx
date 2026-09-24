// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

// RX-24 — the delete refusal an operator can actually answer.
//
// DELETE /jobs/{id} 409s for two unrelated reasons: the row is git-source, which
// nothing here can clear, or reactions watch it, which ?force=true clears. The
// list view used to branch on the STATUS alone and report the git-source reason
// for both — a sentence that cannot be true for the second case, because the git
// check has already passed by the time the reactions guard fires. So an operator
// was told a job they authored in-app was Git-authored, and the reactions that
// actually blocked the delete were never named.
//
// What is pinned here is the pair: the refusal's own error code drives a second,
// forced request; anything else keeps the old message and does NOT offer a force
// the server would refuse anyway.

const deletes: { path: string; jobId: unknown; query: unknown }[] = [];

const JOBS = {
  items: [{ id: 7, name: "upstream-job", type: "bash", scope: "Prod", source: "amadeus", status: "idle", schedule: null }],
  totalItems: 1,
  totalPages: 1,
  page: 1,
  pageSize: 50,
};

const laterTick = () => new Promise((resolve) => setTimeout(resolve, 0));

// Each test installs the DELETE outcome it needs before rendering.
let deleteImpl: () => { data?: unknown; error?: unknown; response: { ok: boolean; status: number } };

vi.mock("../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/client")>();
  return {
    ...actual,
    api: {
      GET: vi.fn(async (path: string) => {
        if (path === "/jobs") {
          await laterTick();
          return { data: JOBS };
        }
        if (path === "/me") return { data: { email: "op@ex.com", roles: ["admin"] } };
        if (path === "/jobs/{jobId}") return { data: undefined };
        if (path === "/runs") return { data: { items: [] } };
        if (path === "/job-reference-bindings/{jobId}") return { data: { bindings: [] } };
        return { data: [] };
      }),
      POST: vi.fn(async () => ({ data: {} })),
      PUT: vi.fn(async () => ({ data: {} })),
      DELETE: vi.fn(async (path: string, opts: { params?: { path?: { jobId?: number }; query?: unknown } }) => {
        deletes.push({ path, jobId: opts?.params?.path?.jobId, query: opts?.params?.query });
        return deleteImpl();
      }),
    } as unknown as typeof actual.api,
    fetchCapabilities: vi.fn(async () => ({
      vault: false,
      apprise: false,
      compose: true,
      manageRoles: false,
      configureApp: false,
      manageEnvVars: false,
      publishSchedule: false,
      triggerJobs: true,
      killJobs: true,
    })),
  };
});

import { Jobs } from "./Jobs";
import { AuthProvider } from "../auth";

const WATCHING =
  'job is watched by 1 reaction(s): job:downstream/nightly — pass ?force=true to delete and leave those reactions dangling';

beforeEach(() => {
  deletes.length = 0;
  deleteImpl = () => ({ data: {}, response: { ok: true, status: 204 } });
});
afterEach(cleanup);

const renderJobs = async () => {
  const { container } = render(
    <MemoryRouter>
      <AuthProvider>
        <Jobs />
      </AuthProvider>
    </MemoryRouter>,
  );
  const q = within(container);
  await waitFor(() => expect(q.getByText("upstream-job")).toBeTruthy());
  return q;
};

const openDeleteDialog = async (q: ReturnType<typeof within>) => {
  fireEvent.click(q.getByText("upstream-job").closest("tr")!);
  const del = await waitFor(() => q.getByRole("button", { name: "Delete" }));
  fireEvent.click(del);
  await waitFor(() => expect(q.getByRole("button", { name: "Delete Job" })).toBeTruthy());
};

describe("Jobs — deleting a job reactions watch (RX-24)", () => {
  it("offers a forced delete, naming the reactions the server named", async () => {
    // First call refuses with the reactions code; the forced retry succeeds.
    let call = 0;
    deleteImpl = () => {
      call += 1;
      return call === 1
        ? { error: { code: "reactions_watching", message: WATCHING }, response: { ok: false, status: 409 } }
        : { data: {}, response: { ok: true, status: 204 } };
    };

    const q = await renderJobs();
    await openDeleteDialog(q);
    fireEvent.click(q.getByRole("button", { name: "Delete Job" }));

    // The dialog stays open and shows the server's own text — the operator has
    // to decide about THESE reactions, so paraphrasing the list is not enough.
    const anyway = await waitFor(() => q.getByRole("button", { name: "Delete anyway" }));
    expect(q.getByText(new RegExp("job:downstream/nightly"))).toBeTruthy();
    // And it says what forcing costs, rather than only that it is possible.
    expect(q.getByText(/missing/)).toBeTruthy();

    fireEvent.click(anyway);

    await waitFor(() => expect(deletes).toHaveLength(2));
    expect(deletes[0].query).toEqual({}); // the first attempt is never forced
    expect(deletes[1].query).toEqual({ force: true });
    expect(deletes[1].jobId).toBe(7);
  });

  it("does not offer a force for the git-source 409, which force cannot clear", async () => {
    deleteImpl = () => ({
      error: { code: "conflict", message: "only amadeus-source jobs are deletable in-app" },
      response: { ok: false, status: 409 },
    });

    const q = await renderJobs();
    await openDeleteDialog(q);
    fireEvent.click(q.getByRole("button", { name: "Delete Job" }));

    await waitFor(() => expect(q.getByText(/Only amadeus-source jobs can be deleted in-app/)).toBeTruthy());
    expect(q.queryByRole("button", { name: "Delete anyway" })).toBeNull();
    expect(deletes).toHaveLength(1); // no retry against a refusal that cannot lift
  });

  it("deletes in one request when nothing watches the job", async () => {
    const q = await renderJobs();
    await openDeleteDialog(q);
    fireEvent.click(q.getByRole("button", { name: "Delete Job" }));

    await waitFor(() => expect(deletes).toHaveLength(1));
    // No `force` on the ordinary path: the guard must stay armed for everyone
    // else, and a client that always forced would silently disarm it.
    expect(deletes[0].query).toEqual({});
    await waitFor(() => expect(q.queryByRole("button", { name: "Delete Job" })).toBeNull());
  });
});

// The refusal is shown to a human standing in front of a button that already IS
// the override, so the server's closing "pass ?force=true …" is dropped — while
// the half only the server can supply, the list of reactions, is kept verbatim.
describe("the refusal text shown to an operator", () => {
  it("keeps the named reactions and drops the API instruction", async () => {
    deleteImpl = () => ({
      error: { code: "reactions_watching", message: WATCHING },
      response: { ok: false, status: 409 },
    });
    const q = await renderJobs();
    await openDeleteDialog(q);
    fireEvent.click(q.getByRole("button", { name: "Delete Job" }));

    await waitFor(() => expect(q.getByRole("button", { name: "Delete anyway" })).toBeTruthy());
    expect(q.getByText(/job:downstream\/nightly/)).toBeTruthy();
    expect(q.queryByText(/\?force=true/)).toBeNull();
  });
});

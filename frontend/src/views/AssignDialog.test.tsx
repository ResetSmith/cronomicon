// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";

// 2.5.0 C7.3 (C10, IN-12; question 98): the dialog that gives a definition to
// an agency. The server decides; the dialog picks the agency and a scope per
// origin scope, sends them, and shows the answer or the refusal whole.

let postAnswer: { data?: unknown; error?: unknown } = {};
const posts: { path: string; params: unknown; body: unknown }[] = [];
const getHeaders: Record<string, unknown> = {};

vi.mock("../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/client")>();
  return {
    ...actual,
    api: {
      GET: vi.fn(async (path: string, opts?: { headers?: unknown }) => {
        getHeaders[path] = opts?.headers;
        if (path === "/agencies")
          return { data: [{ id: "global", name: "Global" }, { id: "ag-fin", name: "Finance" }, { id: "ag-tax", name: "Tax" }] };
        if (path === "/scopes")
          return {
            data: [
              { id: "s1", name: "fin-hosts", agencies: [{ id: "ag-fin", name: "Finance" }], tags: [] },
              { id: "s2", name: "shared", agencies: [{ id: "ag-fin", name: "Finance" }, { id: "ag-tax", name: "Tax" }], tags: [] },
              { id: "s3", name: "tax-hosts", agencies: [{ id: "ag-tax", name: "Tax" }], tags: [] },
            ],
          };
        return { data: [] };
      }),
      POST: vi.fn(async (path: string, opts: { params: unknown; body: unknown }) => {
        posts.push({ path, params: opts.params, body: opts.body });
        return postAnswer;
      }),
    } as unknown as typeof actual.api,
  };
});

import { AssignDialog, workflowOriginScopes } from "./AssignDialog";

beforeEach(() => {
  posts.length = 0;
  postAnswer = {};
});
afterEach(cleanup);

const open = () =>
  render(<AssignDialog kind="job" uid="u-nightly" name="nightly" originAgency="global" originScopes={[""]} onClose={() => {}} />);

describe("AssignDialog", () => {
  it("offers every agency but the origin, and only the scopes the target owns alone", async () => {
    open();
    const agency = (await screen.findByLabelText("Agency")) as HTMLSelectElement;
    const options = [...agency.options].map((o) => o.textContent);
    expect(options).toEqual(["Choose an agency…", "Finance", "Tax"]);
    expect(getHeaders["/scopes"]).toEqual({ "X-Cronomicon-Agency": "none" });
    fireEvent.change(agency, { target: { value: "ag-fin" } });
    // Finance owns one scope alone ("shared" is in two agencies): it is picked.
    const scope = (await screen.findByLabelText("Scope for jobs with no scope")) as HTMLSelectElement;
    expect([...scope.options].map((o) => o.value)).toEqual(["", "fin-hosts"]);
    await waitFor(() => expect(scope.value).toBe("fin-hosts"));
  });

  it("sends the agency, the scope for each origin scope, and shows the answer whole", async () => {
    postAnswer = {
      data: {
        assignmentId: "a1", commitSha: "0123456789abcdef", branch: "main", originCommit: "fedcba9876543210", syncStarted: true,
        files: [
          { path: "jobs/nightly.yaml", role: "definition", action: "added" },
          { path: "scripts/nightly.sh", role: "script", action: "usesOwn", differs: true },
        ],
        bindings: [{ kind: "secret", name: "DB_PASS" }],
        lacks: [{ kind: "secret", name: "DB_PASS" }],
      },
    };
    open();
    fireEvent.change(await screen.findByLabelText("Agency"), { target: { value: "ag-fin" } });
    await waitFor(() => expect((screen.getByLabelText("Scope for jobs with no scope") as HTMLSelectElement).value).toBe("fin-hosts"));
    fireEvent.click(screen.getByRole("button", { name: "Give to the agency" }));
    await waitFor(() => expect(posts).toHaveLength(1));
    expect(posts[0].path).toBe("/agencies/{agencyId}/assign");
    expect((posts[0].params as { path: { agencyId: string } }).path.agencyId).toBe("ag-fin");
    expect(posts[0].body).toEqual({ kind: "job", uid: "u-nightly", scope: "fin-hosts", scopes: { "": "fin-hosts" } });
    await screen.findByText(/Finance's sync has started/);
    expect(screen.getByText("scripts/nightly.sh")).toBeTruthy();
    // "differs" is a comparison of two files, not "changed by the agency".
    expect(screen.getByText(/differs from the origin's file/).getAttribute("title")).toMatch(/not a record of who changed what/);
    expect(screen.getByText(/Bindings to make again in Finance/)).toBeTruthy();
    expect(screen.getByText(/What Finance does not have/)).toBeTruthy();
  });

  it("shows a refusal with what it names", async () => {
    postAnswer = { error: { code: "calendar_missing", message: "Finance has no calendar named holidays", calendars: ["holidays"] } };
    open();
    fireEvent.change(await screen.findByLabelText("Agency"), { target: { value: "ag-fin" } });
    await waitFor(() => expect((screen.getByLabelText("Scope for jobs with no scope") as HTMLSelectElement).value).toBe("fin-hosts"));
    fireEvent.click(screen.getByRole("button", { name: "Give to the agency" }));
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toMatch(/no calendar named holidays/);
    expect(alert.textContent).toMatch(/Calendars: holidays/);
  });

  it("works out a workflow's origin scopes from its jobs and sub-workflows", () => {
    const wf = { name: "release", repoId: "global", source: "git" as const, steps: [
      { type: "job" as const, name: "build" },
      { type: "parallel" as const, jobs: [{ type: "job" as const, name: "nightly" }, { type: "workflow" as const, name: "tell", workflow: "notify" }] },
    ] };
    const notify = { name: "notify", repoId: "global", source: "git" as const, steps: [
      { type: "branch" as const, pass: { steps: [{ type: "job" as const, name: "ping" }] } },
    ] };
    const jobs = [
      { name: "build", repoId: "global", source: "git" as const, scope: "" },
      { name: "nightly", repoId: "global", source: "git" as const, scope: "web" },
      { name: "ping", repoId: "global", source: "git" as const, scope: "web" },
      { name: "ping", repoId: "repo-fin", source: "git" as const, scope: "elsewhere" },
    ];
    expect(workflowOriginScopes(wf as never, [wf, notify] as never, jobs as never)).toEqual(["", "web"]);
  });
});

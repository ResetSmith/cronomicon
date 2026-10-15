// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";

// Settings → Repositories (2.4.0 R8; question 47). Each agency's repository,
// Global's included: an agency's administrator sees their own, and connects
// one when their agency has none; the Test says whether the token may push,
// and what that cannot see (a protected branch); a save the server refuses
// says why. A global administrator also gets the allowed hosts.

let repos: unknown[] = [];
let testAnswer: unknown = {};
let putAnswer: { data?: unknown; error?: unknown; response: { status: number } } = { data: {}, response: { status: 200 } };
const puts: { path: string; body: unknown }[] = [];

const ACCESS = {
  grants: [{ role: "admin", allScopes: false, agencyId: "ag-fin", agencyName: "Finance", scopes: [], permissions: ["configureApp"], groups: [], origin: "group" }],
};

vi.mock("../../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/client")>();
  return {
    ...actual,
    api: {
      GET: vi.fn(async (path: string) => {
        if (path === "/git-repos") return { data: { items: repos } };
        if (path === "/agencies") return { data: [{ id: "global", name: "Global" }, { id: "ag-fin", name: "Finance" }] };
        if (path === "/git-repos/{repoId}/problems") return { data: { repoId: "x", items: [] } };
        if (path === "/settings/general") return { data: { gitAllowedHosts: ["gitlab.example.org"] } };
        return { data: [] };
      }),
      PUT: vi.fn(async (path: string, opts: { body: unknown }) => {
        puts.push({ path, body: opts.body });
        return putAnswer;
      }),
      POST: vi.fn(async (path: string) => {
        if (path === "/agencies/{agencyId}/git-repo/test") return { data: testAnswer };
        return { data: {} };
      }),
    } as unknown as typeof actual.api,
  };
});
vi.mock("../../api/access", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/access")>();
  return { ...actual, fetchMyAccess: vi.fn(async () => ACCESS) };
});

import { RepositoriesSection } from "./Repositories";

beforeEach(() => {
  repos = [];
  puts.length = 0;
  testAnswer = {};
  putAnswer = { data: {}, response: { status: 200 } };
});
afterEach(cleanup);

const finRepo = {
  id: "repo-fin", agencyId: "ag-fin", agencyName: "Finance", repoUrl: "https://gitlab.example.org/fin/defs.git", writeBranch: "main",
  supplies: { jobs: 2, workflows: 1, scripts: 3, schedules: 0, scopes: 1 },
};

describe("Settings → Repositories", () => {
  it("shows an agency administrator their repository, and the Test says whether the token may push and what that cannot see", async () => {
    repos = [finRepo];
    testAnswer = { ok: true, branch: "main", branchFound: true, branches: ["main"], canPush: true, pushNote: "on GitLab only Maintainers may push to the default branch" };
    render(<RepositoriesSection isGlobal={false} />);
    await waitFor(() => expect(screen.getByText("Finance's repository")).toBeTruthy());
    expect(screen.queryByText("Allowed hosts")).toBeNull();
    // Its own webhook URL, and the test.
    fireEvent.click(screen.getByText("Edit"));
    expect((screen.getByDisplayValue(/\/api\/v1\/webhooks\/gitlab\/repo-fin$/) as HTMLInputElement).readOnly).toBe(true);
    fireEvent.click(screen.getByText("Test"));
    await waitFor(() => expect(screen.getByText(/The token may push\./)).toBeTruthy());
    expect(screen.getByText(/only Maintainers may push to the default branch/)).toBeTruthy();
  });

  it("says why a save is refused", async () => {
    repos = [finRepo];
    putAnswer = { error: { code: "token_cannot_push", message: "the token can read the repository but may not push to it" }, response: { status: 422 } };
    render(<RepositoriesSection isGlobal={false} />);
    await waitFor(() => expect(screen.getByText("Edit")).toBeTruthy());
    fireEvent.click(screen.getByText("Edit"));
    fireEvent.click(screen.getByText("Save"));
    await waitFor(() => expect(screen.getByRole("alert").textContent).toMatch(/may not push/));
    // The webhook is sent: a write replaces every field, and one left out is off.
    expect(puts[0].path).toBe("/agencies/{agencyId}/git-repo");
    expect((puts[0].body as { webhookEnabled?: boolean }).webhookEnabled).toBe(true);
  });

  it("offers an agency with no repository its Connect, and a global administrator the allowed hosts", async () => {
    repos = [];
    render(<RepositoriesSection isGlobal={true} />);
    await waitFor(() => expect(screen.getByText("Finance's repository")).toBeTruthy());
    expect(screen.getAllByText(/No repository is connected/).length).toBeGreaterThan(0);
    expect(screen.getAllByText("Connect").length).toBeGreaterThan(0);
    await waitFor(() => expect(screen.getByText("Allowed hosts")).toBeTruthy());
    expect((screen.getByLabelText("Allowed hosts") as HTMLTextAreaElement).value).toBe("gitlab.example.org");
  });
});

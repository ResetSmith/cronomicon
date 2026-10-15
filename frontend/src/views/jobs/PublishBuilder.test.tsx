// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

// Publish to GitLab, per repository (2.4.0 GR-24; R8). A publish names its
// repository (`repoId`): an agency's takes its own publishers', Global's a
// global publisher's only. The picker offers what the caller may publish to,
// the base commit (If-Match) is that repository's last sync, and a push the
// remote refused for a protected branch says so.

let globalPublisher = false;
const posts: { path: string; params: unknown; body: unknown }[] = [];
const historyRepos: (string | undefined)[] = [];
let publishAnswer: unknown = { data: { commitSha: "c0ffee" }, response: { status: 201 } };

vi.mock("../../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/client")>();
  return {
    ...actual,
    fetchCapabilities: vi.fn(async () => ({ publishSchedule: true, publishScheduleGlobal: globalPublisher })),
    api: {
      GET: vi.fn(async (path: string, init?: { params?: { query?: { repo?: string } } }) => {
        if (path === "/jobs") return { data: [] };
        if (path === "/git-repos")
          return {
            data: {
              items: [
                { id: "global", agencyId: "global", agencyName: "Global" },
                { id: "repo-fin", agencyId: "ag-fin", agencyName: "Finance" },
                { id: "repo-tax", agencyId: "ag-tax", agencyName: "Tax" },
              ],
            },
          };
        if (path === "/git/history") {
          const repo = init?.params?.query?.repo;
          historyRepos.push(repo);
          return { data: { items: [{ commit: `sha-${repo}` }] } };
        }
        return { data: [] };
      }),
      POST: vi.fn(async (path: string, init: { params?: unknown; body?: unknown }) => {
        posts.push({ path, params: init.params, body: init.body });
        return publishAnswer;
      }),
    } as unknown as typeof actual.api,
  };
});
vi.mock("../../api/access", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/access")>();
  return {
    ...actual,
    useMyAccess: () => ({
      grants: [{ role: "approver", allScopes: false, agencyId: "ag-fin", agencyName: "Finance", scopes: [], permissions: ["publishSchedule"], groups: [], origin: "group" }],
    }),
  };
});

import { PublishBuilder } from "./PublishBuilder";

beforeEach(() => {
  globalPublisher = false;
  posts.length = 0;
  historyRepos.length = 0;
  publishAnswer = { data: { commitSha: "c0ffee" }, response: { status: 201 } };
});
afterEach(cleanup);

const open = async () => {
  render(
    <MemoryRouter>
      <PublishBuilder />
    </MemoryRouter>,
  );
  return (await screen.findByLabelText("Repository")) as HTMLSelectElement;
};
const publish = async () => {
  fireEvent.change(screen.getByPlaceholderText("e.g. daily-backup"), { target: { value: "nightly" } });
  fireEvent.change(screen.getByPlaceholderText("echo hello"), { target: { value: "echo hi" } });
  const go = screen.getByRole("button", { name: "Publish to GitLab" }) as HTMLButtonElement;
  await waitFor(() => expect(go.disabled).toBe(false));
  fireEvent.click(go);
  const confirm = await waitFor(() => {
    const bs = screen.getAllByRole("button").filter((x) => /Publish|Confirm|Commit/.test(x.textContent ?? "") && !(x as HTMLButtonElement).disabled);
    if (bs.length < 2) throw new Error("no confirm yet");
    return bs[bs.length - 1];
  });
  fireEvent.click(confirm);
  await waitFor(() => expect(posts.length).toBe(1));
};

describe("Publish — per repository (R8)", () => {
  it("offers an agency's publisher their own repository only, bases the publish on it, and names it", async () => {
    const select = await open();
    await waitFor(() => expect(Array.from(select.options).map((o) => o.textContent)).toEqual(["Finance's repository"]));
    await waitFor(() => expect(historyRepos).toContain("repo-fin"));
    await publish();
    expect(posts[0].path).toBe("/schedules/publish");
    expect(posts[0].body).toMatchObject({ filePath: "jobs/nightly.yaml", repoId: "repo-fin", new: true });
    expect((posts[0].params as { header: Record<string, string> }).header["If-Match"]).toBe("sha-repo-fin");
  });

  it("offers a global publisher every repository, Global's first", async () => {
    globalPublisher = true;
    const select = await open();
    await waitFor(() =>
      expect(Array.from(select.options).map((o) => o.textContent)).toEqual(["Global repository", "Finance's repository", "Tax's repository"]),
    );
    fireEvent.change(select, { target: { value: "repo-tax" } });
    await waitFor(() => expect(historyRepos).toContain("repo-tax"));
  });

  it("says a protected branch refused the push", async () => {
    publishAnswer = {
      error: { code: "branch_protected", message: "the remote refused the push: the branch is protected" },
      response: { status: 409 },
    };
    await open();
    await waitFor(() => expect(historyRepos).toContain("repo-fin"));
    await publish();
    expect(await screen.findByText(/The repository refused the push: its branch is protected\./)).toBeTruthy();
  });
});

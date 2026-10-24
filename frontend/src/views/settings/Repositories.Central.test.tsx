// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";

// 2.5.3 CA-15: Central has no repository (CA-6). Working in it, a global
// administrator sees the allowed hosts and, while one is left from before,
// Central's old connection with "Remove the leftover repository" (question
// 203, CA2's repair): no repository card and no Connect card.

vi.mock("../../agency", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../agency")>();
  return {
    ...actual,
    useAgencyOptional: () => ({ agency: "global", agencyName: "Central", onOverview: false, choices: [] }),
  };
});

let leftover: unknown = null;
let deleteError: unknown = null;
const deletes: { path: string; body: unknown; agencyId: string }[] = [];

vi.mock("../../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/client")>();
  return {
    ...actual,
    api: {
      GET: vi.fn(async (path: string) => {
        if (path === "/agencies/{agencyId}/git-repo") {
          return leftover ? { data: leftover } : { error: { code: "not_connected", message: "it runs nothing" } };
        }
        if (path === "/git-repos") return { data: { items: [] } };
        if (path === "/agencies") return { data: [{ id: "global", name: "Central" }, { id: "ag-fin", name: "Finance" }] };
        if (path === "/settings/general") return { data: { gitAllowedHosts: ["gitlab.example.org"] } };
        return { data: [] };
      }),
      DELETE: vi.fn(async (path: string, opts: { body: unknown; params: { path: { agencyId: string } } }) => {
        deletes.push({ path, body: opts.body, agencyId: opts.params.path.agencyId });
        return deleteError ? { error: deleteError } : { data: { removed: true } };
      }),
    } as unknown as typeof actual.api,
  };
});

import { RepositoriesSection } from "./Repositories";

const LEFTOVER = {
  leftover: true,
  id: "global",
  agencyId: "global",
  agencyName: "Central",
  repoUrl: "https://gitlab.example.org/ops/definitions.git",
  writeBranch: "main",
  lastStatus: "ok",
  supplies: { jobs: 4, workflows: 1, scripts: 6, schedules: 2, scopes: 1 },
};

beforeEach(() => {
  leftover = null;
  deleteError = null;
  deletes.length = 0;
});
afterEach(cleanup);

describe("Settings → Repositories, working in Central", () => {
  it("shows the allowed hosts, and no repository or Connect card, when nothing is left", async () => {
    render(<RepositoriesSection isGlobal />);
    expect(await screen.findByText("Allowed hosts")).toBeTruthy();
    await new Promise((r) => setTimeout(r, 20));
    expect(screen.queryByText(/^[\w -]+'s repository$/)).toBeNull();
    expect(screen.queryByRole("button", { name: "Connect" })).toBeNull();
    expect(screen.queryByText(/No repository is connected/)).toBeNull();
  });

  it("shows the leftover connection, and removes it as a repair with a reason", async () => {
    leftover = LEFTOVER;
    render(<RepositoriesSection isGlobal />);
    expect(await screen.findByText(/'s repository, left from before 2\.5\.3/)).toBeTruthy();
    expect(screen.getByText(/no longer syncs and what it supplied runs nowhere/)).toBeTruthy();
    expect(screen.getByText(/supplied\s+4 jobs, 1 workflows, 6 scripts, 2 schedules, 1 scopes/)).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Remove the leftover repository" }));
    const confirm = (await screen.findByRole("button", { name: "Remove" })) as HTMLButtonElement;
    expect(confirm.disabled).toBe(true); // no reason yet
    fireEvent.change(screen.getByLabelText("Reason"), { target: { value: "moved to Finance" } });
    fireEvent.click(confirm);
    await waitFor(() => expect(deletes).toHaveLength(1));
    expect(deletes[0]).toEqual({ path: "/agencies/{agencyId}/git-repo", agencyId: "global", body: { repair: true, reason: "moved to Finance" } });
    expect(await screen.findByText(/Removed\. Connect its URL to the agency whose work it is/)).toBeTruthy();
  });

  it("keeps the dialog open with the server's refusal", async () => {
    leftover = LEFTOVER;
    deleteError = { code: "scopes_busy", message: "a scope it supplied is running a run" };
    render(<RepositoriesSection isGlobal />);
    fireEvent.click(await screen.findByRole("button", { name: "Remove the leftover repository" }));
    fireEvent.change(await screen.findByLabelText("Reason"), { target: { value: "moved" } });
    fireEvent.click(screen.getByRole("button", { name: "Remove" }));
    expect(await screen.findByText(/a scope it supplied is running a run/)).toBeTruthy();
    expect(screen.getByText(/'s repository, left from before 2\.5\.3/)).toBeTruthy();
  });
});

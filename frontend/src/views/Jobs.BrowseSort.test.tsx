// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

// TS-24 — the Jobs catalog opens in BROWSE mode (empty search box), and browse
// mode used to hand FolderBrowser the unsorted rows with a caret-free header.
// Two bugs fell out of that, both visible on a stock Jobs page:
//
//   1. An amadeus-authored job has no sourcePath, so jobDisplayPath fell back to
//      jobName() — the DB id. Every such job entered the folder tree labelled
//      with a UUIDv7, which is time-ordered, so the default view listed jobs in
//      CREATION order while the NAME column rendered metadata.name. It read as
//      no order at all. Workflows and Schedules always fell back to the name.
//   2. The header rendered with sortEnabled={false}, so there was no way to sort
//      by any column without first typing into the search box.
//
// The default sort spec ({ key: "name", dir: "asc" }) was correct the whole
// time — it just never reached the view the user actually lands on.

// Deliberately NOT alpha, and NOT id order: neither a plain pass-through nor an
// id sort can fake a pass here. Ids are the UUIDv7 shape amadeus rows carry.
const JOBS = {
  items: [
    { id: "0198f0a0-0000-7000-8000-000000000001", name: "zulu-job", type: "bash", scope: "Prod", source: "amadeus", status: "idle", schedule: "manual", sourcePath: null },
    { id: "0198f0a0-0000-7000-8000-000000000002", name: "alpha-job", type: "bash", scope: "Prod", source: "amadeus", status: "idle", schedule: "manual", sourcePath: null },
    { id: "0198f0a0-0000-7000-8000-000000000003", name: "mike-job", type: "bash", scope: "Prod", source: "amadeus", status: "idle", schedule: "manual", sourcePath: null },
  ],
  totalItems: 3,
  totalPages: 1,
  page: 1,
  pageSize: 50,
};

const laterTick = () => new Promise((resolve) => setTimeout(resolve, 0));

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
        if (path === "/me") return { data: { email: "op@ex.com", roles: ["operator"] } };
        if (path === "/jobs/{jobId}") return { data: undefined };
        if (path === "/runs") return { data: { items: [] } };
        if (path === "/job-reference-bindings/{jobId}") return { data: { bindings: [] } };
        return { data: [] };
      }),
      POST: vi.fn(async () => ({ data: {} })),
      PUT: vi.fn(async () => ({ data: {} })),
      DELETE: vi.fn(async () => ({ data: {}, response: { ok: true, status: 200 } })),
    } as unknown as typeof actual.api,
    fetchCapabilities: vi.fn(async () => ({
      vault: false,
      apprise: false,
      compose: false,
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

// jsdom shares one localStorage per FILE — a stored `sort:jobs` from an earlier
// test would override the Name default and silently pass the wrong thing.
beforeEach(() => localStorage.clear());
afterEach(cleanup);

// No search input: this is the view as it loads.
const renderBrowse = async () => {
  const { container } = render(
    <MemoryRouter>
      <AuthProvider>
        <Jobs />
      </AuthProvider>
    </MemoryRouter>,
  );
  const q = within(container);
  await waitFor(() => expect(q.getByText("alpha-job")).toBeTruthy());
  return { q, container };
};

const nameColumn = (container: HTMLElement) =>
  Array.from(container.querySelectorAll("tbody tr"))
    .map((tr) => tr.querySelector("td")?.textContent?.trim())
    .filter((t): t is string => !!t && t.endsWith("-job"));

describe("Jobs — browse mode sorts (TS-24)", () => {
  it("lists jobs by Name ascending on load, without touching the search box", async () => {
    const { container } = await renderBrowse();
    expect(nameColumn(container)).toEqual(["alpha-job", "mike-job", "zulu-job"]);
  });

  it("renders a live sort affordance on the Name header in browse mode", async () => {
    const { q } = await renderBrowse();
    // sortEnabled={false} rendered an inert <th>: no aria-sort, no click
    // handler, no title. The sortable header is the same <th> wired up.
    const name = q.getByRole("columnheader", { name: /Name/ });
    expect(name.getAttribute("aria-sort")).toBe("ascending");
    expect(name.getAttribute("title")).toBe("Sort by Name");
  });

  it("flips to descending when the Name header is clicked", async () => {
    const { q, container } = await renderBrowse();
    fireEvent.click(q.getByRole("columnheader", { name: /Name/ }));
    await waitFor(() => expect(nameColumn(container)).toEqual(["zulu-job", "mike-job", "alpha-job"]));
  });

  it("sorts by a non-Name column too", async () => {
    const { q, container } = await renderBrowse();
    fireEvent.click(q.getByRole("columnheader", { name: /Scope/ }));
    // All three share a scope, so this pins that the click is WIRED (the header
    // takes the sort) rather than pinning an ordering the fixture can't vary.
    await waitFor(() =>
      expect(q.getByRole("columnheader", { name: /Scope/ }).getAttribute("aria-sort")).toBe("ascending"),
    );
    expect(nameColumn(container)).toHaveLength(3);
  });
});

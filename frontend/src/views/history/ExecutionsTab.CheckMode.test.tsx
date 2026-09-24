// @vitest-environment jsdom
// RP — check-mode visibility in History. A --check run records success/failure
// like any other run, so before this surface a dry run was indistinguishable
// from the deployment it rehearsed: the badge on the row and the callout in the
// expanded detail are the ONLY post-run record outside the log itself.
import { afterEach, describe, expect, it, vi } from "vitest";
import { MemoryRouter } from "react-router-dom";
import { cleanup, fireEvent, render, waitFor, within } from "@testing-library/react";

// One run list + one detail, both configurable per test.
let listRuns: unknown[] = [];
let runDetail: unknown = undefined;

vi.mock("../../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/client")>();
  return {
    ...actual,
    api: {
      GET: vi.fn(async (path: string) => {
        if (path === "/runs")
          return { data: { items: listRuns, page: 1, pageSize: 25, totalItems: listRuns.length, totalPages: 1 } };
        if (path === "/runs/{traceId}") return { data: runDetail };
        // The log endpoint is parseAs:"text" — data is a plain string.
        if (path === "/runs/{traceId}/log") return { data: "" };
        if (path === "/run-references/{traceId}") return { data: { references: [] } };
        return { data: {} };
      }),
      POST: vi.fn(async () => ({ data: {} })),
    } as unknown as typeof actual.api,
  };
});

import { ExecutionsTab } from "./ExecutionsTab";

afterEach(() => {
  cleanup();
  listRuns = [];
  runDetail = undefined;
});

const checkRun = {
  traceId: "0198aaaa-bbbb-cccc-dddd-eeeeffff0001",
  jobName: "site-playbook",
  type: "ansible",
  executor: "runner",
  status: "success",
  manual: true,
  triggeredBy: "ops@example.com",
  startedAt: "2026-07-31T10:00:00Z",
  completedAt: "2026-07-31T10:01:00Z",
  durationMs: 60000,
  overrides: { ansibleCheck: true, ansibleTags: ["certs"] },
};

const renderTab = () => {
  const { container } = render(
    <MemoryRouter>
      <ExecutionsTab />
    </MemoryRouter>,
  );
  return within(container);
};

describe("History — check-mode visibility", () => {
  it("badges a check run on the row, without expanding it", async () => {
    listRuns = [checkRun];
    const q = renderTab();

    const badge = await q.findByText("Check");
    expect(badge.getAttribute("title")).toMatch(/applied nothing/);
  });

  it("does not badge an ordinary run", async () => {
    listRuns = [{ ...checkRun, overrides: { env: { FOO: "bar" } } }];
    const q = renderTab();

    await q.findByText("site-playbook");
    expect(q.queryByText("Check")).toBeNull();
  });

  it("states the dry-run reinterpretation in the expanded detail", async () => {
    listRuns = [checkRun];
    runDetail = checkRun;
    const q = renderTab();

    fireEvent.click(await q.findByText("site-playbook"));

    await waitFor(() => expect(q.getByText(/Check mode \(dry run\)/)).toBeTruthy());
    expect(q.getByText(/applied nothing/)).toBeTruthy();
    // The rest of the option set renders as the argv it produced.
    expect(q.getByText("--tags certs")).toBeTruthy();
  });

  it("renders the previously-invisible groups and raw --limit override keys", async () => {
    const run = {
      ...checkRun,
      overrides: { groups: ["webservers"], ansibleLimit: "web*:!quarantine" },
    };
    listRuns = [run];
    runDetail = run;
    const q = renderTab();

    fireEvent.click(await q.findByText("site-playbook"));

    await waitFor(() => expect(q.getByText("webservers")).toBeTruthy());
    expect(q.getByText("web*:!quarantine")).toBeTruthy();
  });
});

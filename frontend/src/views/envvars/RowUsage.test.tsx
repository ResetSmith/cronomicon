// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

// A secret's Usage (2.5.1 SG2.4): references, runs that used it, and changes,
// with a mark on an actor outside the agency or a global administrator.

const USAGE = {
  kind: "secret",
  id: "s1",
  name: "FIN_TOKEN",
  owner: "ag:FIN",
  references: [
    { via: "binding", ownerKind: "job", ownerId: "job-fin", name: "fin-deploy", scope: "fin-hosts" },
    { via: "become", ownerKind: "job", ownerId: "job-play", name: "fin-play", scope: "fin-hosts" },
  ],
  uses: { days7: 1, days30: 4, days90: 9 },
  recent: [{ at: "2026-10-21T08:00:00Z", traceId: "run-1", via: "binding", jobName: "fin-deploy", runnerName: "fin-runner" }],
  changes: [
    { at: "2026-10-21T09:00:00Z", actor: "root@corp", action: "revealed", actorRelation: "global_admin" },
    { at: "2026-10-20T09:00:00Z", actor: "ana@corp", action: "created", actorRelation: "member" },
  ],
};
// A key's Usage, two of whose records belong to agencies the reader does not
// read: counted, never named (2.5.1 review A3).
const KEY_USAGE = { ...USAGE, kind: "key", id: "k1", name: "fin-deploy-key", otherAgencyRecords: 2 };
const paths: string[] = [];
const HELD = [
  { kind: "secret", id: "s-old", name: "OLD_SHARED", scope: "fin-hosts" },
  { kind: "key", id: "k-old", name: "old-deploy" },
];

vi.mock("../../agency", () => ({
  useAgencyOptional: () => ({ agency: "ag:FIN", agencyName: "Finance", onOverview: false }),
}));

vi.mock("../../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/client")>();
  return {
    ...actual,
    api: {
      GET: vi.fn(async (path: string) => {
        paths.push(path);
        if (path === "/agencies/{agencyId}/held-by-global") return { data: { items: HELD } };
        if (path === "/ssh/credentials/{credentialId}/usage") return { data: KEY_USAGE };
        return { data: USAGE };
      }),
    } as unknown as typeof actual.api,
  };
});

import { HeldByGlobal, RowUsagePanel } from "./RowUsage";

afterEach(cleanup);

describe("RowUsagePanel", () => {
  it("shows the references, the runs and the changes, and marks a global administrator", async () => {
    render(
      <MemoryRouter>
        <RowUsagePanel kind="secret" id="s1" />
      </MemoryRouter>,
    );
    expect(await screen.findByText("fin-deploy", { selector: "a[href^='/runs?jobUid=']" })).toBeTruthy();
    expect(screen.getByText(/become password/)).toBeTruthy();
    expect(screen.getByText(/1 in 7 days · 4 in 30 · 9 in 90/)).toBeTruthy();
    expect(screen.getByText(/on fin-runner/)).toBeTruthy();
    expect(screen.getByText("Global administrator")).toBeTruthy();
    // The member's own change carries no mark.
    expect(screen.getAllByText(/Global administrator|Outside the agency/)).toHaveLength(1);
    expect(paths).toContain("/env-secrets/{secretId}/usage");
  });

  it("reads a key's usage from the key's route", async () => {
    paths.length = 0;
    render(
      <MemoryRouter>
        <RowUsagePanel kind="key" id="k1" />
      </MemoryRouter>,
    );
    await screen.findAllByText("fin-deploy");
    expect(paths).toContain("/ssh/credentials/{credentialId}/usage");
    expect(screen.getByText("2 host records or bastions of other agencies, not named here.")).toBeTruthy();
  });

  it("does not say nothing refers to a key that only other agencies' records name", async () => {
    render(
      <MemoryRouter>
        <RowUsagePanel kind="key" id="k1" />
      </MemoryRouter>,
    );
    expect(await screen.findByText(/of other agencies, not named here/)).toBeTruthy();
    expect(screen.queryByText("Nothing refers to it.")).toBeNull();
  });
});

describe("HeldByGlobal", () => {
  it("lists the agency's former secrets Global holds, of the tab's kind only, and opens one's usage", async () => {
    render(
      <MemoryRouter>
        <HeldByGlobal kind="secret" />
      </MemoryRouter>,
    );
    expect(await screen.findByText("Held by Global")).toBeTruthy();
    expect(screen.getByText(/only\s+Global's runs use them/)).toBeTruthy();
    expect(screen.queryByText("old-deploy")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: /OLD_SHARED/ }));
    expect(await screen.findByLabelText("Usage")).toBeTruthy();
  });
});

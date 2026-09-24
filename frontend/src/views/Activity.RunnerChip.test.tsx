// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

// AA-3 — the card's actor line. A runner-written row's `actor` is
// `runner:<uuid>`, which was the only thing the card could show before AA-1
// added the name. The chip is the readable half of what a "sort by runner"
// would have bought: the eye finds which runner without sorting anything.

const ROWS = [
  // Agent-written: name in the chip, the redundant id demoted to a tooltip.
  { id: 1, kind: "run-end", outcome: "success", actor: "runner:0198f0a0-1111", runnerName: "runner-east", jobName: "nightly", at: "2026-08-25T10:00:00Z" },
  // Operator STOPPED a run that executed on a runner — both facts are real and
  // both must show. This is the row AA-1's gap left blank.
  { id: 2, kind: "run-end", outcome: "warning", actor: "bob@corp.example", runnerName: "runner-west", jobName: "stopped", at: "2026-08-25T09:00:00Z" },
  // No runner at all: unchanged from before the band.
  { id: 3, kind: "config", actor: "alice@corp.example", runnerName: null, summary: "settings updated", at: "2026-08-25T08:00:00Z" },
];

vi.mock("../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/client")>();
  return {
    ...actual,
    api: {
      GET: vi.fn(async (path: string) => {
        if (path === "/activity") {
          return { data: { items: ROWS, totalItems: ROWS.length, totalPages: 1, page: 1, pageSize: 25 } };
        }
        if (path === "/activity/actors") return { data: { users: [], runners: [], system: [] } };
        if (path === "/me") return { data: { email: "op@ex.com", roles: ["operator"] } };
        return { data: [] };
      }),
    } as unknown as typeof actual.api,
  };
});

import { Activity } from "./Activity";

afterEach(cleanup);

const renderFeed = async () => {
  const { container } = render(
    <MemoryRouter>
      <Activity />
    </MemoryRouter>,
  );
  const q = within(container);
  await waitFor(() => expect(q.getByText("nightly")).toBeTruthy());
  return { q, container };
};

describe("Activity — runner chip (AA-3)", () => {
  it("shows the runner NAME, never the raw runner:<id> actor", async () => {
    const { q, container } = await renderFeed();
    expect(q.getByText("⚙ runner-east")).toBeTruthy();
    // The uuid must not be printed anywhere on the card.
    expect(container.textContent).not.toContain("0198f0a0-1111");
  });

  it("keeps the id reachable as a tooltip rather than deleting it", async () => {
    const { q } = await renderFeed();
    const chip = q.getByText("⚙ runner-east").closest("span[title]");
    expect(chip?.getAttribute("title")).toBe("runner:0198f0a0-1111");
  });

  it("shows BOTH the operator and the runner on a stopped run", async () => {
    const { q } = await renderFeed();
    // Neither fact displaces the other: a person stopped it, a runner ran it.
    expect(q.getByText("⚙ runner-west")).toBeTruthy();
    expect(q.getByText("bob@corp.example")).toBeTruthy();
  });

  it("leaves a runner-less row exactly as it was", async () => {
    const { q, container } = await renderFeed();
    expect(q.getByText("alice@corp.example")).toBeTruthy();
    expect(container.textContent).not.toContain("⚙ null");
    expect(container.querySelectorAll("span[title^='runner:']").length).toBe(1);
  });
});

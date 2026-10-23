// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";

// 2.5.0 C4.3 — an agency's push targets (AL-2). A target is one agency's, its
// administrators' to change (AL-3), and its URL — often the credential — is
// read only by them (question 81): anyone else sees that one is set.

const TARGETS = [
  { id: "t-slack", label: "Ops Slack", service: "slack", url: "slack://a/b/c", urlSet: true, enabled: true, ownerAgency: "a-ops" },
  // As a caller without configureApp on its agency reads it: no url.
  { id: "t-hidden", label: "Ops Pager", service: "pagerduty", urlSet: true, enabled: true, ownerAgency: "a-ops" },
];
const RULES = [
  { id: "r1", targetMode: "all", trigger: "failure", channels: ["apprise"], recipients: "", enabled: true, ownerAgency: "a-ops", targetIds: ["t-slack"] },
  { id: "r2", targetMode: "all", trigger: "success", channels: ["apprise"], recipients: "", enabled: true, ownerAgency: "a-ops", targetIds: ["t-slack"] },
];

const writes: { method: string; path: string; id?: string; body?: Record<string, unknown> }[] = [];

vi.mock("../../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/client")>();
  type Opts = { body?: Record<string, unknown>; params?: { path?: { targetId?: string } } };
  return {
    ...actual,
    api: {
      GET: vi.fn(async (path: string) => {
        if (path === "/alerts") return { data: RULES };
        if (path === "/alert-targets") return { data: TARGETS };
        return { data: {} };
      }),
      POST: vi.fn(async (path: string, opts: Opts) => {
        writes.push({ method: "POST", path, body: opts.body });
        return { data: {} };
      }),
      PUT: vi.fn(async (path: string, opts: Opts) => {
        writes.push({ method: "PUT", path, id: opts.params?.path?.targetId, body: opts.body });
        return { data: {} };
      }),
      DELETE: vi.fn(async (path: string, opts: Opts) => {
        writes.push({ method: "DELETE", path, id: opts.params?.path?.targetId });
        return { data: {} };
      }),
    } as unknown as typeof actual.api,
  };
});

let AG: { agency: string; agencyName: string } | null = null;
vi.mock("../../agency", () => ({ useAgencyOptional: () => AG }));
type Access = import("../../api/access").MyAccess;
let ACCESS: Access | null | undefined = undefined;
vi.mock("../../api/access", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/access")>();
  return { ...actual, useMyAccess: () => ACCESS };
});
const adminOf = (id: string, name: string) =>
  ({
    grants: [{ role: "admin", allScopes: false, agencyId: id, agencyName: name, scopes: [], permissions: ["configureApp"], groups: [], origin: "group" }],
  }) as unknown as Access;

import { AlertsSection } from "./Alerts";

const renderSection = async () => {
  render(<AlertsSection isGlobal={false} />);
  await waitFor(() => expect(targetTable().textContent).toContain("Ops Slack"));
};
// The targets table is the one with a "URL" header.
function targetTable() {
  const th = screen.getAllByText("URL").find((n) => n.tagName === "TH");
  const table = th?.closest("table");
  if (!table) throw new Error("targets table not found");
  return table as HTMLTableElement;
}
const row = (text: string) => within(targetTable()).getAllByRole("row").find((r) => r.textContent?.includes(text))!;

beforeEach(() => {
  writes.length = 0;
  AG = { agency: "a-ops", agencyName: "Ops" };
  ACCESS = adminOf("a-ops", "Ops");
});
afterEach(cleanup);

describe("Push Targets — a target's URL (question 81)", () => {
  it("shows the URL to the agency's administrator, and 'set' with no edit where it is withheld", async () => {
    await renderSection();
    expect(row("Ops Slack").textContent).toContain("slack://a/b/c");
    expect(within(row("Ops Slack")).getByRole("button", { name: "Edit" })).toBeTruthy();
    expect(within(row("Ops Pager")).getByText("set")).toBeTruthy();
    expect(within(row("Ops Pager")).queryByRole("button", { name: "Edit" })).toBeNull();
  });

  it("says how many rules push to each target", async () => {
    await renderSection();
    const cells = within(row("Ops Slack")).getAllByRole("cell").map((c) => c.textContent);
    expect(cells[3]).toBe("2");
    expect(within(row("Ops Pager")).getAllByRole("cell")[3].textContent).toBe("0");
  });
});

describe("Push Targets — writes (AL-2, AL-3)", () => {
  it("creates a target in the agency in use", async () => {
    await renderSection();
    fireEvent.click(screen.getByRole("button", { name: "+ Add target" }));
    fireEvent.change(screen.getByPlaceholderText("Label"), { target: { value: "Dev chat" } });
    fireEvent.change(screen.getByPlaceholderText("service"), { target: { value: "discord" } });
    fireEvent.change(screen.getByPlaceholderText("slack://tokenA/tokenB/tokenC"), { target: { value: "discord://w/t" } });
    fireEvent.click(screen.getByRole("button", { name: "Save target" }));
    await waitFor(() => expect(writes.length).toBe(1));
    expect(writes[0]).toEqual({
      method: "POST",
      path: "/alert-targets",
      body: { label: "Dev chat", service: "discord", url: "discord://w/t", enabled: true, ownerAgency: "a-ops" },
    });
  });

  it("refuses a target with no URL before asking the server", async () => {
    await renderSection();
    fireEvent.click(screen.getByRole("button", { name: "+ Add target" }));
    fireEvent.click(screen.getByRole("button", { name: "Save target" }));
    expect(screen.getByText(/needs its Apprise URL/)).toBeTruthy();
    expect(writes).toEqual([]);
  });

  it("edits a target, sending its URL back", async () => {
    await renderSection();
    fireEvent.click(within(row("Ops Slack")).getByRole("button", { name: "Edit" }));
    fireEvent.click(within(targetTable()).getByLabelText("Enabled"));
    fireEvent.click(within(targetTable()).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(writes.length).toBe(1));
    expect(writes[0]).toMatchObject({ method: "PUT", id: "t-slack", body: { url: "slack://a/b/c", enabled: false, label: "Ops Slack" } });
  });

  it("asks before deleting, saying which rules stop pushing to it", async () => {
    await renderSection();
    fireEvent.click(within(row("Ops Slack")).getByRole("button", { name: "Edit" }));
    fireEvent.click(within(targetTable()).getByRole("button", { name: "Delete" }));
    expect(writes).toEqual([]);
    let dialog: HTMLElement | null = screen.getByRole("button", { name: "Delete target" });
    while (dialog && !dialog.textContent?.includes("Delete push target?")) dialog = dialog.parentElement;
    expect(dialog?.textContent).toMatch(/Ops Slack\s*is named by 2 rules: they stop pushing to it/);
    fireEvent.click(screen.getByRole("button", { name: "Delete target" }));
    await waitFor(() => expect(writes).toEqual([{ method: "DELETE", path: "/alert-targets/{targetId}", id: "t-slack" }]));
  });

  it("disables Add and Edit, with the reason, for someone who only reads the agency", async () => {
    ACCESS = adminOf("a-dev", "Dev");
    await renderSection();
    const why = "Only Ops's administrators can change its alerts.";
    const add = screen.getByRole("button", { name: "+ Add target" }) as HTMLButtonElement;
    expect(add.disabled).toBe(true);
    expect(add.title).toBe(why);
    const edit = within(row("Ops Slack")).getByRole("button", { name: "Edit" }) as HTMLButtonElement;
    expect(edit.disabled).toBe(true);
    expect(edit.title).toBe(why);
  });
});

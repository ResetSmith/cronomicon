// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";

// Phase J (VF-15) — the alert-rule form used to collect five things the
// dispatcher never read: `tag` targeting, an `n-failures-window` trigger with
// its count/window pair, `notifyTriggerer`, and three of its four channels.
// Every one persisted, displayed, and produced nothing.
//
// The Go guard (TestAlertEnumsAreHonoured) stops the CONTRACT regrowing an inert
// value. This one stops the FORM regrowing one, and pins the two behaviours a
// type can't express: that a stored trigger the dropdown does not offer survives
// a save, and that a dead channel is marked rather than quietly shown as live.

const RULES = [
  // The seeded shape: a `warning` trigger the form does not offer, on a channel
  // that never delivered. Both halves are the regression fixtures.
  {
    id: "r-warn",
    targetMode: "job",
    jobName: "nightly-db-backup",
    trigger: "warning",
    channels: ["slack"],
    recipients: "",
    enabled: true,
  },
  // A legacy target mode. `targetMatches` returns false for anything but
  // job/all, so this rule matched no run at all.
  { id: "r-scope", targetMode: "scope", trigger: "failure", channels: ["in-app", "email"], recipients: "", enabled: true },
];

const puts: { path: string; body: Record<string, unknown> }[] = [];
const deletes: string[] = [];

vi.mock("../../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/client")>();
  return {
    ...actual,
    api: {
      GET: vi.fn(async (path: string) => {
        if (path === "/alerts") return { data: RULES };
        if (path === "/alert-destinations") return { data: [] };
        return { data: {} };
      }),
      PUT: vi.fn(async (path: string, opts: { body?: Record<string, unknown> }) => {
        puts.push({ path, body: opts.body ?? {} });
        return { data: {} };
      }),
      POST: vi.fn(async (path: string, opts: { body?: Record<string, unknown> }) => {
        puts.push({ path, body: opts.body ?? {} });
        return { data: {} };
      }),
      DELETE: vi.fn(async (path: string, opts?: { params?: { path?: { alertId?: string } } }) => {
        deletes.push(opts?.params?.path?.alertId ?? path);
        return { data: {} };
      }),
    } as unknown as typeof actual.api,
  };
});

import { NotificationsSection } from "./Notifications";

const renderSection = async () => {
  const out = render(<NotificationsSection />);
  await waitFor(() => expect(screen.getByText("Alert Rules")).toBeTruthy());
  return out;
};

// The rule table is the only table with a "WHEN" header; scope queries to it so
// the Run Notifications card above (which also says "Apprise") cannot match.
const ruleTable = () => {
  const th = screen.getAllByText("When").find((n) => n.tagName === "TH");
  const table = th?.closest("table");
  if (!table) throw new Error("rule table not found");
  return table as HTMLTableElement;
};

// RuleFormFields' root: a flex column whose sections are Target / When /
// Channels / Recipients. The add form renders above the table and the edit form
// inside a colspan row, so anchor on the label rather than on either container.
const formRoot = (idx = 0): HTMLElement => {
  // Filter out the table's CHANNELS header cell, which carries the same text.
  const label = screen.getAllByText("Channels").filter((n) => n.tagName === "DIV")[idx];
  const root = label?.parentElement?.parentElement;
  if (!root) throw new Error("no rule form open");
  return root as HTMLElement;
};

// A Chip is a <span> carrying cursor:pointer; the dropped-channel notice also
// names "Apprise" in a <strong>, so match on the affordance, not the word.
const chip = (root: HTMLElement, label: string): HTMLElement => {
  const hit = within(root)
    .getAllByText(label)
    .find((el) => (el as HTMLElement).style.cursor === "pointer");
  if (!hit) throw new Error(`no ${label} chip`);
  return hit as HTMLElement;
};

const openEditor = (rowText: string) => {
  const row = within(ruleTable())
    .getAllByRole("row")
    .find((r) => r.textContent?.includes(rowText));
  if (!row) throw new Error(`no row matching ${rowText}`);
  fireEvent.click(within(row).getByText("Edit"));
};

beforeEach(() => {
  puts.length = 0;
  deletes.length = 0;
});
afterEach(cleanup);

describe("Alert Rules form — the removals (J-2)", () => {
  it("no longer offers a target, trigger or channel the dispatcher ignores", async () => {
    await renderSection();
    fireEvent.click(screen.getByText("+ Add Alert Rule"));

    const panel = formRoot();
    const text = panel.textContent ?? "";

    // The five removed controls.
    expect(text).not.toContain("By Tag");
    expect(text).not.toContain("N failures in window");
    expect(text).not.toContain("failures within");
    expect(text).not.toMatch(/notify the user who started/i);

    // Channels: only the two the notifier speaks. Apprise is here for the first
    // time — it worked all along and the form never offered it.
    expect(chip(panel, "Email")).toBeTruthy();
    expect(chip(panel, "Apprise")).toBeTruthy();
    for (const dead of ["Slack", "Webhook", "In-App"]) {
      expect(within(panel).queryByText(dead)).toBeNull();
    }
  });

  it("offers exactly the triggers an operator may create", async () => {
    await renderSection();
    fireEvent.click(screen.getByText("+ Add Alert Rule"));
    const select = within(formRoot()).getByRole("combobox") as HTMLSelectElement;
    // SL added the two non-run-outcome triggers. They are offered deliberately:
    // a trigger the dispatcher honours but the UI cannot reach is the second
    // half of the VF-15 defect (see alert_enum_conformance_test.go).
    //
    // FX-D4 adds `skipped` for exactly that reason: until then nothing could
    // EMIT a suppression event, so offering it would have been the first half of
    // the same defect — a rule an operator can create that can never fire. Now
    // that recordSuppression emits one, withholding it from the form would be
    // the second half.
    expect([...select.options].map((o) => o.value)).toEqual([
      "failure",
      "success",
      "skipped",
      "sla-breach",
      "missed-run",
    ]);
    // And never a raw wire token on screen (G-1's rule).
    expect([...select.options].map((o) => o.textContent)).toEqual([
      "Job fails",
      "Job succeeds",
      "Run suppressed (calendar, pause, or concurrency)",
      "Job runs late (past its deadline)",
      "Scheduled run never happened",
    ]);
  });
});

describe("Alert Rules form — legacy values (J-4)", () => {
  it("keeps a stored trigger the dropdown does not offer, instead of eating it on save", async () => {
    await renderSection();
    openEditor("nightly-db-backup");

    const select = within(formRoot()).getByRole("combobox") as HTMLSelectElement;
    // The defect: with no matching <option> the select rendered unset and Save
    // rewrote the rule to `failure`. The stored value now has its own option...
    expect(select.value).toBe("warning");
    // ...labelled through the canonical function, not printed raw.
    expect(select.options[select.selectedIndex].textContent).toBe("Job result: Warn");

    // Pick a channel that delivers, then save. The trigger must survive.
    fireEvent.click(chip(formRoot(), "Apprise"));
    fireEvent.click(within(ruleTable()).getByText("Save"));

    await waitFor(() => expect(puts.length).toBe(1));
    expect(puts[0].body.trigger).toBe("warning");
    expect(puts[0].body.channels).toEqual(["apprise"]);
  });

  it("says which channel it dropped, rather than emptying the row in silence", async () => {
    await renderSection();
    openEditor("nightly-db-backup");
    // Save would otherwise refuse with "Pick at least one channel" and no
    // explanation of where the operator's Slack chip went.
    expect(formRoot().textContent).toMatch(/Slack was removed in v0\.52\.23/);
    expect(formRoot().textContent).toMatch(/nothing was ever delivered over it/);
  });

  it("marks a dead channel in the table instead of showing it as live", async () => {
    await renderSection();
    const table = ruleTable();
    const dead = within(table).getAllByText("In-App")[0];
    expect(dead.style.textDecoration).toContain("line-through");
    expect(dead.getAttribute("title")).toMatch(/not delivered/);

    // The live one beside it is not marked — otherwise the signal means nothing.
    const live = within(table).getAllByText("Email")[0];
    expect(live.style.textDecoration).not.toContain("line-through");
    expect(live.getAttribute("title")).toBeNull();
  });

  it("names an unsupported target rather than printing a tag list that looks like one", async () => {
    await renderSection();
    expect(within(ruleTable()).getByText(/unsupported target/)).toBeTruthy();
  });
});

// FX-9 — deleting an alert rule fired on the first click. The rule that tells an
// operator a job failed could vanish by mis-aim, with nothing naming what went.
describe("Alert Rules — delete asks first (FX-9)", () => {
  const deleteBtn = () => screen.getByRole("button", { name: "Delete" });

  it("does not delete on the first click — it asks, naming the rule", async () => {
    await renderSection();
    openEditor("nightly-db-backup");
    fireEvent.click(deleteBtn());

    expect(deletes).toEqual([]);
    expect(screen.getByText("Delete alert rule?")).toBeTruthy();
    // The dialog says WHICH rule, so a mis-aimed click is recoverable by reading.
    // Walked up from the confirm button to the node that also carries the title,
    // rather than a fixed ancestor count that Modal's internals could break.
    let dialog: HTMLElement | null = screen.getByRole("button", { name: "Delete rule" });
    while (dialog && !dialog.textContent?.includes("Delete alert rule?")) dialog = dialog.parentElement;
    expect(dialog).not.toBeNull();
    dialog = dialog!;
    expect(dialog.textContent).toContain("nightly-db-backup");
    // ...and what stops being sent, because that is the consequence.
    expect(dialog.textContent).toMatch(/active/);
  });

  it("deletes the rule it named once confirmed", async () => {
    await renderSection();
    openEditor("nightly-db-backup");
    fireEvent.click(deleteBtn());
    fireEvent.click(screen.getByRole("button", { name: "Delete rule" }));

    await waitFor(() => expect(deletes).toEqual(["r-warn"]));
  });

  it("cancelling leaves the rule alone", async () => {
    await renderSection();
    openEditor("nightly-db-backup");
    fireEvent.click(deleteBtn());
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));

    expect(deletes).toEqual([]);
    expect(screen.queryByText("Delete alert rule?")).toBeNull();
  });
});

// FX-12 — `editing` compared editId (set from `r.id ?? null`) against `r.id ?? ""`,
// so an id-less row could never match and its Edit button did nothing forever.
describe("Alert Rules — the Edit button works on every row it renders (FX-12)", () => {
  it("opens the editor for each listed rule", async () => {
    await renderSection();
    for (const rowText of ["nightly-db-backup", "unsupported target"]) {
      openEditor(rowText);
      // The editor is open when its own Save/Delete pair is on screen. Scoped to
      // the rule table: the Run Notifications card above has a Save of its own.
      const table = within(ruleTable());
      expect(table.getByRole("button", { name: "Delete" })).toBeTruthy();
      expect(table.getByRole("button", { name: /Save/ })).toBeTruthy();
      const row = table.getAllByRole("row").find((r) => r.textContent?.includes(rowText))!;
      fireEvent.click(within(row).getByText("Close"));
    }
  });

  it("offers no Edit button on a row that could not be edited", async () => {
    // An alert rule always has an id (alert_config.id is the primary key and the
    // Go scan target is a non-pointer string), which is exactly why the dead
    // button went unnoticed. The typed id is optional all the same: if one ever
    // arrives without one, the row shows no action rather than a dead one.
    const withoutId = { targetMode: "all", trigger: "failure", channels: ["email"], recipients: "", enabled: true };
    RULES.push(withoutId as (typeof RULES)[number]);
    try {
      await renderSection();
      const rows = within(ruleTable()).getAllByRole("row");
      const orphan = rows.find((r) => r.textContent?.includes("All jobs"))!;
      expect(orphan).toBeTruthy();
      expect(within(orphan).queryByText("Edit")).toBeNull();
    } finally {
      RULES.pop();
    }
  });
});

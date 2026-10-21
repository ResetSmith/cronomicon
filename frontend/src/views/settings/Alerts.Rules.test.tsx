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
//
// 2.5.0 C4.3 — the rules moved from Notifications to Alerts, one agency's
// (AL-1, AL-3), and a rule names the push targets it sends to (AL-2).

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
    ownerAgency: "a-ops",
    targetIds: [] as string[],
  },
  // A legacy target mode. `targetMatches` returns false for anything but
  // job/all, so this rule matched no run at all.
  {
    id: "r-scope",
    targetMode: "scope",
    trigger: "failure",
    channels: ["in-app", "email"],
    recipients: "",
    enabled: true,
    ownerAgency: "a-ops",
    targetIds: [] as string[],
  },
];

// The agency in use's targets, and one of Global's: the switcher narrows the
// list to the agency in use, and the picker filters by the rule's agency too.
const TARGETS = [
  { id: "t-slack", label: "Ops Slack", service: "slack", url: "slack://a/b/c", urlSet: true, enabled: true, ownerAgency: "a-ops" },
  { id: "t-pager", label: "Pager", service: "pagerduty", url: "pagerduty://k", urlSet: true, enabled: false, ownerAgency: "a-ops" },
  { id: "t-global", label: "Global Teams", service: "msteams", urlSet: true, enabled: true, ownerAgency: "global" },
];

const puts: { path: string; body: Record<string, unknown> }[] = [];
const deletes: string[] = [];
let targetsFail = false;

vi.mock("../../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/client")>();
  return {
    ...actual,
    api: {
      GET: vi.fn(async (path: string) => {
        if (path === "/alerts") return { data: RULES };
        if (path === "/alert-targets") return targetsFail ? { error: { message: "boom" } } : { data: TARGETS };
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

// The agency in use (the switcher) and the caller's grants.
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

let IS_GLOBAL = false;
const renderSection = async () => {
  const out = render(<AlertsSection isGlobal={IS_GLOBAL} />);
  await waitFor(() => expect(screen.getByText("nightly-db-backup")).toBeTruthy());
  return out;
};

// The rule table is the only table with a "WHEN" header; scope queries to it so
// the Push Targets table above cannot match.
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
  targetsFail = false;
  AG = { agency: "a-ops", agencyName: "Ops" };
  ACCESS = adminOf("a-ops", "Ops");
  IS_GLOBAL = false;
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
      // 2.5.1 SG-17: not about a job; backup-key-changed is a Global rule's only.
      "secret-revealed",
      "secret-changed",
    ]);
    // And never a raw wire token on screen (G-1's rule).
    expect([...select.options].map((o) => o.textContent)).toEqual([
      "Job fails",
      "Job succeeds",
      "Run suppressed (calendar, pause, or concurrency)",
      "Job runs late (past its deadline)",
      "Scheduled run never happened",
      "A secret is revealed",
      "A secret or SSH key changes",
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
      // the rule table: the Push Targets card above may have one of its own.
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

// AL-3 (2.5.0 C4) — POST/PUT/DELETE /alerts need configureApp on the rule's
// agency; GET /alerts is the agency in use's, to whoever reads it. So the rules
// stay listed for someone who only reads the agency, and "+ Add Alert Rule" and
// each row's Edit — the only doors to Save and Delete — are disabled with the
// reason (FX-7). Until 2.5.0 they were a global administrator's alone (GC).
describe("Alert Rules — the agency's administrators change them (AL-3)", () => {
  const expectLocked = (why: string) => {
    const add = screen.getByRole("button", { name: "+ Add Alert Rule" }) as HTMLButtonElement;
    expect(add.disabled).toBe(true);
    expect(add.title).toBe(why);
    const edits = within(ruleTable()).getAllByRole("button", { name: "Edit" }) as HTMLButtonElement[];
    expect(edits.length).toBe(RULES.length);
    for (const e of edits) {
      expect(e.disabled).toBe(true);
      expect(e.title).toBe(why);
    }
    // Clicking a disabled Edit opens no editor, so Save and Delete are unreachable.
    fireEvent.click(edits[0]);
    expect(screen.queryByRole("button", { name: "Delete" })).toBeNull();
    expect(puts.length).toBe(0);
    expect(deletes.length).toBe(0);
  };

  it("lists the rules but disables Add and Edit for an administrator of another agency, naming whose they are", async () => {
    ACCESS = adminOf("a-dev", "Dev");
    await renderSection();
    expectLocked("Only Ops's administrators can change its alerts.");
  });

  it("says Global's are a global administrator's when Global is in use", async () => {
    AG = { agency: "global", agencyName: "Global" };
    RULES.forEach((r) => (r.ownerAgency = "global"));
    try {
      await renderSection();
      expectLocked("Global's alerts are a global administrator's (a role on every agency) to change.");
    } finally {
      RULES.forEach((r) => (r.ownerAgency = "a-ops"));
    }
  });

  it("leaves Add and Edit enabled for the agency's administrator", async () => {
    await renderSection();
    const add = screen.getByRole("button", { name: "+ Add Alert Rule" }) as HTMLButtonElement;
    expect(add.disabled).toBe(false);
    expect(add.title).toBe("");
    for (const e of within(ruleTable()).getAllByRole("button", { name: "Edit" }) as HTMLButtonElement[]) {
      expect(e.disabled).toBe(false);
    }
  });

  it("leaves them enabled for a global administrator in any agency", async () => {
    ACCESS = { grants: [] } as unknown as Access;
    IS_GLOBAL = true;
    await renderSection();
    expect((screen.getByRole("button", { name: "+ Add Alert Rule" }) as HTMLButtonElement).disabled).toBe(false);
  });

  it("claims nothing while the caller's grants are still loading: the server decides", async () => {
    ACCESS = undefined;
    await renderSection();
    expect((screen.getByRole("button", { name: "+ Add Alert Rule" }) as HTMLButtonElement).disabled).toBe(false);
  });
});

// AL-2 (2.5.0 C4) — a push goes to the targets the matched rules name, and a
// rule names its own agency's only (question 79). The API reads an absent
// `targetIds` on an edit as "keep the rule's" and `[]` as "none", so the form
// sends the key only when it showed the picker.
describe("Alert Rules — the push-target picker (AL-2)", () => {
  const picker = () => screen.queryByText("Push to", { selector: "div" });
  const tick = (label: string) => fireEvent.click(within(formRoot()).getByLabelText(new RegExp(label)));
  const save = () => fireEvent.click(within(ruleTable()).getByText("Save"));

  it("is not shown for an Email-only rule, and appears with Apprise", async () => {
    await renderSection();
    fireEvent.click(screen.getByText("+ Add Alert Rule"));
    expect(picker()).toBeNull();
    fireEvent.click(chip(formRoot(), "Apprise"));
    expect(picker()).not.toBeNull();
  });

  it("offers the rule's agency's targets only, a disabled one marked", async () => {
    await renderSection();
    fireEvent.click(screen.getByText("+ Add Alert Rule"));
    fireEvent.click(chip(formRoot(), "Apprise"));
    const text = formRoot().textContent ?? "";
    expect(text).toContain("Ops Slack");
    expect(text).toMatch(/Pager\s*\(off\)/);
    expect(text).not.toContain("Global Teams");
    // Nothing picked yet: it says the push goes nowhere.
    expect(text).toMatch(/No target picked/);
  });

  it("creates the rule in the agency in use with the targets picked", async () => {
    await renderSection();
    fireEvent.click(screen.getByText("+ Add Alert Rule"));
    fireEvent.click(chip(formRoot(), "Apprise"));
    fireEvent.change(within(formRoot()).getByPlaceholderText("Job name"), { target: { value: "nightly-db-backup" } });
    tick("Ops Slack");
    fireEvent.click(screen.getByText("Save Rule"));
    await waitFor(() => expect(puts.length).toBe(1));
    expect(puts[0].path).toBe("/alerts");
    expect(puts[0].body.ownerAgency).toBe("a-ops");
    expect(puts[0].body.targetIds).toEqual(["t-slack"]);
  });

  it("sends no targetIds for an Email-only edit, so the rule keeps its targets", async () => {
    RULES[1].targetIds = ["t-slack"];
    try {
      await renderSection();
      openEditor("unsupported target");
      // Its legacy `scope` target opens as a job with no name; Save would refuse.
      fireEvent.click(chip(formRoot(), "All Jobs"));
      save();
      await waitFor(() => expect(puts.length).toBe(1));
      expect(puts[0].body.channels).toEqual(["email"]);
      expect("targetIds" in puts[0].body).toBe(false);
    } finally {
      RULES[1].targetIds = [];
    }
  });

  it("sends [] when every target is unticked, and the named ones otherwise", async () => {
    RULES[0].channels = ["apprise"];
    RULES[0].targetIds = ["t-slack", "t-pager"];
    try {
      await renderSection();
      openEditor("nightly-db-backup");
      tick("Pager");
      save();
      await waitFor(() => expect(puts.length).toBe(1));
      expect(puts[0].body.targetIds).toEqual(["t-slack"]);

      openEditor("nightly-db-backup");
      tick("Ops Slack");
      tick("Pager");
      save();
      await waitFor(() => expect(puts.length).toBe(2));
      expect(puts[1].body.targetIds).toEqual([]);
    } finally {
      RULES[0].channels = ["slack"];
      RULES[0].targetIds = [];
    }
  });

  it("shows no picker and sends no targetIds when the targets could not be read", async () => {
    targetsFail = true;
    RULES[0].channels = ["apprise"];
    RULES[0].targetIds = ["t-slack"];
    try {
      await renderSection();
      openEditor("nightly-db-backup");
      expect(picker()).toBeNull();
      save();
      await waitFor(() => expect(puts.length).toBe(1));
      expect("targetIds" in puts[0].body).toBe(false);
    } finally {
      RULES[0].channels = ["slack"];
      RULES[0].targetIds = [];
    }
  });

  it("names a rule's targets in the table, and warns when an Apprise rule names none", async () => {
    RULES[0].channels = ["apprise"];
    RULES[0].targetIds = ["t-slack", "t-pager"];
    RULES[1].channels = ["apprise"];
    try {
      await renderSection();
      const row = (text: string) => within(ruleTable()).getAllByRole("row").find((r) => r.textContent?.includes(text))!;
      expect(row("nightly-db-backup").textContent).toContain("Ops Slack, Pager");
      expect(within(row("unsupported target")).getByText("no target")).toBeTruthy();
    } finally {
      RULES[0].channels = ["slack"];
      RULES[0].targetIds = [];
      RULES[1].channels = ["in-app", "email"];
    }
  });
});

// Question 80 (2.5.0 C4) — the relay's default recipients are added to Global
// rules' mail only.
describe("Alert Rules — whose mail the default recipients join (question 80)", () => {
  it("says an agency's rule mails its own recipients only", async () => {
    await renderSection();
    fireEvent.click(screen.getByText("+ Add Alert Rule"));
    expect(formRoot().textContent).toContain("Email goes to these addresses only.");
  });

  it("says a Global rule's mail also goes to the relay's default recipients", async () => {
    AG = { agency: "global", agencyName: "Global" };
    IS_GLOBAL = true;
    await renderSection();
    fireEvent.click(screen.getByText("+ Add Alert Rule"));
    expect(formRoot().textContent).toContain("and to the relay's default recipients");
  });
});

// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { components } from "../../api/schema";

// SB — the runner host-key UI.
//
// What these tests hold in place is what the owner asked the screen to promise:
//
//   - nothing is trusted before the operator has been shown every fingerprint,
//     in full, in one list, with the changed keys first;
//   - "select all" never ticks a changed key, and the accept button says how
//     many keys and on which runner;
//   - what was approved here and what the runner's own file holds are two
//     separately titled lists — a key seeded by hand is visibly "not approved
//     here", and an approval that never landed is visibly "not in the file";
//   - what goes on the wire is exactly the ticked rows.

type Candidate = components["schemas"]["HostKeyCandidate"];
type RunnerHostKeys = components["schemas"]["RunnerHostKeys"];
type LedgerRow = components["schemas"]["HostKeyLedgerRow"];
type Coverage = components["schemas"]["ScopeHostKeyCoverage"];

interface Call {
  method: string;
  path: string;
  body?: unknown;
  query?: unknown;
}
const calls: Call[] = [];
let scopes: unknown[] = [];
let fleet: unknown[] = [];
let pending: Candidate[] = [];
let allPending: unknown[] = [];
let hostKeys: RunnerHostKeys;
let coverage: Coverage;
let batch: LedgerRow[] = [];
let post: (path: string, body: Record<string, unknown>) => { data?: unknown; error?: unknown };
let pendingError: unknown = undefined;

let ACCESS: unknown = null;
vi.mock("../../api/access", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/access")>();
  return { ...actual, useMyAccess: () => ACCESS };
});

vi.mock("../../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/client")>();
  return {
    ...actual,
    api: {
      GET: vi.fn(async (path: string) => {
        if (path === "/scopes") return { data: scopes };
        if (path === "/runners") return { data: fleet };
        if (path === "/runners/host-keys/pending") return { data: allPending };
        if (path === "/runners/{runnerId}/host-keys/pending") return pendingError ? { error: pendingError } : { data: pending };
        if (path === "/runners/{runnerId}/host-keys") return { data: hostKeys };
        if (path === "/scopes/{scopeId}/host-key-coverage") return { data: coverage };
        if (path === "/host-key-batches/{batchId}") return { data: batch };
        return { data: [] };
      }),
      POST: vi.fn(async (path: string, opts: { body?: Record<string, unknown>; params?: { query?: unknown } }) => {
        calls.push({ method: "POST", path, body: opts.body, query: opts.params?.query });
        return post(path, opts.body ?? {});
      }),
    } as unknown as typeof actual.api,
  };
});

import {
  HostKeyBatchLink,
  HostKeysDialog,
  KeyReviewTable,
  PendingKeysBanner,
  ScopeKeyCoverage,
  TrustedHostKeys,
  bulkSelectable,
  hostKeyBatchId,
  reviewAsText,
  sortForReview,
  type ReviewRow,
} from "./HostKeys";

const RUNNER = { id: "r-dmz", name: "runner-dmz-01" };
const FP = (n: string) => `SHA256:${n.repeat(43).slice(0, 43)}`;

const cand = (over: Partial<Candidate> & { host: string }): Candidate => ({
  keyType: "ssh-ed25519",
  fingerprint: FP(over.host[0]),
  status: "new",
  matchedServerPin: false,
  ...over,
});
const led = (over: Partial<LedgerRow> & { id: number; host: string }): LedgerRow => ({
  batchId: "b1",
  runnerId: RUNNER.id,
  runnerName: RUNNER.name,
  keyType: "ssh-ed25519",
  fingerprint: FP(over.host[0]),
  decision: "approved",
  source: "scan",
  matchedServerPin: false,
  actor: "alice@example.com",
  decidedAt: "2026-10-01T10:00:00Z",
  deliveredAt: "2026-10-01T10:00:05Z",
  ...over,
});

beforeEach(() => {
  calls.length = 0;
  scopes = [
    { id: "s-open", scope: "aaa-open", agencies: [{ id: "global", name: "Global" }], boundRunners: [] },
    { id: "s-dmz", scope: "dmz-web", agencies: [{ id: "global", name: "Global" }], boundRunners: [{ runnerId: RUNNER.id }] },
  ];
  fleet = [
    { id: RUNNER.id, name: RUNNER.name, status: "online", agencies: [{ id: "global", name: "Global" }] },
    { id: "r-old", name: "runner-old-01", status: "online", agencies: [{ id: "global", name: "Global" }] },
  ];
  pending = [];
  allPending = [];
  hostKeys = { inForce: [], history: [], pending: 0, knownHosts: { reportedAt: null, truncated: false, entries: [] } };
  coverage = { scope: "dmz-web", hosts: [], runners: [] };
  batch = [];
  pendingError = undefined;
  post = () => ({ data: {} });
});
afterEach(cleanup);

const posted = (path: string) => calls.filter((c) => c.path === path);
const button = (name: RegExp | string) => screen.getByRole("button", { name }) as HTMLButtonElement;

describe("the review list", () => {
  const list: ReviewRow[] = [
    { key: "t", host: "10.0.0.4", hostName: "web04", keyType: "ssh-ed25519", fingerprint: FP("t"), source: "Scanned", status: "trusted" },
    { key: "m", host: "10.0.0.3", hostName: "web03", keyType: "ssh-ed25519", fingerprint: FP("m"), source: "Scanned", status: "match", matchedServerPin: true },
    { key: "n", host: "10.0.0.2", hostName: "web02", keyType: "ssh-ed25519", fingerprint: FP("n"), source: "Scanned", status: "new" },
    { key: "c", host: "10.0.0.1", hostName: "web01", keyType: "ssh-ed25519", fingerprint: FP("c"), source: "Scanned", status: "changed", previousFingerprint: FP("o"), previousSource: "runner" },
    { key: "e", host: "", keyType: "", fingerprint: "", source: "Pasted", status: "", error: "not a known_hosts line" },
  ];

  it("puts what needs a decision first: unacceptable, then changed, new, matching, already trusted", () => {
    expect(sortForReview(list).map((r) => r.key)).toEqual(["e", "c", "n", "m", "t"]);
  });

  it("lets select-all tick everything except a changed key and an unacceptable line", () => {
    expect(list.filter(bulkSelectable).map((r) => r.key).sort()).toEqual(["m", "n", "t"]);
    let selected = new Set<string>();
    const { rerender } = render(<KeyReviewTable list={list} selected={selected} onChange={(s) => (selected = s)} />);
    fireEvent.click(screen.getByLabelText("Select all keys except changed ones"));
    expect([...selected].sort()).toEqual(["m", "n", "t"]);
    // A changed key is ticked on its own.
    rerender(<KeyReviewTable list={list} selected={selected} onChange={(s) => (selected = s)} />);
    fireEvent.click(screen.getByLabelText("Select web01 ssh-ed25519"));
    expect(selected.has("c")).toBe(true);
    // And an unacceptable line cannot be ticked at all.
    expect((screen.getAllByRole("checkbox").find((b) => (b as HTMLInputElement).disabled && b.getAttribute("aria-label") !== "Select all keys except changed ones") as HTMLInputElement).disabled).toBe(true);
  });

  it("shows every fingerprint in full, and the one a changed key would replace", () => {
    render(<KeyReviewTable list={list} selected={new Set()} onChange={() => {}} />);
    for (const r of list.filter((r) => r.fingerprint)) expect(screen.getByText(r.fingerprint)).toBeTruthy();
    expect(screen.getByText(FP("o"))).toBeTruthy();
    expect(screen.getByText(/Differs from the key this runner trusts now/)).toBeTruthy();
    expect(screen.getByText("not a known_hosts line")).toBeTruthy();
  });

  it("copies as plain text, one key per line, in review order", () => {
    const lines = reviewAsText(list).split("\n");
    expect(lines).toHaveLength(5);
    expect(lines[1]).toBe(["CHANGED", "web01", "10.0.0.1", "ssh-ed25519", FP("c")].join("\t"));
  });
});

describe("the host keys dialog", () => {
  const open = (props: Partial<Parameters<typeof HostKeysDialog>[0]> = {}) =>
    render(<HostKeysDialog runner={RUNNER} onClose={() => {}} {...props} />);

  it("offers a scope, typed hosts, pasted keys or another runner, and lists this runner's own scopes first", async () => {
    open();
    for (const label of ["Scan a scope", "Scan hosts", "Paste keys", "Copy from a runner"]) expect(screen.getByText(label)).toBeTruthy();
    const select = (await screen.findByLabelText("Scope to scan")) as HTMLSelectElement;
    await waitFor(() => expect(select.value).toBe("s-dmz"));
    expect(within(select).getAllByRole("option").map((o) => o.textContent)).toEqual(["dmz-web (bound to this runner)", "aaa-open"]);
  });

  it("offers only the scopes this runner may serve", async () => {
    // A runner that serves Global cannot serve a scope that belongs to an agency.
    scopes = [...scopes, { id: "s-fin", scope: "finance-hosts", agencies: [{ id: "ag-fin" }], boundRunners: [] }];
    open();
    const select = (await screen.findByLabelText("Scope to scan")) as HTMLSelectElement;
    await waitFor(() => expect(select.value).toBe("s-dmz"));
    expect(within(select).getAllByRole("option").map((o) => o.textContent)).not.toContain("finance-hosts");
  });

  // LR-63 — a caller who administers an agency this runner serves, and does
  // not own the runner: one source (scan a scope of theirs), and never a key
  // that would replace one the runner trusts.
  it("in guest mode offers only a scope scan, and will not trust a key that replaces one", async () => {
    post = (path) => {
      if (path === "/runners/{runnerId}/keyscan") {
        pending = [
          cand({ id: "p1", host: "10.1.0.5", hostName: "web01", scopeName: "dmz-web", status: "new" }),
          cand({ id: "p2", host: "10.1.0.6", hostName: "web02", scopeName: "dmz-web", status: "changed", previousFingerprint: FP("z"), previousSource: "runner" }),
        ];
        return { data: { hosts: ["10.1.0.5", "10.1.0.6"], skipped: [] } };
      }
      return { data: { batchId: "b9", approved: 1, rejected: 0, unchanged: 0 } };
    };
    open({ guest: true, initialSource: "paste" });
    // Told why, and none of the owner's sources — whatever the caller asked to open on.
    expect(screen.getByText(/is not your agency's runner, but it serves your agency/)).toBeTruthy();
    for (const label of ["Scan hosts", "Paste keys", "Copy from a runner"]) expect(screen.queryByText(label)).toBeNull();
    await waitFor(() => expect((screen.getByLabelText("Scope to scan") as HTMLSelectElement).value).toBe("s-dmz"));
    fireEvent.click(button("Queue scan"));
    expect(posted("/runners/{runnerId}/keyscan")[0].body).toEqual({ scopeId: "s-dmz" });

    // Tick both: the changed one blocks the whole batch, with the reason.
    await screen.findByText("web02");
    for (const box of screen.getAllByRole("checkbox")) if (!(box as HTMLInputElement).checked && !(box as HTMLInputElement).disabled) fireEvent.click(box);
    expect(await screen.findByText(/would replace a key runner-dmz-01 already trusts\. That is for the runner's owner/)).toBeTruthy();
    expect((screen.getByRole("button", { name: /^Trust / }) as HTMLButtonElement).disabled).toBe(true);
    expect(posted("/runners/{runnerId}/host-keys/resolve-batch")).toHaveLength(0);
  });

  // Guest mode is the server's answer about THIS runner (`canManage: false` on
  // its row), so it holds wherever the dialog is opened from — the Scopes
  // page's coverage panel opens it without knowing. And what a guest is offered
  // is the scopes of an agency they ADMINISTER, which is the server's own list:
  // being able to read a scope the runner serves is not enough.
  it("works out guest mode from the runner's own row, and offers only the caller's agencies' scopes", async () => {
    const FIN = { id: "ag-fin", name: "Finance" };
    const TAX = { id: "ag-tax", name: "Tax" };
    fleet = [{ id: RUNNER.id, name: RUNNER.name, status: "online", agencies: [FIN, TAX], ownerAgency: { id: "global", name: "Global" }, canManage: false }];
    scopes = [
      { id: "s-fin", scope: "fin-web", agencies: [FIN], boundRunners: [] },
      { id: "s-tax", scope: "tax-web", agencies: [TAX], boundRunners: [] },
    ];
    ACCESS = { grants: [{ role: "admin", allScopes: false, agencyId: "ag-fin", agencyName: "Finance", scopes: [], permissions: ["configureApp"], groups: [], origin: "group" }] };
    open(); // no `guest` prop
    expect(await screen.findByText(/is not your agency's runner, but it serves your agency/)).toBeTruthy();
    for (const label of ["Scan hosts", "Paste keys", "Copy from a runner"]) expect(screen.queryByText(label)).toBeNull();
    const select = (await screen.findByLabelText("Scope to scan")) as HTMLSelectElement;
    await waitFor(() => expect(select.value).toBe("s-fin"));
    expect(within(select).getAllByRole("option").map((o) => o.textContent)).toEqual(["fin-web"]);
    ACCESS = null;
  });

  it("tells a guest an offline runner cannot scan without pointing at Paste, which is not theirs", async () => {
    fleet = [{ id: RUNNER.id, name: RUNNER.name, status: "offline", agencies: [{ id: "global", name: "Global" }], canManage: false }];
    open();
    expect(await screen.findByText(/is not online, so it cannot scan\.\s+Try again when it is back, or ask its owner\./)).toBeTruthy();
    expect(screen.queryByText(/Pasted keys do not need it/)).toBeNull();
  });

  it("will not queue a scan on a runner that is not online, and says pasting still works", async () => {
    fleet = [{ id: RUNNER.id, name: RUNNER.name, status: "offline", agencies: [{ id: "global", name: "Global" }] }];
    open();
    expect(await screen.findByText(/runner-dmz-01 is not online, so it cannot scan/)).toBeTruthy();
    expect(button("Queue scan").disabled).toBe(true);
    fireEvent.click(screen.getByText("Paste keys"));
    expect(button("Review keys").disabled).toBe(false);
  });

  it("scans a scope, names the hosts it could not scan, and trusts only what is ticked", async () => {
    post = (path) => {
      if (path === "/runners/{runnerId}/keyscan") {
        // The runner answers a moment later.
        pending = [
          cand({ id: "p1", host: "10.1.0.5", hostName: "web01", scopeName: "dmz-web", status: "match", matchedServerPin: true }),
          cand({ id: "p2", host: "[10.1.0.6]:2222", hostName: "web02", scopeName: "dmz-web", status: "changed", previousFingerprint: FP("z"), previousSource: "runner" }),
        ];
        return {
          data: {
            hosts: ["10.1.0.5", "10.1.0.6:2222"],
            skipped: [{ host: "db01", pattern: "10.2.0.5", reason: "reached through bastion jump-a; not scannable, provide the key" }],
          },
        };
      }
      return { data: { batchId: "b9", approved: 1, rejected: 0, unchanged: 0 } };
    };
    open();
    await waitFor(() => expect((screen.getByLabelText("Scope to scan") as HTMLSelectElement).value).toBe("s-dmz"));
    fireEvent.click(button("Queue scan"));

    expect(await screen.findByText(/1 host not scanned/)).toBeTruthy();
    expect(screen.getByText(/reached through bastion jump-a; not scannable, provide the key/)).toBeTruthy();
    // And what to paste its key under: the name the runner looks it up by.
    expect(screen.getByText("10.2.0.5")).toBeTruthy();
    expect(posted("/runners/{runnerId}/keyscan")[0].body).toEqual({ scopeId: "s-dmz" });

    // Both keys are on screen with their fingerprints. They arrived while the
    // list was open, so NEITHER is ticked: the count on the button never grows
    // under the operator's cursor.
    expect(await screen.findByText(FP("1"))).toBeTruthy();
    expect(screen.getByText(FP("["))).toBeTruthy();
    expect(button("Trust 0 keys on runner-dmz-01").disabled).toBe(true);
    expect(screen.getByText(/1 key arrived after this list opened and is not selected/)).toBeTruthy();
    // "Select all" takes the matching key and leaves the changed one alone.
    fireEvent.click(screen.getByLabelText("Select all keys except changed ones"));
    fireEvent.click(button("Trust 1 key on runner-dmz-01"));
    await waitFor(() => expect(posted("/runners/{runnerId}/host-keys/resolve-batch")).toHaveLength(1));
    expect(posted("/runners/{runnerId}/host-keys/resolve-batch")[0].body).toEqual({ approve: ["p1"], acknowledgeChanged: [] });
    expect(await screen.findByText(/1 key trusted\. runner-dmz-01 receives it on its next poll\./)).toBeTruthy();
  });

  it("ticks by rule the keys that were already waiting, and never one that arrives later", async () => {
    pending = [cand({ id: "p1", host: "10.1.0.5" })];
    open({ startAt: "review" });
    expect(await screen.findByRole("button", { name: "Trust 1 key on runner-dmz-01" })).toBeTruthy();
    // A second key lands on the next poll.
    pending = [...pending, cand({ id: "p2", host: "10.1.0.7", fingerprint: FP("7") })];
    expect(await screen.findByText(FP("7"), undefined, { timeout: 5000 })).toBeTruthy();
    expect(screen.getByText("Arrived after this list opened")).toBeTruthy();
    expect(button("Trust 1 key on runner-dmz-01")).toBeTruthy();
    fireEvent.click(button("Trust 1 key on runner-dmz-01"));
    await waitFor(() => expect(posted("/runners/{runnerId}/host-keys/resolve-batch")).toHaveLength(1));
    expect(posted("/runners/{runnerId}/host-keys/resolve-batch")[0].body).toEqual({ approve: ["p1"], acknowledgeChanged: [] });
  }, 10000);

  it("says the list could not be loaded, rather than that nothing is waiting", async () => {
    pendingError = { message: "you do not have configureApp on the agency that owns this runner" };
    open({ startAt: "review" });
    expect(await screen.findByText(/The keys waiting for runner-dmz-01 could not be loaded/)).toBeTruthy();
    expect(screen.queryByText("No keys are waiting for review.")).toBeNull();
  });

  it("warns before a changed key is accepted, and says what accepting does", async () => {
    pending = [cand({ id: "p2", host: "10.1.0.6", status: "changed", previousFingerprint: FP("z"), previousSource: "runner" })];
    open({ startAt: "review" });
    expect(await screen.findByText(FP("1"))).toBeTruthy();
    // Nothing ticked: a changed key is never selected for the operator.
    expect(button("Trust 0 keys on runner-dmz-01").disabled).toBe(true);
    fireEvent.click(screen.getByLabelText("Select 10.1.0.6 ssh-ed25519"));
    expect(button("Trust 1 key on runner-dmz-01").disabled).toBe(false);
    expect(screen.getByText(/1 selected key differs from a key already trusted\. Accepting it replaces the old key on runner-dmz-01/)).toBeTruthy();
    // On the wire the key is named twice: approved, and acknowledged as a change.
    post = () => ({ data: { batchId: "b1", approved: 1, rejected: 0, unchanged: 0 } });
    fireEvent.click(button("Trust 1 key on runner-dmz-01"));
    await waitFor(() => expect(posted("/runners/{runnerId}/host-keys/resolve-batch")).toHaveLength(1));
    expect(posted("/runners/{runnerId}/host-keys/resolve-batch")[0].body).toEqual({ approve: ["p2"], acknowledgeChanged: ["p2"] });
  });

  it("shows the server's refusal when what was reviewed has changed", async () => {
    pending = [cand({ id: "p1", host: "10.1.0.5" })];
    post = () => ({ error: { message: "the keys changed after they were reviewed (10.1.0.5, ssh-ed25519 now replaces a trusted key); nothing was trusted. Reload the list and review it again" } });
    open({ startAt: "review" });
    fireEvent.click(await screen.findByRole("button", { name: "Trust 1 key on runner-dmz-01" }));
    expect(await screen.findByRole("alert")).toBeTruthy();
    expect(screen.getByRole("alert").textContent).toMatch(/nothing was trusted/);
  });

  it("rejects the ticked keys", async () => {
    pending = [cand({ id: "p1", host: "10.1.0.5" })];
    post = () => ({ data: { batchId: "b2", approved: 0, rejected: 1, unchanged: 0 } });
    open({ startAt: "review" });
    fireEvent.click(await screen.findByRole("button", { name: "Reject 1 selected" }));
    await waitFor(() => expect(posted("/runners/{runnerId}/host-keys/resolve-batch")).toHaveLength(1));
    expect(posted("/runners/{runnerId}/host-keys/resolve-batch")[0].body).toEqual({ reject: ["p1"] });
  });

  it("shows pasted keys for review before trusting any, and commits only the ticked ones", async () => {
    const paste = "web01 ssh-ed25519 AAAA\nweb02 ssh-ed25519 BBBB";
    post = (path, body) => {
      if (path !== "/runners/{runnerId}/host-keys/provide") return { data: {} };
      if (body.dryRun) {
        return {
          data: {
            acceptable: true,
            unverifiable: 0,
            candidates: [
              cand({ host: "web01", input: "web01 ssh-ed25519 AAAA", status: "new", fingerprint: FP("a") }),
              cand({ host: "web02", input: "web02 ssh-ed25519 BBBB", status: "changed", fingerprint: FP("b"), previousFingerprint: FP("q"), previousSource: "server" }),
            ],
          },
        };
      }
      return { data: { batchId: "b3", approved: 1, rejected: 0, unchanged: 0 } };
    };
    open({ initialSource: "paste" });
    fireEvent.change(screen.getByLabelText("known_hosts lines"), { target: { value: paste } });
    fireEvent.click(button("Review keys"));

    expect(await screen.findByText(FP("a"))).toBeTruthy();
    expect(screen.getByText(FP("b"))).toBeTruthy();
    expect(screen.getByText(FP("q"))).toBeTruthy();
    expect(screen.getByText(/Differs from the key the server pins for this host/)).toBeTruthy();
    // The dry run wrote nothing; the commit names the one ticked row.
    expect(posted("/runners/{runnerId}/host-keys/provide")).toHaveLength(1);
    expect(posted("/runners/{runnerId}/host-keys/provide")[0].body).toEqual({ lines: [paste], dryRun: true });
    fireEvent.click(await screen.findByRole("button", { name: "Trust 1 key on runner-dmz-01" }));
    await waitFor(() => expect(posted("/runners/{runnerId}/host-keys/provide")).toHaveLength(2));
    // Named by the fingerprint that was on screen, and the status it was shown with.
    expect(posted("/runners/{runnerId}/host-keys/provide")[1].body).toEqual({
      lines: [paste],
      select: [{ host: "web01", keyType: "ssh-ed25519", fingerprint: FP("a"), status: "new" }],
    });
  });

  it("will not accept a paste that has a line it cannot read", async () => {
    post = () => ({
      data: {
        acceptable: false,
        unverifiable: 0,
        candidates: [
          cand({ host: "web01", input: "web01 ssh-ed25519 AAAA" }),
          { host: "", keyType: "", fingerprint: "", status: "", matchedServerPin: false, input: "@revoked web02 …", error: "@revoked lines are not accepted here; give the host's own key line" },
        ],
      },
    });
    open({ initialSource: "paste" });
    fireEvent.change(screen.getByLabelText("known_hosts lines"), { target: { value: "x" } });
    fireEvent.click(button("Review keys"));
    expect(await screen.findByText(/@revoked lines are not accepted here/)).toBeTruthy();
    expect(screen.getByText(/a paste is accepted whole or not at all/)).toBeTruthy();
    expect(button("Trust 1 key on runner-dmz-01").disabled).toBe(true);
  });

  it("copies another runner's keys only after the list has been reviewed", async () => {
    post = (path, body) => {
      if (path !== "/runners/{runnerId}/host-keys/carry") return { data: {} };
      if (body.dryRun) return { data: { acceptable: true, unverifiable: 2, candidates: [cand({ host: "web07" })] } };
      return { data: { batchId: "b4", approved: 1, rejected: 0, unchanged: 0 } };
    };
    open({ initialSource: "carry", initialCarryFrom: "r-old" });
    fireEvent.click(button("Review keys"));
    expect(await screen.findByText(FP("w"))).toBeTruthy();
    expect(screen.getByText(/Keys runner-old-01 trusts that runner-dmz-01 does not/)).toBeTruthy();
    expect(screen.getByText(/2 keys recorded before this version cannot be verified and are left out/)).toBeTruthy();
    expect(posted("/runners/{runnerId}/host-keys/carry")[0]).toMatchObject({ body: { dryRun: true }, query: { from: "r-old" } });
    fireEvent.click(await screen.findByRole("button", { name: "Trust 1 key on runner-dmz-01" }));
    await waitFor(() => expect(posted("/runners/{runnerId}/host-keys/carry")).toHaveLength(2));
    expect(posted("/runners/{runnerId}/host-keys/carry")[1]).toMatchObject({
      body: { select: [{ host: "web07", keyType: "ssh-ed25519", fingerprint: FP("w"), status: "new" }] },
      query: { from: "r-old" },
    });
  });

  it("can copy from a runner that is no longer registered, when the caller names one", async () => {
    open({ initialSource: "carry", carryFromPrevious: { id: "r-gone", name: "runner-dmz-01's previous registration" } });
    const select = (await screen.findByLabelText("Runner to copy keys from")) as HTMLSelectElement;
    expect(select.value).toBe("r-gone");
    expect(within(select).getAllByRole("option")[0].textContent).toBe("runner-dmz-01's previous registration (no longer registered)");
  });
});

describe("what a runner trusts", () => {
  it("keeps what was approved here apart from what the runner's file holds", async () => {
    hostKeys = {
      pending: 2,
      inForce: [
        led({ id: 1, host: "10.1.0.5", hostName: "web01", scopeName: "dmz-web", presentInFile: true }),
        led({ id: 2, host: "10.1.0.6", hostName: "web02", presentInFile: false }),
      ],
      history: [led({ id: 3, host: "10.1.0.9", decision: "rejected", supersededAt: "2026-09-01T00:00:00Z" })],
      knownHosts: {
        reportedAt: "2026-10-02T08:00:00Z",
        truncated: false,
        entries: [
          { line: 1, hosts: "10.1.0.5", hashed: false, marker: "", keyType: "ssh-ed25519", fingerprint: FP("1"), approvedHere: true },
          { line: 2, hosts: "legacy-box", hashed: false, marker: "", keyType: "ssh-rsa", fingerprint: FP("L"), approvedHere: false },
          { line: 3, hosts: "", hashed: true, marker: "", keyType: "ssh-ed25519", fingerprint: FP("H"), approvedHere: false },
          { line: 4, hosts: "*.old", hashed: false, marker: "revoked", keyType: "ssh-rsa", fingerprint: FP("R"), approvedHere: false },
        ],
      },
    };
    render(<TrustedHostKeys runner={RUNNER} canScan />);

    // Two lists, each under its own title.
    expect(await screen.findByText("Approved in Cronomicon (2)")).toBeTruthy();
    expect(screen.getByText("Present in the runner's known_hosts file (4)")).toBeTruthy();

    // The file's list says where each line came from, and never invents a name for a hashed one.
    expect(screen.getByText("Approved here")).toBeTruthy();
    expect(screen.getAllByText("Not approved here")).toHaveLength(2);
    expect(screen.getByText("hashed entry")).toBeTruthy();
    expect(screen.getByText("Revoked")).toBeTruthy();
    expect(screen.getByText(FP("L"))).toBeTruthy();

    // An approval that never landed is flagged, with its remedy.
    expect(screen.getByText("Not in the file")).toBeTruthy();
    expect(screen.getByText(/1 approved key is not in runner-dmz-01's known_hosts file/)).toBeTruthy();
    fireEvent.click(button("Send the key for web02 again"));
    await waitFor(() => expect(posted("/runners/{runnerId}/host-keys/{ledgerId}/resend")).toHaveLength(1));

    // Keys still waiting are one click away, and history is there to open.
    expect(button("Review 2 waiting")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: /History \(1\)/ }));
    expect(screen.getByText("Rejected")).toBeTruthy();
  });

  it("says so when the runner has not reported its file, rather than showing an empty list as fact", async () => {
    hostKeys = { ...hostKeys, inForce: [led({ id: 1, host: "10.1.0.5" })] };
    render(<TrustedHostKeys runner={RUNNER} canScan />);
    expect(await screen.findByText(/runner-dmz-01 has not reported its file yet/)).toBeTruthy();
    // Sent, but with no report there is nothing to say about the file either way.
    expect(screen.getByText("Sent")).toBeTruthy();
    expect(screen.queryByText("Not in the file")).toBeNull();
  });

  it("removes a key only after a confirmation that says what stops working", async () => {
    hostKeys = { ...hostKeys, inForce: [led({ id: 7, host: "10.1.0.5", hostName: "web01", presentInFile: true })] };
    render(<TrustedHostKeys runner={RUNNER} canScan />);
    fireEvent.click(await screen.findByRole("button", { name: "Remove the key for web01" }));
    expect(posted("/runners/{runnerId}/host-keys/{ledgerId}/remove")).toHaveLength(0);
    expect(screen.getByText(/its runs on that host fail until a key is approved again/)).toBeTruthy();
    fireEvent.click(button("Remove key"));
    await waitFor(() => expect(posted("/runners/{runnerId}/host-keys/{ledgerId}/remove")).toHaveLength(1));
  });

  it("explains a disabled scan on a runner that is not online, and leaves pasting available", async () => {
    render(<TrustedHostKeys runner={RUNNER} canScan={false} />);
    const scan = (await screen.findByRole("button", { name: "Scan keys…" })) as HTMLButtonElement;
    expect(scan.disabled).toBe(true);
    expect(scan.title).toMatch(/must be online to scan/);
    expect(button("Paste keys…").disabled).toBe(false);
  });
});

describe("keys awaiting review", () => {
  it("stays out of the way when nothing is waiting", async () => {
    const { container } = render(<PendingKeysBanner refreshKey={0} />);
    await waitFor(() => expect(container.textContent).toBe(""));
  });

  it("groups by runner and opens that runner's review", async () => {
    allPending = [
      { id: "p1", runnerId: RUNNER.id, runnerName: RUNNER.name },
      { id: "p2", runnerId: RUNNER.id, runnerName: RUNNER.name },
      { id: "p3", runnerId: "r-b", runnerName: "runner-b" },
    ];
    pending = [cand({ id: "p1", host: "10.1.0.5" })];
    render(<PendingKeysBanner refreshKey={0} />);
    expect(await screen.findByText("Host keys awaiting review")).toBeTruthy();
    expect(screen.getByText("3 keys")).toBeTruthy();
    fireEvent.click(button("Review host keys for runner-dmz-01"));
    expect(await screen.findByText("Host keys — runner-dmz-01")).toBeTruthy();
    expect(await screen.findByText(FP("1"))).toBeTruthy();
  });
});

describe("a scope's host keys on its bound runners", () => {
  it("renders nothing for a scope with no bound runner", () => {
    const { container } = render(<ScopeKeyCoverage scopeId="s-open" bound={0} canConfig />);
    expect(container.textContent).toBe("");
  });

  it("names the runner and how many hosts it does not trust, and offers the scan", async () => {
    coverage = {
      scope: "dmz-web",
      hosts: [
        { host: "web01", pattern: "10.1.0.5", target: "10.1.0.5" },
        { host: "web02", pattern: "10.1.0.6", target: "10.1.0.6" },
        { host: "db01", pattern: "10.2.0.5", target: "", notScannable: "reached through bastion jump-a; not scannable, provide the key" },
      ],
      runners: [
        { runnerId: RUNNER.id, name: RUNNER.name, registered: true, states: ["approved", "in-file", ""], missing: 1, reportedAt: null },
        { runnerId: "r-gone", name: "runner-gone", registered: false, states: ["queued", "", ""], missing: 2, reportedAt: null },
      ],
    };
    render(<ScopeKeyCoverage scopeId="s-dmz" bound={2} canConfig />);
    expect(await screen.findByText(/1 of 3 hosts not yet trusted by runner-dmz-01\./)).toBeTruthy();
    expect(screen.getByText(/2 of 3 hosts not yet trusted by runner-gone\./)).toBeTruthy();
    // A deregistered runner cannot scan, so it gets no scan button.
    expect(screen.getAllByRole("button", { name: /Scan this scope from/ })).toHaveLength(1);

    fireEvent.click(screen.getByRole("button", { name: /Host by host/ }));
    expect(screen.getByText("Trusted")).toBeTruthy();
    expect(screen.getByText("In its file")).toBeTruthy();
    expect(screen.getAllByText("Not trusted")).toHaveLength(3);
    expect(screen.getByText("Approved, not yet sent")).toBeTruthy();
    expect(screen.getByText(/not scannable, provide the key/)).toBeTruthy();

    // The scan opens on this scope, from that runner.
    fireEvent.click(button("Scan this scope from runner-dmz-01"));
    await waitFor(() => expect((screen.getByLabelText("Scope to scan") as HTMLSelectElement).value).toBe("s-dmz"));
  });

  it("says so when every bound runner trusts every host", async () => {
    coverage = {
      scope: "dmz-web",
      hosts: [{ host: "web01", pattern: "10.1.0.5", target: "10.1.0.5" }],
      runners: [{ runnerId: RUNNER.id, name: RUNNER.name, registered: true, states: ["approved"], missing: 0, reportedAt: null }],
    };
    render(<ScopeKeyCoverage scopeId="s-dmz" bound={1} canConfig />);
    expect(await screen.findByText(/Every bound runner trusts all 1 host of this scope/)).toBeTruthy();
  });
});

describe("a host-key change-log row", () => {
  it("finds the batch a row names", () => {
    expect(hostKeyBatchId("2 approved, 1 rejected; source scan; batch 019f-abc")).toBe("019f-abc");
    expect(hostKeyBatchId("web01 (ssh-ed25519) SHA256:x; batch 019f-def")).toBe("019f-def");
    expect(hostKeyBatchId("SHA256:abcdef")).toBeNull();
    expect(hostKeyBatchId(undefined)).toBeNull();
  });

  it("opens the keys the batch decided", async () => {
    batch = [
      led({ id: 1, host: "10.1.0.5", hostName: "web01", fingerprint: FP("n"), previousFingerprint: FP("o") }),
      led({ id: 2, host: "10.1.0.9", decision: "rejected", fingerprint: FP("x") }),
    ];
    render(<HostKeyBatchLink details="1 approved, 1 rejected; source scan; batch 019f-abc" />);
    expect(screen.getByText(/1 approved, 1 rejected; source scan/)).toBeTruthy();
    fireEvent.click(button("Show keys"));
    expect(await screen.findByText("Host keys in this change")).toBeTruthy();
    expect(await screen.findByText(FP("n"))).toBeTruthy();
    expect(screen.getByText(FP("x"))).toBeTruthy();
    expect(screen.getByText(FP("o"))).toBeTruthy();
    expect(screen.getByText("rejected")).toBeTruthy();
  });

  it("leaves a row that names no batch as plain text", () => {
    render(<HostKeyBatchLink details="SHA256:legacy-row" />);
    expect(screen.getByText("SHA256:legacy-row")).toBeTruthy();
    expect(screen.queryByRole("button")).toBeNull();
  });
});

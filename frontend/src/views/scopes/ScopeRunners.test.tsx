// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import type { components } from "../../api/schema";

// SB — the scope↔runner binding UI.
//
// Three things here are easy to get subtly wrong and are what these tests pin:
//
//   - a binding whose runner is GONE must stay loud. It still restricts the
//     scope, nothing can claim its runs, and the UI is the only place an operator
//     learns that — so a ghost is shown, named, and warned about, never tidied
//     away as "no runners";
//   - the dialog previews BEFORE it saves. Save stays disabled until the preview
//     for the current selection has arrived, and what the server says would
//     change is on screen in words;
//   - what goes on the wire is the full selected set, ghosts included when they
//     are left ticked — the endpoint is a full replace.

type BoundRunner = components["schemas"]["BoundRunner"];
type Preview = components["schemas"]["ScopeRunnersPreview"];

const calls: { method: string; path: string; body?: unknown }[] = [];
let fleet: unknown[] = [];
let notices: unknown[] = [];
let preview: (runnerIds: string[]) => Preview;
let putResult: { data?: unknown; error?: unknown } = { data: {} };
let replaceResult: { data?: unknown; error?: unknown } = { data: { scopes: ["dmz-web", "dmz-db"] } };

vi.mock("../../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/client")>();
  return {
    ...actual,
    api: {
      GET: vi.fn(async (path: string) => {
        if (path === "/runners") return { data: fleet };
        if (path === "/scope-binding-notices") return { data: notices };
        return { data: [] };
      }),
      POST: vi.fn(async (path: string, opts: { body?: { runnerIds?: string[] } }) => {
        calls.push({ method: "POST", path, body: opts.body });
        if (path === "/scopes/{scopeId}/runners/preview") return { data: preview(opts.body?.runnerIds ?? []) };
        if (path === "/scope-runners/replace") return replaceResult;
        return { data: { dismissed: 1 } };
      }),
      PUT: vi.fn(async (path: string, opts: { body?: unknown }) => {
        calls.push({ method: "PUT", path, body: opts.body });
        return putResult;
      }),
    } as unknown as typeof actual.api,
  };
});

import { BindingNotices, BoundRunnersCell, ReplaceRunnerDialog, RunsOn, ScopeRunnersField, type BindableScope } from "./ScopeRunners";

const br = (over: Partial<BoundRunner> & { name: string }): BoundRunner => ({
  runnerId: `id-${over.name}`,
  registered: true,
  status: "online",
  eligible: true,
  ...over,
});
const emptyPreview = (ids: string[], over: Partial<Preview> = {}): Preview => ({
  scope: "dmz-web",
  currentlyBound: false,
  willBeBound: ids.length > 0,
  runnersLosing: [],
  runnersGaining: [],
  jobsNeedingAgent: [],
  runTypes: [],
  jobsNeedingInjection: 0,
  scopeHosts: 0,
  runners: [],
  ...over,
});

const FIN = { id: "ag-fin", name: "Finance" };
const scope = (bound: BoundRunner[] = []): BindableScope => ({
  id: "s-dmz",
  scope: "dmz-web",
  agencies: [FIN],
  boundRunners: bound,
});

beforeEach(() => {
  calls.length = 0;
  fleet = [];
  notices = [];
  preview = (ids) => emptyPreview(ids);
  putResult = { data: {} };
  replaceResult = { data: { scopes: ["dmz-web", "dmz-db"] } };
});
afterEach(cleanup);

describe("BoundRunnersCell — the catalog column", () => {
  it("says plainly when a scope is not bound", () => {
    const { container } = render(<BoundRunnersCell bound={[]} />);
    expect(container.textContent).toBe("any eligible");
  });

  it("names the runner, and counts the rest, without a warning while one can serve", () => {
    const { container } = render(<BoundRunnersCell bound={[br({ name: "runner-dmz-01" }), br({ name: "runner-dmz-02", status: "offline" })]} />);
    expect(container.textContent).toBe("runner-dmz-01 +1");
    expect(within(container).queryByLabelText("No bound runner can claim work")).toBeNull();
  });

  it("warns when nobody bound can claim work — the scope's runs are waiting", () => {
    const { container } = render(<BoundRunnersCell bound={[br({ name: "runner-dmz-01", registered: false, status: "", eligible: false })]} />);
    expect(within(container).getByLabelText("No bound runner can claim work")).toBeTruthy();
    expect(container.textContent).toContain("runner-dmz-01");
  });
});

describe("ScopeRunnersField — the expanded row", () => {
  it("names the pool an unbound scope draws from, and offers to bind", () => {
    const { container } = render(<ScopeRunnersField scope={scope()} canEdit onSaved={vi.fn()} />);
    expect(container.textContent).toMatch(/Not bound — any runner that serves Finance may take this scope's runs: its agents, and the local\s+runner if it serves Finance/);
    expect(within(container).getByRole("button", { name: "Bind runners…" })).toBeTruthy();
  });

  it("says Global for a scope that lists no other agency", () => {
    const { container } = render(<ScopeRunnersField scope={{ ...scope(), agencies: [] }} canEdit onSaved={vi.fn()} />);
    expect(container.textContent).toMatch(/any runner that serves Global/);
  });

  it("keeps a deregistered runner on screen, says why nothing runs, and offers to replace it", () => {
    const ghost = br({ name: "runner-dmz-01", registered: false, status: "", eligible: false });
    const { container } = render(<ScopeRunnersField scope={scope([ghost])} canEdit onSaved={vi.fn()} />);
    expect(container.textContent).toContain("runner-dmz-01");
    expect(container.textContent).toContain("deregistered");
    expect(container.textContent).toMatch(/No bound runner can claim work right now/);
    expect(within(container).getByRole("button", { name: "replace" })).toBeTruthy();
  });

  it("names each reason a bound runner is not serving", () => {
    const { container } = render(
      <ScopeRunnersField
        scope={scope([br({ name: "a" }), br({ name: "b", status: "offline" }), br({ name: "c", eligible: false })])}
        canEdit
        onSaved={vi.fn()}
      />,
    );
    expect(container.textContent).toContain("b· offline");
    expect(container.textContent).toContain("c· not in this scope's agency");
    // One can serve, so the scope is not flagged as waiting.
    expect(container.textContent).not.toMatch(/No bound runner can claim work/);
  });

  it("is read-only for a caller who cannot configure the app", () => {
    const ghost = br({ name: "runner-dmz-01", registered: false, status: "", eligible: false });
    const { container } = render(<ScopeRunnersField scope={scope([ghost])} canEdit={false} onSaved={vi.fn()} />);
    expect(container.textContent).toContain("runner-dmz-01");
    expect(within(container).queryAllByRole("button")).toHaveLength(0);
  });
});

describe("ScopeRunnersDialog — choose, preview, save", () => {
  const open = async (bound: BoundRunner[] = []) => {
    const onSaved = vi.fn();
    const utils = render(<ScopeRunnersField scope={scope(bound)} canEdit onSaved={onSaved} />);
    fireEvent.click(within(utils.container).getByRole("button", { name: bound.length ? "Change…" : "Bind runners…" }));
    const dialog = within(document.body);
    await waitFor(() => expect(dialog.getByText("What saving will change")).toBeTruthy());
    // The name can also appear on the field's own chip behind the dialog; the
    // checkbox is the one inside a <label>.
    const box = (name: string) =>
      dialog
        .getAllByText(name)
        .map((el) => el.closest("label"))
        .find(Boolean)!
        .querySelector("input") as HTMLInputElement;
    // "Bind 2 runners" / "Remove binding" — not the field's own "Bind runners…".
    const save = () => dialog.getAllByRole("button").find((b) => /^(Bind \d|Remove binding|Saving)/.test(b.textContent ?? "")) as HTMLButtonElement;
    return { dialog, box, save, onSaved };
  };

  it("offers eligible runners, disables the rest with the reason, and lists a ghost so it can be removed", async () => {
    fleet = [
      { id: "id-fin-1", name: "runner-fin-01", status: "online", agencies: [FIN] },
      { id: "id-tax-1", name: "runner-tax-01", status: "online", agencies: [{ id: "ag-tax", name: "Tax" }] },
      { id: "id-pool", name: "runner-pool-01", status: "online", agencies: [] },
    ];
    const ghost = br({ name: "runner-old", registered: false, status: "", eligible: false });
    const { dialog, box } = await open([ghost]);
    expect(box("runner-fin-01").disabled).toBe(false);
    expect(box("runner-tax-01").disabled).toBe(true);
    expect(box("runner-pool-01").disabled).toBe(true);
    // Both are refused for the same reason on an agency scope, and each row says it.
    expect(dialog.getAllByText("does not serve Finance")).toHaveLength(2);
    // The ghost is ticked (it is bound) and can be unticked.
    expect(box("runner-old").checked).toBe(true);
    expect(box("runner-old").disabled).toBe(false);
    expect(dialog.getByText("deregistered — untick to remove")).toBeTruthy();
  });

  // The local runner is a runner like the others here (LR-62): a scope it
  // serves may be bound to it.
  it("offers the local runner as a binding target when it serves the scope's agency", async () => {
    fleet = [
      { id: "id-fin-1", name: "runner-fin-01", status: "online", agencies: [FIN] },
      { id: "id-local", name: "Local runner", kind: "server", status: "online", agencies: [FIN] },
    ];
    const { box } = await open();
    expect(box("runner-fin-01").disabled).toBe(false);
    expect(box("Local runner").disabled).toBe(false);
  });

  it("keeps Save disabled until something changed AND its preview has arrived, then sends the whole set", async () => {
    fleet = [{ id: "id-fin-1", name: "runner-fin-01", status: "online", agencies: [FIN] }];
    const ghost = br({ name: "runner-old", registered: false, status: "", eligible: false });
    const { box, save, onSaved } = await open([ghost]);
    expect(save().disabled).toBe(true); // nothing changed yet

    fireEvent.click(box("runner-fin-01"));
    // The preview for the NEW selection has to land before Save opens.
    await waitFor(() => expect(save().disabled).toBe(false));
    expect(save().textContent).toBe("Bind 2 runners");
    fireEvent.click(save());
    await waitFor(() => expect(onSaved).toHaveBeenCalled());
    const put = calls.find((c) => c.method === "PUT")!;
    expect(put.path).toBe("/scopes/{scopeId}/runners");
    // Full replace: the ghost left ticked goes back with the new runner.
    expect((put.body as { runnerIds: string[] }).runnerIds.sort()).toEqual(["id-fin-1", "id-runner-old"]);
  });

  it("puts what the server says would change on screen, in words", async () => {
    fleet = [{ id: "id-fin-1", name: "runner-fin-01", status: "online", agencies: [FIN] }];
    preview = (ids) =>
      emptyPreview(ids, {
        runnersLosing: ids.length
          ? [
              { runnerId: "id-local", name: "Local runner", local: true, status: "online" },
              { runnerId: "id-fin-2", name: "runner-fin-02", local: false, status: "offline" },
            ]
          : [],
        jobsNeedingInjection: ids.length ? 1 : 0,
        scopeHosts: 12,
        runners: ids.map((id) => ({
          runnerId: id,
          name: "runner-fin-01",
          local: false,
          registered: true,
          eligible: true,
          capabilities: ["bash"],
          missingRunTypes: ["ansible"],
          allowsSecretInjection: false,
          hostsWithoutKey: 3,
        })),
      });
    const { dialog, box, save } = await open();
    fireEvent.click(box("runner-fin-01"));
    await waitFor(() => expect(save().disabled).toBe(false));
    const text = document.body.textContent ?? "";
    // Unbound, every runner of the agency took the scope's runs — the server
    // among them. Bound, the ones not named stop.
    expect(text).toMatch(/2 runners will stop taking this scope's runs: Local runner \(this server\), runner-fin-02\./);
    expect(text).toMatch(/runner-fin-01 cannot run ansible jobs/);
    expect(text).toMatch(/runner-fin-01 may not receive secrets, and 1 job on this scope binds one/);
    // Whose host-key trust applies is decided by which runner takes the run.
    expect(text).toMatch(/runner-fin-01 does not yet trust 3 of this scope's 12 hosts/);
    // One runner is a single point of failure, and the dialog says so.
    expect(dialog.getByText(/One runner serves this scope/)).toBeTruthy();
  });

  it("says who starts taking the scope's runs when a binding is cleared, and labels the button for what it does", async () => {
    fleet = [{ id: "id-runner-dmz-01", name: "runner-dmz-01", status: "online", agencies: [FIN] }];
    preview = (ids) =>
      emptyPreview(ids, {
        currentlyBound: true,
        runnersGaining: ids.length === 0 ? [{ runnerId: "id-local", name: "Local runner", local: true, status: "offline" }] : [],
      });
    const { box, save } = await open([br({ name: "runner-dmz-01" })]);
    fireEvent.click(box("runner-dmz-01"));
    await waitFor(() => expect(save().disabled).toBe(false));
    expect(save().textContent).toBe("Remove binding");
    expect(document.body.textContent).toMatch(/1 runner will start taking this scope's runs:\s+Local runner \(this server\)/);
  });

  // Only an agent can deliver a key file (LR-47). Binding a scope to the local
  // runner alone leaves its key-bound jobs with nobody, and the dialog says so
  // before the save does it.
  it("warns when no agent would serve a scope that has key-bound jobs", async () => {
    fleet = [{ id: "id-local", name: "Local runner", kind: "server", status: "online", agencies: [FIN] }];
    preview = (ids) =>
      emptyPreview(ids, {
        jobsNeedingAgent: ids.length ? [{ uid: "u1", name: "deploy", source: "git", runType: "bash" }] : [],
      });
    const { box, save } = await open();
    fireEvent.click(box("Local runner"));
    await waitFor(() => expect(save().disabled).toBe(false));
    const text = document.body.textContent ?? "";
    expect(text).toMatch(/1 job binds an\s+SSH key, and no agent will serve this scope\. Only an agent can deliver a key file, so\s+its runs will be refused: deploy/);
  });

  it("shows the server's refusal and stays open", async () => {
    fleet = [{ id: "id-fin-1", name: "runner-fin-01", status: "online", agencies: [FIN] }];
    putResult = { error: { code: "runner_not_eligible", message: "runner runner-fin-01 is not eligible for scope dmz-web" } };
    const { dialog, box, save, onSaved } = await open();
    fireEvent.click(box("runner-fin-01"));
    await waitFor(() => expect(save().disabled).toBe(false));
    fireEvent.click(save());
    await waitFor(() => expect(dialog.getByRole("alert").textContent).toMatch(/not eligible/));
    expect(onSaved).not.toHaveBeenCalled();
  });
});

describe("ReplaceRunnerDialog — hand a runner's scopes over", () => {
  it("posts both ids and reports the scopes that moved", async () => {
    fleet = [
      { id: "id-old", name: "runner-old", status: "offline" },
      { id: "id-new", name: "runner-new", status: "online" },
    ];
    const onDone = vi.fn();
    render(<ReplaceRunnerDialog from={{ id: "id-old", name: "runner-old" }} onClose={vi.fn()} onDone={onDone} />);
    const dialog = within(document.body);
    const select = (await waitFor(() => dialog.getByRole("combobox"))) as HTMLSelectElement;
    // The runner being replaced is not offered as its own replacement.
    expect([...select.options].map((o) => o.value)).toEqual(["", "id-new"]);
    const replace = () => dialog.getByRole("button", { name: "Replace" }) as HTMLButtonElement;
    expect(replace().disabled).toBe(true);
    fireEvent.change(select, { target: { value: "id-new" } });
    fireEvent.click(replace());
    await waitFor(() => expect(onDone).toHaveBeenCalled());
    expect(calls.find((c) => c.path === "/scope-runners/replace")!.body).toEqual({ fromRunnerId: "id-old", toRunnerId: "id-new" });
    expect(onDone.mock.calls[0][0]).toBe("runner-new now serves dmz-web, dmz-db in place of runner-old");
  });

  it("shows the refusal that names the scopes in the way", async () => {
    fleet = [{ id: "id-new", name: "runner-new", status: "online" }];
    replaceResult = { error: { code: "runner_not_eligible", message: "runner runner-new is not eligible for scope tax-t" } };
    const onDone = vi.fn();
    render(<ReplaceRunnerDialog from={{ id: "id-old", name: "runner-old" }} onClose={vi.fn()} onDone={onDone} />);
    const dialog = within(document.body);
    const select = (await waitFor(() => dialog.getByRole("combobox"))) as HTMLSelectElement;
    fireEvent.change(select, { target: { value: "id-new" } });
    fireEvent.click(dialog.getByRole("button", { name: "Replace" }));
    await waitFor(() => expect(dialog.getByRole("alert").textContent).toMatch(/tax-t/));
    expect(onDone).not.toHaveBeenCalled();
  });
});

describe("BindingNotices — pins that could not become a binding", () => {
  const pin = (id: number, jobName: string, scopeName: string, reason: string) => ({
    id,
    jobUid: `u${id}`,
    jobName,
    jobSource: "git",
    scope: scopeName,
    runnerTag: "vlan-dmz",
    reason,
    recordedAt: "2026-10-05T00:00:00Z",
  });

  const show = () =>
    render(
      <MemoryRouter>
        <BindingNotices dep={0} />
      </MemoryRouter>,
    );

  it("renders nothing when there is nothing to resolve", async () => {
    const { container } = show();
    await waitFor(() => expect(container.textContent).toBe(""));
  });

  // LR-85 — the list, the reason for each pin and the dismissal are in the
  // Notices inbox now. What stays here is the count, the scopes concerned and
  // where to act, because the remedy (binding the scope) is made on this page.
  it("says how many jobs lost their confinement and on which scopes, and points at Notices", async () => {
    notices = [pin(1, "deploy", "mixed", "mixed_pins"), pin(2, "restart", "mixed", "mixed_pins"), pin(3, "sweep", "", "no_scope")];
    const { container } = show();
    const q = within(container);
    await waitFor(() => expect(container.textContent).toMatch(/3 jobs used to be confined to particular runners/));
    expect(container.textContent).toMatch(/\(scope mixed\)/);
    expect(container.textContent).toMatch(/Bind the scope's runners below to confine them again\./);
    expect((q.getByRole("link", { name: "Review in Notices" }) as HTMLAnchorElement).getAttribute("href")).toBe("/notices");
    // It holds no list of its own and decides nothing here.
    expect(q.queryByRole("button")).toBeNull();
    expect(calls.find((c) => c.path === "/scope-binding-notices/dismiss")).toBeUndefined();
  });
});

describe("RunsOn — the job detail's answer", () => {
  // LR-50 — the row states the claim rule, by run type: there is no executor
  // beside it to say that a shell job may also be taken by the server.
  it("states the rule when the scope is not bound: any runner for a shell job, an agent for a toolchain one", () => {
    const shell = render(<RunsOn bound={[]} scope="dmz-web" runType="bash" />);
    expect(shell.container.textContent).toBe("Any runner that serves its scope");
    // The finer print — the server may be that runner; a key binding makes it
    // an agent's only — is the tooltip's, since it is not true of every job.
    expect(shell.container.querySelector("span")?.title).toMatch(/local runner \(this server\).*binds an SSH key is taken by an agent only/);
    cleanup();
    const ansible = render(<RunsOn bound={[]} scope="dmz-web" runType="ansible" />);
    expect(ansible.container.textContent).toBe("Any agent that serves its scope");
    cleanup();
    // A job with no scope is the Global agency's.
    const unscoped = render(<RunsOn bound={[]} scope="" runType="bash" />);
    expect(unscoped.container.textContent).toBe("Any runner that serves the Global agency");
  });

  it("names the bound runners, why one is not serving, and the scope that binds them", () => {
    const { container } = render(<RunsOn bound={[br({ name: "a" }), br({ name: "b", status: "offline" })]} scope="dmz-web" />);
    expect(container.textContent).toBe("ab· offlinebound to scope dmz-web");
  });
});

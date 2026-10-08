// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

// v2.3.0 (Phase G3/G4, MA-26) — a runner has an OWNER, and an agent serves
// exactly the agency that owns it. What the Runners view owes the operator:
//
//   - who owns each runner and what it serves, with nothing to edit on an agent;
//   - on a LEGACY PLACEMENT (it served several agencies before 2.3.0): the
//     agencies it serves, removable and never addable, a badge that leads to its
//     notice, and the hand-over once it serves one;
//   - row controls that follow the server's per-row `canManage`, disabled with
//     whose runner it is, not hidden — and Scan keys for an administrator of an
//     agency the runner serves (LR-63);
//   - a registration token that says whose agent it will enrol.

const base = { status: "online", protocolVersion: 14, capabilities: ["bash"], canManage: true, canReviewHostKeys: true, legacyPlacement: false };
const FIN = { id: "ag-fin", name: "Finance" };
const TAX = { id: "ag-tax", name: "Tax" };
const GLOBAL = { id: "global", name: "Global" };

const AGENT = { ...base, id: "r-fin", name: "fin-agent", ownerAgency: FIN, agencies: [FIN] };
const THEIRS = { ...base, id: "r-tax", name: "tax-agent", ownerAgency: TAX, agencies: [TAX], canManage: false, canReviewHostKeys: false };
const LEGACY = { ...base, id: "r-legacy", name: "legacy-runner", ownerAgency: GLOBAL, agencies: [FIN, TAX], legacyPlacement: true };
const LEGACY_GUEST = { ...LEGACY, canManage: false, canReviewHostKeys: true };
const NARROWED = { ...base, id: "r-narrow", name: "narrowed-runner", ownerAgency: GLOBAL, agencies: [FIN], legacyPlacement: true };

let RUNNERS: unknown[] = [];
let CAPS: Record<string, boolean> = {};
let ACCESS: unknown = null;
let TOKENS: unknown[] = [];
let SCOPES: unknown[] = [];
let LOCAL_STATE: Record<string, unknown> = {};
let HOST_KEYS: unknown = { inForce: [], history: [], knownHosts: { reportedAt: null, truncated: false, entries: [] }, pending: 0 };

const { POST, PUT, PATCH } = vi.hoisted(() => ({
  PATCH: vi.fn(async (_path: string, _init?: unknown) => ({ data: {} }) as { data?: unknown; error?: unknown }),
  POST: vi.fn(async (_path: string, _init?: unknown) => ({ data: { id: 1, token: "crn_reg_x", status: "pending" } }) as { data?: unknown; error?: unknown }),
  PUT: vi.fn(async (_path: string, _init?: unknown) => ({ data: [] }) as { data?: unknown; error?: unknown }),
}));

vi.mock("../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/client")>();
  return {
    ...actual,
    api: {
      GET: vi.fn(async (path: string) => {
        if (path === "/runners") return { data: { items: RUNNERS } };
        if (path === "/runners/host-keys/pending") return { data: [] };
        if (path === "/runners/registration-tokens") return { data: TOKENS };
        if (path === "/agencies") return { data: [GLOBAL, FIN, TAX] };
        if (path === "/scopes") return { data: SCOPES };
        if (path === "/local-runner") return { data: LOCAL_STATE };
        if (path === "/runners/{runnerId}/host-keys") return { data: HOST_KEYS };
        return { data: { items: [] } };
      }),
      POST,
      PUT,
      PATCH,
    } as unknown as typeof actual.api,
    fetchCapabilities: vi.fn(async () => ({ configureApp: true, ...CAPS })),
    fetchVersion: vi.fn(async () => ({ version: "2.3.0" })),
  };
});
vi.mock("../api/access", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/access")>();
  // useMyAccess calls fetchMyAccess from inside its own module, where a mocked
  // export is not seen; the hook itself is what the view consumes.
  return { ...actual, fetchMyAccess: vi.fn(async () => ACCESS), useMyAccess: () => ACCESS };
});

import { Runners } from "./Runners";

const grant = (a: { id: string; name: string }, permissions: string[]) => ({
  role: "admin", allScopes: false, agencyId: a.id, agencyName: a.name, scopes: [], permissions, groups: [], origin: "group",
});

beforeEach(() => {
  RUNNERS = [AGENT, THEIRS, LEGACY, NARROWED];
  CAPS = { configureAppGlobal: true };
  ACCESS = null;
  TOKENS = [];
  SCOPES = [];
  LOCAL_STATE = { runnerId: "r-local", enabled: false, forbidden: false, status: "offline", maxConcurrent: 4, serves: [GLOBAL] };
  HOST_KEYS = { inForce: [], history: [], knownHosts: { reportedAt: null, truncated: false, entries: [] }, pending: 0 };
});
afterEach(() => {
  cleanup();
  POST.mockClear();
  PUT.mockClear();
});

const renderRunners = () =>
  render(
    <MemoryRouter>
      <Runners />
    </MemoryRouter>,
  );
const expand = async (name: string) => {
  fireEvent.click(await screen.findByRole("button", { name: new RegExp(`Runner ${name} — expand details`) }));
  const heading = await screen.findByText("Agency", { selector: "h3, h4, div, span" }).catch(() => null);
  return heading;
};
const agencySection = async () => {
  const owner = await screen.findByText("Owner");
  return within(owner.parentElement!.parentElement!);
};

describe("Runners — owner and serves (MA-26)", () => {
  it("shows an agent's owner and the one agency it serves, with nothing to edit", async () => {
    renderRunners();
    await expand("fin-agent");
    const sec = await agencySection();
    expect(sec.getByText("Serves")).toBeTruthy();
    expect(sec.getAllByText("Finance").length).toBeGreaterThanOrEqual(2); // owner, and the serve chip
    expect(sec.getByText(/An agent serves the agency that owns it/)).toBeTruthy();
    // No remove control, no badge, no hand-over: there is nothing to change.
    expect(sec.queryByRole("button", { name: /Stop serving/ })).toBeNull();
    expect(sec.queryByText("Legacy placement")).toBeNull();
    expect(sec.queryByRole("button", { name: /^Hand to/ })).toBeNull();
    // And no way to ADD an agency anywhere in the section.
    expect(sec.queryByRole("combobox")).toBeNull();
  });

  it("lets a legacy placement lose an agency, never gain one, and links to its notice", async () => {
    renderRunners();
    await expand("legacy-runner");
    const sec = await agencySection();
    expect(sec.getByText("Global")).toBeTruthy(); // the owner
    const badge = sec.getByText("Legacy placement") as HTMLAnchorElement;
    expect(badge.getAttribute("href")).toBe("/notices");
    expect(sec.queryByRole("combobox")).toBeNull();
    // Serving two, it cannot be handed over yet.
    expect(sec.queryByRole("button", { name: /^Hand to/ })).toBeNull();

    // It cannot be undone, so it is confirmed first, and the dialog says so.
    fireEvent.click(sec.getByRole("button", { name: "Stop serving Tax" }));
    expect(PUT).not.toHaveBeenCalled();
    expect(await screen.findByText(/This cannot be undone:/)).toBeTruthy();
    const confirm = screen.getAllByRole("button", { name: "Stop serving Tax" });
    fireEvent.click(confirm[confirm.length - 1]);
    await waitFor(() => expect(PUT).toHaveBeenCalled());
    // What is posted is the list it has, with that agency taken away.
    expect(PUT).toHaveBeenCalledWith(
      "/runner-agencies",
      expect.objectContaining({ body: [{ runnerId: "r-legacy", agencyIds: ["ag-fin"] }] }),
    );
  });

  it("offers the hand-over once a legacy placement serves one agency, behind a confirmation", async () => {
    renderRunners();
    await expand("narrowed-runner");
    const sec = await agencySection();
    // The last agency cannot be taken away: a runner always serves one.
    expect(sec.queryByRole("button", { name: /Stop serving/ })).toBeNull();
    fireEvent.click(sec.getByRole("button", { name: "Hand to Finance" }));
    expect(POST).not.toHaveBeenCalled();
    expect(await screen.findByText(/Finance's administrators will manage this runner from now on/)).toBeTruthy();
    // Two buttons carry the words now: the one in the row, and the dialog's.
    const buttons = screen.getAllByRole("button", { name: "Hand to Finance" });
    expect(buttons).toHaveLength(2);
    fireEvent.click(buttons[buttons.length - 1]);
    await waitFor(() =>
      expect(POST).toHaveBeenCalledWith(
        "/runners/{runnerId}/owner",
        expect.objectContaining({ params: expect.objectContaining({ path: { runnerId: "r-narrow" } }), body: { agencyId: "ag-fin" } }),
      ),
    );
  });

  it("disables a runner's controls for someone who does not administer its owner, and says whose it is", async () => {
    CAPS = { configureAppGlobal: false };
    RUNNERS = [THEIRS];
    renderRunners();
    await expand("tax-agent");
    // Every write on the row is its owner's: the three in the detail's action
    // strip, the two on the compact row, the managed-settings editor.
    for (const name of ["Resync", "Drain", "Scan keys", "Test", "Deregister", "⚙ Edit"]) {
      const btn = (await screen.findByRole("button", { name })) as HTMLButtonElement;
      expect(btn.disabled, name).toBe(true);
      expect(btn.title, name).toMatch(/This runner belongs to Tax — only that agency's administrators can manage it\./);
    }
    // And its tags are read-only: the editor shows them without its add field.
    expect(screen.queryByPlaceholderText("Add a tag…")).toBeNull();
    // The owner-only panels are not offered at all: their reads are refused.
    expect(screen.queryByText("Trusted host keys")).toBeNull();
  });

  // v2.3.1 — the sandbox caps ride on a per-run systemd scope, which an agent
  // that is not root cannot create. The dialog where they are typed says so for
  // a runner that reports no sandbox, and says nothing where there is one.
  it("says the sandbox caps do nothing on a runner that reports no sandbox", async () => {
    RUNNERS = [{ ...AGENT, toolchains: { sandboxed: false } }];
    renderRunners();
    await expand("fin-agent");
    fireEvent.click(await screen.findByRole("button", { name: "⚙ Edit" }));
    expect(await screen.findByText(/This runner reports no sandbox, so these caps do nothing on it\./)).toBeTruthy();
    // And it points at what does limit the runner: the section below.
    expect(screen.getByText("Unit limits", { selector: "strong" })).toBeTruthy();
  });

  it("offers the sandbox caps without that warning on a runner that has a sandbox", async () => {
    RUNNERS = [{ ...AGENT, toolchains: { sandboxed: true } }];
    renderRunners();
    await expand("fin-agent");
    fireEvent.click(await screen.findByRole("button", { name: "⚙ Edit" }));
    expect(await screen.findByPlaceholderText("memory (2G)")).toBeTruthy();
    expect(screen.queryByText(/these caps do nothing on it/)).toBeNull();
  });

  // v2.3.1 — the limit that does bound such a runner is on its systemd unit,
  // and neither the server nor the agent can set it. The drawer builds the
  // command for root on the machine, for THIS runner, and saves nothing.
  // v2.3.2 — how often an agent asks for work is a managed setting. The agent's
  // own interval lives in its configuration on its host; this is the one the
  // server sends it, and the server refuses a value outside 30–90 seconds. The
  // field says so before Save can be pressed.
  it("sets a runner's poll interval, and holds it within the server's bounds", async () => {
    PATCH.mockClear();
    RUNNERS = [{ ...AGENT, managedSettings: { pollIntervalSeconds: 30 } }];
    renderRunners();
    await expand("fin-agent");
    // The summary shows what is in force.
    expect(await screen.findByText("30s")).toBeTruthy();
    fireEvent.click(await screen.findByRole("button", { name: "⚙ Edit" }));
    const field = (await screen.findByLabelText("Poll interval in seconds")) as HTMLInputElement;
    expect(field.value).toBe("30");
    // The field says what the number buys. The server holds a poll for 30
    // seconds, so the wait for a new run is the interval LESS 30 — which is why
    // 30 is the floor (the agent is then connected all the time) — and it says
    // what the number does NOT decide: how soon a Stop reaches a job.
    expect(screen.getByText("the longest a new run waits is this value less 30", { selector: "strong" })).toBeTruthy();
    expect(screen.getByText(/an\s+agent with a run in flight checks in every few seconds/)).toBeTruthy();
    const save = screen.getByRole("button", { name: "Save" }) as HTMLButtonElement;

    for (const bad of ["29", "120"]) {
      fireEvent.change(field, { target: { value: bad } });
      expect(screen.getByText(/a runner that has not asked\s+for two minutes is shown as degraded/)).toBeTruthy();
      expect(save.disabled, `Save with an interval of ${bad}`).toBe(true);
    }
    fireEvent.click(save);
    expect(PATCH).not.toHaveBeenCalled();

    fireEvent.change(field, { target: { value: "45" } });
    expect(screen.queryByText(/is shown as degraded/)).toBeNull();
    expect(save.disabled).toBe(false);
    fireEvent.click(save);
    await waitFor(() => expect(PATCH).toHaveBeenCalledTimes(1));
    const [path, init] = PATCH.mock.calls[0] as [string, { body: unknown }];
    expect(path).toBe("/runners/{runnerId}/settings");
    expect(init.body).toEqual({ pollIntervalSeconds: 45 });
  });

  it("builds the command that sets a runner's unit limits, and sends nothing to the server", async () => {
    const id = "01a11cd7-2939-7abe-ae1b-8862a7262645";
    RUNNERS = [{ ...AGENT, id, toolchains: { sandboxed: false } }];
    renderRunners();
    await expand("fin-agent");
    fireEvent.click(await screen.findByRole("button", { name: "⚙ Edit" }));
    expect(await screen.findByText("Unit limits", { selector: "div" })).toBeTruthy();
    expect(screen.getByText(/Save does not apply this section\./)).toBeTruthy();
    // The server is not told whether a runner is a container, so the section
    // says what to do for one: it has no unit for the command to find.
    expect(screen.getByText("If this runner is a container")).toBeTruthy();
    expect(screen.getByText(/docker update --memory 4g --memory-swap 4g --cpus 2 --pids-limit 1024 <container>/)).toBeTruthy();
    // Nothing to run until a limit is typed.
    expect(screen.queryByLabelText("Limits command")).toBeNull();

    fireEvent.change(screen.getByPlaceholderText("e.g. 4G"), { target: { value: "4G" } });
    fireEvent.change(screen.getByPlaceholderText("e.g. 200%"), { target: { value: "none" } });
    const cmd = (await screen.findByLabelText("Limits command")).textContent ?? "";
    expect(cmd).toContain(`RUNNER_ID="${id}"`);
    expect(cmd).toContain('systemctl set-property "$UNIT" MemoryMax=4G CPUQuota=');
    expect(cmd).not.toContain("TasksMax=");

    // A value that is not a limit: no command, and the field group says why.
    fireEvent.change(screen.getByPlaceholderText("e.g. 1024"), { target: { value: "lots" } });
    await waitFor(() => expect(screen.queryByLabelText("Limits command")).toBeNull());
    expect(screen.getByText(/No command is shown until every field is valid, or empty\./)).toBeTruthy();
    expect(PUT).not.toHaveBeenCalled();
  });

  // v2.3.1 — Add Runner offers the installer's three limit flags, beside the
  // instance name, and the copied command carries what was typed.
  it("writes the limits typed in Add Runner into the install command, and none when a value is not valid", async () => {
    RUNNERS = [AGENT];
    renderRunners();
    await screen.findByText("fin-agent");
    fireEvent.click(screen.getByRole("button", { name: "+ Add Runner" }));
    fireEvent.click(await screen.findByRole("button", { name: "Mint & build command" }));
    await screen.findByText(/\/install\/crn_reg_x \| sudo bash$/);
    // The page's other install helper shows the same fields over the same
    // values (they describe the machine); type into the dialog's, the last.
    const field = (placeholder: string) => screen.getAllByPlaceholderText(placeholder).at(-1) as HTMLInputElement;

    fireEvent.change(field("e.g. 4G"), { target: { value: "4G" } });
    fireEvent.change(field("e.g. 1024"), { target: { value: "512" } });
    expect(await screen.findByText(/\/install\/crn_reg_x \| sudo bash -s -- --memory-max 4G --tasks-max 512$/)).toBeTruthy();
    fireEvent.change(field("e.g. tax"), { target: { value: "tax" } });
    expect(await screen.findByText(/\| sudo bash -s -- --instance tax --memory-max 4G --tasks-max 512$/)).toBeTruthy();

    fireEvent.change(field("e.g. 200%"), { target: { value: "200" } });
    expect((await screen.findAllByText("# A resource limit is not valid, so no install command is shown. Correct it above.")).length).toBeGreaterThan(0);
    expect(screen.queryByText(/sudo bash -s -- --instance tax/)).toBeNull();
  });

  it("leaves every control live when the server sent no per-row flag — unknown is not 'not yours'", async () => {
    CAPS = { configureAppGlobal: false };
    const { canManage: _m, canReviewHostKeys: _r, ...unflagged } = THEIRS;
    RUNNERS = [unflagged];
    renderRunners();
    await expand("tax-agent");
    for (const name of ["Resync", "Drain", "Scan keys", "Test", "Deregister", "⚙ Edit"]) {
      expect(((await screen.findByRole("button", { name })) as HTMLButtonElement).disabled, name).toBe(false);
    }
    expect(screen.getByPlaceholderText("Add a tag…")).toBeTruthy();
  });

  // LR-62 — replacing a runner on its scopes is judged by the server on the
  // SCOPES, not on who owns the runner: an agency whose scopes are bound to a
  // runner Global owns may hand them to its own. So the button follows the flat
  // permission, where every other control on this row follows the owner.
  it("offers Replace this runner to a configureApp holder who does not own the runner", async () => {
    CAPS = { configureAppGlobal: false };
    RUNNERS = [LEGACY_GUEST];
    SCOPES = [{ id: "s1", scope: "fin-web", boundRunners: [{ runnerId: "r-legacy", name: "legacy-runner", registered: true, status: "online", eligible: true }] }];
    renderRunners();
    await expand("legacy-runner");
    expect(((await screen.findByRole("button", { name: "Replace this runner…" })) as HTMLButtonElement).disabled).toBe(false);
  });

  it("gives an administrator of an agency a legacy placement serves Scan keys, in guest mode, and nothing else", async () => {
    CAPS = { configureAppGlobal: false };
    RUNNERS = [LEGACY_GUEST];
    renderRunners();
    await expand("legacy-runner");
    for (const name of ["Resync", "Drain"]) {
      const btn = (await screen.findByRole("button", { name })) as HTMLButtonElement;
      expect(btn.disabled).toBe(true);
      expect(btn.title).toMatch(/This runner is Global's — only a global administrator/);
    }
    // They cannot narrow it or hand it over either.
    const sec = await agencySection();
    expect((sec.getByRole("button", { name: "Stop serving Tax" }) as HTMLButtonElement).disabled).toBe(true);

    const scan = screen.getByRole("button", { name: "Scan keys" }) as HTMLButtonElement;
    expect(scan.disabled).toBe(false);
    fireEvent.click(scan);
    expect(await screen.findByText(/is not your agency's runner, but it serves your agency/)).toBeTruthy();
    expect(screen.queryByText("Paste keys")).toBeNull();
  });
});

// The local runner — this server running shell jobs itself (LR-38). It is listed
// with the agents and has no agent: nothing on its row deregisters, drains,
// re-declares, tests or upgrades it, and it points at where it is turned on.
describe("Runners — the local runner's row (LR-38)", () => {
  const LOCAL = { ...base, id: "r-local", name: "Local runner", kind: "server", status: "offline", ownerAgency: GLOBAL, agencies: [GLOBAL, FIN], allowSecretInjection: true };

  it("badges it as this server, with a link to its settings where an agent has Test and Deregister", async () => {
    RUNNERS = [LOCAL, AGENT];
    renderRunners();
    const row = (await screen.findByText("Local runner")).closest("tr")!;
    expect(within(row).getByText("this server")).toBeTruthy();
    expect(within(row).queryByRole("button", { name: "Test" })).toBeNull();
    expect(within(row).queryByRole("button", { name: "Deregister" })).toBeNull();
    expect((within(row).getByRole("link", { name: "Settings" }) as HTMLAnchorElement).getAttribute("href")).toBe("/settings?tab=localrunner");
    // It serves more than its owner by design: not a legacy placement.
    expect(within(row).queryByText("legacy")).toBeNull();
    // The agent beside it keeps its own.
    const agentRow = screen.getByText("fin-agent").closest("tr")!;
    expect(within(agentRow).getByRole("button", { name: "Deregister" })).toBeTruthy();
  });

  it("shows what it serves, where that is changed, and none of the agent's sections", async () => {
    RUNNERS = [{ ...LOCAL, status: "online" }];
    renderRunners();
    await expand("Local runner");
    const sec = await agencySection();
    expect(sec.getByText(/A global administrator chooses the agencies it serves under/)).toBeTruthy();
    expect(sec.queryByRole("button", { name: /Stop serving/ })).toBeNull();
    expect(sec.queryByText("Legacy placement")).toBeNull();
    for (const name of ["Resync", "Drain", "⚙ Edit"]) expect(screen.queryByRole("button", { name }), name).toBeNull();
    // It is bound to scopes like any runner, and its row says which.
    expect(screen.getByText(/Scopes served/)).toBeTruthy();
    expect(screen.queryByText("Copy upgrade command")).toBeNull();
    expect(screen.getByText(/Always on\. This server is the secret store/)).toBeTruthy();
  });

  // Its host keys are reviewed here like an agent's (2.3.0): it scans from this
  // server, on or off; an approved key is in force at once; and there is no
  // known_hosts file to send a key to or to read back.
  it("shows the keys approved for it, in force, with no file to report and nothing to send again", async () => {
    HOST_KEYS = {
      inForce: [
        { id: 7, host: "10.0.0.5", hostName: "web1", keyType: "ssh-ed25519", fingerprint: "SHA256:abc", source: "carried", actor: "upgrade", decidedAt: "2026-10-07T00:00:00Z", deliveredAt: "2026-10-07T00:00:00Z", presentInFile: false },
      ],
      history: [],
      knownHosts: { reportedAt: null, truncated: false, entries: [] },
      pending: 0,
    };
    RUNNERS = [LOCAL]; // offline: turned off
    renderRunners();
    await expand("Local runner");
    expect(await screen.findByText("In force")).toBeTruthy();
    expect(screen.getByText(/It connects only to a host that has one here/)).toBeTruthy();
    // Scanning is not running a job: it works with the local runner turned off.
    expect((screen.getByRole("button", { name: "Scan keys…" }) as HTMLButtonElement).disabled).toBe(false);
    expect(screen.queryByText(/Present in the runner's known_hosts file/)).toBeNull();
    expect(screen.queryByRole("button", { name: /Send the key for/ })).toBeNull();
    expect(screen.queryByText(/not in Local runner's known_hosts file/)).toBeNull();
    expect(screen.getByRole("button", { name: "Remove the key for web1" })).toBeTruthy();
  });
});

// The Settings section it is turned on in is a global administrator's. Nobody
// else is linked to it (they would land somewhere else), and everyone can read
// WHY its row is offline: turned off, or forbidden by the host.
describe("Runners — the local runner, for someone who is not a global administrator", () => {
  const LOCAL = { ...base, id: "r-local", name: "Local runner", kind: "server", status: "offline", ownerAgency: GLOBAL, agencies: [GLOBAL, FIN], canManage: false, canReviewHostKeys: false };

  it("links nobody but a global administrator to Settings → Local runner", async () => {
    CAPS = { configureAppGlobal: false };
    RUNNERS = [LOCAL];
    renderRunners();
    const row = (await screen.findByText("Local runner")).closest("tr")!;
    expect(within(row).queryByRole("link", { name: "Settings" })).toBeNull();
    await expand("Local runner");
    expect((await screen.findAllByText(/Settings → Local runner/)).length).toBeGreaterThan(0);
    expect(screen.queryByRole("link", { name: "Settings → Local runner" })).toBeNull();
    expect(screen.queryByRole("link", { name: "turn on the local runner" })).toBeNull();
    expect(screen.getByText(/a global administrator can also turn on the local runner/)).toBeTruthy();
  });

  it("says whether it is turned off or forbidden on the host", async () => {
    CAPS = { configureAppGlobal: false };
    RUNNERS = [LOCAL];
    const first = renderRunners();
    await expand("Local runner");
    expect(await screen.findByText(/It is turned off\./)).toBeTruthy();
    first.unmount();

    LOCAL_STATE = { ...LOCAL_STATE, forbidden: true };
    renderRunners();
    await expand("Local runner");
    expect(await screen.findByText(/It is forbidden on this host \(CRONOMICON_LOCAL_RUNNER=forbid is set there\)/)).toBeTruthy();
  });

  it("links a global administrator from every mention", async () => {
    RUNNERS = [{ ...LOCAL, canManage: true }];
    renderRunners();
    await expand("Local runner");
    const links = await screen.findAllByRole("link", { name: "Settings → Local runner" });
    expect(links.length).toBe(2);
    for (const a of links) expect(a.getAttribute("href")).toBe("/settings?tab=localrunner");
  });
});

describe("Runners — nothing to run a job (LR-38)", () => {
  const OFF = { ...base, id: "r-local", name: "Local runner", kind: "server", status: "offline", ownerAgency: GLOBAL, agencies: [GLOBAL] };
  const note = /Nothing can run a job yet: no agent is registered and the local runner is off\./;

  it("says so when the local runner is off and no agent is registered, and points at both ways out", async () => {
    RUNNERS = [OFF];
    renderRunners();
    expect(await screen.findByText(note)).toBeTruthy();
    expect((screen.getByRole("link", { name: "turn on the local runner" }) as HTMLAnchorElement).getAttribute("href")).toBe("/settings?tab=localrunner");
  });

  it("says nothing once the local runner is on, or an agent exists", async () => {
    RUNNERS = [{ ...OFF, status: "online" }];
    const first = renderRunners();
    await screen.findByText("Local runner");
    expect(screen.queryByText(note)).toBeNull();
    first.unmount();

    RUNNERS = [OFF, AGENT];
    renderRunners();
    await screen.findByText("fin-agent");
    expect(screen.queryByText(note)).toBeNull();
  });
});

describe("Runners — a registration token names the owner (LR-61)", () => {
  const ownerSelects = () => screen.getAllByRole("combobox", { name: "Agency the runner is for" }) as HTMLSelectElement[];

  it("offers a global administrator Global and every agency, and sends the one chosen", async () => {
    renderRunners();
    await screen.findByText("fin-agent");
    await waitFor(() => expect(ownerSelects()[0].options.length).toBe(3));
    const sel = ownerSelects()[0];
    expect(Array.from(sel.options).map((o) => o.textContent)).toEqual(["For Global", "For Finance", "For Tax"]);
    expect(sel.value).toBe("global");
    fireEvent.change(sel, { target: { value: "ag-tax" } });
    fireEvent.click(screen.getByRole("button", { name: "Mint token" }));
    await waitFor(() =>
      expect(POST).toHaveBeenCalledWith("/runners/registration-tokens", expect.objectContaining({ body: { agencyId: "ag-tax" } })),
    );
  });

  it("gives an administrator of one agency that agency, and nothing to choose", async () => {
    CAPS = { configureAppGlobal: false };
    ACCESS = { grants: [grant(FIN, ["configureApp"])] };
    renderRunners();
    await screen.findByText("fin-agent");
    await waitFor(() => expect(ownerSelects()[0].value).toBe("ag-fin"));
    expect(ownerSelects()[0].disabled).toBe(true);
    expect(Array.from(ownerSelects()[0].options).map((o) => o.textContent)).toEqual(["For Finance"]);
    fireEvent.click(screen.getByRole("button", { name: "Mint token" }));
    await waitFor(() =>
      expect(POST).toHaveBeenCalledWith("/runners/registration-tokens", expect.objectContaining({ body: { agencyId: "ag-fin" } })),
    );
  });

  it("makes an administrator of several agencies say which, before anything is minted", async () => {
    CAPS = { configureAppGlobal: false };
    ACCESS = { grants: [grant(FIN, ["configureApp"]), grant(TAX, ["configureApp"])] };
    renderRunners();
    await screen.findByText("fin-agent");
    await waitFor(() => expect(ownerSelects()[0].options.length).toBe(3));
    const mint = screen.getByRole("button", { name: "Mint token" }) as HTMLButtonElement;
    expect(mint.disabled).toBe(true);
    expect(mint.title).toBe("Choose the agency this runner is for");
    // Global is not on offer to them.
    expect(Array.from(ownerSelects()[0].options).map((o) => o.textContent)).toEqual(["Runner for…", "For Finance", "For Tax"]);
    fireEvent.change(ownerSelects()[0], { target: { value: "ag-tax" } });
    await waitFor(() => expect((screen.getByRole("button", { name: "Mint token" }) as HTMLButtonElement).disabled).toBe(false));
  });

  it("lists whose agent each token enrols, and says when that agency is gone", async () => {
    TOKENS = [
      { id: 1, label: "web-01", status: "pending", agencyId: "ag-fin", agencyName: "Finance", createdAt: "2026-10-07T00:00:00Z", expiresAt: "2026-10-08T00:00:00Z" },
      { id: 2, label: "old", status: "pending", agencyId: "ag-gone", agencyName: "", createdAt: "2026-10-07T00:00:00Z", expiresAt: "2026-10-08T00:00:00Z" },
    ];
    renderRunners();
    const row = (await screen.findByText("web-01")).closest("tr")!;
    expect(within(row).getByText("Finance")).toBeTruthy();
    const gone = (await screen.findByText("old")).closest("tr")!;
    expect(within(gone).getByText("deleted agency")).toBeTruthy();
    expect(screen.getByTitle("Sort by For")).toBeTruthy();
  });
});

// The Host keys dialog for the local runner: it scans from this server whether
// it is turned on or not (scanning is not running a job), so an "offline" local
// runner does not block a scan the way an offline agent does.
describe("Host keys dialog — the local runner", () => {
  it("lets a scan be queued while the local runner is turned off", async () => {
    RUNNERS = [{ ...base, id: "r-local", name: "Local runner", kind: "server", status: "offline", ownerAgency: GLOBAL, agencies: [GLOBAL] }];
    SCOPES = [{ id: "sc1", scope: "global-hosts", agencies: [GLOBAL], boundRunners: [] }];
    renderRunners();
    await expand("Local runner");
    fireEvent.click(await screen.findByRole("button", { name: "Scan keys…" }));
    const queue = (await screen.findByRole("button", { name: "Queue scan" })) as HTMLButtonElement;
    expect(queue.title).toBe("");
    expect(screen.queryByText(/is not online, so it cannot scan/)).toBeNull();
  });
});

// An agent always has the shell types to claim, so one whose lookup for its
// local toolchains never returned is online and looks healthy. The row is
// where an operator would see that Ansible is missing, so the row says the
// agent could not look — which is not the same as having none.
describe("Runners — a toolchain the agent could not look for", () => {
  it("says so on the row, and says nothing for an agent that simply has none", async () => {
    RUNNERS = [
      { ...AGENT, capabilities: ["bash", "perl", "powershell", "python"], toolchains: { undetermined: ["ansible", "terraform"] } },
      { ...THEIRS, capabilities: ["bash", "perl", "powershell", "python"], toolchains: { checkout: false } },
    ];
    renderRunners();
    const warning = await screen.findByText("could not check for ansible, terraform");
    expect(warning.getAttribute("title")).toContain("SystemCallErrorNumber=EPERM");
    expect(screen.getAllByText(/could not check for/).length).toBe(1);
  });
});

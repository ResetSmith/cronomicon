// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import type { ComponentProps } from "react";
import { cleanup, fireEvent, render, within } from "@testing-library/react";
import type { components } from "../api/schema";

// SB — the Run dialog on a scope that is bound to runners.
//
// Until 2.3.0 the dialog sent an executor, and on a bound scope had to send
// "runner" or be refused. There is no executor to send any more (LR-50): the
// claim decides who takes a run, and a binding is one of its rules. What these
// tests protect is what the operator is TOLD — the runners a bound scope's run
// goes to, by name, and the open rule on a scope nobody bound — and that no
// executor reaches the wire either way.

type BoundRunner = components["schemas"]["BoundRunner"];
let scopeRows: { scope: string; boundRunners: BoundRunner[] }[] = [];

vi.mock("../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/client")>();
  return {
    ...actual,
    api: {
      GET: vi.fn(async (path: string) => {
        if (path === "/scopes") return { data: scopeRows };
        if (path === "/runners") return { data: [] };
        if (path === "/job-reference-bindings/{jobId}") return { data: { bindings: [] } };
        if (path === "/script-reference-bindings/{name}") return { data: { bindings: [] } };
        if (path === "/env-vars" || path === "/env-secrets" || path === "/ssh/credentials") return { data: [] };
        return { data: undefined };
      }),
      POST: vi.fn(async () => ({ data: {} })),
    } as unknown as typeof actual.api,
    fetchCapabilities: vi.fn(async () => ({ manageEnvVars: false, configureApp: false })),
  };
});

import { RunDialog } from "./Jobs";

type Job = components["schemas"]["Job"];
type OnRun = ComponentProps<typeof RunDialog>["onRun"];

const bound = (name: string): BoundRunner => ({ runnerId: `id-${name}`, name, registered: true, status: "online", eligible: true });

afterEach(() => {
  cleanup();
  scopeRows = [];
});

// A shell job: the kind the local runner could take on a scope nobody bound.
const makeJob = (over: Partial<Job> = {}): Job =>
  ({ id: 1, name: "deploy-api", type: "bash", scope: "dmz-web", ...over }) as unknown as Job;

const renderDialog = (job: Job, onRunImpl?: OnRun, onDone: () => void = vi.fn()) => {
  const onRun = vi.fn<OnRun>(onRunImpl ?? (async () => ({ ok: true })));
  const { container } = render(
    <RunDialog job={job} scopes={[]} busy={false} onCancel={vi.fn()} onRun={onRun} onDone={onDone} />,
  );
  const q = within(container);
  const runBtn = () => {
    const buttons = q.getAllByRole("button");
    return buttons[buttons.length - 1] as HTMLButtonElement;
  };
  const openOptions = () => {
    fireEvent.click(q.getByRole("button", { name: /Targets/ }));
    fireEvent.click(q.getByRole("button", { name: /Method/ }));
  };
  // What the old executor cards were: buttons whose first line is the label.
  const card = (label: "SSH" | "Runner") => q.getAllByRole("button").find((b) => b.textContent?.startsWith(label));
  // RC — every run takes two presses: the first arms the confirmation window,
  // the second commits.
  const run = async () => {
    fireEvent.click(runBtn());
    fireEvent.click(runBtn());
    await vi.waitFor(() => expect(onRun).toHaveBeenCalled());
  };
 // The whole argument list, as text: an executor in ANY position would show.
  const sent = (call = 0) => JSON.stringify(onRun.mock.calls[call] ?? []);
  return { onRun, q, container, openOptions, card, run, sent };
};

describe("RunDialog — a scope bound to runners (SB)", () => {
  it("names the bound runners, offers no executor, and sends none", async () => {
    scopeRows = [{ scope: "dmz-web", boundRunners: [bound("runner-dmz-01"), bound("runner-dmz-02")] }];
    // A job row that still carries an executor from before 2.3.0 changes nothing.
    const { openOptions, card, run, sent, container, q } = renderDialog(makeJob({ executor: "ssh" } as Partial<Job>));
    openOptions();
    // The binding arrives after mount; the statement must follow it.
    await vi.waitFor(() => expect(container.textContent).toMatch(/Scope dmz-web is bound to these runners/));
    expect(q.getAllByText("runner-dmz-01").length).toBeGreaterThan(0);
    expect(q.getAllByText("runner-dmz-02").length).toBeGreaterThan(0);
    // The collapsed Method line and the recap name them too.
    expect(container.textContent).toMatch(/on runner-dmz-01, runner-dmz-02/);
    expect(card("SSH")).toBeUndefined();
    expect(card("Runner")).toBeUndefined();
    await run();
    expect(sent()).not.toMatch(/"ssh"|"runner"/);
  });

  it("states the open rule on a scope that is not bound", async () => {
    scopeRows = [{ scope: "dmz-web", boundRunners: [] }, { scope: "elsewhere", boundRunners: [bound("runner-x")] }];
    const { openOptions, card, run, sent, container } = renderDialog(makeJob());
    openOptions();
    await vi.waitFor(() => expect(container.textContent).toMatch(/Any runner that serves this scope/));
    // Another scope's binding is not this one's.
    expect(container.textContent).not.toMatch(/runner-x/);
    expect(container.textContent).toMatch(/on any runner/);
    expect(card("SSH")).toBeUndefined();
    await run();
    expect(sent()).not.toMatch(/"ssh"|"runner"/);
  });

  it("shows a refusal the server still makes and keeps the dialog open", async () => {
    // The dialog cannot know every reason a run is refused (no agent serves the
    // scope and the job binds a key, say). It shows the server's sentence.
    scopeRows = [{ scope: "dmz-web", boundRunners: [] }];
    const onDone = vi.fn();
    const { run, container } = renderDialog(
      makeJob(),
      async () => ({
        ok: false,
        code: "key_binding_requires_runner",
        message: "this job binds SSH key CRONOMICON_KEY_deploy, which only an agent can deliver as a file",
      }),
      onDone,
    );
    await run();
    await vi.waitFor(() => expect(container.textContent).toMatch(/only an agent can deliver as a file/));
    expect(onDone).not.toHaveBeenCalled();
  });
});

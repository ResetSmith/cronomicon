// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import type { ComponentProps } from "react";
import { cleanup, fireEvent, render, within } from "@testing-library/react";
import type { components } from "../api/schema";

// SB — the Run dialog on a scope that is bound to runners.
//
// The dialog ALWAYS sends an explicit executor. Before it knew about bindings
// that made every manual run of a shell job on a bound scope a refusal: the job
// defaults to SSH, the dialog sent "ssh", and the server answered
// scope_requires_runner — for a run the operator had done nothing unusual to.
// What these tests protect is the EXECUTOR REACHING THE WIRE: on a bound scope it
// is "runner" whatever the job says, and SSH is not on offer; on an unbound scope
// nothing has changed. The assertions are on the onRun arguments, because a
// dialog that merely LOOKED right while still sending "ssh" is the bug.

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

// A shell job with no executor of its own — the common case, and the one that
// used to be refused.
const makeJob = (over: Partial<Job> = {}): Job =>
  ({ id: 1, name: "deploy-api", type: "bash", scope: "dmz-web", ...over }) as unknown as Job;

const renderDialog = (job: Job, onRunImpl?: OnRun) => {
  const onRun = vi.fn<OnRun>(onRunImpl ?? (async () => ({ ok: true })));
  const { container } = render(
    <RunDialog job={job} scopes={[]} busy={false} onCancel={vi.fn()} onRun={onRun} onDone={vi.fn()} />,
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
  // The executor cards are buttons whose first line is the label.
  const card = (label: "SSH" | "Runner") =>
    q.getAllByRole("button").find((b) => b.textContent?.startsWith(label)) as HTMLButtonElement;
  // RC — every run takes two presses: the first arms the confirmation window,
  // the second commits.
  const run = async () => {
    fireEvent.click(runBtn());
    fireEvent.click(runBtn());
    await vi.waitFor(() => expect(onRun).toHaveBeenCalled());
  };
  const sentExecutor = (call = 0) => onRun.mock.calls[call]?.[1];
  return { onRun, q, container, openOptions, card, run, sentExecutor };
};

describe("RunDialog — a scope bound to runners (SB)", () => {
  it("sends the runner executor for a shell job with no executor of its own, and takes SSH off the table", async () => {
    scopeRows = [{ scope: "dmz-web", boundRunners: [bound("runner-dmz-01")] }];
    const { openOptions, card, run, sentExecutor, container } = renderDialog(makeJob());
    openOptions();
    // The binding arrives after mount; the card must follow it.
    await vi.waitFor(() => expect(card("SSH").disabled).toBe(true));
    expect(card("SSH").title).toMatch(/bound to runners/);
    // The explanation names the scope and the runner, so a disabled card is not a mystery.
    expect(container.textContent).toMatch(/Scope dmz-web is bound to runner-dmz-01/);
    await run();
    expect(sentExecutor()).toBe("runner");
  });

  it("sends the runner executor even when the JOB says ssh — that choice would be refused", async () => {
    scopeRows = [{ scope: "dmz-web", boundRunners: [bound("runner-dmz-01"), bound("runner-dmz-02")] }];
    const { openOptions, card, run, sentExecutor } = renderDialog(makeJob({ executor: "ssh" } as Partial<Job>));
    openOptions();
    await vi.waitFor(() => expect(card("SSH").disabled).toBe(true));
    await run();
    expect(sentExecutor()).toBe("runner");
  });

  it("changes nothing on a scope that is not bound", async () => {
    scopeRows = [{ scope: "dmz-web", boundRunners: [] }, { scope: "elsewhere", boundRunners: [bound("runner-x")] }];
    const { openOptions, card, run, sentExecutor } = renderDialog(makeJob());
    openOptions();
    // Give the scope list time to land, then confirm it did not disable anything.
    await vi.waitFor(() => expect(card("Runner")).toBeTruthy());
    expect(card("SSH").disabled).toBe(false);
    await run();
    expect(sentExecutor()).toBe("ssh");
  });

  it("recovers when the server refuses with scope_requires_runner — the binding was made after the dialog opened", async () => {
    // The dialog's own view says unbound, so it sends ssh; the server knows better.
    scopeRows = [{ scope: "dmz-web", boundRunners: [] }];
    let calls = 0;
    const { openOptions, run, sentExecutor, onRun, container, q } = renderDialog(makeJob(), async () => {
      calls++;
      return calls === 1
        ? { ok: false, code: "scope_requires_runner", message: "scope dmz-web is bound to runners, so its jobs run on those runners" }
        : { ok: true };
    });
    openOptions();
    await run();
    expect(sentExecutor(0)).toBe("ssh");
    // The dialog stays open with the server's reason, and switches to the runner.
    await vi.waitFor(() => expect(container.textContent).toMatch(/is bound to runners/));
    // Re-query for each press: arming the confirmation re-renders the footer,
    // so the element pressed first is not the one that commits.
    const press = () => {
      const buttons = q.getAllByRole("button");
      fireEvent.click(buttons[buttons.length - 1]);
    };
    press();
    press();
    await vi.waitFor(() => expect(onRun).toHaveBeenCalledTimes(2));
    expect(sentExecutor(1)).toBe("runner");
  });
});

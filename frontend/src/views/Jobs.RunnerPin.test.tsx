// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import type { ComponentProps } from "react";
import { cleanup, fireEvent, render, within } from "@testing-library/react";
import type { components } from "../api/schema";

// RT-3 — the per-run runner pin in the Run dialog
// (the runner-targeting plan).
//
// What these tests actually protect is the TRI-STATE reaching the wire. The
// dialog can express three things and they are not two:
//
//   · untouched          → send no runnerTag; the server resolves the job's pin
//   · a tag              → send that tag
//   · "run on any"       → send "" — the per-run unpin
//
// The failure mode is silent in both directions: a truthiness guard anywhere on
// this path turns the third case into the first, and the operator's deliberate
// unpin becomes "inherit the pin I was trying to escape". No rendering assertion
// would catch it, so the assertions here are on the onRun arguments.

let detailResponse: unknown = undefined;
let runners: { status: string; tags: string[] }[] = [];

vi.mock("../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/client")>();
  return {
    ...actual,
    api: {
      GET: vi.fn(async (path: string) => {
        if (path === "/runners") return { data: runners };
        if (path === "/job-reference-bindings/{jobId}") return { data: { bindings: [] } };
        if (path === "/script-reference-bindings/{name}") return { data: { bindings: [] } };
        if (path === "/env-vars" || path === "/env-secrets" || path === "/ssh/credentials") return { data: [] };
        return { data: detailResponse };
      }),
      POST: vi.fn(async () => ({ data: {} })),
    } as unknown as typeof actual.api,
    fetchCapabilities: vi.fn(async () => ({ manageEnvVars: false, configureApp: false })),
  };
});

import { RunDialog } from "./Jobs";

type Job = components["schemas"]["Job"];
type OnRun = ComponentProps<typeof RunDialog>["onRun"];

afterEach(() => {
  cleanup();
  detailResponse = undefined;
  runners = [];
});

// executor: "runner" throughout — the pin control is hidden on the SSH executor
// because the server 422s that combination (RT-Q5), so an ssh job would render
// no control to test.
const makeJob = (over: Partial<Job> = {}): Job =>
  ({ id: 1, name: "deploy-api", type: "bash", scope: "Prod", executor: "runner", ...over }) as unknown as Job;

const renderDialog = (job: Job) => {
  const onRun = vi.fn<OnRun>(async () => ({ ok: true }));
  const { container } = render(
    <RunDialog job={job} scopes={[]} busy={false} onCancel={vi.fn()} onRun={onRun} onDone={vi.fn()} />,
  );
  const q = within(container);
  const runBtn = () => {
    const buttons = q.getAllByRole("button");
    return buttons[buttons.length - 1] as HTMLButtonElement;
  };
  // RD5 — the pin lives in Method; Targets is what the RV review gate wants.
  const openOptions = () => {
    fireEvent.click(q.getByRole("button", { name: /Targets/ }));
    fireEvent.click(q.getByRole("button", { name: /Method/ }));
  };
  // The pin input is the only control with the accessible name "Run on".
  const pinInput = () => q.getByLabelText("Run on") as HTMLInputElement;
  // RC — every run takes two presses: the first arms the confirmation window,
  // the second commits. A single press calls nothing, which would make every
  // assertion below pass vacuously against an onRun that never fired.
  const run = async () => {
    fireEvent.click(runBtn());
    fireEvent.click(runBtn());
    await vi.waitFor(() => expect(onRun).toHaveBeenCalled());
  };
  // placement is the 11th positional argument (trailing, after ansibleOpts).
  const placementArg = () => onRun.mock.calls[0]?.[10];
  return { onRun, q, runBtn, openOptions, pinInput, run, placementArg };
};

describe("RunDialog — runner pin (RT-3)", () => {
  it("sends no pin at all when the control is untouched", async () => {
    const { openOptions, run, placementArg } = renderDialog(makeJob({ runnerTagEffective: "vlan-dmz" } as Partial<Job>));
    openOptions();
    await run();

    // Not { runnerTag: "vlan-dmz" }: echoing the job's own pin back would make an
    // ordinary run's body differ from a pre-RT one and would freeze a stale pin
    // onto a run whose job changed between opening and pressing Run.
    expect(placementArg()).toBeUndefined();
  });

  it("prefills the job's effective pin and sends an edited one", async () => {
    const { openOptions, pinInput, run, placementArg } = renderDialog(
      makeJob({ runnerTagEffective: "vlan-dmz" } as Partial<Job>),
    );
    openOptions();
    expect(pinInput().value).toBe("vlan-dmz");

    fireEvent.change(pinInput(), { target: { value: "vlan-core" } });
    await run();

    expect(placementArg()).toEqual({ runnerTag: "vlan-core" });
  });

  // The break-glass case, and the whole reason the state is `string | null`.
  it("sends an EMPTY string when the operator unpins a pinned job", async () => {
    const { q, openOptions, run, placementArg } = renderDialog(
      makeJob({ runnerTagEffective: "vlan-dmz" } as Partial<Job>),
    );
    openOptions();
    fireEvent.click(q.getByRole("button", { name: /Run on any runner/ }));
    await run();

    // "" and undefined are different requests: undefined inherits the pin this
    // operator is deliberately escaping.
    expect(placementArg()).toEqual({ runnerTag: "" });
    expect(placementArg()?.runnerTag).not.toBeUndefined();
  });

  it("can go back to the job's pin after unpinning, sending nothing again", async () => {
    const { q, openOptions, run, placementArg } = renderDialog(
      makeJob({ runnerTagEffective: "vlan-dmz" } as Partial<Job>),
    );
    openOptions();
    fireEvent.click(q.getByRole("button", { name: /Run on any runner/ }));
    fireEvent.click(q.getByRole("button", { name: /Use the job's pin/ }));
    await run();

    expect(placementArg()).toBeUndefined();
  });

  it("pins an unpinned job for one run", async () => {
    const { openOptions, pinInput, run, placementArg } = renderDialog(makeJob());
    openOptions();
    expect(pinInput().value).toBe("");

    fireEvent.change(pinInput(), { target: { value: "vlan-lab" } });
    await run();

    expect(placementArg()).toEqual({ runnerTag: "vlan-lab" });
  });

  it("hides the control entirely on the SSH executor", () => {
    // RT-Q5 — the server rejects a pin that resolves to ssh, so offering a
    // control whose only outcome is a 422 is worse than offering none.
    const { q, openOptions } = renderDialog(makeJob({ executor: "ssh" } as Partial<Job>));
    openOptions();
    expect(q.queryByLabelText(/Run on/)).toBeNull();
  });

  it("warns, unsuppressibly, when nothing carrying the pin is online", async () => {
    runners = [{ status: "offline", tags: ["vlan-dmz"] }];
    const { q, openOptions } = renderDialog(makeJob({ runnerTagEffective: "vlan-dmz" } as Partial<Job>));
    openOptions();

    // The run is still allowed (RT-Q4) — the warning explains the queue, it does
    // not gate the button.
    const warning = await q.findByText(/No runner tagged/);
    expect(warning.textContent).toContain("stays queued");
  });
});

// RS-1 (the run-summary plan) — the pin was invisible to the DEVIATION
// model. RT-3 shipped after that list was built and nobody extended it, so all
// three of its consumers missed a per-run pin: the rail, the confirm window, and
// the arm signature. These tests pin each consumer, because the three are wired
// separately and a fix to one does not imply the others.
describe("RunDialog — the pin as a deviation (RS-1)", () => {
  // RC-2 — every press opens the confirm window; whether the window NAMES the
  // pin is what distinguishes a deviation from a silent change.
  const pressRun = (runBtn: () => HTMLButtonElement) => fireEvent.click(runBtn());

  it("names a re-pinned run against the job's default, and forces the window", async () => {
    const { q, runBtn, openOptions, pinInput } = renderDialog(
      makeJob({ runnerTagEffective: "vlan-dmz" } as Partial<Job>),
    );
    openOptions();
    fireEvent.change(pinInput(), { target: { value: "vlan-lab" } });
    pressRun(runBtn);

    // The window exists AND says which pin replaced which — a row that merely
    // said "Runner pin" would not tell an operator what they had done.
    const row = await q.findByText("Runner pin");
    const detail = row.parentElement?.textContent ?? "";
    expect(detail).toContain("vlan-lab");
    expect(detail).toContain("job default: vlan-dmz");
  });

  it("reads 'unpinned' when the operator explicitly unpins a pinned job", async () => {
    const { q, runBtn, openOptions, pinInput } = renderDialog(
      makeJob({ runnerTagEffective: "vlan-dmz" } as Partial<Job>),
    );
    openOptions();
    fireEvent.change(pinInput(), { target: { value: "" } });
    pressRun(runBtn);

    const row = await q.findByText("Runner pin");
    const detail = row.parentElement?.textContent ?? "";
    // "unpinned", not an empty cell: sending this run to the general pool when
    // the job says otherwise is the deviation, and a blank would read as "none".
    expect(detail).toContain("unpinned");
    expect(detail).toContain("job default: vlan-dmz");
  });

  it("raises no pin deviation when the control is untouched", async () => {
    const { q, runBtn, openOptions } = renderDialog(
      makeJob({ runnerTagEffective: "vlan-dmz" } as Partial<Job>),
    );
    openOptions();
    pressRun(runBtn);
    // Running a job where its own definition says is not a deviation — the
    // window opens (RC-2) but carries no pin row.
    await new Promise((r) => setTimeout(r, 0));
    expect(q.queryByText("Runner pin")).toBeNull();
  });

  it("does not raise one on the SSH executor, where a pin is meaningless", async () => {
    const { q, runBtn, openOptions } = renderDialog(
      makeJob({ executor: "ssh", runnerTagEffective: "vlan-dmz" } as Partial<Job>),
    );
    openOptions();
    pressRun(runBtn);
    await new Promise((r) => setTimeout(r, 0));
    expect(q.queryByText("Runner pin")).toBeNull();
  });


});

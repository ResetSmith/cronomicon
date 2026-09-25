// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import type { ComponentProps } from "react";
import { act, cleanup, fireEvent, render, waitFor, within } from "@testing-library/react";
import type { components } from "../api/schema";

// The dialog fetches its own job detail (prompts/env are detail-only). `detailResponse`
// lets a test choose what that fetch returns; undefined ⇒ the dialog falls back to the
// job prop it was handed.
let detailResponse: unknown = undefined;
// T1.7 — the job's declared reference bindings and the verdicts the validator
// returns for them. Empty by default so the existing run-input tests are unaffected.
let jobBindings: { kind: string; name: string; reference: string }[] = [];
let refVerdicts: Record<string, unknown> = {};
// V2-11 — the stored Env Vars rows the add-for-this-run dropdowns list, and the
// caller's capability set (additions render only with manageEnvVars).
let knownVars: string[] = [];
let knownSecrets: string[] = [];
let knownKeys: string[] = [];
let caps = { manageEnvVars: false, configureApp: false };

vi.mock("../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/client")>();
  return {
    ...actual,
    api: {
      GET: vi.fn(async (path: string) => {
        if (path === "/job-reference-bindings/{jobId}") return { data: { bindings: jobBindings } };
        if (path === "/script-reference-bindings/{name}") return { data: { bindings: [] } };
        if (path === "/env-vars") return { data: knownVars.map((k) => ({ id: k, key: k })) };
        if (path === "/env-secrets") return { data: knownSecrets.map((k) => ({ id: k, key: k })) };
        if (path === "/ssh/credentials") return { data: knownKeys.map((l) => ({ id: l, label: l })) };
        return { data: detailResponse };
      }),
      POST: vi.fn(async (path: string, opts: { body?: unknown }) => {
        if (path !== "/references/validate") return { data: {} };
        const body = opts.body as { references?: { kind: string; name: string }[] };
        // The real endpoint answers one verdict per submitted reference; a test that
        // configures no verdict for a reference gets none back (never `undefined`).
        return { data: { results: (body.references ?? []).map((r) => refVerdicts[`${r.kind} ${r.name}`]).filter(Boolean) } };
      }),
    } as unknown as typeof actual.api,
    fetchCapabilities: vi.fn(async () => caps),
  };
});

import { RunDialog } from "./Jobs";

type Job = components["schemas"]["Job"];
type JobPrompt = components["schemas"]["JobPrompt"];
type OnRun = ComponentProps<typeof RunDialog>["onRun"];

afterEach(() => {
  cleanup();
  detailResponse = undefined;
  jobBindings = [];
  refVerdicts = {};
  knownVars = [];
  knownSecrets = [];
  knownKeys = [];
  caps = { manageEnvVars: false, configureApp: false };
});

const makeJob = (prompts: JobPrompt[], env?: Record<string, string>): Job =>
  ({ id: 1, name: "deploy-api", type: "bash", scope: "Prod", executor: "ssh", prompts, env }) as unknown as Job;

// Queries are scoped to this render's container: `screen` reads all of document.body,
// which collides when a test renders the dialog more than once.
const renderDialog = (job: Job) => {
  const onRun = vi.fn<OnRun>(async () => ({ ok: true }));
  const { container } = render(
    <RunDialog job={job} scopes={[]} busy={false} onCancel={vi.fn()} onRun={onRun} onDone={vi.fn()} />,
  );
  const q = within(container);
  // The primary action is the LAST button in the modal footer. Selecting it by label
  // would be brittle: it reads "Run now" / "Run without N answers" / "Queuing…" /
  // "N answers still needed" depending on state, which is the point of the tests.
  const runBtn = () => {
    const buttons = q.getAllByRole("button");
    return buttons[buttons.length - 1] as HTMLButtonElement;
  };
  // RD1/RV — the dialog is three sections. Opening "Where it runs" is also what
  // satisfies the review gate for a job with declared inputs, so tests that run
  // such a job call openOptions() first, exactly as an operator must.
  // RD5 — "Where it runs" is now two sections. openOptions opens BOTH (the old
  // fold's whole content), which satisfies the RV gate (Targets) and reaches the
  // executor/identity/pin controls (Method). openInputs reaches References.
  const openTargets = () => fireEvent.click(q.getByRole("button", { name: /^▶ Targets|Targets/ }));
  const openMethod = () => fireEvent.click(q.getByRole("button", { name: /Method/ }));
  const openInputs = () => fireEvent.click(q.getByRole("button", { name: /^▶ Inputs|Inputs/ }));
  const openOptions = () => {
    openTargets();
    openMethod();
  };
  const openAdvanced = () => fireEvent.click(q.getByRole("button", { name: /Advanced/ }));
  // RU-5 — the per-run env overrides are a subgroup of "Run inputs" (which is
  // itself open by default whenever the job declares prompts).
  const openOverrides = () => fireEvent.click(q.getByRole("button", { name: /Add an override/ }));
  return { onRun, q, runBtn, openOptions, openTargets, openMethod, openInputs, openAdvanced, openOverrides };
};

describe("RunDialog — run inputs", () => {
  // T1.7/JR-Q4b — an unfilled required input gates the button, and the escape is an
  // explicit act rather than a dead end.
  it("gates the Run button on an unfilled required input, and the escape ungates it", () => {
    const { q, runBtn, openOptions } = renderDialog(makeJob([{ name: "TARGET_ENV", required: true }]));
    openOptions(); // satisfy the RV review gate; this test is about the input gate

    expect(runBtn().disabled).toBe(true);
    expect(runBtn().textContent).toContain("Run without 1 answer");
    expect(q.getByText(/1 answer is still missing/)).toBeTruthy();

    fireEvent.click(q.getByRole("checkbox"));

    expect(runBtn().disabled).toBe(false);
    // Still labelled honestly — ungating doesn't pretend the input is filled.
    expect(runBtn().textContent).toContain("Run without 1 answer");
  });

  it("typing a value clears the gate and marks provenance as 'you'", () => {
    const { q, runBtn, openOptions } = renderDialog(makeJob([{ name: "TARGET_ENV", required: true }]));
    openOptions();

    fireEvent.change(q.getByLabelText("TARGET_ENV"), { target: { value: "prod" } });

    expect(runBtn().disabled).toBe(false);
    expect(runBtn().textContent).toBe("Run now");
    expect(q.getByText("TARGET_ENV=prod")).toBeTruthy();
    expect(q.getByText("you")).toBeTruthy();
  });

  // T1.5/JR-Q7 — a default IS sent, so it never gates; but it stays "unconfirmed" until
  // the operator affirms it. Two distinct states, only one of which blocks.
  it("a required default is sent, shown as 'default', and confirmable without gating", () => {
    const { q, runBtn, openOptions } = renderDialog(makeJob([{ name: "REPLICAS", required: true, default: "3" }]));
    openOptions();

    expect(runBtn().disabled).toBe(false);
    expect(runBtn().textContent).toBe("Run now");
    expect(q.getByText("REPLICAS=3")).toBeTruthy();
    expect(q.getByText("default")).toBeTruthy();
    expect(q.getByText("Using a default")).toBeTruthy();

    fireEvent.click(q.getByRole("button", { name: /Use this/ }));

    expect(q.queryByRole("button", { name: /Use this/ })).toBeNull();
    expect(q.getByText("Ready")).toBeTruthy();
    expect(q.getByText("REPLICAS=3")).toBeTruthy();
  });

  // T1.4 — an override row beats the prompt answer in submit(), so the audit line must
  // say so. RU-5 — and the editor now sits directly under the answers it outranks,
  // which is the whole point of the move: the precedence reads top-to-bottom.
  it("a per-run override row wins the provenance over the prompt answer", () => {
    const { q, openOverrides } = renderDialog(makeJob([{ name: "TARGET_ENV", required: true, default: "dev" }]));
    expect(q.getByText("TARGET_ENV=dev")).toBeTruthy();

    openOverrides();
    fireEvent.click(q.getByRole("button", { name: /Add variable/ }));
    fireEvent.change(q.getByPlaceholderText("KEY"), { target: { value: "TARGET_ENV" } });
    fireEvent.change(q.getByPlaceholderText("value"), { target: { value: "prod" } });

    expect(q.getByText("TARGET_ENV=prod")).toBeTruthy();
    expect(q.getByText("override")).toBeTruthy();
  });

  // Job-level env is the BASE layer of the server's effective env
  // (envmerge.Merge(jr.Env, body.Env)), so it genuinely satisfies a required input —
  // unlike a scope Env Vars row, which does not (JR-Q1, fixed in Phase 0).
  it("job-level env satisfies a required input and is labelled 'job env'", () => {
    const { q, runBtn, openOptions } = renderDialog(makeJob([{ name: "REGION", required: true }], { REGION: "us-west-2" }));
    openOptions();

    expect(runBtn().disabled).toBe(false);
    expect(q.getByText("REGION=us-west-2")).toBeTruthy();
    expect(q.getByText("job env")).toBeTruthy();
  });

  // RD2 — the headline is the at-a-glance summary, and it counts the two outstanding
  // states together: an operator wants "how much is left", not a taxonomy.
  it("summarises readiness as a count over every declared input", () => {
    const { q } = renderDialog(
      makeJob([
        { name: "TARGET_ENV", required: true },
        { name: "REPLICAS", required: true, default: "3" },
        { name: "NOTE" },
      ]),
    );

    // NOTE is optional and empty — nothing is wanted from it, so it counts as ready.
    expect(q.getByText("1 of 3 ready")).toBeTruthy();
    expect(q.getByText("Needs a value")).toBeTruthy();
    expect(q.getByText("Using a default")).toBeTruthy();
    expect(q.getByText("Optional")).toBeTruthy();
  });

  // RU-4/RU-5 — the section itself survives a job with no declared prompts (it
  // carries the per-run overrides), but it holds no answers panel and stays
  // collapsed, and the one-click run (RV-Q2) is untouched.
  it("renders no answers panel, and no gate, when the job declares none", () => {
    const { q, runBtn } = renderDialog(makeJob([]));

    expect(q.getByText("Inputs")).toBeTruthy();
    expect(q.getByText("none declared")).toBeTruthy();
    expect(q.queryByText(/ready$/)).toBeNull();
    expect(runBtn().disabled).toBe(false);
    expect(runBtn().textContent).toBe("Run now");
  });

  // RD4 — the author's label leads and the raw env var name moves to the tooltip, so a
  // non-technical operator reads "Target environment" rather than TARGET_ENV. The name
  // stays reachable on the audit line, which is why nothing is lost by demoting it.
  it("leads with the author's label and keeps the variable name on the tooltip", () => {
    const { q } = renderDialog(makeJob([{ name: "TARGET_ENV", label: "Target environment", required: true }]));

    const label = q.getByText("Target environment");
    expect(label.getAttribute("title")).toBe("TARGET_ENV");
    expect(q.queryByText("TARGET_ENV")).toBeNull();
  });

  // Regression — `prompts`/`env` are DETAIL-only, and the Jobs list row that opens this
  // dialog carries neither. The dialog must fetch its own detail; when it didn't, the
  // whole run-input surface silently rendered nothing in the real app while unit tests
  // (which hand the component a job prop directly) stayed green.
  it("renders inputs that arrive from the detail fetch, not just from the job prop", async () => {
    detailResponse = makeJob([{ name: "TARGET_ENV", required: true }], { REGION: "eu-1" });

    // The job prop is a LIST row: no prompts, no env — exactly what Jobs.tsx passes.
    const { q, runBtn } = renderDialog({ id: 1, name: "deploy-api", type: "bash", scope: "Prod" } as unknown as Job);

    // RU-4/RU-5 — wait on the INPUT, not the section title: the "Run inputs" fold
    // renders from the first paint now (it carries the overrides editor), so
    // waiting for the title would no longer wait for the fetch at all and this
    // regression test would pass without the detail ever arriving.
    await vi.waitFor(() => expect(q.getByLabelText("TARGET_ENV")).toBeTruthy());
    expect(runBtn().disabled).toBe(true);
    expect(q.getByText(/1 answer is still missing/)).toBeTruthy();
  });

  // UDV2/UDV8 — answers (including an untouched default) ride the per-run env override.
  it("submits prompt answers as per-run env overrides", async () => {
    const { onRun, runBtn, openOptions } = renderDialog(makeJob([{ name: "REPLICAS", required: true, default: "3" }]));
    openOptions();

    fireEvent.click(runBtn());
    fireEvent.click(runBtn()); // RC-2 — the last button is now the window's Confirm & run
    await vi.waitFor(() => expect(onRun).toHaveBeenCalled());

    expect(onRun.mock.calls[0][2]).toEqual({ REPLICAS: "3" });
  });

  // T3.3/JR-Q10 — a `block` job must HIDE the escape, not offer one the server will
  // reject. Offering a checkbox that only produces a 422 is worse than offering none.
  it("a block-enforcement job gates with no escape", () => {
    detailResponse = {
      ...makeJob([{ name: "TARGET_ENV", required: true }]),
      promptEnforcement: "block",
    };
    const { q, runBtn } = renderDialog({ id: 1, name: "deploy-api", type: "bash", scope: "Prod" } as unknown as Job);

    return vi.waitFor(() => {
      expect(runBtn().disabled).toBe(true);
      expect(q.queryByRole("checkbox")).toBeNull();
      expect(runBtn().textContent).toContain("1 answer still needed");
      expect(q.getByText(/There is no override/)).toBeTruthy();
    });
  });

  // A warn job keeps the escape — enforcement is opt-in and must not leak.
  it("a warn-enforcement job still offers the escape", async () => {
    detailResponse = {
      ...makeJob([{ name: "TARGET_ENV", required: true }]),
      promptEnforcement: "warn",
    };
    const { q, runBtn, openOptions } = renderDialog({ id: 1, name: "deploy-api", type: "bash", scope: "Prod" } as unknown as Job);

    await vi.waitFor(() => expect(q.getByRole("checkbox")).toBeTruthy());
    openOptions();
    fireEvent.click(q.getByRole("checkbox"));
    expect(runBtn().disabled).toBe(false);
  });

  // T2.4 — the audit payload records how the operator arrived here. This is the Phase-2
  // field that makes "was warned and proceeded" distinguishable from "nobody looked".
  it("sends promptAnswers provenance and promptAcknowledged when the escape is used", async () => {
    const { onRun, q, runBtn, openOptions } = renderDialog(
      makeJob([
        { name: "TARGET_ENV", required: true },
        { name: "REPLICAS", required: true, default: "3" },
      ]),
    );

    openOptions();
    fireEvent.click(q.getByRole("checkbox"));
    fireEvent.click(runBtn());
    fireEvent.click(runBtn()); // RC-2 — the last button is now the window's Confirm & run
    await vi.waitFor(() => expect(onRun).toHaveBeenCalled());

    const audit = onRun.mock.calls[0][6];
    expect(audit?.promptAcknowledged).toBe(true);
    // TARGET_ENV has no value at all, so it contributes no provenance entry.
    expect(audit?.promptAnswers).toEqual({ REPLICAS: "default" });
  });

  it("does not claim an acknowledgment when nothing was unfilled", async () => {
    const { onRun, runBtn, openOptions } = renderDialog(makeJob([{ name: "REPLICAS", required: true, default: "3" }]));
    openOptions();

    fireEvent.click(runBtn());
    fireEvent.click(runBtn()); // RC-2 — the last button is now the window's Confirm & run
    await vi.waitFor(() => expect(onRun).toHaveBeenCalled());

    expect(onRun.mock.calls[0][6]?.promptAcknowledged).toBe(false);
  });
});

// RD5 — a job may declare many inputs. Pagination keeps the primary action reachable,
// and the filter is what keeps pagination honest: neither may let a required input hide.
describe("RunDialog — many run inputs", () => {
  const manyPrompts = (n: number, required = true): JobPrompt[] =>
    Array.from({ length: n }, (_, i) => ({ name: `VAR_${i + 1}`, required }));

  it("does not paginate a list that fits", () => {
    const { q } = renderDialog(makeJob(manyPrompts(5)));

    expect(q.queryByRole("button", { name: /Next →/ })).toBeNull();
    expect(q.getByLabelText("VAR_5")).toBeTruthy();
  });

  it("paginates a long list and pages through it", () => {
    const { q } = renderDialog(makeJob(manyPrompts(8)));

    expect(q.getByText("1–5 of 8")).toBeTruthy();
    expect(q.getByLabelText("VAR_1")).toBeTruthy();
    expect(q.queryByLabelText("VAR_6")).toBeNull();

    fireEvent.click(q.getByRole("button", { name: /Next →/ }));

    expect(q.getByText("6–8 of 8")).toBeTruthy();
    expect(q.getByLabelText("VAR_6")).toBeTruthy();
    expect(q.queryByLabelText("VAR_1")).toBeNull();
  });

  // The whole safety argument for paginating a form: an input the operator cannot
  // currently see must still be counted, and must still be reachable.
  it("counts and names an outstanding input that is on another page", () => {
    const { q, runBtn } = renderDialog(makeJob(manyPrompts(8)));

    expect(q.getByText("0 of 8 ready")).toBeTruthy();
    expect(runBtn().disabled).toBe(true);
    // VAR_8 is on page 2, and the gate below the list still names it.
    expect(q.getByText(/VAR_8/)).toBeTruthy();
  });

  it("collapses the list to just what is outstanding", () => {
    const prompts: JobPrompt[] = [
      ...manyPrompts(6, false).map((p) => ({ ...p, default: "x" })),
      { name: "NEEDED", required: true },
    ];
    const { q } = renderDialog(makeJob(prompts));

    fireEvent.click(q.getByRole("checkbox", { name: /Show only what needs an answer/ }));

    expect(q.getByLabelText("NEEDED")).toBeTruthy();
    expect(q.queryByLabelText("VAR_1")).toBeNull();
    // One row left, so the pager goes away with it.
    expect(q.queryByRole("button", { name: /Next →/ })).toBeNull();
  });
});

// FX-6 / FX-10 — where the commit zone lives, and what may be in it.
//
// FX-6: "Where it runs" expands into a full targeting form, so it must scroll
// with the answers rather than sit in the pinned bar, which used to paint over
// them. FX-10 then replaced the hand-rolled sticky bar with Modal's `footer`,
// which pins by living OUTSIDE the scrollport rather than by being sticky inside
// it — so the assertion is now "not in the thing that scrolls", which is the
// property that actually makes overlap impossible.
describe("RunDialog — commit bar layout", () => {
  /** The element that scrolls: the nearest ancestor with overflowY auto. */
  const scrollport = (el: Element | null): HTMLElement | null => {
    for (let n = el?.parentElement ?? null; n; n = n.parentElement) {
      if (n.style.overflowY === "auto") return n;
    }
    return null;
  };

  it("keeps the targeting fold in the scrolling body, expanded or not", () => {
    const { q, openOptions } = renderDialog(makeJob([{ name: "TARGET_ENV", required: true }]));

    const fold = q.getByRole("button", { name: /Targets/ });
    expect(scrollport(fold)).not.toBeNull();

    openOptions();
    // The expanded targeting form scrolls with the answers rather than growing a
    // pinned bar over them.
    expect(scrollport(q.getByText("Target scope"))).not.toBeNull();
  });

  // RU-2 — what must stay pinned is what the operator has to REACH: the buttons and
  // the escape checkbox that ungates them. The consequence prose moved into the
  // answers panel, beside the rows it names, so it now scrolls with them.
  it("pins the action row and the escape outside the scrollport, prose with the answers", () => {
    const { q, runBtn } = renderDialog(makeJob([{ name: "TARGET_ENV", required: true }]));

    expect(scrollport(runBtn())).toBeNull();
    expect(scrollport(q.getByRole("checkbox"))).toBeNull();
    expect(scrollport(q.getByText(/1 answer is still missing/))).not.toBeNull();
  });

  // RU-1 — and the recap sentence is pinned too: "what will this do" must be
  // answerable without scrolling, at any moment.
  it("pins the recap line outside the scrollport", () => {
    const { q } = renderDialog(makeJob([]));

    expect(scrollport(q.getByText(/all 2 hosts in Prod|its targets in Prod/))).toBeNull();
  });

  // The residual FX-6 exposure this closes: the warning names every unfilled
  // input, so it is unbounded. Inside a sticky bar a long list could regrow the
  // bar over the answers. RU-2 removes the exposure at the source — the unbounded
  // list is no longer in the footer at all, so the footer's height is now fixed by
  // the recap line, the one-line escape and the buttons.
  it("cannot be pushed over the answers by a long list of missing inputs", () => {
    const many = Array.from({ length: 12 }, (_, i) => ({ name: `VERY_LONG_INPUT_NAME_${i + 1}`, required: true }));
    const { q, runBtn } = renderDialog(makeJob(many));

    // The unbounded list scrolls with the answers it names…
    expect(scrollport(q.getByText(/12 answers are still missing/))).not.toBeNull();
    // …while the commit zone stays outside the scrollport, bounded.
    expect(scrollport(runBtn())).toBeNull();
    // The answers panel is still in the scrolling body, not underneath a bar.
    expect(scrollport(q.getByText("Inputs"))).not.toBeNull();
    // The recap carries only the compact count, never the list.
    expect(q.getByText("12 answers missing")).toBeTruthy();
  });
});

// RU-1 — the recap line. It is built from the same live state submit() reads, so
// these tests are really asserting that it cannot drift from the run.
describe("RunDialog — the recap line", () => {
  const recap = (q: ReturnType<typeof within>) =>
    (q.getByText("deploy-api").parentElement as HTMLElement).textContent ?? "";

  it("states target, executor and timing for a default run", () => {
    const { q } = renderDialog(makeJob([]));

    const line = recap(q);
    expect(line).toContain("deploy-api");
    expect(line).toContain("Prod");
    expect(line).toContain("via SSH");
    expect(line).toContain("now");
  });

  it("follows the executor live", () => {
    const { q } = renderDialog(makeJob([]));
    fireEvent.click(q.getByRole("button", { name: /Targets/ }));
    fireEvent.click(q.getByRole("button", { name: /Method/ }));

    expect(recap(q)).toContain("via SSH");
    fireEvent.click(q.getByText("Runner", { selector: "div" }));
    expect(recap(q)).toContain("via Runner");
  });

  it("carries the missing-answer count", () => {
    const { q } = renderDialog(makeJob([{ name: "TARGET_ENV", required: true }]));

    expect(recap(q)).toContain("1 answer missing");
    fireEvent.change(q.getByLabelText("TARGET_ENV"), { target: { value: "prod" } });
    expect(recap(q)).not.toContain("answer missing");
  });
});

// RU-4..RU-8 — the regrouping. These assert WHERE things are, which is the whole
// deliverable of Phase B: nothing about what a run submits changed.
describe("RunDialog — section structure (RU)", () => {
  // Disclosure bars render as buttons whose text is "▶ <title><summary>", so match
  // the title anywhere and report it in DOM order.
  const NAMES = ["Inputs", "Targets", "Method", "Timing", "Advanced"];
  const sectionTitles = (q: ReturnType<typeof within>): string[] =>
    q
      .getAllByRole("button")
      .map((b: HTMLElement) => NAMES.find((n: string) => (b.textContent ?? "").includes(n)))
      .filter((n: string | undefined): n is string => Boolean(n));

  // "When to run" is promoted above Advanced: timing is an everyday question and
  // it used to sit below ansible's verbosity selector.
  it("orders the five sections inputs → targets → method → timing → advanced", () => {
    const { q } = renderDialog(makeJob([{ name: "TARGET_ENV", required: true }]));

    expect(sectionTitles(q)).toEqual(["Inputs", "Targets", "Method", "Timing", "Advanced"]);
  });

  // RD5 — RU-7's Targeting/Connection subheadings became the Targets and Method
  // sections: Targets owns the scope picker, Method owns the executor.
  it("splits Targets (scope) from Method (executor)", () => {
    const { q, openTargets, openMethod } = renderDialog(makeJob([]));
    openTargets();
    expect(q.getByText("Target scope")).toBeTruthy();
    expect(q.queryByText("Executor")).toBeNull();
    openMethod();
    expect(q.getByText("Executor")).toBeTruthy();
  });

  // RU-5 — the override editor is inside Run inputs, not Advanced.
  it("puts the override subgroup in Run inputs and not in Advanced", () => {
    const { q, openAdvanced } = renderDialog(makeJob([{ name: "TARGET_ENV", required: true }]));

    expect(q.getByRole("button", { name: /Add an override/ })).toBeTruthy();
    openAdvanced();
    expect(q.queryByText("Environment overrides")).toBeNull();
  });

  // RU-8/RU-Q5 — opening "When to run" is now recorded, so History can tell a run
  // whose timing was considered from one where the default was never looked at.
  it("records when-to-run in the audit once the section is opened", async () => {
    const { onRun, q, runBtn } = renderDialog(makeJob([]));

    fireEvent.click(q.getByRole("button", { name: /Timing/ }));
    fireEvent.click(runBtn());
    fireEvent.click(runBtn());
    await vi.waitFor(() => expect(onRun).toHaveBeenCalled());

    expect(onRun.mock.calls[0][6]?.reviewedSections).toContain("when-to-run");
  });

  it("omits when-to-run when the section was never opened", async () => {
    const { onRun, runBtn } = renderDialog(makeJob([]));

    fireEvent.click(runBtn());
    fireEvent.click(runBtn());
    await vi.waitFor(() => expect(onRun).toHaveBeenCalled());

    expect(onRun.mock.calls[0][6]?.reviewedSections ?? []).not.toContain("when-to-run");
  });
});

// RU-10/RU-11 — helpers on engagement. The classification is by CONSEQUENCE:
// "what the control does" hides at rest, "what could go wrong or leak" never does.
describe("RunDialog — helper visibility (RU)", () => {
  // The host picker only renders against a scope that HAS hosts.
  const scoped = [{ id: "s1", scope: "Prod", hosts: ["web-1", "web-2"] }] as unknown as ComponentProps<
    typeof RunDialog
  >["scopes"];
  const renderScoped = () => {
    const { container } = render(
      <RunDialog
        job={makeJob([])}
        scopes={scoped}
        busy={false}
        onCancel={vi.fn()}
        onRun={vi.fn<OnRun>(async () => ({ ok: true }))}
        onDone={vi.fn()}
      />,
    );
    const q = within(container);
    fireEvent.click(q.getByRole("button", { name: /Targets/ }));
    fireEvent.click(q.getByRole("button", { name: /Method/ }));
    return q;
  };

  it("hides the host-subset semantics until the subset is switched on", () => {
    const q = renderScoped();

    expect(q.queryByText(/Runs on all/)).toBeNull();
    fireEvent.click(q.getByRole("checkbox", { name: /Limit to specific hosts/ }));
    // Switching it on makes the field non-default, so its semantics appear.
    expect(q.getByText(/Select at least one host/)).toBeTruthy();
  });

  // The danger arm is unsuppressible in FormField itself — an empty subset is a
  // verdict, not a description, and it must survive the engaged classification.
  it("keeps the invalid-subset verdict visible without focus", () => {
    const q = renderScoped();

    fireEvent.click(q.getByRole("checkbox", { name: /Limit to specific hosts/ }));
    fireEvent.blur(q.getByRole("checkbox", { name: /Limit to specific hosts/ }));

    expect(q.getByText(/Select at least one host/)).toBeTruthy();
  });

  // Secret hygiene is the other always-on class: the operator who most needs the
  // caveat is precisely the one not engaging with the field.
  it("keeps both plaintext caveats on permanently", () => {
    const { q, openAdvanced } = renderDialog(makeJob([{ name: "TARGET_ENV", required: true }]));

    expect(q.getByText(/never paste a password or key/)).toBeTruthy();
    openAdvanced();
    // The ansible extra-vars process-list warning is a leak warning, not a
    // description, so it is always-on too (this job is bash — assert on the
    // inputs caveat, which covers the override editor as well).
    expect(q.getByText(/never paste a password or key/)).toBeTruthy();
  });
});

// RU-13/RU-14 — the rail. Every other test in this file runs at jsdom's default
// 1024px and lands in the stacked branch, which is exactly the point: the rail
// path gets NO incidental coverage, so everything it needs is asserted here
// behind an explicit viewport.
describe("RunDialog — rail layout (RU)", () => {
  const setViewport = (width: number) => {
    Object.defineProperty(window, "innerWidth", { value: width, configurable: true, writable: true });
    act(() => {
      window.dispatchEvent(new Event("resize"));
    });
  };
  afterEach(() => setViewport(1024));

  const railScopes = [
    { id: "s1", scope: "Prod", hosts: ["web-1", "web-2"], groups: [{ name: "webservers", hostCount: 2 }] },
  ] as unknown as ComponentProps<typeof RunDialog>["scopes"];
  const renderRail = (job: Job) => {
    setViewport(1600);
    const onRun = vi.fn<OnRun>(async () => ({ ok: true }));
    const { container } = render(
      <RunDialog job={job} scopes={railScopes} busy={false} onCancel={vi.fn()} onRun={onRun} onDone={vi.fn()} />,
    );
    const q = within(container);
    const runBtn = () => {
      const buttons = q.getAllByRole("button");
      return buttons[buttons.length - 1] as HTMLButtonElement;
    };
    // RS-3 — several group titles ("Where it runs") deliberately match the
    // dialog's own section headers, so rail assertions must be scoped or they
    // match the left-hand pane instead. The rail is the subtree owning the
    // "This run" eyebrow.
    const rail = () => within(q.getByText("This run").parentElement as HTMLElement);
    return { onRun, q, runBtn, rail };
  };
  const ansible = () =>
    ({ id: 2, name: "site-playbook", type: "ansible", scope: "Prod", executor: "runner", prompts: [] }) as unknown as Job;

  it("renders the rail with the recap, and exactly one commit zone", () => {
    const { q } = renderRail(ansible());

    expect(q.getByText("This run")).toBeTruthy();
    // One recap, one Run button — the footer did not render alongside the rail.
    expect(q.getAllByText("site-playbook")).toHaveLength(1);
    expect(q.getAllByRole("button", { name: /Run now/ })).toHaveLength(1);
  });

  // RU-14's whole point: the deviations list is LIVE. Today-before-this it was
  // visible only inside the confirm window, after the first press of Run.
  it("shows a deviation row the moment a setting changes, before any press of Run", () => {
    const { q } = renderRail(ansible());

    expect(q.queryByText("Check mode")).toBeNull();
    fireEvent.click(q.getByRole("button", { name: /Advanced/ }));
    fireEvent.click(q.getByRole("checkbox", { name: /Check mode/ }));

    // The rail row: LABEL + detail, live.
    expect(q.getByText("Check mode", { selector: "div" })).toBeTruthy();
    expect(q.getByText(/dry run — applies nothing/)).toBeTruthy();
  });

  // RU-Q7 — the confirm window is NOT replaced by the rail. It still fences a
  // deviating run and still writes the `confirmation` audit key.
  it("keeps the deviation confirm window on a deviating run", async () => {
    const { onRun, q, runBtn } = renderRail(ansible());

    fireEvent.click(q.getByRole("button", { name: /Advanced/ }));
    fireEvent.click(q.getByRole("checkbox", { name: /Check mode/ }));
    fireEvent.click(runBtn());

    expect(q.getByText(/Confirm run — site-playbook/)).toBeTruthy();
    expect(onRun).not.toHaveBeenCalled();

    fireEvent.click(q.getByRole("button", { name: /Confirm & run/ }));
    await vi.waitFor(() => expect(onRun).toHaveBeenCalled());
    expect(onRun.mock.calls[0][6]?.reviewedSections).toContain("confirmation");
  });

  // RS-3 — the rail renders the SUMMARY now, not the deviations list. The bug it
  // fixes: a stock job with a filled answer produced an empty rail, because
  // "what did I change" and "what will this use" are different questions and
  // only the first was being answered.
  it("states a stock run in full, with no deviations anywhere", () => {
    const { rail } = renderRail(ansible());
    const r = rail();

    // All three always-on groups, on a run where nothing was touched.
    expect(r.getByText("Inputs")).toBeTruthy();
    // "Targets" is both the group title and its row label, so match by count.
    expect(r.getAllByText("Targets").length).toBeGreaterThan(0);
    expect(r.getByText("Method")).toBeTruthy();
    expect(r.getByText("Timing")).toBeTruthy();
    // Advanced is the one group that vanishes when unused.
    expect(r.queryByText("Advanced")).toBeNull();
    // ...and it says what it will actually use, not merely that nothing changed.
    // Scoped to the group: the recap sentence above the summary also says "now",
    // which is the point — they answer from one state and agree.
    const whenGroup = within(r.getByText("Timing").parentElement as HTMLElement);
    expect(whenGroup.getByText("now")).toBeTruthy();
    expect(r.getByText("Prod")).toBeTruthy();
  });

  it("keeps the Answers header for a job that declares no prompts", () => {
    // A silently missing group reads as "didn't load", not "nothing to say".
    const { rail } = renderRail(ansible());
    expect(rail().getByText("No inputs")).toBeTruthy();
    expect(rail().getByText("this job declares none")).toBeTruthy();
  });

  it("shows an answer's VALUE in the rail as it is typed, before any press of Run", () => {
    // The RU-14 "live" property, extended from settings to answers — and the
    // value, not just the name: naming an override without saying what it was
    // set to is the gap this band closed.
    const withPrompt = {
      ...ansible(),
      prompts: [{ name: "TARGET_ENV", required: false }],
    } as unknown as Job;
    const { q, rail } = renderRail(withPrompt);

    expect(rail().queryByText("staging")).toBeNull();
    fireEvent.change(q.getByLabelText(/TARGET_ENV/), { target: { value: "staging" } });
    expect(rail().getByText("staging")).toBeTruthy();
  });

  it("still shows a changed setting, now inside the summary", () => {
    const { q, rail } = renderRail(ansible());

    expect(rail().queryByText("Advanced")).toBeNull();
    fireEvent.click(q.getByRole("button", { name: /Advanced/ }));
    fireEvent.click(q.getByRole("checkbox", { name: /Check mode/ }));
    // The Advanced group appears with it — it was absent a moment ago.
    expect(rail().getByText("Advanced")).toBeTruthy();
    expect(rail().getByText(/dry run — applies nothing/)).toBeTruthy();
  });

  it("falls back to the stacked footer below the breakpoint", () => {
    setViewport(1100);
    const { container } = render(
      <RunDialog
        job={ansible()}
        scopes={railScopes}
        busy={false}
        onCancel={vi.fn()}
        onRun={vi.fn<OnRun>(async () => ({ ok: true }))}
        onDone={vi.fn()}
      />,
    );
    const q = within(container);

    expect(q.queryByText("This run")).toBeNull();
    // The recap still renders — Phase A is the narrow fallback, not a loss.
    expect(q.getByText("site-playbook")).toBeTruthy();
  });
});

// RP-1/RP-2/RP-3/RP-Q4 — run parity: an ansible run gets the same targeting
// controls as a shell run, worded for what it actually does, and the raw
// --limit escape hatch excludes them rather than silently combining.
describe("RunDialog — SSH↔Ansible parity (RP)", () => {
  const scopes = [
    { id: "s1", scope: "Prod", hosts: ["web-1", "web-2"], groups: [{ name: "webservers", hostCount: 2 }] },
  ] as unknown as ComponentProps<typeof RunDialog>["scopes"];

  const renderFor = (job: Job) => {
    const onRun = vi.fn<OnRun>(async () => ({ ok: true }));
    const { container } = render(
      <RunDialog job={job} scopes={scopes} busy={false} onCancel={vi.fn()} onRun={onRun} onDone={vi.fn()} />,
    );
    const q = within(container);
    const runBtn = () => {
      const buttons = q.getAllByRole("button");
      return buttons[buttons.length - 1] as HTMLButtonElement;
    };
    fireEvent.click(q.getByRole("button", { name: /Targets/ }));
    fireEvent.click(q.getByRole("button", { name: /Method/ }));
    // RU-6 — the raw --limit moved to Advanced (it was labelled "(advanced)" while
    // living in Where it runs); its mutual exclusion with the chips is state-level,
    // so the guard has to keep working ACROSS the two folds. Opening both is what
    // lets these tests still assert that.
    const openAdv = () => fireEvent.click(q.getByRole("button", { name: /Advanced/ }));
    return { onRun, q, runBtn, openAdv };
  };

  const ansibleJob = () =>
    ({ id: 2, name: "site-playbook", type: "ansible", scope: "Prod", executor: "runner", prompts: [] }) as unknown as Job;

  // RP-1 — the whole point: the host picker used to be hidden for ansible and
  // replaced with a static "applies to SSH runs" note.
  it("offers the host subset to an ansible run and says what it does", () => {
    const { q } = renderFor(ansibleJob());

    expect(q.getByText(/Limit to specific hosts in/)).toBeTruthy();
    expect(q.queryByText(/Host selection applies to SSH runs/)).toBeNull();

    fireEvent.click(q.getByRole("checkbox", { name: /Limit to specific hosts/ }));
    fireEvent.click(q.getByRole("checkbox", { name: "web-1" }));

    expect(q.getByText(/Ansible: passes the 1 selected host as --limit/)).toBeTruthy();
  });

  it("submits an ansible host subset as targetHosts", async () => {
    const { onRun, q, runBtn } = renderFor(ansibleJob());

    fireEvent.click(q.getByRole("checkbox", { name: /Limit to specific hosts/ }));
    fireEvent.click(q.getByRole("checkbox", { name: "web-2" }));
    fireEvent.click(runBtn());
    fireEvent.click(runBtn()); // RC-2 — the last button is now the window's Confirm & run

    await vi.waitFor(() => expect(onRun).toHaveBeenCalled());
    expect(onRun.mock.calls[0][3]).toEqual(["web-2"]);
  });

  // RP-2 — the group helper hardcoded "Ansible:" for every runner run, so a
  // terraform run claimed it was passing groups to a tool that never sees them.
  it("does not tell a terraform run it is passing groups to ansible", () => {
    const tf = { id: 3, name: "infra", type: "terraform", scope: "Prod", executor: "runner", prompts: [] } as unknown as Job;
    const { q } = renderFor(tf);

    fireEvent.click(q.getByRole("checkbox", { name: /Limit to inventory groups/ }));
    fireEvent.click(q.getByRole("checkbox", { name: /webservers/ }));

    expect(q.queryByText(/Ansible: passes/)).toBeNull();
    expect(q.getByText(/terraform does not consume group targeting/)).toBeTruthy();
  });

  // RP-Q4 — mutually exclusive, both directions, and the guard survives into
  // the submitted payload rather than living only in the disabled attribute.
  it("makes the raw --limit and the host/group pickers mutually exclusive", async () => {
    const { onRun, q, runBtn, openAdv } = renderFor(ansibleJob());
    openAdv();

    const rawLimit = q.getByPlaceholderText(/webservers/) as HTMLInputElement;
    fireEvent.change(rawLimit, { target: { value: "webservers:!quarantine" } });

    // Chips are locked out while a raw pattern is set...
    expect((q.getByRole("checkbox", { name: /Limit to specific hosts/ }) as HTMLInputElement).disabled).toBe(true);
    expect((q.getByRole("checkbox", { name: /Limit to inventory groups/ }) as HTMLInputElement).disabled).toBe(true);

    fireEvent.click(runBtn());
    fireEvent.click(runBtn()); // RC-2 — the last button is now the window's Confirm & run
    await vi.waitFor(() => expect(onRun).toHaveBeenCalled());
    // ...and only the raw pattern travels.
    expect(onRun.mock.calls[0][3]).toBeUndefined();
    expect(onRun.mock.calls[0][5]).toBe("webservers:!quarantine");
  });

  it("locks out the raw --limit while a host subset is picked", () => {
    const { q, openAdv } = renderFor(ansibleJob());
    openAdv();

    fireEvent.click(q.getByRole("checkbox", { name: /Limit to specific hosts/ }));

    expect((q.getByPlaceholderText(/webservers/) as HTMLInputElement).disabled).toBe(true);
  });

  // RU-6 — the chips' disabled tooltip used to say "below"; the field it points at
  // is now one fold away, and a stale direction is worse than none.
  it("points at Advanced options when the raw pattern locks the chips", () => {
    const { q, openAdv } = renderFor(ansibleJob());
    openAdv();

    fireEvent.change(q.getByPlaceholderText(/webservers/), { target: { value: "webservers:!quarantine" } });

    const hostsLabel = q.getByRole("checkbox", { name: /Limit to specific hosts/ }).closest("label");
    expect(hostsLabel?.getAttribute("title")).toContain("Advanced");
  });

  // RU-6 — and the pattern shows in the collapsed Advanced summary, so a raw
  // --limit can never be active with nothing on screen naming it.
  it("names an active raw --limit in the collapsed Advanced summary", () => {
    const { q, openAdv } = renderFor(ansibleJob());
    openAdv();

    fireEvent.change(q.getByPlaceholderText(/webservers/), { target: { value: "webservers:!quarantine" } });
    openAdv(); // collapse again

    // Exact text: the recap line (RU-1) also names the pattern, but as
    // "--limit … in Prod" — this asserts the summary's own bare fragment.
    expect(q.getByText("--limit webservers:!quarantine")).toBeTruthy();
  });

  // RP-10 — Phase 2 lifted the server's 422 for ansible, so the dialog offers
  // the identity fields there too, with copy that states the precedence: an
  // override is delivered as extra-vars, which BEAT the scope inventory.
  it("offers the connect-as identity fields to an ansible run", () => {
    const { q } = renderFor(ansibleJob());

    expect(q.getByText("Connect as")).toBeTruthy();
    expect(q.getByPlaceholderText("Inventory default user")).toBeTruthy();

    // RU-11 — the RP-10 precedence copy is "what the control does", so it is
    // engaged rather than always-on: hidden while the field sits at its default,
    // present the moment the operator touches it. The words are unchanged.
    expect(q.queryByText(/beats that inventory for every host/)).toBeNull();
    fireEvent.focus(q.getByPlaceholderText("Inventory default user"));
    expect(q.getByText(/beats that inventory for every host/)).toBeTruthy();
  });

  it("submits an ansible run's connect-as identity", async () => {
    caps = { manageEnvVars: true, configureApp: false };
    knownKeys = ["prod-key"];
    const { onRun, q, runBtn } = renderFor(ansibleJob());

    fireEvent.change(q.getByPlaceholderText("Inventory default user"), { target: { value: "deploy" } });
    fireEvent.click(runBtn());
    fireEvent.click(runBtn()); // RC-2 — the last button is now the window's Confirm & run

    await vi.waitFor(() => expect(onRun).toHaveBeenCalled());
    expect(onRun.mock.calls[0][8]).toEqual({ sshUser: "deploy", sshCredential: undefined });
  });

  // RP-Q2 — terraform keeps the block hidden: its providers authenticate, so
  // the server still 422s the fields and offering them would be a lie.
  it("still hides the connect-as identity fields for a terraform run", () => {
    const tf = { id: 3, name: "infra", type: "terraform", scope: "Prod", executor: "runner", prompts: [] } as unknown as Job;
    const { q } = renderFor(tf);

    expect(q.queryByText("Connect as")).toBeNull();
  });

  // RP-3 — a job with no pinned executor resolves to a concrete one here, and
  // the dialog says the resolution is the job's Auto default rather than a pin.
  it("names an Auto job's executor resolution instead of presenting it as pinned", () => {
    const auto = { id: 4, name: "deploy", type: "bash", scope: "Prod", prompts: [] } as unknown as Job;
    const { q } = renderFor(auto);

    expect(q.getByText(/resolved from the job's Auto default/)).toBeTruthy();
    expect(q.getByRole("button", { name: /Auto → SSH/ })).toBeTruthy();
  });

  it("drops the Auto wording once the operator picks an executor", () => {
    const auto = { id: 4, name: "deploy", type: "bash", scope: "Prod", prompts: [] } as unknown as Job;
    const { q } = renderFor(auto);

    fireEvent.click(q.getByRole("button", { name: /^Runner/ }));

    expect(q.queryByText(/resolved from the job's Auto default/)).toBeNull();
  });
});

// Phase 3 (RP-19) — advanced ansible options. Ansible-only, collapsed by
// default, and only sent when something was actually set.
describe("RunDialog — advanced ansible options (Phase 3)", () => {
  const scopes = [
    { id: "s1", scope: "Prod", hosts: ["web-1"], groups: [{ name: "webservers", hostCount: 1 }] },
  ] as unknown as ComponentProps<typeof RunDialog>["scopes"];

  const renderFor = (job: Job) => {
    const onRun = vi.fn<OnRun>(async () => ({ ok: true }));
    const { container } = render(
      <RunDialog job={job} scopes={scopes} busy={false} onCancel={vi.fn()} onRun={onRun} onDone={vi.fn()} />,
    );
    const q = within(container);
    const runBtn = () => {
      const buttons = q.getAllByRole("button");
      return buttons[buttons.length - 1] as HTMLButtonElement;
    };
    fireEvent.click(q.getByRole("button", { name: /Targets/ }));
    fireEvent.click(q.getByRole("button", { name: /Method/ }));
    return { onRun, q, runBtn };
  };

  const ansibleJob = () =>
    ({ id: 2, name: "site-playbook", type: "ansible", scope: "Prod", executor: "runner", prompts: [] }) as unknown as Job;

  const openAnsible = (q: ReturnType<typeof within>) =>
    fireEvent.click(q.getByRole("button", { name: /Advanced/ }));

  it("offers the ansible set under Advanced for ansible runs only", () => {
    const { q } = renderFor(ansibleJob());
    openAnsible(q);
    expect(q.getByText(/Check mode/)).toBeTruthy();

    cleanup();
    const bash = { id: 1, name: "deploy", type: "bash", scope: "Prod", executor: "ssh", prompts: [] } as unknown as Job;
    const { q: q2 } = renderFor(bash);
    openAnsible(q2);
    // RU-5 — Advanced still exists for every run type, but now that the env
    // overrides moved to Run inputs there is genuinely nothing in it for a bash
    // run, and it says so rather than looking broken.
    expect(q2.getByText(/these options are ansible's/)).toBeTruthy();
    // …and the ansible flags don't leak onto a bash run.
    expect(q2.queryByText(/Check mode/)).toBeNull();
  });

  it("sends nothing when no option is set", async () => {
    const { onRun, runBtn } = renderFor(ansibleJob());

    fireEvent.click(runBtn());
    fireEvent.click(runBtn()); // RC-2 — the last button is now the window's Confirm & run
    await vi.waitFor(() => expect(onRun).toHaveBeenCalled());
    expect(onRun.mock.calls[0][9]).toBeUndefined();
  });

  it("submits the options an operator set", async () => {
    const { onRun, q, runBtn } = renderFor(ansibleJob());
    openAnsible(q);

    fireEvent.click(q.getByRole("checkbox", { name: /Check mode/ }));
    fireEvent.change(q.getByPlaceholderText(/only these tags/), { target: { value: "certs, config" } });
    fireEvent.change(q.getByPlaceholderText(/skip these tags/), { target: { value: "reboot" } });
    fireEvent.click(runBtn());
    fireEvent.click(runBtn()); // RC-2 — the last button is now the window's Confirm & run

    await vi.waitFor(() => expect(onRun).toHaveBeenCalled());
    const opts = onRun.mock.calls[0][9];
    expect(opts?.ansibleCheck).toBe(true);
    expect(opts?.ansibleTags).toEqual(["certs", "config"]);
    expect(opts?.ansibleSkipTags).toEqual(["reboot"]);
  });

  // A dry run reads as an ordinary run everywhere else in the app, so the one
  // line that is always visible has to say it.
  it("names check mode in the always-visible collapsed summary", () => {
    const { q } = renderFor(ansibleJob());
    openAnsible(q);
    fireEvent.click(q.getByRole("checkbox", { name: /Check mode/ }));

    expect(q.getByRole("button", { name: /CHECK MODE/ })).toBeTruthy();
  });

  // The two connection vars belong to Connect as; `-e` is the same precedence
  // tier, so letting one through could make the run connect as someone other
  // than what its own record says. The server 422s it; the dialog blocks first.
  it("blocks an extra-var that would forge the run's identity", async () => {
    const { q, runBtn } = renderFor(ansibleJob());
    openAnsible(q);

    fireEvent.click(q.getByRole("button", { name: /Add extra-var/ }));
    fireEvent.change(q.getAllByPlaceholderText("KEY")[0], { target: { value: "ansible_user" } });

    expect(runBtn().disabled).toBe(true);
    expect(q.getByText(/is set by/)).toBeTruthy();
  });
});

// RV — visited-gating: a job with declared inputs cannot be run until "Where it
// runs" has been open at least once; a job without inputs keeps its one-click
// run. The review is recorded in the audit metadata so History can tell a
// reviewed manual run from an API call.
describe("RunDialog — section review gate (RV)", () => {
  it("gates Run behind viewing Where it runs when the job declares inputs", () => {
    const { runBtn, openOptions } = renderDialog(
      makeJob([{ name: "REPLICAS", required: true, default: "3" }]),
    );

    // The input itself is satisfied (default) — only the review gates.
    expect(runBtn().disabled).toBe(true);
    expect(runBtn().textContent).toBe("Review where it runs");

    openOptions();

    expect(runBtn().disabled).toBe(false);
    expect(runBtn().textContent).toBe("Run now");
  });

  it("keeps the gate satisfied after the section is closed again", () => {
    const { runBtn, openOptions } = renderDialog(
      makeJob([{ name: "REPLICAS", required: true, default: "3" }]),
    );

    openOptions(); // open…
    openOptions(); // …and close: visited means has-been-open, not is-open

    expect(runBtn().disabled).toBe(false);
  });

  it("does not gate a job with no declared inputs (RV-Q2: one-click runs)", () => {
    const { runBtn } = renderDialog(makeJob([]));

    expect(runBtn().disabled).toBe(false);
    expect(runBtn().textContent).toBe("Run now");
  });

  it("records the reviewed sections in the audit metadata", async () => {
    const { onRun, runBtn, openOptions, openAdvanced } = renderDialog(
      makeJob([{ name: "REPLICAS", required: true, default: "3" }]),
    );

    openOptions();
    openAdvanced();
    fireEvent.click(runBtn());
    fireEvent.click(runBtn()); // RC-2 — the last button is now the window's Confirm & run
    await vi.waitFor(() => expect(onRun).toHaveBeenCalled());

    expect(onRun.mock.calls[0][6]?.reviewedSections).toEqual(["variables", "targets", "method", "advanced", "confirmation"]);
  });

  it("records only what was actually visited", async () => {
    const { onRun, runBtn, openOptions } = renderDialog(
      makeJob([{ name: "REPLICAS", required: true, default: "3" }]),
    );

    openOptions();
    fireEvent.click(runBtn());
    fireEvent.click(runBtn()); // RC-2 — the last button is now the window's Confirm & run
    await vi.waitFor(() => expect(onRun).toHaveBeenCalled());

    expect(onRun.mock.calls[0][6]?.reviewedSections).toEqual(["variables", "targets", "method", "confirmation"]);
  });
});

// RC-2 — one confirmation for every run: a default-settings run opens the
// same confirmation window a deviating run does (the armed press-again path
// is gone), and the window names each deviation against the job's default.
describe("RunDialog — run confirmation (RC)", () => {
  it("a default-settings run opens the confirmation window and submits from it", async () => {
    const { onRun, q, runBtn } = renderDialog(makeJob([]));

    fireEvent.click(runBtn());
    expect(onRun).not.toHaveBeenCalled();
    expect(q.getByText("Confirm run — deploy-api")).toBeTruthy();
    expect(q.getByText(/No settings changed/)).toBeTruthy();

    fireEvent.click(q.getByRole("button", { name: "Confirm & run" }));
    await vi.waitFor(() => expect(onRun).toHaveBeenCalled());
    expect(onRun.mock.calls[0][6]?.reviewedSections).toContain("confirmation");
  });

  it("opens the deviation window instead when settings differ from the job's defaults", async () => {
    caps = { manageEnvVars: true, configureApp: false };
    const scopes = [
      { id: "s1", scope: "Prod", hosts: ["web-1", "web-2"] },
    ] as unknown as ComponentProps<typeof RunDialog>["scopes"];
    const onRun = vi.fn<OnRun>(async () => ({ ok: true }));
    const { container } = render(
      <RunDialog job={makeJob([])} scopes={scopes} busy={false} onCancel={vi.fn()} onRun={onRun} onDone={vi.fn()} />,
    );
    const q = within(container);
    fireEvent.click(q.getByRole("button", { name: /Targets/ }));
    fireEvent.click(q.getByRole("button", { name: /Method/ }));
    fireEvent.click(q.getByRole("checkbox", { name: /Limit to specific hosts/ }));
    fireEvent.click(q.getByRole("checkbox", { name: "web-1" }));

    const buttons = () => q.getAllByRole("button");
    fireEvent.click(buttons()[buttons().length - 1]);

    // Not run — the window is up, naming the deviation.
    expect(onRun).not.toHaveBeenCalled();
    expect(q.getByText("Confirm run — deploy-api")).toBeTruthy();
    expect(q.getByText("Host subset")).toBeTruthy();
    expect(q.getByText(/1 of 2: web-1/)).toBeTruthy();

    fireEvent.click(q.getByRole("button", { name: "Confirm & run" }));
    await vi.waitFor(() => expect(onRun).toHaveBeenCalled());
    expect(onRun.mock.calls[0][3]).toEqual(["web-1"]);
    // The confirmation is part of the audit record.
    expect(onRun.mock.calls[0][6]?.reviewedSections).toContain("confirmation");
  });

  // RC-3 — every declared input is restated in the window, defaults included.
  // A default is the value nobody typed in this dialog, which is exactly why it
  // must be on screen before it is sent.
  it("lists every declared input in the confirmation window, even when it is a default", () => {
    const { q, runBtn, openOptions } = renderDialog(
      makeJob([
        { name: "REPLICAS", required: true, default: "3" },
        { name: "REGION", required: false, default: "eu-west" },
      ]),
    );
    openOptions();
    fireEvent.click(runBtn());

    expect(q.getByText("Confirm run — deploy-api")).toBeTruthy();
    const list = q.getByRole("list", { name: "Inputs this run will use" });
    const rows = within(list).getAllByRole("listitem");
    expect(rows).toHaveLength(2);
    expect(rows[0].textContent).toContain("REPLICAS");
    expect(rows[0].textContent).toContain("3");
    expect(rows[0].textContent).toContain("(default)");
    expect(rows[1].textContent).toContain("REGION");
    expect(rows[1].textContent).toContain("eu-west");
    // Nothing changed, and the window still says so beneath the inputs.
    expect(q.getByText(/No settings changed/)).toBeTruthy();
  });

  it("marks an operator-typed input as yours in the confirmation window", () => {
    const { q, runBtn, openOptions } = renderDialog(makeJob([{ name: "TARGET_ENV", required: true }]));
    openOptions();
    fireEvent.change(q.getByLabelText("TARGET_ENV"), { target: { value: "prod" } });
    fireEvent.click(runBtn());

    const list = q.getByRole("list", { name: "Inputs this run will use" });
    expect(list.textContent).toContain("TARGET_ENV");
    expect(list.textContent).toContain("prod");
    expect(list.textContent).toContain("(you)");
  });

  it("shows no inputs list when the job declares no prompts", () => {
    const { q, runBtn } = renderDialog(makeJob([]));
    fireEvent.click(runBtn());
    expect(q.queryByRole("list", { name: "Inputs this run will use" })).toBeNull();
  });

  it("Back returns to the form without running", () => {
    const scopes = [
      { id: "s1", scope: "Prod", hosts: ["web-1"] },
    ] as unknown as ComponentProps<typeof RunDialog>["scopes"];
    const onRun = vi.fn<OnRun>(async () => ({ ok: true }));
    const { container } = render(
      <RunDialog job={makeJob([])} scopes={scopes} busy={false} onCancel={vi.fn()} onRun={onRun} onDone={vi.fn()} />,
    );
    const q = within(container);
    fireEvent.click(q.getByRole("button", { name: /Targets/ }));
    fireEvent.click(q.getByRole("button", { name: /Method/ }));
    fireEvent.click(q.getByRole("checkbox", { name: /Limit to specific hosts/ }));
    fireEvent.click(q.getByRole("checkbox", { name: "web-1" }));
    const buttons = () => q.getAllByRole("button");
    fireEvent.click(buttons()[buttons().length - 1]);

    fireEvent.click(q.getByRole("button", { name: "← Back" }));

    expect(onRun).not.toHaveBeenCalled();
    // The form is back, selection intact.
    expect(q.getByRole("checkbox", { name: "web-1" })).toBeTruthy();
    expect((q.getByRole("checkbox", { name: "web-1" }) as HTMLInputElement).checked).toBe(true);
  });

});

describe("RunDialog — reference preflight (T1.7)", () => {
  // The regression this pins: the preflight used to live INSIDE the "Where it
  // runs" disclosure, which unmounts its children while collapsed. The check
  // therefore never ran until the operator opened the fold — and an unresolvable
  // reference opening the fold is the entire point. Caught only by driving the
  // real app; every unit test passed with the check dead.
  it("springs the fold open and names the count while still collapsed", async () => {
    jobBindings = [{ kind: "secret", name: "RH8_BECOME_PASS", reference: "CRONOMICON_SECRET_RH8_BECOME_PASS" }];
    refVerdicts["secret RH8_BECOME_PASS"] = {
      kind: "secret",
      name: "RH8_BECOME_PASS",
      reference: "CRONOMICON_SECRET_RH8_BECOME_PASS",
      ok: false,
      outcome: "out_of_scope",
      reason: 'exists, but only in scope "Prod" — not visible from scope "Staging"',
      resolvedScope: null,
      otherScopes: ["Prod"],
    };
    const { q } = renderDialog(makeJob([]));
    // The count reaches the collapsed summary...
    await waitFor(() =>
      expect(q.getByRole("button", { name: /1 reference unresolved/ })).toBeTruthy(),
    );
    // ...and the fold opened itself, so the reason is on screen without a click.
    await waitFor(() =>
      expect(q.getAllByText(/only in scope "Prod"/).length).toBeGreaterThan(0),
    );
  });

  it("stays quiet when every reference resolves", async () => {
    jobBindings = [{ kind: "var", name: "REGION", reference: "CRONOMICON_VAR_REGION" }];
    refVerdicts["var REGION"] = {
      kind: "var",
      name: "REGION",
      reference: "CRONOMICON_VAR_REGION",
      ok: true,
      outcome: "resolved",
      reason: "resolves to the global variable",
      resolvedScope: "",
    };
    const { q } = renderDialog(makeJob([]));
    await waitFor(() => expect(q.getByRole("button", { name: /Targets/ })).toBeTruthy());
    expect(q.queryByText(/unresolved/)).toBeNull();
  });
});

// V2-11 — per-run stored-reference additions: dropdowns inside "Inputs",
// gated on ManageEnvVars, riding onRun as the trailing references argument.
describe("RunDialog — per-run reference additions", () => {
  it("adds a stored variable from the dropdowns and passes it to onRun", async () => {
    caps = { manageEnvVars: true, configureApp: false };
    knownVars = ["REGION"];
    const { onRun, q, runBtn, openInputs, openOptions } = renderDialog(makeJob([]));
    openInputs();
    openOptions();

    const nameSelect = await q.findByLabelText(/Add a stored variable to this run/);
    fireEvent.change(nameSelect, { target: { value: "REGION" } });
    // The addition renders as a removable row under "Added for this run".
    await q.findByText("CRONOMICON_VAR_REGION");
    expect(q.getByText("Added for this run")).toBeTruthy();
    // ...and reaches the collapsed summary's wording via the added count.
    expect(q.getByRole("button", { name: /1 reference added/ })).toBeTruthy();

    fireEvent.click(runBtn());
    fireEvent.click(runBtn()); // RC-2 — the last button is now the window's Confirm & run
    await vi.waitFor(() => expect(onRun).toHaveBeenCalled());
    expect(onRun.mock.calls[0][7]).toEqual([{ kind: "var", name: "REGION" }]);
  });

  it("removing an addition drops it from the submitted run", async () => {
    caps = { manageEnvVars: true, configureApp: false };
    knownVars = ["REGION"];
    const { onRun, q, runBtn, openInputs, openOptions } = renderDialog(makeJob([]));
    openInputs();
    openOptions();

    const nameSelect = await q.findByLabelText(/Add a stored variable to this run/);
    fireEvent.change(nameSelect, { target: { value: "REGION" } });
    await q.findByText("CRONOMICON_VAR_REGION");
    fireEvent.click(q.getByRole("button", { name: "Remove reference REGION" }));
    await waitFor(() => expect(q.queryByText("CRONOMICON_VAR_REGION")).toBeNull());

    fireEvent.click(runBtn());
    fireEvent.click(runBtn()); // RC-2 — the last button is now the window's Confirm & run
    await vi.waitFor(() => expect(onRun).toHaveBeenCalled());
    expect(onRun.mock.calls[0][7]).toBeUndefined();
  });

  it("hides the add controls without ManageEnvVars", async () => {
    knownVars = ["REGION"]; // caps stay manageEnvVars: false
    jobBindings = [{ kind: "var", name: "BASE", reference: "CRONOMICON_VAR_BASE" }];
    const { q, openInputs, openOptions } = renderDialog(makeJob([]));
    openInputs();
    openOptions();

    // The read-only preflight still renders the declared binding...
    await q.findByText("CRONOMICON_VAR_BASE");
    // ...but neither dropdown exists for a caller who may not grant references.
    expect(q.queryByLabelText("Reference kind to add")).toBeNull();
    expect(q.queryByLabelText(/Add a stored/)).toBeNull();
  });

  it("warns that an added key gets the run refused on the SSH executor", async () => {
    caps = { manageEnvVars: true, configureApp: false };
    knownKeys = ["deploy"];
    const { q, openInputs, openOptions } = renderDialog(makeJob([])); // makeJob executor: ssh
    openInputs();
    openOptions();

    const kindSelect = await q.findByLabelText("Reference kind to add");
    fireEvent.change(kindSelect, { target: { value: "key" } });
    const nameSelect = await q.findByLabelText(/Add a stored ssh key to this run/i);
    fireEvent.change(nameSelect, { target: { value: "deploy" } });

    await q.findByText("CRONOMICON_KEY_deploy");
    expect(q.getByText(/This run will be refused.*cannot deliver SSH keys/)).toBeTruthy();
  });

  // KB — the server refuses a key-bound run that resolved to ssh with 422
  // key_binding_requires_runner. The dialog shows the message and stays open; it
  // must NOT silently force the runner executor the way invalid_executor does,
  // because binding the key as a Secret is the other legitimate way out.
  it("shows the key-binding refusal inline and keeps the dialog open", async () => {
    const onDone = vi.fn();
    const onRun = vi.fn<OnRun>(async () => ({
      ok: false,
      code: "key_binding_requires_runner",
      message: "this job binds SSH key CRONOMICON_KEY_deploy, which only a runner can deliver; this run resolved to the ssh executor — run it on a runner, or bind the key as a Secret and write the file in the job body",
    }));
    const { container } = render(
      <RunDialog job={makeJob([])} scopes={[]} busy={false} onCancel={vi.fn()} onRun={onRun} onDone={onDone} />,
    );
    const q = within(container);
    const runBtn = () => {
      const buttons = q.getAllByRole("button");
      return buttons[buttons.length - 1] as HTMLButtonElement;
    };
    fireEvent.click(runBtn());
    // RC-2 — a second press confirms when the window opened; harmless otherwise.
    const confirm = q.queryByRole("button", { name: /Confirm/ });
    if (confirm) fireEvent.click(confirm);
    await vi.waitFor(() => expect(onRun).toHaveBeenCalled());
    await q.findByText(/only a runner can deliver/);
    expect(onDone).not.toHaveBeenCalled();
    expect(q.getByText(/bind the key as a Secret/)).toBeTruthy();
  });
});

// RB-29 — the UI half of RB-26, and the reason that rule is shippable at all.
//
// The server now refuses an unscoped job from a restricted caller (403
// scope_required). A rule whose only compliance path is an error message is the
// worst possible shape for a change to a job that worked yesterday, so the dialog
// has to surface the remedy: open the section that holds the picker, say the scope
// is required, and block submission with an explanation rather than let the request
// go and come back refused.
describe("RunDialog — unscoped jobs must bind a scope (RB-29)", () => {
  const unscoped = { id: 1, name: "restart-service", type: "bash" } as unknown as Job;

  it("blocks a restricted caller until a scope is chosen, and says so", async () => {
    caps = { manageEnvVars: false, configureApp: false, unrestricted: false } as typeof caps;
    detailResponse = makeJob([]);
    const { q, runBtn } = renderDialog(unscoped);

    await vi.waitFor(() => expect(runBtn().disabled).toBe(true));
    // Naming the missing thing is the whole point — a greyed-out button with no
    // reason is what sends someone to the API docs.
    expect(runBtn().textContent).toContain("Choose a scope");
    // The picker must be VISIBLE, not folded inside a collapsed section.
    expect(q.getByText(/Target scope \(required\)/)).toBeTruthy();
  });

  it("lets an unrestricted caller run it unbound", async () => {
    caps = { manageEnvVars: false, configureApp: false, unrestricted: true } as typeof caps;
    detailResponse = makeJob([]);
    const { runBtn } = renderDialog(unscoped);

    // The system-global case §2.5 preserves: "back up the Cronomicon DB" stays
    // runnable by an admin with no scope named.
    await vi.waitFor(() => expect(runBtn().disabled).toBe(false));
    expect(runBtn().textContent).not.toContain("Choose a scope");
  });

  it("leaves a scoped job alone", async () => {
    caps = { manageEnvVars: false, configureApp: false, unrestricted: false } as typeof caps;
    detailResponse = makeJob([]);
    const { runBtn } = renderDialog({ id: 1, name: "deploy-api", type: "bash", scope: "Prod" } as unknown as Job);

    // A job that declares its own scope carries its own authority; nothing changes.
    await vi.waitFor(() => expect(runBtn().disabled).toBe(false));
  });
});

// FX-Q1/FX-C — a paused job is runnable from the UI, and the operator is told.
//
// The server deliberately lets a human click override a pause: pause exists to
// stop automation, not the operator standing in front of it. That override is
// only defensible if it is a choice, so a paused job always goes through the
// confirmation window — even with nothing else deviating, where an unpaused job
// would confirm inline with a second button press.
describe("RunDialog — paused job", () => {
  const pausedJob = () => ({ ...makeJob([]), status: "paused" }) as unknown as Job;

  it("names the pause in a confirmation window before running", async () => {
    const { q, runBtn, onRun } = renderDialog(pausedJob());

    fireEvent.click(runBtn());

    // Not the inline two-press path: a real window, naming the pause.
    await waitFor(() => expect(q.getByText(/This job is paused or disabled/)).toBeTruthy());
    expect(q.getByText(/that for this run only/)).toBeTruthy();
    // And nothing has been submitted yet.
    expect(onRun).not.toHaveBeenCalled();

    const confirm = q.getByRole("button", { name: /Confirm & run/ });
    await act(async () => {
      fireEvent.click(confirm);
    });
    expect(onRun).toHaveBeenCalledTimes(1);
  });

  it("does not show the pause notice for a job that is not paused", () => {
    const { q, runBtn, onRun } = renderDialog(makeJob([]));

    fireEvent.click(runBtn());

    // The window still opens (RC-2), but without the notice — this is the
    // control that keeps the test above honest about WHY the notice appeared.
    expect(q.getByText("Confirm run — deploy-api")).toBeTruthy();
    expect(q.queryByText(/This job is paused or disabled/)).toBeNull();
    expect(onRun).not.toHaveBeenCalled();
  });
});

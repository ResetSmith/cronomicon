// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, waitFor, within } from "@testing-library/react";

// Authoring-time reference validation (agencies plan T1.4-T1.7, T1.11). The
// verdict states and the Run-dialog preflight are the whole deliverable of Phase 1
// — the originating debugging session was lost because none of this was visible
// until dispatch — so they are asserted here rather than left to a manual look.
//
// The editor renders one merged row per reference since VU-20 (chip + separate
// unresolved list collapsed into a line), so these assert against rows; the
// preflight panel still renders chips and is unchanged.

// GET responses keyed by the path the component asks for.
let getResponses: Record<string, unknown> = {};
// Whether the mocked capabilities grant ManageEnvVars — the editor's write
// controls, including each row's remove button, hang off it.
let manageEnvVars = false;
// Every /references/validate body the component posted, in order.
const postedBodies: { scope?: string; references?: { kind: string; name: string }[] }[] = [];
// Every binding-set PUT, in order — the promoted key field and the secrets/variables
// editor write the SAME set from two places, so what each one sends is the thing
// worth asserting (EV-6).
const putBodies: { path: string; body: { bindings?: { kind: string; name: string }[] } }[] = [];
// The verdicts the mocked validator returns, keyed by `<kind> <name>`.
let verdicts: Record<string, unknown> = {};

vi.mock("../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/client")>();
  return {
    ...actual,
    api: {
      GET: vi.fn(async (path: string) => ({ data: getResponses[path] ?? {} })),
      PUT: vi.fn(async (path: string, opts: { body?: unknown }) => {
        putBodies.push({ path, body: opts.body as { bindings?: { kind: string; name: string }[] } });
        return { data: {} };
      }),
      POST: vi.fn(async (path: string, opts: { body?: unknown }) => {
        if (path !== "/references/validate") return { data: {} };
        const body = opts.body as { scope?: string; references?: { kind: string; name: string }[] };
        postedBodies.push(body);
        return {
          data: {
            // A reference this test did not stub simply gets no verdict, as it would
            // if the server omitted it — not an undefined hole in the results array.
            results: (body.references ?? []).map((r) => verdicts[`${r.kind} ${r.name}${(r as { as?: string }).as ? ` as ${(r as { as?: string }).as}` : ""}`]).filter(Boolean),
          },
        };
      }),
    } as unknown as typeof actual.api,
    fetchCapabilities: vi.fn(async () => ({ manageEnvVars, configureApp: false })),
  };
});

import {
  JobKeyField,
  ReferenceBindingsEditor,
  ReferencePreflightPanel,
  useRunReferencePreflight,
} from "./ReferenceBindings";

// The preflight is a hook + a presentational panel rather than one component,
// because the Run dialog's "Where it runs" disclosure unmounts its children while
// collapsed — a check living inside the panel would never run until opened, and
// opening it is precisely what an unresolved reference is supposed to trigger.
// This harness mirrors that split: the hook is called by a always-mounted host.
function PreflightHost({
  jobId,
  scriptRef,
  scope,
  onState,
}: {
  jobId?: number;
  scriptRef?: string | null;
  scope: string;
  onState?: (n: number) => void;
}) {
  const state = useRunReferencePreflight({ jobId, scriptRef, scope });
  onState?.(state.unresolved);
  return <ReferencePreflightPanel state={state} scope={scope} />;
}

const binding = (kind: string, name: string) => ({
  kind,
  name,
  reference: `CRONOMICON_${kind === "secret" ? "SECRET" : kind === "key" ? "KEY" : "VAR"}_${name}`,
});

const verdict = (kind: string, name: string, over: Record<string, unknown>) => ({
  ...binding(kind, name),
  ok: false,
  outcome: "not_found",
  reason: "",
  resolvedScope: null,
  ...over,
});

beforeEach(() => {
  getResponses = {};
  verdicts = {};
  manageEnvVars = false;
  postedBodies.length = 0;
  putBodies.length = 0;
});
afterEach(cleanup);

describe("reference rows", () => {
  it("shows the resolved scope on a reference that resolves (T1.6)", async () => {
    getResponses["/job-reference-bindings/{jobId}"] = { bindings: [binding("secret", "DB_PASS")] };
    verdicts["secret DB_PASS"] = verdict("secret", "DB_PASS", {
      ok: true,
      outcome: "resolved",
      resolvedScope: "prod",
      reason: 'resolves to the secret in scope "prod"',
    });

    const { container } = render(<ReferenceBindingsEditor owner={{ job: 1 }} scope="prod" />);
    const q = within(container);
    await waitFor(() => expect(q.getByText("CRONOMICON_SECRET_DB_PASS")).toBeTruthy());
    // The row states WHICH row won, not merely that one did. The scope pill lived
    // only on the chip before VU-20 and had to survive the merge.
    await waitFor(() => expect(q.getByText("prod")).toBeTruthy());
    expect(q.getByText("✓")).toBeTruthy();
    expect(q.getAllByRole("listitem")).toHaveLength(1);
  });

  it('labels the global row "global" rather than leaving it blank (T1.8)', async () => {
    getResponses["/job-reference-bindings/{jobId}"] = { bindings: [binding("var", "REGION")] };
    verdicts["var REGION"] = verdict("var", "REGION", {
      ok: true,
      outcome: "resolved",
      resolvedScope: "",
      reason: "resolves to the global variable",
    });

    const { container } = render(<ReferenceBindingsEditor owner={{ job: 1 }} scope="dev" />);
    await waitFor(() => expect(within(container).getByText("global")).toBeTruthy());
  });

  it("states the precise reason for an out-of-scope reference (G1)", async () => {
    getResponses["/job-reference-bindings/{jobId}"] = { bindings: [binding("secret", "RH8_BECOME_PASS")] };
    verdicts["secret RH8_BECOME_PASS"] = verdict("secret", "RH8_BECOME_PASS", {
      outcome: "out_of_scope",
      otherScopes: ["dwss_nwd"],
      reason: 'exists, but only in scope "dwss_nwd" — not visible from scope "dev"',
    });

    const { container } = render(<ReferenceBindingsEditor owner={{ job: 1 }} scope="dev" />);
    const q = within(container);
    // The distinguishing fact the dispatch 409 deliberately withholds, now on the
    // authoring surface and in words, not only in a hover.
    //
    // ONCE, not twice (VU-20): the reason used to appear both as the chip's
    // screen-reader text and again as a line in the list beneath it. It is one
    // node now, visible on the row AND wired as the row's accessible description,
    // so it still cannot be hover-only — which is how it was missed originally.
    const reason = await waitFor(() => q.getByText(/exists, but only in scope "dwss_nwd"/));
    expect(q.getAllByText(/exists, but only in scope "dwss_nwd"/)).toHaveLength(1);
    expect(q.getByRole("listitem").getAttribute("aria-describedby")).toBe(reason.id);
    expect(q.getByText("only dwss_nwd")).toBeTruthy();
  });

  it("marks a reference that exists nowhere as missing", async () => {
    getResponses["/job-reference-bindings/{jobId}"] = { bindings: [binding("secret", "TYPO")] };
    verdicts["secret TYPO"] = verdict("secret", "TYPO", {
      outcome: "not_found",
      reason: 'no secret named "TYPO" is visible from scope "dev"',
    });

    const { container } = render(<ReferenceBindingsEditor owner={{ job: 1 }} scope="dev" />);
    await waitFor(() => expect(within(container).getByText("missing")).toBeTruthy());
  });

  it("does not validate when no scope is supplied", async () => {
    getResponses["/job-reference-bindings/{jobId}"] = { bindings: [binding("secret", "DB_PASS")] };
    const { container } = render(<ReferenceBindingsEditor owner={{ job: 1 }} />);
    await waitFor(() => expect(within(container).getByText("CRONOMICON_SECRET_DB_PASS")).toBeTruthy());
    expect(postedBodies).toHaveLength(0);
  });

  it("renders a row for EVERY reference when they all resolve (VU-20)", async () => {
    // The regression the chip/list merge is most likely to introduce, and the case
    // nothing covered before it.
    //
    // The list this row replaced filtered to failures (`verdicts.filter(v => !v.ok)`).
    // If the merged rows inherit that filter, a job whose references all resolve
    // shows an EMPTY References section — the operator loses the inventory of what
    // the job consumes, which is the section's whole purpose. Three healthy
    // references must therefore produce three rows, not zero.
    getResponses["/job-reference-bindings/{jobId}"] = {
      bindings: [binding("secret", "DB_PASS"), binding("var", "REGION"), binding("key", "ANSIBLE_RH8")],
    };
    verdicts["secret DB_PASS"] = verdict("secret", "DB_PASS", {
      ok: true,
      outcome: "resolved",
      resolvedScope: "prod",
      reason: 'resolves to the secret in scope "prod"',
    });
    verdicts["var REGION"] = verdict("var", "REGION", {
      ok: true,
      outcome: "resolved",
      resolvedScope: "",
      reason: "resolves to the global variable",
    });
    // resolvedScope is null for keys — a ✓ with no scope pill, which must not
    // disqualify the row from rendering either.
    verdicts["key ANSIBLE_RH8"] = verdict("key", "ANSIBLE_RH8", {
      ok: true,
      outcome: "resolved",
      resolvedScope: null,
      reason: "resolves to the SSH key credential",
    });

    const { container } = render(<ReferenceBindingsEditor owner={{ job: 1 }} scope="prod" />);
    const q = within(container);
    await waitFor(() => expect(q.getAllByText("✓")).toHaveLength(3));
    expect(q.getAllByRole("listitem")).toHaveLength(3);
    for (const name of ["CRONOMICON_SECRET_DB_PASS", "CRONOMICON_VAR_REGION", "CRONOMICON_KEY_ANSIBLE_RH8"]) {
      expect(q.getByText(name)).toBeTruthy();
    }
    // Nothing says "No references declared." while three are declared.
    expect(q.queryByText(/No references declared/)).toBeNull();
  });

  it("anchors the × to its own row rather than letting a wrapped reason carry it away", async () => {
    // The nit this fixes: `marginLeft: auto` put the × at the end of the FLEX FLOW,
    // so a reason long enough to wrap took the × down to the next line — between two
    // rows, reading as the neighbour's control. jsdom cannot measure the wrap, so the
    // anchoring is what gets asserted; the visual proof is a screenshot.
    manageEnvVars = true;
    getResponses["/job-reference-bindings/{jobId}"] = { bindings: [binding("secret", "DB_PASS")] };
    getResponses["/env-secrets"] = { items: [] };
    verdicts["secret DB_PASS"] = verdict("secret", "DB_PASS", {
      outcome: "not_found",
      reason: 'no secret named "DB_PASS" is visible from scope "dev" — a reason long enough to wrap',
    });

    const { container } = render(<ReferenceBindingsEditor owner={{ job: 1 }} scope="dev" />);
    const q = within(container);
    const btn = await waitFor(() => q.getByRole("button", { name: "Remove reference DB_PASS" }));
    expect(btn.style.position).toBe("absolute");
    expect(btn.style.marginLeft).toBe("");
    expect(q.getByRole("listitem").style.position).toBe("relative");
  });

  it("keeps a remove control on each row (VU-20)", async () => {
    // The × is the only way to delete a declared reference, and it lived on the
    // chip. It has to arrive on the merged row or the editor silently becomes
    // append-only.
    manageEnvVars = true;
    getResponses["/job-reference-bindings/{jobId}"] = {
      bindings: [binding("secret", "DB_PASS"), binding("var", "REGION")],
    };
    verdicts["secret DB_PASS"] = verdict("secret", "DB_PASS", { ok: true, outcome: "resolved", resolvedScope: "prod" });
    verdicts["var REGION"] = verdict("var", "REGION", { ok: true, outcome: "resolved", resolvedScope: "" });

    const { container } = render(<ReferenceBindingsEditor owner={{ job: 1 }} scope="prod" />);
    const q = within(container);
    await waitFor(() => expect(q.getAllByRole("button", { name: /^Remove reference/ })).toHaveLength(2));

    q.getByRole("button", { name: "Remove reference DB_PASS" }).click();
    await waitFor(() => expect(q.queryByText("CRONOMICON_SECRET_DB_PASS")).toBeNull());
    expect(q.getByText("CRONOMICON_VAR_REGION")).toBeTruthy();
  });
});

// ── EV-6: the SSH key promoted out of the mixed list ─────────────────────────
// Job detail now edits ONE binding set from two surfaces — JobKeyField (keys) and
// the editor narrowed to secret+var. The split is only safe if each write carries
// back the kinds it does not display, so that is what these pin: a key save must
// keep the secrets, and a secrets save must keep the key. Getting this wrong deletes
// live bindings rather than merely looking wrong.
describe("promoted SSH key field", () => {
  const creds = (...labels: string[]) => ({ items: labels.map((label, i) => ({ id: `c${i}`, label, source: "stored" })) });

  it("renders the bound key with its verdict, and never a resolved-scope pill", async () => {
    getResponses["/job-reference-bindings/{jobId}"] = {
      bindings: [binding("key", "ANSIBLE_RH8"), binding("secret", "DB_PASS")],
    };
    verdicts["key ANSIBLE_RH8"] = verdict("key", "ANSIBLE_RH8", {
      ok: true,
      outcome: "resolved",
      resolvedScope: null,
      reason: "resolves to the SSH key credential",
    });

    const { container } = render(<JobKeyField jobId={1} scope="prod" executor="runner" />);
    const q = within(container);
    await waitFor(() => expect(q.getByText("CRONOMICON_KEY_ANSIBLE_RH8")).toBeTruthy());
    expect(q.getByText("✓")).toBeTruthy();
    expect(q.getByText("SSH key")).toBeTruthy();
    // The secret belongs to the other surface and must not appear in this one.
    expect(q.queryByText("CRONOMICON_SECRET_DB_PASS")).toBeNull();
  });

  it("states the SSH-executor refusal, and only where it applies", async () => {
    // Only the RUNNER path materializes a declared key (runref/resolve.go, D8); a
    // run that resolves to the in-app SSH executor is refused at enqueue (KB). A
    // field that presented every executor as honouring the binding would be the
    // wrong kind of prominent.
    getResponses["/job-reference-bindings/{jobId}"] = { bindings: [binding("key", "PROD_DEPLOY")] };

    const ssh = render(<JobKeyField jobId={1} scope="" executor="ssh" />);
    await waitFor(() => expect(within(ssh.container).getByText(/Refused on the in-app SSH executor/)).toBeTruthy());
    cleanup();

    const runner = render(<JobKeyField jobId={1} scope="" executor="runner" />);
    await waitFor(() => expect(within(runner.container).getByText("CRONOMICON_KEY_PROD_DEPLOY")).toBeTruthy());
    expect(within(runner.container).queryByText(/Refused/)).toBeNull();
  });

  it("renders on every job so a key can always be declared", async () => {
    // The field is NOT gated on the executor: with keys removed from the editor
    // below, a hidden field would leave no way to declare one at all — and the
    // tempting gate is backwards anyway, since the runner path is the one that
    // delivers keys.
    getResponses["/job-reference-bindings/{jobId}"] = { bindings: [] };
    const { container } = render(<JobKeyField jobId={1} scope="" executor="runner" />);
    const q = within(container);
    await waitFor(() => expect(q.getByText("SSH key")).toBeTruthy());
    expect(q.getByText(/None declared/)).toBeTruthy();
  });

  it("offers stored key labels — the picker no source had before EV-6", async () => {
    manageEnvVars = true;
    getResponses["/job-reference-bindings/{jobId}"] = { bindings: [] };
    getResponses["/ssh/credentials"] = creds("BOUND", "PROD_DEPLOY");

    const { container } = render(<JobKeyField jobId={1} scope="" executor="runner" />);
    const q = within(container);
    await waitFor(() => expect(q.getByRole("option", { name: "PROD_DEPLOY" })).toBeTruthy());
    expect(q.getByRole("option", { name: "BOUND" })).toBeTruthy();
  });

  // v0.55.14 — once a key is bound the add picker is gone (no "Add another
  // key…"): the field shows the row, its verdict and the × remove; swapping is
  // remove-then-assign. Multi-key binding lives in the Job Composer.
  it("hides the picker once a key is bound", async () => {
    manageEnvVars = true;
    getResponses["/job-reference-bindings/{jobId}"] = { bindings: [binding("key", "BOUND")] };
    getResponses["/ssh/credentials"] = creds("BOUND", "PROD_DEPLOY");

    const { container } = render(<JobKeyField jobId={1} scope="" executor="runner" />);
    const q = within(container);
    await waitFor(() => expect(q.getByText("CRONOMICON_KEY_BOUND")).toBeTruthy());
    expect(q.queryByRole("combobox")).toBeNull();
  });

  it("preserves the secrets and variables it does not display when assigning a key", async () => {
    manageEnvVars = true;
    getResponses["/job-reference-bindings/{jobId}"] = {
      bindings: [binding("secret", "DB_PASS"), binding("var", "REGION")],
    };
    getResponses["/ssh/credentials"] = creds("PROD_DEPLOY");

    const { container } = render(<JobKeyField jobId={1} scope="" executor="runner" />);
    const q = within(container);
    const select = await waitFor(() => q.getByRole("combobox"));
    fireEvent.change(select, { target: { value: "PROD_DEPLOY" } });

    await waitFor(() => expect(putBodies).toHaveLength(1));
    expect(putBodies[0].body.bindings).toEqual([
      { kind: "secret", name: "DB_PASS" },
      { kind: "var", name: "REGION" },
      { kind: "key", name: "PROD_DEPLOY" },
    ]);
  });

  it("preserves the key when the narrowed editor saves", async () => {
    manageEnvVars = true;
    getResponses["/job-reference-bindings/{jobId}"] = {
      bindings: [binding("key", "PROD_DEPLOY"), binding("secret", "DB_PASS")],
    };

    const { container } = render(
      <ReferenceBindingsEditor owner={{ job: 1 }} scope="prod" kinds={["secret", "var"]} />,
    );
    const q = within(container);
    // The key is not this editor's business any more…
    await waitFor(() => expect(q.getByText("CRONOMICON_SECRET_DB_PASS")).toBeTruthy());
    expect(q.queryByText("CRONOMICON_KEY_PROD_DEPLOY")).toBeNull();
    expect(q.queryByRole("option", { name: "SSH Key" })).toBeNull();

    q.getByRole("button", { name: "Remove reference DB_PASS" }).click();
    await waitFor(() => expect(q.getByRole("button", { name: "Save references" })).toBeTruthy());
    q.getByRole("button", { name: "Save references" }).click();

    // …but dropping the last secret must not drop the key with it.
    await waitFor(() => expect(putBodies).toHaveLength(1));
    expect(putBodies[0].body.bindings).toEqual([{ kind: "key", name: "PROD_DEPLOY" }]);
  });
});

describe("ReferencePreflight", () => {
  beforeEach(() => {
    getResponses["/job-reference-bindings/{jobId}"] = { bindings: [binding("secret", "DB_PASS")] };
    verdicts["secret DB_PASS"] = verdict("secret", "DB_PASS", {
      outcome: "out_of_scope",
      otherScopes: ["prod"],
      reason: 'exists, but only in scope "prod" — not visible from scope "dev"',
    });
  });

  it("recomputes when the effective scope changes (T1.11/G6)", async () => {
    const { container, rerender } = render(<PreflightHost jobId={1} scope="dev" />);
    await waitFor(() => expect(postedBodies).toHaveLength(1));
    expect(postedBodies[0].scope).toBe("dev");

    // The Run dialog's scope override moving is exactly the case job detail cannot
    // predict — the preflight has to re-ask, not reuse the first answer.
    verdicts["secret DB_PASS"] = verdict("secret", "DB_PASS", {
      ok: true,
      outcome: "resolved",
      resolvedScope: "prod",
      reason: 'resolves to the secret in scope "prod"',
    });
    rerender(<PreflightHost jobId={1} scope="prod" />);
    await waitFor(() => expect(postedBodies).toHaveLength(2));
    expect(postedBodies[1].scope).toBe("prod");
    await waitFor(() =>
      expect(within(container).getByText(/All references resolve in prod/)).toBeTruthy(),
    );
  });

  it("reports the unresolved count to its host so the fold cannot hide it", async () => {
    const onUnresolved = vi.fn();
    render(<PreflightHost jobId={1} scope="dev" onState={onUnresolved} />);
    await waitFor(() => expect(onUnresolved).toHaveBeenCalledWith(1));
  });

  it("merges the referenced script's bindings, as dispatch does", async () => {
    getResponses["/script-reference-bindings/{name}"] = {
      bindings: [binding("secret", "DB_PASS"), binding("var", "REGION")],
    };
    verdicts["var REGION"] = verdict("var", "REGION", { ok: true, outcome: "resolved", resolvedScope: "" });

    render(<PreflightHost jobId={1} scriptRef="deploy" scope="dev" />);
    await waitFor(() => expect(postedBodies).toHaveLength(1));
    // DB_PASS appears in BOTH sets and must be validated once (dispatch dedupes by
    // kind+name); REGION comes only from the script.
    expect(postedBodies[0].references).toEqual([
      { kind: "secret", name: "DB_PASS" },
      { kind: "var", name: "REGION" },
    ]);
  });

  it("renders nothing when there is nothing to check", async () => {
    getResponses["/job-reference-bindings/{jobId}"] = { bindings: [] };
    const { container } = render(<PreflightHost jobId={1} scope="dev" />);
    await waitFor(() => expect(container.textContent).toBe(""));
    expect(postedBodies).toHaveLength(0);
  });
});

// ── RA-1 / RA-Q16 — the alias (the runas-update plan §11.3, Unit 4) ──
//
// Phase A shipped aliasing as an API-only mechanism in v0.57.0: the thing built so
// an agency user could run one shared job with their own department's credential
// was not reachable by one without curl. These cover the UI that closes that.
//
// The property worth protecting is that an alias is a DESTINATION, never a
// selector. It changes the key a value lands on; it must never change which row
// resolves, or the whole entitlement argument inverts.
describe("alias (RA-1)", () => {
  it("sends the alias to the validator, so the verdict describes what will happen", async () => {
    manageEnvVars = true;
    getResponses["/job-reference-bindings/{jobId}"] = {
      bindings: [{ ...binding("secret", "TEAMA_SUDO"), as: "BECOME_PASSWORD" }],
    };

    render(<ReferenceBindingsEditor owner={{ job: 1 }} scope="prod" />);
    // A validator asked about the bare row would answer about a different injection
    // than the one the job performs — the alias has to ride along.
    await waitFor(() => expect(postedBodies.length).toBeGreaterThan(0));
    const refs = postedBodies[postedBodies.length - 1].references ?? [];
    expect(refs).toHaveLength(1);
    expect((refs[0] as { as?: string }).as).toBe("BECOME_PASSWORD");
  });

  it("shows the row AND the destination it lands on", async () => {
    getResponses["/job-reference-bindings/{jobId}"] = {
      bindings: [{ ...binding("secret", "TEAMA_SUDO"), as: "BECOME_PASSWORD" }],
    };

    const { container } = render(<ReferenceBindingsEditor owner={{ job: 1 }} scope="prod" />);
    const q = within(container);
    // Both names, because each answers a different question: the row says WHOSE
    // credential this is, the destination says what the playbook reads. A chip
    // showing only one of them cannot answer both.
    await waitFor(() => expect(q.getByText("CRONOMICON_SECRET_TEAMA_SUDO")).toBeTruthy());
    expect(q.getByText("CRONOMICON_SECRET_BECOME_PASSWORD")).toBeTruthy();
  });

  it("persists the alias on save", async () => {
    manageEnvVars = true;
    getResponses["/job-reference-bindings/{jobId}"] = { bindings: [] };

    const { container } = render(<ReferenceBindingsEditor owner={{ job: 1 }} scope="prod" />);
    const q = within(container);
    await waitFor(() => expect(q.getByPlaceholderText("reference name…")).toBeTruthy());
    fireEvent.change(q.getByPlaceholderText("reference name…"), { target: { value: "TEAMA_SUDO" } });
    fireEvent.change(q.getByPlaceholderText("inject as… (optional)"), { target: { value: "BECOME_PASSWORD" } });
    fireEvent.click(q.getByText("Add"));
    await waitFor(() => expect(q.getByText("Save references")).toBeTruthy());
    fireEvent.click(q.getByText("Save references"));

    await waitFor(() => expect(putBodies.length).toBeGreaterThan(0));
    const sent = (putBodies[putBodies.length - 1].body.bindings ?? []) as { name: string; as?: string }[];
    // A PUT that dropped `as` would silently downgrade the binding to inject under
    // the row's own name — the job body would then read nothing at all.
    expect(sent).toHaveLength(1);
    expect(sent[0].name).toBe("TEAMA_SUDO");
    expect(sent[0].as).toBe("BECOME_PASSWORD");
  });

  it("gives each destination its OWN verdict when one row is bound twice", async () => {
    // This is what makes the alias part of a binding's IDENTITY load-bearing. Both
    // rows name TEAMA_SUDO; only their destinations differ. If identity were
    // kind+name, the two would collapse into one verdict lookup and the second row
    // would inherit the first's answer — showing a green tick over a reference that
    // will not resolve, which is the precise failure the validator exists to prevent.
    getResponses["/job-reference-bindings/{jobId}"] = {
      bindings: [
        { ...binding("secret", "TEAMA_SUDO"), as: "BECOME_PASSWORD" },
        { ...binding("secret", "TEAMA_SUDO"), as: "SUDO_PASS" },
      ],
    };
    verdicts["secret TEAMA_SUDO as BECOME_PASSWORD"] = verdict("secret", "TEAMA_SUDO", {
      as: "BECOME_PASSWORD",
      ok: true,
      outcome: "resolved",
      resolvedScope: "prod",
      reason: "resolves fine under this destination",
    });
    verdicts["secret TEAMA_SUDO as SUDO_PASS"] = verdict("secret", "TEAMA_SUDO", {
      as: "SUDO_PASS",
      ok: false,
      outcome: "invalid",
      reason: "this destination is refused",
    });

    const { container } = render(<ReferenceBindingsEditor owner={{ job: 1 }} scope="prod" />);
    const q = within(container);
    await waitFor(() => expect(q.getByText("this destination is refused")).toBeTruthy());
    expect(q.getByText("resolves fine under this destination")).toBeTruthy();
  });

  it("treats one row under two destinations as two bindings", async () => {
    manageEnvVars = true;
    getResponses["/job-reference-bindings/{jobId}"] = {
      bindings: [{ ...binding("secret", "TEAMA_SUDO"), as: "BECOME_PASSWORD" }],
    };

    const { container } = render(<ReferenceBindingsEditor owner={{ job: 1 }} scope="prod" />);
    const q = within(container);
    await waitFor(() => expect(q.getByPlaceholderText("reference name…")).toBeTruthy());
    // Same row, different destination. Keying identity on kind+name alone would
    // silently swallow this as a duplicate — it is not one: it lands a second key.
    fireEvent.change(q.getByPlaceholderText("reference name…"), { target: { value: "TEAMA_SUDO" } });
    fireEvent.change(q.getByPlaceholderText("inject as… (optional)"), { target: { value: "SUDO_PASS" } });
    fireEvent.click(q.getByText("Add"));

    await waitFor(() => expect(q.getByText("CRONOMICON_SECRET_SUDO_PASS")).toBeTruthy());
    expect(q.getByText("CRONOMICON_SECRET_BECOME_PASSWORD")).toBeTruthy();
    expect(q.getAllByText("CRONOMICON_SECRET_TEAMA_SUDO")).toHaveLength(2);
  });

  it("previews the destination key while the alias is being typed", async () => {
    manageEnvVars = true;
    getResponses["/job-reference-bindings/{jobId}"] = { bindings: [] };

    const { container } = render(<ReferenceBindingsEditor owner={{ job: 1 }} scope="prod" />);
    const q = within(container);
    await waitFor(() => expect(q.getByPlaceholderText("inject as… (optional)")).toBeTruthy());
    fireEvent.change(q.getByPlaceholderText("inject as… (optional)"), { target: { value: "BECOME_PASSWORD" } });
    // The alias is a bare name but the injected key is prefixed; showing the derived
    // form removes the one guess an author would otherwise have to make.
    await waitFor(() => expect(q.getByText(/CRONOMICON_SECRET_BECOME_PASSWORD/)).toBeTruthy());
  });
});

import { describe, expect, it } from "vitest";
import { SUMMARY_VALUE_CAP, buildRunSummary, truncate, type RunSummaryInput } from "./runSummary";
import type { InputState } from "./RunInputs";

// RS-2 (the run-summary plan) — the summary model.
//
// These test the PURE function, which is the whole reason it is one: the rail it
// feeds lives inside a 3000-line dialog behind a 1200px breakpoint and a review
// gate, and every one of the rules below would otherwise need all three set up to
// assert one string.

const prompt = (over: Partial<InputState> = {}): InputState => ({
  p: { name: "TARGET_ENV" },
  value: "staging",
  from: "you",
  unfilled: false,
  unconfirmed: false,
  ...over,
});

// A stock run: nothing overridden, nothing advanced, nothing deferred.
const base = (over: Partial<RunSummaryInput> = {}): RunSummaryInput => ({
  inputStates: [],
  envRows: [],
  addedRefs: [],
  scope: "Prod",
  jobScope: "Prod",
  hostsPhrase: "all 3 hosts in Prod",
  targetsChanged: false,
  targetNames: [],
  executorWord: "a runner agent",
  executorChanged: false,
  sshUser: "",
  sshCredential: "",
  jobSshUser: "",
  jobSshCredential: "",
  identityCapable: true,
  effectivePin: "",
  jobPin: "",
  pinApplies: true,
  pinChanged: false,
  whenPhrase: "now",
  deferred: false,
  ansCheck: false,
  ansDiff: false,
  ansTagList: [],
  ansSkipTagList: [],
  ansVerbosity: 0,
  ansBecome: false,
  ansBecomeUser: "",
  ansExtraVarMap: {},
  ...over,
});

const group = (i: RunSummaryInput, key: string) => buildRunSummary(i).find((g) => g.key === key);
const row = (i: RunSummaryInput, gk: string, rk: string) => group(i, gk)?.rows.find((r) => r.key === rk);

describe("buildRunSummary — groups", () => {
  it("always returns Inputs, Targets, Method and Timing, in that order", () => {
    const keys = buildRunSummary(base()).map((g) => g.key);
    expect(keys).toEqual(["inputs", "targets", "method", "timing"]);
  });

  it("keeps the Answers header even when the job declares nothing", () => {
    // A group that silently vanishes reads as "didn't load", not "nothing to
    // say" — and "what will this run use" deserves an answer either way.
    const g = group(base(), "inputs")!;
    expect(g.rows).toHaveLength(1);
    expect(g.rows[0].value).toBe("this job declares none");
    expect(g.rows[0].accent).toBeUndefined();
  });

  it("omits Advanced entirely when every option is default, and shows only active rows", () => {
    expect(group(base(), "advanced")).toBeUndefined();
    const g = group(base({ ansDiff: true, ansVerbosity: 2 }), "advanced")!;
    expect(g.rows.map((r) => r.key)).toEqual(["diff", "verbosity"]);
    expect(g.rows[1].value).toBe("-vv");
  });
});

describe("buildRunSummary — answers", () => {
  it("accents an answer the operator typed, and notes it as theirs", () => {
    const r = row(base({ inputStates: [prompt()] }), "inputs", "prompt:TARGET_ENV")!;
    expect(r.value).toBe("staging");
    expect(r.accent).toBe("warning");
    expect(r.note).toBe("you");
  });

  it("leaves an untouched default quiet, and says it is a default", () => {
    const r = row(base({ inputStates: [prompt({ from: "default", value: "prod" })] }), "inputs", "prompt:TARGET_ENV")!;
    expect(r.accent).toBeUndefined();
    expect(r.note).toBe("default");
  });

  it("renders an unfilled required answer as an em dash, accented, and says why", () => {
    const r = row(
      base({ inputStates: [prompt({ value: "", from: null, unfilled: true })] }),
      "inputs",
      "prompt:TARGET_ENV",
    )!;
    expect(r.value).toBe("—");
    expect(r.accent).toBe("warning");
    expect(r.note).toBe("required — not filled");
  });

  it("shows an env override's VALUE, not just its name", () => {
    // The deviations list summarized overrides as a list of keys, which said
    // something had been overridden without saying to what. That gap is the
    // reason this band exists.
    const r = row(base({ envRows: [{ key: "API_URL", value: "https://staging.example" }] }), "inputs", "env:API_URL")!;
    expect(r.value).toBe("https://staging.example");
    expect(r.accent).toBe("warning");
    expect(r.note).toBe("override");
  });

  it("ignores a blank override row", () => {
    // The editor always carries one empty row for typing into; it is not an input.
    const g = group(base({ envRows: [{ key: "  ", value: "" }] }), "inputs")!;
    expect(g.rows.map((r) => r.key)).toEqual(["none"]);
  });

  it("lists added references by name", () => {
    const r = row(base({ addedRefs: [{ name: "DB_PASSWORD" }, { name: "API_KEY" }] }), "inputs", "refs")!;
    expect(r.value).toBe("DB_PASSWORD, API_KEY");
  });

  it("truncates a long value for display", () => {
    const long = "x".repeat(200);
    const r = row(base({ inputStates: [prompt({ value: long })] }), "inputs", "prompt:TARGET_ENV")!;
    expect(r.value.length).toBe(SUMMARY_VALUE_CAP);
    expect(r.value.endsWith("…")).toBe(true);
    // The caller carries the full string in a title, so nothing is lost — but a
    // 200-char answer must not blow out a 300px rail.
    expect(truncate("short")).toBe("short");
  });
});

describe("buildRunSummary — where it runs", () => {
  it("states scope, targets and executor quietly on a stock run", () => {
    const g = group(base(), "targets")!;
    expect(g.rows.map((r) => r.key)).toEqual(["scope", "targets"]);
    expect(g.rows.every((r) => r.accent === undefined)).toBe(true);
    const m = group(base(), "method")!;
    expect(m.rows.map((r) => r.key)).toEqual(["executor"]);
    expect(m.rows.every((r) => r.accent === undefined)).toBe(true);
  });

  it("accents an overridden scope and names the job's default", () => {
    const r = row(base({ scope: "Staging" }), "targets", "scope")!;
    expect(r.accent).toBe("warning");
    expect(r.note).toBe("job default: Prod");
  });

  it("says (none) when the job has no scope of its own", () => {
    const r = row(base({ scope: "Staging", jobScope: "" }), "targets", "scope")!;
    expect(r.note).toBe("job default: (none)");
  });

  it("accents targets and executor only when they changed", () => {
    expect(row(base({ targetsChanged: true }), "targets", "targets")!.accent).toBe("warning");
    expect(row(base({ executorChanged: true }), "method", "executor")!.accent).toBe("warning");
  });

  it("itemizes an explicit target subset — the names, not just the count", () => {
    const r = row(
      base({ hostsPhrase: "2 groups in Prod", targetsChanged: true, targetNames: ["dockerhost_carson", "dockerhost_vegas"] }),
      "targets",
      "targets",
    )!;
    expect(r.value).toBe("2 groups in Prod");
    expect(r.detail).toBe("dockerhost_carson, dockerhost_vegas");
  });

  it("carries no detail line when the run targets the whole scope", () => {
    expect(row(base(), "targets", "targets")!.detail).toBeUndefined();
  });

  it("caps a long target list at the detail cap, not the value cap", () => {
    const names = Array.from({ length: 40 }, (_, n) => `host-${n}`);
    const detail = row(base({ targetNames: names }), "targets", "targets")!.detail!;
    expect(detail.length).toBeLessThanOrEqual(140);
    expect(detail.endsWith("…")).toBe(true);
  });

  it("omits Connect as when neither the run nor the job declares one", () => {
    expect(row(base(), "method", "connectAs")).toBeUndefined();
  });

  it("states the JOB's identity, unaccented, when this run overrode nothing", () => {
    // The gap this replaced: these fields were read from the per-run state
    // alone, which the dialog seeds to "", so a job that declares its own
    // identity ran under it while the rail said nothing at all.
    const r = row(base({ jobSshUser: "deploy", jobSshCredential: "prod_key" }), "method", "connectAs")!;
    expect(r.value).toBe("user deploy · key prod_key");
    expect(r.accent).toBeUndefined();
    expect(r.note).toBe("job default");
  });

  it("accents the row and names the job's default when this run overrode it", () => {
    const r = row(base({ sshUser: "root", jobSshUser: "deploy" }), "method", "connectAs")!;
    expect(r.value).toBe("user root");
    expect(r.accent).toBe("warning");
    expect(r.note).toBe("job default: user deploy");
  });

  it("folds the job's identity beneath the run's PER FIELD (CA-10)", () => {
    // The server's fold is field-wise: a per-run user with no per-run key still
    // connects with the job's key. A row that dropped the inherited half would
    // promise a login the run does not use.
    const r = row(base({ sshUser: "root", jobSshUser: "deploy", jobSshCredential: "prod_key" }), "method", "connectAs")!;
    expect(r.value).toBe("user root · key prod_key");
    expect(r.accent).toBe("warning");
  });

  it("says (none) when this run sets an identity the job never had", () => {
    const r = row(base({ sshCredential: "prod_key" }), "method", "connectAs")!;
    expect(r.value).toBe("key prod_key");
    expect(r.note).toBe("job default: (none)");
  });

  it("omits Connect as entirely on a run type that cannot carry one", () => {
    // terraform: the server drops a stored identity outright, so stating it
    // would describe a field with no effect on the run.
    expect(row(base({ identityCapable: false, jobSshUser: "deploy", sshUser: "root" }), "method", "connectAs")).toBeUndefined();
  });
});

describe("buildRunSummary — the runner pin (RS-1's row, promoted)", () => {
  it("states an inherited pin without accenting it", () => {
    // Stating where a run goes is not the same as flagging that somebody moved
    // it. The row is the fact; the accent is the change.
    const r = row(base({ effectivePin: "vlan-dmz", jobPin: "vlan-dmz" }), "method", "pin")!;
    expect(r.value).toBe("vlan-dmz");
    expect(r.accent).toBeUndefined();
    expect(r.note).toBeUndefined();
  });

  it("accents a re-pinned run and names the job's default", () => {
    const r = row(base({ effectivePin: "vlan-lab", jobPin: "vlan-dmz", pinChanged: true }), "method", "pin")!;
    expect(r.value).toBe("vlan-lab");
    expect(r.accent).toBe("warning");
    expect(r.note).toBe("job default: vlan-dmz");
  });

  it("reads 'unpinned' when a run is deliberately sent to the general pool", () => {
    const r = row(base({ effectivePin: "", jobPin: "vlan-dmz", pinChanged: true }), "method", "pin")!;
    expect(r.value).toBe("unpinned");
    expect(r.accent).toBe("warning");
  });

  it("is absent on an SSH run, where a pin means nothing", () => {
    expect(row(base({ effectivePin: "vlan-dmz", pinApplies: false }), "method", "pin")).toBeUndefined();
  });

  it("is absent when there is no pin anywhere", () => {
    expect(row(base(), "method", "pin")).toBeUndefined();
  });
});

describe("buildRunSummary — when & advanced", () => {
  it("reads 'now' quietly, and accents a deferred run as info", () => {
    expect(row(base(), "timing", "when")!.accent).toBeUndefined();
    const later = row(base({ whenPhrase: "at Aug 15, 02:00", deferred: true }), "timing", "when")!;
    expect(later.value).toBe("at Aug 15, 02:00");
    // info, not warning: a deferred run is not a deviation from how the job is
    // configured, it is a fact that reinterprets everything above it.
    expect(later.accent).toBe("info");
  });

  it("marks check mode as info, matching the recap's treatment of a rehearsal", () => {
    expect(row(base({ ansCheck: true }), "advanced", "check")!.accent).toBe("info");
  });

  it("renders extra variables as k=v rows rather than a count", () => {
    const g = group(base({ ansExtraVarMap: { region: "eu-west-1", tier: "blue" } }), "advanced")!;
    expect(g.rows.map((r) => [r.label, r.value])).toEqual([
      ["region", "eu-west-1"],
      ["tier", "blue"],
    ]);
  });

  it("carries become, tags and skip tags in the shape the deviations list used", () => {
    const g = group(
      base({ ansBecome: true, ansBecomeUser: "root", ansTagList: ["deploy"], ansSkipTagList: ["slow"] }),
      "advanced",
    )!;
    const byKey = Object.fromEntries(g.rows.map((r) => [r.key, r.value]));
    expect(byKey.tags).toBe("only: deploy");
    expect(byKey.skipTags).toBe("slow");
    expect(byKey.become).toBe("as root");
    expect(row(base({ ansBecome: true }), "advanced", "become")!.value).toBe("root");
  });
});

describe("buildRunSummary — label omission", () => {
  it("leaves the When row unlabelled, because its group heading already says it", () => {
    // Found by the rail integration test, which could not disambiguate the group
    // title from the row label — and neither could a reader: "WHEN / WHEN / now".
    expect(row(base(), "timing", "when")!.label).toBe("");
    // Every other row names itself, since its group holds more than one.
    for (const k of ["targets", "method"]) {
      expect(group(base(), k)!.rows.every((r) => r.label !== "")).toBe(true);
    }
  });
});

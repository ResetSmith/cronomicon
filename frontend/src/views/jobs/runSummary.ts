import type { InputState } from "./RunInputs";

// ── buildRunSummary (RS-2, the run-summary plan) ───────────────────
//
// The Run dialog's "This run" rail (RU-14) listed only DEVIATIONS from the job's
// defaults. That made it empty for the most ordinary run there is — a stock job
// with a prompt filled in — so the one surface whose job is to say what is about
// to happen said nothing precisely when nothing unusual was happening. A
// deviations list answers "what did I change"; the rail's question is "what will
// this run use", and those are different questions.
//
// This builds the second answer. It is a PURE FUNCTION over the state `submit()`
// already reads, which is what preserves the RU-1 invariant — the rail cannot
// disagree with what the run will do — by construction rather than by anyone
// remembering to keep two lists in step. It is also why this is testable without
// mounting a 3000-line dialog.
//
// It returns DATA. No theme tokens, no JSX: the caller owns styling, and hoisting
// `c.*` into a module-scope structure is the trap in
// [[theme-token-module-const-staleness]]. `accent` names the MEANING ("the
// operator changed this" / "this is deferred or a rehearsal"); the rail decides
// what those look like.

export type SummaryAccent = "warning" | "info";

export type SummaryRow = {
  /** Stable within its group — the React key. */
  key: string;
  /**
   * "" when the group's title already names the row. A single-row group whose
   * row repeats its own heading ("WHEN / WHEN / now") is pure noise; the
   * renderer omits the label line entirely for these.
   */
  label: string;
  /** The resolved EFFECTIVE value, never the input's raw state. "—" when empty. */
  value: string;
  /** warning = the operator changed it; info = deferred or a rehearsal. */
  accent?: SummaryAccent;
  /** Provenance or the default this replaced — "you", "default", "job default: X". */
  note?: string;
  /**
   * A supplementary second line under the value — the itemization of a value
   * that is itself a summary (the Targets phrase says "2 groups", this names
   * them). Quieter than the value in the renderer; capped like it.
   */
  detail?: string;
};

export type SummaryGroupKey = "inputs" | "targets" | "method" | "timing" | "advanced";
export type SummaryGroup = { key: SummaryGroupKey; title: string; rows: SummaryRow[] };

/**
 * Long values are truncated for display; the caller puts the full string in a
 * `title`. These are plaintext per-run values by design (v0.55.12: run inputs and
 * variables are log-visible, and secrets never travel this path — the dialog
 * itself tells operators not to paste a password here), so there is nothing to
 * redact, only something too long to fit a 300px rail.
 */
export const SUMMARY_VALUE_CAP = 64;
export const truncate = (v: string, cap = SUMMARY_VALUE_CAP) => (v.length > cap ? `${v.slice(0, cap - 1)}…` : v);

/**
 * A detail line is a LIST (target names), so it earns more room than a single
 * value before the ellipsis — but still a cap: an operator who picks 50 hosts
 * gets the first few plus the row's title tooltip, not a rail-length column.
 */
const SUMMARY_DETAIL_CAP = 140;

/** Provenance → the short note the rail appends to a label. */
const provenanceNote = (from: InputState["from"]): string | undefined => {
  switch (from) {
    case "you":
      return "you";
    case "override":
      return "override";
    case "default":
      return "default";
    case "job env":
      return "job";
    default:
      return undefined;
  }
};

export type RunSummaryInput = {
  // Answers
  inputStates: InputState[];
  envRows: { key: string; value: string }[];
  addedRefs: { name: string }[];
  // Where it runs
  scope: string;
  jobScope: string;
  hostsPhrase: string;
  targetsChanged: boolean;
  /**
   * The NAMES behind the Targets phrase when the operator narrowed the run to an
   * explicit subset — the picked hosts, or the picked inventory groups. Empty
   * when the run targets the whole scope or a raw --limit (the phrase already
   * carries the literal there). The phrase states the count; this states which.
   */
  targetNames: string[];
  executorWord: string;
  executorChanged: boolean;
  /** The operator's per-run identity. "" ⇒ the job's own value stands. */
  sshUser: string;
  sshCredential: string;
  /** The job spec's declared identity — the value a per-run field falls back to. */
  jobSshUser: string;
  jobSshCredential: string;
  /**
   * False on a run type that cannot carry an identity (terraform), where the
   * server ignores a stored one outright — see the fold in `where` below.
   */
  identityCapable: boolean;
  /** "" when unpinned. Omitted entirely on an SSH run — see `pinApplies`. */
  effectivePin: string;
  jobPin: string;
  pinApplies: boolean;
  pinChanged: boolean;
  // When
  whenPhrase: string;
  deferred: boolean;
  // Advanced
  ansCheck: boolean;
  ansDiff: boolean;
  ansTagList: string[];
  ansSkipTagList: string[];
  ansVerbosity: number;
  ansBecome: boolean;
  ansBecomeUser: string;
  ansExtraVarMap: Record<string, string>;
};

export function buildRunSummary(i: RunSummaryInput): SummaryGroup[] {
  // ── Answers ────────────────────────────────────────────────────────────────
  // ALWAYS rendered, header included, even with nothing to say. The rail exists
  // to answer "what will this run use"; a group that silently disappears reads
  // as "didn't load", not as "nothing to report".
  const answers: SummaryRow[] = [];
  for (const s of i.inputStates) {
    const name = s.p.name ?? "";
    answers.push({
      key: `prompt:${name}`,
      label: name,
      // An unfilled REQUIRED answer is the one row here that is a problem rather
      // than a fact, so it gets the accent and an explicit em dash.
      value: s.value === "" ? "—" : truncate(s.value),
      accent: s.from === "you" || s.from === "override" || s.unfilled ? "warning" : undefined,
      note: s.unfilled ? "required — not filled" : provenanceNote(s.from),
    });
  }
  // VALUES, not just keys. The deviations list summarized overrides as a list of
  // names, which told an operator that something was overridden but not to what —
  // the gap that started this band.
  for (const r of i.envRows) {
    const key = r.key.trim();
    if (key === "") continue;
    answers.push({
      key: `env:${key}`,
      label: key,
      value: r.value.trim() === "" ? "—" : truncate(r.value.trim()),
      // An override is operator-supplied by definition; there is no quiet case.
      accent: "warning",
      note: "override",
    });
  }
  if (i.addedRefs.length > 0) {
    answers.push({
      key: "refs",
      label: "References added",
      value: truncate(i.addedRefs.map((b) => b.name).join(", ")),
      accent: "warning",
    });
  }
  if (answers.length === 0) {
    answers.push({ key: "none", label: "No inputs", value: "this job declares none" });
  }

  // ── Where it runs ──────────────────────────────────────────────────────────
  // ALWAYS, effective values, quiet unless the operator moved something.
  const where: SummaryRow[] = [
    {
      key: "scope",
      label: "Scope",
      value: i.scope || "—",
      accent: i.scope !== i.jobScope ? "warning" : undefined,
      note: i.scope !== i.jobScope ? `job default: ${i.jobScope || "(none)"}` : undefined,
    },
    {
      key: "targets",
      label: "Targets",
      value: i.hostsPhrase || "—",
      accent: i.targetsChanged ? "warning" : undefined,
      // The itemization of the phrase above — "2 groups" is a count, and the
      // rail's question is "what will this run use", so the names are the
      // answer. Same reason env overrides carry values, one row up in spirit.
      detail: i.targetNames.length > 0 ? truncate(i.targetNames.join(", "), SUMMARY_DETAIL_CAP) : undefined,
    },
    {
      key: "executor",
      label: "Executor",
      value: i.executorWord,
      accent: i.executorChanged ? "warning" : undefined,
    },
  ];
  // The identity the run will ACTUALLY connect with, which is not the same as
  // the identity this dialog set. The server folds the job spec's identity
  // beneath the operator's PER FIELD (CA-10, execution_mount.go) — a per-run
  // user with no per-run key still uses the job's key — so the effective value
  // is a field-wise `perRun || job`, and modelling it any other way would have
  // the rail promise a login the run does not use.
  //
  // This row previously read the per-run fields ALONE, so a job that declares
  // its own "connect as" ran under that identity while the rail said nothing:
  // the RS-2 contract is every setting the run will use, not just the ones
  // changed here. That is the RT-3 pin bug one row over.
  //
  // Gated on identityCapable like the dialog's own section is: on a run type
  // that cannot carry an identity the server drops a stored one, and stating a
  // field with no effect is worse than omitting it.
  if (i.identityCapable) {
    const effUser = i.sshUser.trim() || i.jobSshUser.trim();
    const effCred = i.sshCredential || i.jobSshCredential;
    if (effUser || effCred) {
      // Per field, again: setting only the user IS an override, even though the
      // key below it is still the job's.
      const changed = !!(i.sshUser.trim() || i.sshCredential);
      const jobPhrase =
        [i.jobSshUser.trim() && `user ${i.jobSshUser.trim()}`, i.jobSshCredential && `key ${i.jobSshCredential}`]
          .filter(Boolean)
          .join(" · ") || "(none)";
      where.push({
        key: "connectAs",
        label: "Connect as",
        value: [effUser && `user ${effUser}`, effCred && `key ${effCred}`].filter(Boolean).join(" · "),
        accent: changed ? "warning" : undefined,
        // Unchanged, the note carries PROVENANCE — it is what distinguishes
        // "the job connects as this" from "you typed this", which is the whole
        // reason the row is worth showing when nobody touched it.
        note: changed ? `job default: ${jobPhrase}` : "job default",
      });
    }
  }
  // RS-1's row, promoted out of the deviations list. Shown whenever a pin is in
  // play at all — stating where a run goes is not the same as flagging that
  // somebody moved it, and the accent carries the second meaning.
  if (i.pinApplies && (i.effectivePin || i.pinChanged)) {
    where.push({
      key: "pin",
      label: "Runner pin",
      value: i.effectivePin || "unpinned",
      accent: i.pinChanged ? "warning" : undefined,
      note: i.pinChanged ? `job default: ${i.jobPin || "(none)"}` : undefined,
    });
  }

  // ── When ───────────────────────────────────────────────────────────────────
  // One row, always. `info` rather than `warning` for a deferred run: it is not a
  // deviation from how the job is configured, it is a fact about this run that
  // reinterprets everything above it.
  const when: SummaryRow[] = [
    // Empty label: the group heading already says "When", and repeating it on
    // the only row inside would read "WHEN / WHEN / now".
    { key: "when", label: "", value: i.whenPhrase, accent: i.deferred ? "info" : undefined },
  ];

  // ── Advanced ───────────────────────────────────────────────────────────────
  // Non-default rows ONLY; the whole group vanishes when nothing is set. This is
  // the one group that may disappear, because "no advanced options" is the
  // overwhelmingly common case and a permanent empty section is noise.
  const advanced: SummaryRow[] = [];
  if (i.ansCheck) {
    advanced.push({ key: "check", label: "Check mode", value: "dry run — applies nothing", accent: "info" });
  }
  if (i.ansDiff) advanced.push({ key: "diff", label: "Diff", value: "--diff", accent: "warning" });
  if (i.ansTagList.length > 0) {
    advanced.push({ key: "tags", label: "Tags", value: truncate(`only: ${i.ansTagList.join(", ")}`), accent: "warning" });
  }
  if (i.ansSkipTagList.length > 0) {
    advanced.push({ key: "skipTags", label: "Skip tags", value: truncate(i.ansSkipTagList.join(", ")), accent: "warning" });
  }
  if (i.ansVerbosity > 0) {
    advanced.push({ key: "verbosity", label: "Verbosity", value: `-${"v".repeat(i.ansVerbosity)}`, accent: "warning" });
  }
  if (i.ansBecome || i.ansBecomeUser.trim()) {
    advanced.push({
      key: "become",
      label: "Become",
      value: i.ansBecomeUser.trim() ? `as ${i.ansBecomeUser.trim()}` : "root",
      accent: "warning",
    });
  }
  // k=v rows, not a count. Same reason the env overrides carry values: a count
  // says something was set without saying what it was set to.
  for (const [k, v] of Object.entries(i.ansExtraVarMap)) {
    advanced.push({ key: `extra:${k}`, label: k, value: truncate(v === "" ? "—" : v), accent: "warning" });
  }

  // RD5 — the rail's groups mirror the dialog's five sections by name, so the
  // section an operator opens and the group that restates it share one word.
  // "Where it runs" is split at the same seam as the dialog: Targets carries
  // the scope + targets rows, Method the executor, identity and pin rows.
  const targets = where.filter((r) => r.key === "scope" || r.key === "targets");
  const method = where.filter((r) => r.key !== "scope" && r.key !== "targets");
  const groups: SummaryGroup[] = [
    { key: "inputs", title: "Inputs", rows: answers },
    { key: "targets", title: "Targets", rows: targets },
    { key: "method", title: "Method", rows: method },
    { key: "timing", title: "Timing", rows: when },
  ];
  if (advanced.length > 0) groups.push({ key: "advanced", title: "Advanced", rows: advanced });
  return groups;
}

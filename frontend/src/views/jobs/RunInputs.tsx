// The "Answers this run needs" panel of the Run dialog (Jobs.tsx → RunDialog).
//
// Why this lives in its own file: the run-input surface grew to be the ONLY part
// of the Run dialog that is job-specific and genuinely needs the operator — every
// other control (scope, executor, host/group subset, env overrides) has a working
// default and now sits behind the "Where it runs" disclosure. Giving the answers
// their own module keeps that asymmetry visible in the code, and keeps Jobs.tsx
// from absorbing another 250 lines.
//
// The design brief (RD1): the dialog had become seven equally-weighted sections
// stacked flat, each input carrying two redundant warning lines, with the primary
// action below the fold. An operator who is NOT the job's author could not tell
// where to start. The rewrite makes one thing loud — what this run still needs —
// and quiets everything else.
//
// The device that carries it (RD2) is a two-part status reading:
//   · a segmented meter, one segment per input ACROSS ALL PAGES, so pagination can
//     never hide the fact that something is outstanding; and
//   · a colored left edge on each row, which stacks into a continuous rail down
//     the list that turns from amber to green as the operator works.
// Both are backed by a plain-language state word on every row, so the state is
// never carried by color alone (RD3).
import { useEffect, useMemo, useState } from "react";
import { c } from "../../theme";
import { Btn, Input, Select } from "../../components/ui";
import type { components } from "../../api/schema";

type JobPrompt = components["schemas"]["JobPrompt"];

// Where the value this run will actually receive came from. Mirrors the server's
// precedence exactly (see resolveInput in Jobs.tsx) so the dialog can never
// disagree with the promptWarnings the run will record.
export type Provenance = "override" | "you" | "default" | "job env";

export interface InputState {
  p: JobPrompt;
  value: string;
  from: Provenance | null;
  /** Required, and nothing at all will be sent. Gates the Run button. */
  unfilled: boolean;
  /** Required, a default WILL be sent, but the operator hasn't affirmed it. Never gates. */
  unconfirmed: boolean;
}

// Four states, in the order an operator resolves them. `outstanding` is the union
// that the meter, the count and the filter all key off, so they cannot drift.
type Kind = "unfilled" | "unconfirmed" | "ready" | "optional";

function kindOf(s: InputState): Kind {
  if (s.unfilled) return "unfilled";
  if (s.unconfirmed) return "unconfirmed";
  if (s.from) return "ready";
  return "optional";
}

const isOutstanding = (s: InputState) => s.unfilled || s.unconfirmed;

// Tint for the provenance word on the audit line. Deliberately low-contrast: this
// is the run's paper trail, not a call to action.
function provenanceTone(from: Provenance): string {
  switch (from) {
    case "you":
      return c.success;
    case "override":
      return c.primary;
    case "default":
      return c.info;
    case "job env":
      return c.textSec;
  }
}

// One tone table for the meter, the row edge and the state word — so a row and its
// segment can never disagree. `warning` and `accent` are the same hex in this
// palette, so unconfirmed deliberately takes `info` (blue) rather than a second
// amber: "look at this" must read differently from "this is missing".
function tone(k: Kind): { color: string; word: string } {
  switch (k) {
    case "unfilled":
      return { color: c.warning, word: "Needs a value" };
    case "unconfirmed":
      return { color: c.info, word: "Using a default" };
    case "ready":
      return { color: c.success, word: "Ready" };
    case "optional":
      return { color: c.textMuted, word: "Optional" };
  }
}

// ── ReadinessMeter ───────────────────────────────────────────────────────────
// One segment per declared input, always ALL of them regardless of the current
// page. This is the whole reason pagination is safe here: an unanswered input on
// page 2 is still visibly amber on page 1. Segments are buttons so the operator
// can jump straight to the one that needs them.
function ReadinessMeter({ states, onJump }: { states: InputState[]; onJump: (index: number) => void }) {
  const ready = states.filter((s) => !isOutstanding(s)).length;
  return (
    <div
      role="img"
      aria-label={`${ready} of ${states.length} inputs ready`}
      style={{ display: "flex", gap: 2, marginTop: 8 }}
    >
      {states.map((s, i) => {
        const k = kindOf(s);
        const { color, word } = tone(k);
        return (
          <button
            key={s.p.name}
            type="button"
            aria-hidden
            tabIndex={-1}
            onClick={() => onJump(i)}
            title={`${s.p.label || s.p.name} — ${word}`}
            style={{
              flex: 1,
              height: 6,
              minWidth: 8,
              padding: 0,
              border: "none",
              borderRadius: c.radiusChip,
              // An optional-empty input is neutral, not a gap: nothing is wanted
              // from it, so it reads as quiet rather than unfinished.
              background: k === "optional" ? c.borderLight : color,
              opacity: k === "ready" ? 0.85 : 1,
              cursor: "pointer",
            }}
          />
        );
      })}
    </div>
  );
}

// ── RunInputRow ──────────────────────────────────────────────────────────────
// One declared input. Three lines, no more: label + state, the control, and the
// quiet audit line. The two per-row warning paragraphs the old dialog repeated
// for every unfilled input ("Nothing will be sent for this input." / "Required —
// the run will still proceed…") are gone: the first is now the state word, and
// the second is stated ONCE in the gate block below the list.
function RunInputRow({
  st,
  val,
  onChange,
  onConfirm,
}: {
  st: InputState;
  val: string;
  onChange: (v: string) => void;
  onConfirm: () => void;
}) {
  const { p, value, from, unconfirmed } = st;
  const k = kindOf(st);
  const { color, word } = tone(k);
  // A value coming from an override row or job-level env is not editable here —
  // this control isn't the source of truth for it, so say so rather than showing
  // a misleadingly empty box.
  const external = from === "override" || from === "job env";
  const opts = p.options ?? [];
  // A declared default must be selectable even if the author left it out of the
  // options list, so the control can actually show what the audit line claims.
  const optList = p.default && !opts.includes(p.default) ? [p.default, ...opts] : opts;
  // RD4 — the author's label leads and the raw env var name moves to the tooltip
  // and the audit line, so a non-technical operator reads "Target environment"
  // rather than TARGET_ENV. With no label the name is all we have, so it stays.
  const heading = p.label || p.name;

  return (
    <div
      style={{
        display: "flex",
        gap: 10,
        padding: "10px 12px 11px",
        borderRadius: c.radiusSurface,
        background: k === "unfilled" ? c.warningBg : k === "unconfirmed" ? c.infoBg : "transparent",
        border: `1px solid ${k === "ready" || k === "optional" ? c.borderLight : `${color}33`}`,
      }}
    >
      {/* The rail: stacked across rows these edges read as one thread down the list. */}
      <div aria-hidden style={{ flex: "0 0 3px", borderRadius: c.radiusChip, background: k === "optional" ? c.borderLight : color }} />
      <div style={{ flex: 1, minWidth: 0 }}>
        <div style={{ display: "flex", alignItems: "baseline", gap: 10, marginBottom: 6 }}>
          <label
            htmlFor={`run-input-${p.name}`}
            title={p.label ? p.name : undefined}
            style={{ flex: 1, minWidth: 0, fontSize: c.fontSm, fontWeight: 600, color: c.text }}
          >
            {heading}
          </label>
          <span style={{ fontSize: c.fontXs, fontWeight: 600, color, whiteSpace: "nowrap" }}>{word}</span>
        </div>

        {opts.length > 0 ? (
          <Select id={`run-input-${p.name}`} value={val} onChange={(e) => onChange(e.target.value)}>
            <option value="">— choose one —</option>
            {optList.map((o) => (
              <option key={o} value={o}>
                {o}
              </option>
            ))}
          </Select>
        ) : (
          <Input
            id={`run-input-${p.name}`}
            value={val}
            onChange={(e) => onChange(e.target.value)}
            placeholder={p.default ?? "Type a value"}
          />
        )}

        {/* The audit line. Kept on every row that will actually send something (it is
            the run's paper trail) but demoted to a footnote: 11px, muted, mono only
            for the literal. A row with NO value doesn't get one — "Needs a value"
            above already says it, and repeating it per row is the noise this
            redesign set out to remove. */}
        {from && (
          <div style={{ display: "flex", alignItems: "center", flexWrap: "wrap", gap: 6, marginTop: 6, fontSize: c.fontXs, color: c.textMuted }}>
            <span>
              will send{" "}
              <code style={{ fontFamily: c.mono, color: c.textSec }}>
                {p.name}={value}
              </code>{" "}
              ·{" "}
              {/* The provenance word keeps a quiet tint rather than the uppercase
                  chip it used to wear: still scannable down the list, no longer
                  competing with the label for the row's attention. */}
              <span style={{ fontWeight: 600, color: provenanceTone(from) }}>{from}</span>
            </span>
            {external && (
              <span>
                {from === "override"
                  ? "— an override under “Inputs” wins over this field."
                  : "— fixed on the job; you don't need to fill this in."}
              </span>
            )}
            {unconfirmed && (
              <Btn small onClick={onConfirm}>
                Use this
              </Btn>
            )}
          </div>
        )}
      </div>
    </div>
  );
}

// How many inputs render before the list paginates. Chosen so the primary action
// stays reachable without scrolling on a laptop viewport; below it, pagination is
// pure overhead and doesn't appear at all.
const INPUTS_PER_PAGE = 5;

// ── RunInputsPanel ───────────────────────────────────────────────────────────
// The whole answers section: headline count, meter, optional filter, the paged
// list, and the pager.
export function RunInputsPanel({
  states,
  promptVals,
  setPrompt,
  confirmDefault,
  embedded,
  blocking,
}: {
  states: InputState[];
  promptVals: Record<string, string>;
  setPrompt: (name: string, v: string) => void;
  confirmDefault: (name: string) => void;
  /** RV — rendered inside the "Required variables" section, whose header and
      collapsed summary already carry the title and the ready-count. Suppresses
      the panel's own headline row so the count isn't stated twice; the meter,
      filter and pager stay — they are controls, not headings. */
  embedded?: boolean;
  /** RU-2 — the job's promptEnforcement, needed only for the consequence prose
      below the list ("recorded as unanswered" vs "there is no override"). The
      escape checkbox itself stays in the dialog footer, where it is reachable
      without scrolling. */
  blocking?: boolean;
}) {
  const [page, setPage] = useState(0);
  // RD5 — pagination alone would let a required input hide on page 2, so it ships
  // with its counterpart: a filter that collapses the list to just what is still
  // outstanding. Together they answer "this job declares fifteen variables"
  // without ever burying one.
  const [onlyOutstanding, setOnlyOutstanding] = useState(false);

  const outstanding = states.filter(isOutstanding).length;
  const ready = states.length - outstanding;
  // RU-2 — "unfilled" is narrower than "outstanding" (which also counts inputs
  // whose default has not been confirmed). Only unfilled ones carry a
  // consequence, so only they get the prose below the list.
  const unfilled = states.filter((s) => s.unfilled);
  const visible = useMemo(
    () => (onlyOutstanding ? states.filter(isOutstanding) : states),
    [states, onlyOutstanding],
  );
  const paginated = visible.length > INPUTS_PER_PAGE;
  const pageCount = Math.max(1, Math.ceil(visible.length / INPUTS_PER_PAGE));
  // Answering the last outstanding input while filtered shrinks the list under
  // the current page; clamp rather than showing an empty page.
  const safePage = Math.min(page, pageCount - 1);
  useEffect(() => {
    if (page !== safePage) setPage(safePage);
  }, [page, safePage]);
  const shown = paginated ? visible.slice(safePage * INPUTS_PER_PAGE, (safePage + 1) * INPUTS_PER_PAGE) : visible;

  // Jumping from a meter segment: turn the filter off so the index is meaningful
  // against the full list, then page to it.
  const jumpTo = (index: number) => {
    setOnlyOutstanding(false);
    setPage(Math.floor(index / INPUTS_PER_PAGE));
  };

  const headline =
    outstanding === 0
      ? states.length === 1
        ? "Ready to run"
        : `All ${states.length} ready`
      : `${ready} of ${states.length} ready`;

  return (
    <div>
      {!embedded && (
        <div style={{ display: "flex", alignItems: "baseline", gap: 10 }}>
          <div style={{ flex: 1, fontSize: c.fontBody, fontWeight: 600, color: c.text }}>Answers this run needs</div>
          <div style={{ fontSize: c.fontSm, fontWeight: 600, color: outstanding === 0 ? c.success : c.textSec }}>{headline}</div>
        </div>
      )}
      <ReadinessMeter states={states} onJump={jumpTo} />

      {/* Filter and pager share one row ABOVE the list. The pager used to sit under
          it, where the pinned action bar covered it at rest — hiding the only cue
          that more inputs exist. Navigation belongs with the count it navigates. */}
      {(paginated || (outstanding > 0 && states.length > INPUTS_PER_PAGE)) && (
        <div style={{ display: "flex", alignItems: "center", gap: 10, marginTop: 10, minHeight: 26 }}>
          {outstanding > 0 && states.length > INPUTS_PER_PAGE ? (
            <label style={{ display: "inline-flex", alignItems: "center", gap: 7, fontSize: c.fontSm, color: c.textSec, cursor: "pointer" }}>
              <input
                type="checkbox"
                checked={onlyOutstanding}
                onChange={(e) => {
                  setOnlyOutstanding(e.target.checked);
                  setPage(0);
                }}
              />
              Show only what needs an answer ({outstanding})
            </label>
          ) : (
            <span />
          )}
          <span style={{ flex: 1 }} />
          {paginated && (
            <>
              <span style={{ fontSize: c.fontXs, color: c.textMuted, whiteSpace: "nowrap" }}>
                {safePage * INPUTS_PER_PAGE + 1}–{Math.min(visible.length, (safePage + 1) * INPUTS_PER_PAGE)} of{" "}
                {visible.length}
                {onlyOutstanding ? " outstanding" : ""}
              </span>
              <Btn small disabled={safePage === 0} onClick={() => setPage(safePage - 1)}>
                ← Back
              </Btn>
              <Btn small disabled={safePage >= pageCount - 1} onClick={() => setPage(safePage + 1)}>
                Next →
              </Btn>
            </>
          )}
        </div>
      )}

      <div style={{ display: "flex", flexDirection: "column", gap: 8, marginTop: 10 }}>
        {shown.map((st) => (
          <RunInputRow
            key={st.p.name}
            st={st}
            val={promptVals[st.p.name] ?? ""}
            onChange={(v) => setPrompt(st.p.name, v)}
            onConfirm={() => confirmDefault(st.p.name)}
          />
        ))}
      </div>

      {/* RU-2 — the consequence prose. It used to live in the dialog footer, where
          it permanently occupied ~120px above the buttons and described rows the
          operator could not see. Stated once, here, next to the rows it is about;
          the footer keeps only the escape checkbox (it ungates the button, so it
          has to stay reachable without scrolling — the RD1 lesson).
          T1.7/JR-Q4b — UDV4's "a required prompt never blocks the run" still
          holds for the non-blocking case; the server accepts it either way. */}
      {unfilled.length > 0 && (
        <div
          style={{
            fontSize: c.fontSm,
            color: c.textSec,
            background: c.warningBg,
            border: `1px solid ${c.warning}40`,
            borderRadius: c.radiusSurface,
            padding: "10px 12px",
            marginTop: 12,
          }}
        >
          <div style={{ fontWeight: 600, color: c.warning, marginBottom: 4 }}>
            {unfilled.length === 1 ? "1 answer is still missing" : `${unfilled.length} answers are still missing`}
          </div>
          <div>
            {unfilled.map((s) => s.p.label || s.p.name).join(", ")}
            {blocking ? (
              <>
                {" "}— this job can't run without {unfilled.length === 1 ? "it" : "them"}, here or through the API. There
                is no override.
              </>
            ) : (
              <> — the run will still go ahead, and each one is recorded as unanswered against it.</>
            )}
          </div>
        </div>
      )}
    </div>
  );
}

// ── Disclosure ───────────────────────────────────────────────────────────────
// RP-4 — promoted to components/ui.tsx so the Composer shares it; re-exported
// here so the fold's birthplace keeps working as an import site.
export { Disclosure } from "../../components/ui";

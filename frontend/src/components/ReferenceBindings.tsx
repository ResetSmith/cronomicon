import { useEffect, useId, useMemo, useState, type CSSProperties } from "react";
import { api, csrfHeader, errMsg, fetchCapabilities } from "../api/client";
import { useGet, rows } from "../hooks";
import { c } from "../theme";
import { Btn, InlineLoading, Section } from "./ui";
import type { components } from "../api/schema";

// Reference-binding UI (vault-integration.md P1.8). A job or script declares which
// Env Vars references (Secrets / Variables / SSH Keys) it consumes; the dispatch
// resolver injects ONLY the declared set (D2). This file exposes an editor (Jobs /
// Scripts) and a read-only display (run detail). NEVER shows values — names only.

type ReferenceBinding = components["schemas"]["ReferenceBinding"];
type ReferenceValidation = components["schemas"]["ReferenceValidation"];
type EnvSecret = components["schemas"]["EnvSecret"];
type EnvVar = components["schemas"]["EnvVar"];
type SshCredential = components["schemas"]["SshCredential"];
type Kind = "secret" | "var" | "key";

const KINDS: Kind[] = ["secret", "var", "key"];
const KIND_LABEL: Record<Kind, string> = { secret: "Secret", var: "Variable", key: "SSH Key" };
// Secret = danger (values), key = warning (material), var = primary (log-safe).
function kindColor(k: string): string {
  return k === "secret" ? c.danger : k === "key" ? c.warning : c.primary;
}

// GLOBAL_SCOPE_LABEL — the "" scope convention rendered as a word. A blank cell
// used to be indistinguishable from "unset" (T1.8); every reference surface says
// the same thing now.
export const GLOBAL_SCOPE_LABEL = "global";

// ── Authoring-time validation (agencies plan T1.4-T1.7) ─────────────────────
// Until now the ONLY feedback on a bad binding was a 409 at dispatch, and that
// message deliberately withholds the distinguishing fact (out-of-scope vs
// missing) so a run cannot be used as a cross-scope existence oracle. The
// authoring surface has no such constraint: POST /references/validate answers
// precisely, bounded by the caller's own Env Vars visibility (see the backend's
// runref/validate.go). These verdicts are that answer, rendered where the
// operator declares the reference rather than where the run fails.

// SR_ONLY holds no theme tokens on purpose, so it is safe as a module constant —
// `c` is mutated in place by applyTheme, and a frozen c.* value would survive a
// Light/Dark toggle.
const SR_ONLY: CSSProperties = {
  position: "absolute",
  width: 1,
  height: 1,
  overflow: "hidden",
  clip: "rect(0 0 0 0)",
  whiteSpace: "nowrap",
};

// useReferenceValidation resolves a binding set against `scope` ("" = global).
// Pass scope=null to skip validation entirely (nothing to resolve against).
export function useReferenceValidation(refs: { kind: string; name: string; as?: string }[], scope: string | null) {
  // The refs array is a fresh object every render; its sorted key list is the
  // stable identity, so the POST re-fires only when the SET or the scope changes
  // — which is exactly what the Run dialog needs when its override moves.
  const sig = refs.map(bkey).sort().join("|");
  const enabled = scope !== null && refs.length > 0;
  const q = useGet<{ results?: ReferenceValidation[] }>(
    () =>
      enabled
        ? api.POST("/references/validate", {
            params: { header: csrfHeader },
            body: { scope: scope ?? "", references: refs.map((r) => ({ kind: r.kind as Kind, name: r.name, as: r.as || undefined })) },
          })
        : Promise.resolve({ data: { results: [] } }),
    [sig, enabled ? `s:${scope}` : "off"],
  );
  const byKey = useMemo(() => {
    const m: Record<string, ReferenceValidation> = {};
    for (const v of q.data?.results ?? []) m[bkey(v)] = v;
    return m;
  }, [q.data]);
  return { byKey, loading: enabled && q.loading, error: q.error, enabled };
}

// statusOf renders one verdict as a glyph + a short scope/state pill. The pill is
// the T1.6 ask: the operator sees WHICH row won (global vs a named scope), not
// just that something did.
function statusOf(v: ReferenceValidation | undefined): { glyph: string; color: string; pill: string } | null {
  if (!v) return null;
  if (v.ok) {
    // resolvedScope is null for keys (no scope column) and when the winning row
    // sits in a scope this actor may not read — show the ✓ without inventing one.
    const pill = v.resolvedScope == null ? "" : v.resolvedScope === "" ? GLOBAL_SCOPE_LABEL : v.resolvedScope;
    return { glyph: "✓", color: c.success, pill };
  }
  if (v.outcome === "out_of_scope") {
    const other = (v.otherScopes ?? []).join(", ");
    return { glyph: "✗", color: c.warning, pill: other ? `only ${other}` : "out of scope" };
  }
  if (v.outcome === "invalid") return { glyph: "✗", color: c.danger, pill: "invalid" };
  return { glyph: "✗", color: c.danger, pill: "missing" };
}

// UnresolvedList explains the ✗ chips in words underneath them. A chip title is a
// hover, and the originating debugging session was lost precisely because the
// reason was somewhere you had to go looking for.
//
// The Run-dialog preflight is its only caller since VU-20: the editor now carries
// each reason on the reference's own row, where it cannot be read as a second,
// separate finding about the same reference.
function UnresolvedList({ verdicts }: { verdicts: ReferenceValidation[] }) {
  const bad = verdicts.filter((v) => !v.ok);
  if (bad.length === 0) return null;
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 3, fontSize: c.fontXs, color: c.textSec }}>
      {bad.map((v) => (
        <div key={bkey(v)}>
          <span style={{ fontFamily: c.mono, color: v.outcome === "out_of_scope" ? c.warning : c.danger }}>
            {v.reference || v.name}
          </span>{" "}
          — {v.reason}
        </div>
      ))}
    </div>
  );
}

// A binding chip: the derived CRONOMICON_<SECTION>_<name> reference with a kind dot,
// optionally removable, and — when a verdict is available — the resolution state
// plus the scope the row resolved from.
function BindingChip({ b, onRemove, v }: { b: ReferenceBinding; onRemove?: () => void; v?: ReferenceValidation }) {
  const col = kindColor(b.kind);
  const st = statusOf(v);
  const label = `${KIND_LABEL[b.kind as Kind] ?? b.kind}: ${b.name}`;
  return (
    <span
      title={st ? `${label} — ${v?.reason ?? ""}` : label}
      style={{
        display: "inline-flex",
        alignItems: "center",
        gap: 6,
        padding: onRemove ? "2px 4px 2px 9px" : "2px 9px",
        borderRadius: c.radiusChip,
        fontSize: c.fontXs,
        fontFamily: c.mono,
        background: `${col}1a`,
        color: c.text,
        // A failed verdict re-tints the BORDER rather than the whole chip: the fill
        // still encodes kind, which is the thing the operator scans for.
        border: `1px solid ${st && st.glyph === "✗" ? `${st.color}aa` : `${col}55`}`,
        whiteSpace: "nowrap",
      }}
    >
      <span style={{ width: 6, height: 6, borderRadius: "50%", background: col, flexShrink: 0 }} />
      {b.reference || b.name}
      {b.as && (
        <>
          <span aria-hidden style={{ color: c.textMuted }}>&rarr;</span>
          <span style={SR_ONLY}>injected as</span>
          <span style={{ color: c.textSec }}>{aliasDestination(b)}</span>
        </>
      )}
      {st && (
        <>
          <span aria-hidden style={{ color: st.color, fontWeight: 700, lineHeight: 1 }}>
            {st.glyph}
          </span>
          {st.pill && <span style={{ color: c.textMuted, fontSize: c.fontXs }}>{st.pill}</span>}
          {/* The glyph is decorative; the verdict itself has to reach a screen
              reader, and a title attribute alone does not reliably do that. */}
          <span style={{ position: "absolute", width: 1, height: 1, overflow: "hidden", clip: "rect(0 0 0 0)", whiteSpace: "nowrap" }}>
            {v?.reason}
          </span>
        </>
      )}
      {onRemove && (
        <button
          onMouseDown={(e) => e.preventDefault()}
          onClick={onRemove}
          title={`Remove ${b.name}`}
          aria-label={`Remove reference ${b.name}`}
          style={{ border: "none", background: "transparent", color: c.textMuted, cursor: "pointer", fontSize: c.fontSm, lineHeight: 1, padding: "0 2px" }}
        >
          ×
        </button>
      )}
    </span>
  );
}

// ReferenceRow is the editor's line item (VU-20). The editor used to render every
// reference twice — once as a BindingChip capsule and again as a sentence in
// UnresolvedList directly beneath — so one unresolved reference read as two
// separate findings. This is the merge of the two: kind dot, derived name, verdict
// glyph, state pill, reason, remove control, in one left-to-right line.
//
// Rows render for HEALTHY references too. UnresolvedList filtered to failures, and
// inheriting that filter would empty the section exactly when everything resolves —
// losing the inventory of what the job consumes, which is the section's purpose.
//
// BindingChip stays: ReferencePreflightPanel and RunReferences render chips with no
// list beneath them, so they never had the duplication and have nothing to fall
// back on.
function ReferenceRow({
  b,
  v,
  onRemove,
  unsavedUnknown,
  trailing,
}: {
  b: ReferenceBinding;
  v?: ReferenceValidation;
  onRemove?: () => void;
  unsavedUnknown?: boolean;
  /** Extra inline content at the end of the row (JP-4b: the provenance chip).
   *  A slot rather than a wrapper element because the row IS the <li> — nesting
   *  one inside another list item is invalid and wrapped the chip onto its own
   *  line. */
  trailing?: React.ReactNode;
}) {
  const col = kindColor(b.kind);
  const st = statusOf(v);
  const kind = KIND_LABEL[b.kind as Kind] ?? b.kind;
  // With the reason inline, it can BE the row's accessible description rather than
  // a second visually-hidden copy of itself. The hidden span existed because a
  // title attribute does not reliably reach assistive tech; aria-describedby does.
  const reasonId = useId();
  const reason = st ? (v?.reason ?? "") : "";
  return (
    <li
      aria-describedby={reason ? reasonId : undefined}
      style={{
        display: "flex",
        alignItems: "center",
        flexWrap: "wrap",
        gap: 8,
        // The × is ANCHORED (absolute, top-right) rather than pushed there by
        // `marginLeft: auto`. Pushed, it rode the wrap: a reason long enough to take
        // its own line — routine in a narrow column, and every key row states one —
        // carried the × down with it, so in a two-row list the control sat between
        // the rows and read as belonging to the wrong one. Anchoring keeps it on the
        // line whose reference it deletes, and leaves the reason inline (forcing the
        // reason onto its own line instead would double the height of every row in a
        // healthy list, which is the common case).
        position: "relative",
        paddingRight: onRemove ? 22 : 0,
        paddingTop: 3,
        paddingBottom: 3,
        fontSize: c.fontSm,
        lineHeight: 1.4,
      }}
    >
      {/* The fill encodes kind — Secret / Variable / SSH Key — which is what an
          operator scans a reference list for. The dot is the only carrier of it,
          so the word goes to assistive tech alongside. */}
      <span aria-hidden title={kind} style={{ width: 7, height: 7, borderRadius: "50%", background: col, flexShrink: 0 }} />
      <span style={SR_ONLY}>{kind}</span>
      <span style={{ fontFamily: c.mono, color: c.text, flexShrink: 0 }}>{b.reference || b.name}</span>
      {/* RA-1 — an aliased binding shows BOTH names: the row it resolves (left) and
          the key the value lands on (right). Showing only the destination would make
          the chip unable to answer "whose credential is this?", which is the entire
          point of aliasing in a departmental estate; showing only the row would hide
          what the job body actually reads. The arrow is the direction of delivery. */}
      {b.as && (
        <span style={{ display: "inline-flex", alignItems: "center", gap: 4, flexShrink: 0, fontFamily: c.mono, fontSize: c.fontXs, color: c.textSec }}>
          <span aria-hidden>&rarr;</span>
          <span style={SR_ONLY}>injected as</span>
          {aliasDestination(b)}
        </span>
      )}
      {st && (
        <span aria-hidden style={{ color: st.color, fontWeight: 700, lineHeight: 1, flexShrink: 0 }}>
          {st.glyph}
        </span>
      )}
      {/* Chip radius, not radiusPill: the fully-round shape stays reserved for run
          status, so a verdict cannot mimic one (VU-6 / VU-17). */}
      {st?.pill && (
        <span
          style={{
            padding: "1px 7px",
            borderRadius: c.radiusChip,
            fontSize: c.fontXs,
            background: `${st.color}1a`,
            border: `1px solid ${st.color}55`,
            color: st.color,
            whiteSpace: "nowrap",
            flexShrink: 0,
          }}
        >
          {st.pill}
        </span>
      )}
      {/* Before the reason rather than after it, so a marker sits beside what it
          marks. Today the two cannot both appear — the ⚠ is for an UNSAVED row and a
          reason comes from a verdict, which only a saved row has — so this is
          ordering for whichever of them renders, not a fix for a seen collision. */}
      {unsavedUnknown && (
        <span
          title="No Env Vars row with this name exists yet — the run will fail closed unless it is created."
          style={{ color: c.warning, fontSize: c.fontXs, flexShrink: 0 }}
        >
          ⚠
        </span>
      )}
      {/* Before the reason, for the same reason the ⚠ is: the reason span is the
          row's flexible element, so anything after it gets pushed onto its own
          line by a long one — where a provenance chip reads as belonging to the
          next row rather than this one. */}
      {trailing}
      {reason && (
        <span id={reasonId} style={{ flex: "1 1 auto", minWidth: 0, fontSize: c.fontXs, color: c.textSec }}>
          {reason}
        </span>
      )}
      {onRemove && (
        <button
          onMouseDown={(e) => e.preventDefault()}
          onClick={onRemove}
          title={`Remove ${b.name}`}
          aria-label={`Remove reference ${b.name}`}
          style={{
            position: "absolute",
            top: 2,
            right: 0,
            border: "none",
            background: "transparent",
            color: c.textMuted,
            cursor: "pointer",
            fontSize: c.fontBody,
            lineHeight: 1,
            padding: "0 2px",
          }}
        >
          ×
        </button>
      )}
    </li>
  );
}

// RA-1 — identity includes the ALIAS, matching dispatch's own dedupe key
// (kind\0name\0as). One row bound twice under TWO destinations is a legitimate
// pair — it lands two keys — so keying on kind+name alone would collapse them and
// silently drop the second everywhere this is used: dedupe, verdict lookup, React
// keys. What is refused (422, server-side) is two DIFFERENT rows aliased to ONE
// destination, which is a collision rather than a duplicate.
const bkey = (b: { kind: string; name: string; as?: string }) => `${b.kind} ${b.name} ${b.as ?? ""}`;

// Owner discriminates the endpoint: a job (by numeric id) or a script (by name).
type Owner = { job: number } | { script: string };

const derived = (kind: Kind, name: string) =>
  `CRONOMICON_${kind === "secret" ? "SECRET" : kind === "key" ? "KEY" : "VAR"}_${name}`;

// aliasDestination — the key an aliased binding's value actually lands on. Derived
// locally rather than read from the row so it renders on an UNSAVED draft too, where
// no server verdict exists yet; the validator's `injectReference` is the same string
// and is what confirms it once the binding is saved.
const aliasDestination = (b: { kind: string; as?: string }) =>
  b.as ? derived(b.kind as Kind, b.as) : "";

// ── One binding set, two surfaces (EV-6) ─────────────────────────────────────
// Job detail now edits the SAME binding set from two places: the promoted SSH-key
// field (JobKeyField) and the secrets/variables editor below it. The endpoint takes
// the WHOLE set, so each write has to carry back the kinds it does not display —
// and carry them as the SERVER has them at write time. Preserving a copy fetched
// when the component mounted would let either surface silently revert the other's
// save, which is the one way this split could corrupt data rather than merely look
// wrong. Hence: re-read, merge, then PUT.
async function fetchBindings(owner: Owner): Promise<{ bindings: ReferenceBinding[]; error?: string }> {
  const r =
    "job" in owner
      ? await api.GET("/job-reference-bindings/{jobId}", { params: { path: { jobId: owner.job } } })
      : await api.GET("/script-reference-bindings/{name}", { params: { path: { name: owner.script } } });
  if (r.error) return { bindings: [], error: errMsg(r.error) };
  return { bindings: (r.data as { bindings?: ReferenceBinding[] })?.bindings ?? [] };
}

// putBindings replaces the `mine` kinds and preserves every other kind from a fresh
// read. Returns an error string, or null on success. Exported for the Job Composer,
// whose draft-until-save form writes its key bindings only after the job row saves.
export async function putBindings(
  owner: Owner,
  mine: ReferenceBinding[],
  isMine: (b: ReferenceBinding) => boolean,
): Promise<string | null> {
  const fresh = await fetchBindings(owner);
  // A failed re-read must ABORT the write: PUTting without the kinds we could not
  // read would delete them.
  if (fresh.error) return fresh.error;
  const merged = [...fresh.bindings.filter((b) => !isMine(b)), ...mine];
  const body = { bindings: merged.map((b) => ({ kind: b.kind, name: b.name, as: b.as || undefined })) };
  const { error } =
    "job" in owner
      ? await api.PUT("/job-reference-bindings/{jobId}", { params: { path: { jobId: owner.job }, header: csrfHeader }, body })
      : await api.PUT("/script-reference-bindings/{name}", { params: { path: { name: owner.script }, header: csrfHeader }, body });
  return error ? errMsg(error) : null;
}

// useKnownKeys lists the stored SSH-key LABELS an operator can bind. The add picker
// had no source for keys at all before EV-6 — /env-secrets and /env-vars fed the
// datalist and keys fell through it — so the one kind a job nearly always wants was
// the one you had to type from memory, with no wrong-name warning either.
function useKnownKeys() {
  const q = useGet<unknown>(() => api.GET("/ssh/credentials"), []);
  const labels = useMemo(
    () =>
      rows<SshCredential>(q.data)
        .map((k) => k.label)
        .filter((l): l is string => !!l)
        .sort((a, b) => a.localeCompare(b)),
    [q.data],
  );
  return labels;
}

// ReferenceBindingsEditor renders and edits a job's or script's declared reference
// bindings. Reads are session-gated; writes need ManageEnvVars — without it the
// editor renders read-only (rows, no controls). For scripts it also offers a body
// scan (prefill suggestions) and surfaces the bare-name migration lint.
//
// `scope` is the scope each declared reference is resolved against for the T1.4/T1.5
// verdicts — a job's own scope ("" = global), and "" for a script (scripts have
// no scope of their own; a script binding resolves against whatever scope the run
// carries, and the global answer is the one that holds for every job).
//
// `kinds` narrows which kinds this instance OWNS — it renders and edits only those,
// and its save preserves the rest untouched. Job detail passes secret+var because
// keys are promoted to their own field (see JobKeyField); Scripts keeps all three,
// having no host of its own to connect to.
export function ReferenceBindingsEditor({
  owner,
  scope,
  kinds = KINDS,
}: {
  owner: Owner;
  scope?: string | null;
  kinds?: Kind[];
}) {
  const isJob = "job" in owner;
  const ownsKind = (k: string) => kinds.includes(k as Kind);
  const [refresh, setRefresh] = useState(0);
  const [canManage, setCanManage] = useState(false);
  useEffect(() => {
    fetchCapabilities().then((caps) => setCanManage(caps.manageEnvVars));
  }, []);

  const bindingsQ = useGet<{ bindings?: ReferenceBinding[] }>(
    () =>
      isJob
        ? api.GET("/job-reference-bindings/{jobId}", { params: { path: { jobId: owner.job } } })
        : api.GET("/script-reference-bindings/{name}", { params: { path: { name: owner.script } } }),
    [isJob ? owner.job : owner.script, refresh],
  );
  // Only the kinds this instance owns reach the draft, the rows and the verdict
  // POST; the rest are preserved by putBindings from a fresh read at save time.
  const serverBindings = useMemo<ReferenceBinding[]>(
    () => (bindingsQ.data?.bindings ?? []).filter((b) => ownsKind(b.kind)),
    [bindingsQ.data, kinds.join(",")],
  );

  // Known Env Vars rows by kind — for the add picker (datalist) so an operator binds
  // a real reference, and so unknown names are flagged before save.
  const secQ = useGet<unknown>(() => api.GET("/env-secrets"), []);
  const varQ = useGet<unknown>(() => api.GET("/env-vars"), []);
  const keyLabels = useKnownKeys();
  const known = useMemo(() => {
    const m: Record<Kind, string[]> = { secret: [], var: [], key: keyLabels };
    for (const s of rows<EnvSecret>(secQ.data)) if (s.key) m.secret.push(s.key);
    for (const v of rows<EnvVar>(varQ.data)) if (v.key) m.var.push(v.key);
    return m;
  }, [secQ.data, varQ.data, keyLabels]);

  const [draft, setDraft] = useState<ReferenceBinding[]>([]);
  const [addKind, setAddKind] = useState<Kind>(kinds[0] ?? "secret");
  const [addName, setAddName] = useState("");
  const [addAs, setAddAs] = useState("");
  const [saving, setSaving] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [scanLint, setScanLint] = useState<ReferenceBinding[] | null>(null);

  // Reset the editable draft whenever the server set changes (load / after save).
  useEffect(() => {
    setDraft(serverBindings.map((b) => ({ ...b })));
  }, [serverBindings]);

  // bkey carries the alias, so changing ONLY a destination reads as dirty — which it
  // is: the value lands on a different key.
  const dirty = useMemo(() => {
    const a = draft.map(bkey).sort();
    const b = serverBindings.map(bkey).sort();
    return a.length !== b.length || a.some((x, i) => x !== b[i]);
  }, [draft, serverBindings]);

  const addBinding = (kind: Kind, name: string, as?: string) => {
    const n = name.trim();
    if (!n) return;
    const a = (as ?? "").trim();
    // Dedupe on the FULL identity: the same row under two aliases is two bindings.
    if (draft.some((b) => b.kind === kind && b.name === n && (b.as ?? "") === a)) return;
    setDraft([...draft, { kind, name: n, as: a || undefined, reference: derived(kind, n) }]);
    setAddName("");
    setAddAs("");
  };
  const removeBinding = (b: ReferenceBinding) =>
    setDraft(draft.filter((x) => !(x.kind === b.kind && x.name === b.name && (x.as ?? "") === (b.as ?? ""))));

  const save = async () => {
    setSaving(true);
    setErr(null);
    const error = await putBindings(owner, draft, (b) => ownsKind(b.kind));
    setSaving(false);
    if (error) setErr(error);
    else setRefresh((n) => n + 1);
  };

  // Scripts only: scan the body for referenced names → prefill suggestions + lint.
  const scan = async () => {
    if (isJob) return;
    setErr(null);
    const { data, error } = await api.GET("/script-reference-scan/{name}", { params: { path: { name: owner.script } } });
    if (error) {
      setErr(errMsg(error));
      return;
    }
    const d = data as { suggested?: ReferenceBinding[]; bareReferences?: ReferenceBinding[] };
    const merged = [...draft];
    for (const s of d.suggested ?? []) {
      if (!merged.some((b) => b.kind === s.kind && b.name === s.name)) merged.push({ ...s });
    }
    setDraft(merged);
    setScanLint(d.bareReferences ?? []);
  };

  // T1.4/T1.5 — resolve the SERVER-stored set, not the draft: an unsaved chip has
  // no dispatch meaning yet, and validating each keystroke would POST per character.
  const effScope = scope === undefined ? null : (scope ?? "");
  const { byKey: verdicts, error: verdictErr } = useReferenceValidation(serverBindings, effScope);
  const saved = useMemo(() => new Set(serverBindings.map(bkey)), [serverBindings]);

  // Keys are no longer exempt: useKnownKeys gives them the same name list the other
  // kinds have had, so a mistyped label is caught at authoring time like any other.
  const unknown = (b: ReferenceBinding): boolean => !(known[b.kind as Kind] ?? []).includes(b.name);

  if (bindingsQ.loading) return <InlineLoading what="references" />;

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 10 }}>
      {draft.length === 0 ? (
        <span style={{ fontSize: c.fontSm, color: c.textMuted }}>
          {kinds.length === KINDS.length
            ? "No references declared."
            : `No ${kinds.map((k) => `${KIND_LABEL[k].toLowerCase()}s`).join(" or ")} declared.`}
        </span>
      ) : (
        <ul style={{ listStyle: "none", margin: 0, padding: 0, display: "flex", flexDirection: "column", gap: 2 }}>
          {draft.map((b) => (
            <ReferenceRow
              key={bkey(b)}
              b={b}
              v={saved.has(bkey(b)) ? verdicts[bkey(b)] : undefined}
              onRemove={canManage ? () => removeBinding(b) : undefined}
              // The ⚠ predates the validator and now only covers what it cannot:
              // an UNSAVED reference, which has no verdict because it has no
              // dispatch meaning yet. A saved row's state is its ✓/✗.
              unsavedUnknown={!saved.has(bkey(b)) && unknown(b)}
            />
          ))}
        </ul>
      )}

      {verdictErr && (
        <div style={{ fontSize: c.fontXs, color: c.textMuted }}>
          Could not check whether these references resolve: {verdictErr}
        </div>
      )}

      {canManage && (
        <div style={{ display: "flex", flexWrap: "wrap", gap: 6, alignItems: "center" }}>
          <select
            value={addKind}
            onChange={(e) => setAddKind(e.target.value as Kind)}
            style={{ fontSize: c.fontSm, padding: "4px 6px", borderRadius: c.radiusChip, background: c.panel2, color: c.text, border: `1px solid ${c.borderStrong}` }}
          >
            {kinds.map((k) => (
              <option key={k} value={k}>
                {KIND_LABEL[k]}
              </option>
            ))}
          </select>
          <input
            value={addName}
            list={`known-${addKind}`}
            placeholder="reference name…"
            onChange={(e) => setAddName(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter") {
                e.preventDefault();
                addBinding(addKind, addName);
              }
            }}
            style={{ fontSize: c.fontSm, padding: "4px 8px", borderRadius: c.radiusChip, background: c.panel2, color: c.text, border: `1px solid ${c.borderStrong}`, fontFamily: c.mono, minWidth: 180 }}
          />
          <datalist id={`known-${addKind}`}>
            {(known[addKind] ?? []).map((n) => (
              <option key={n} value={n} />
            ))}
          </datalist>
          {/* RA-1 / RA-Q16 — the ALIAS. Optional and deliberately quiet: the common
              binding needs none, and a required-looking second field would imply the
              plain case is incomplete. It is a DESTINATION, never a selector — the
              name above still decides WHICH row resolves, so an alias can never reach
              a row the operator could not already bind. What it buys is one shared
              job body serving every department: each department's own row, injected
              under the single name the script or playbook reads. */}
          <input
            value={addAs}
            placeholder="inject as… (optional)"
            aria-label="Alias — the name this reference is injected under (optional)"
            title="Optional. The bare destination name the value is injected under, so one job body can consume any department's row. Leave empty to inject under the row's own name."
            onChange={(e) => setAddAs(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter") {
                e.preventDefault();
                addBinding(addKind, addName, addAs);
              }
            }}
            style={{ fontSize: c.fontSm, padding: "4px 8px", borderRadius: c.radiusChip, background: c.panel2, color: c.text, border: `1px solid ${c.border}`, fontFamily: c.mono, minWidth: 150 }}
          />
          {addAs.trim() && (
            <span style={{ fontSize: c.fontXs, color: c.textSec, fontFamily: c.mono }}>
              &rarr; {derived(addKind, addAs.trim())}
            </span>
          )}
          <Btn small onClick={() => addBinding(addKind, addName, addAs)}>
            Add
          </Btn>
          {!isJob && (
            <Btn small onClick={scan} title="Scan the script body for referenced names and prefill suggestions">
              Suggest from body
            </Btn>
          )}
          {dirty && (
            <>
              <Btn small primary onClick={save} disabled={saving}>
                {saving ? "Saving…" : "Save references"}
              </Btn>
              <Btn small onClick={() => setDraft(serverBindings.map((b) => ({ ...b })))} disabled={saving}>
                Reset
              </Btn>
            </>
          )}
        </div>
      )}

      {err && <div style={{ fontSize: c.fontXs, color: c.danger }}>Save failed: {err}</div>}

      {scanLint && scanLint.length > 0 && (
        <div style={{ fontSize: c.fontXs, color: c.textMuted }}>
          Bare references still on the legacy fallback (consider migrating to the derived form):{" "}
          {scanLint.map((b) => (
            <span key={bkey(b)} style={{ fontFamily: c.mono, color: c.warning, marginRight: 8 }}>
              {b.name}
            </span>
          ))}
        </div>
      )}
    </div>
  );
}

// ── JobKeyField — the promoted SSH-key binding (EV-6) ────────────────────────
// A declared key is not payload the way a secret or a variable is. It resolves to
// key MATERIAL that the executor writes out as a key FILE for the run to use
// (runref/resolve.go's Keys / D8), so it is what a playbook or script consumes as
// CRONOMICON_KEY_<label> — and in practice it is single-valued, a second one only for a
// bastion. It is also the one kind outside the References section's scope model:
// ssh_credentials carries no scope column, so a key is agency-filtered rather than
// scope-filtered and NEVER gets a resolved-scope pill (runref/validate.go — keys take
// the !scoped branch, then an agency-membership check). Both facts argued for lifting
// it out of the mixed list, beside Executor, rather than leaving it as one chip among
// a dozen.
//
// It still IS an ordinary declared reference underneath, so the row keeps the derived
// name visible. Writes go through putBindings, which preserves the secrets and
// variables this field does not show.
//
// NOT gated on the executor. The tempting gate is backwards: it is the RUNNER path
// that materializes a declared key (D8), while a run that resolves to the in-app
// SSH executor is REFUSED at enqueue (KB — the executor cannot place a key on the
// target). And with keys removed from the References editor below, a hidden field
// would leave no way to declare one at all — so it always renders, and the
// executor consequence is stated instead of guessed.
//
// The component owns its grid cell (span 2): it is dropped straight into the job
// overview grid and needs more room than a one-word field.
export function JobKeyField({
  jobId,
  scope,
  executor,
}: {
  jobId: number;
  scope?: string | null;
  // The job's executor, for the delivery caveat. null ⇒ resolved from the run type at
  // trigger (shell types ⇒ ssh), which is exactly when the caveat can bite.
  executor?: "runner" | "ssh" | null;
}) {
  const owner = useMemo<Owner>(() => ({ job: jobId }), [jobId]);
  const [refresh, setRefresh] = useState(0);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [canManage, setCanManage] = useState(false);
  useEffect(() => {
    fetchCapabilities().then((caps) => setCanManage(caps.manageEnvVars));
  }, []);

  const q = useGet<{ bindings?: ReferenceBinding[] }>(
    () => api.GET("/job-reference-bindings/{jobId}", { params: { path: { jobId } } }),
    [jobId, refresh],
  );
  const keys = useMemo<ReferenceBinding[]>(
    () => (q.data?.bindings ?? []).filter((b) => b.kind === "key"),
    [q.data],
  );
  const labels = useKnownKeys();
  const effScope = scope === undefined ? null : (scope ?? "");
  const { byKey: verdicts } = useReferenceValidation(keys, effScope);

  const write = async (next: ReferenceBinding[]) => {
    setBusy(true);
    setErr(null);
    const error = await putBindings(owner, next, (b) => b.kind === "key");
    setBusy(false);
    if (error) setErr(error);
    else setRefresh((n) => n + 1);
  };
  const assign = (name: string) => {
    if (!name || keys.some((k) => k.name === name)) return;
    write([...keys, { kind: "key", name, reference: derived("key", name) }]);
  };

  const unbound = labels.filter((l) => !keys.some((k) => k.name === l));
  if (q.loading) return null;

  return (
    <div style={{ gridColumn: "span 2", minWidth: 0 }}>
      <div style={{ fontSize: c.fontXs, fontFamily: c.sansCond, fontWeight: 600, color: c.textMuted, textTransform: "uppercase", letterSpacing: 0.7, marginBottom: 3 }}>
        SSH key
      </div>
      {keys.length === 0 ? (
        <div style={{ fontSize: c.fontSm, color: c.textMuted, maxWidth: "70ch" }}>
          None declared — for a playbook or script that reads{" "}
          <span style={{ fontFamily: c.mono }}>CRONOMICON_KEY_&lt;label&gt;</span>. In-app SSH auth uses the host record's
          own key.
        </div>
      ) : (
        <ul style={{ listStyle: "none", margin: 0, padding: 0, display: "flex", flexDirection: "column", gap: 2 }}>
          {keys.map((b) => (
            <ReferenceRow
              key={bkey(b)}
              b={b}
              v={verdicts[bkey(b)]}
              onRemove={canManage && !busy ? () => write(keys.filter((k) => k.name !== b.name)) : undefined}
            />
          ))}
        </ul>
      )}
      {/* The consequence an operator cannot infer from anywhere else in the UI:
          only the runner path materializes a declared key, and a run that
          resolves to the in-app SSH executor is refused at enqueue (KB) — the
          executor connects from Cronomicon and cannot place a key on the target.
          Stated here, where the key is declared, rather than discovered at the
          Run button. EV-1: a consequence of current input stays inline. */}
      {keys.length > 0 && executor !== "runner" && (
        <div style={{ fontSize: c.fontXs, color: c.warning, marginTop: 4, maxWidth: "70ch" }}>
          {executor == null
            ? "Refused if a run resolves to the in-app SSH executor (this job's executor resolves at trigger)"
            : "Refused on the in-app SSH executor — runs of this job are rejected unless overridden to a runner"}
          {" — SSH keys are delivered on the runner path only. To use the key on the target, bind it as a Secret and write the file in the job body."}
        </div>
      )}
      {/* The picker renders only while NO key is bound. Assigning the FIRST key must
          stay here — for a git-synced job this field is the only persistent authoring
          surface (the composer is cronomicon-source only, D6). But once one is bound the
          "Add another key…" state is gone by request: the multi-key case (a second
          key for a bastion) is composer territory, and here it read as an open-ended
          list invitation on every expanded job. Remove (×) above still works, so
          swapping a key is remove-then-assign. */}
      {canManage && keys.length === 0 && (
        <div style={{ display: "flex", alignItems: "center", gap: 6, marginTop: 6 }}>
          {labels.length === 0 ? (
            <span style={{ fontSize: c.fontXs, color: c.textMuted }}>
              No stored SSH keys yet — add one under Env Vars → SSH Keys.
            </span>
          ) : (
            <>
              {/* Single-select and apply-on-change: the field is one decision, so a
                  draft plus a Save button (the References section's shape) would be
                  ceremony around picking one name from a list. */}
              <select
                value=""
                disabled={busy}
                aria-label="Assign an SSH key"
                onChange={(e) => assign(e.target.value)}
                style={{ fontSize: c.fontSm, padding: "3px 6px", borderRadius: c.radiusChip, background: c.panel2, color: c.text, border: `1px solid ${c.borderStrong}`, maxWidth: 260 }}
              >
                <option value="">Assign a key…</option>
                {unbound.map((l) => (
                  <option key={l} value={l}>
                    {l}
                  </option>
                ))}
              </select>
              {busy && <span style={{ fontSize: c.fontXs, color: c.textMuted }}>Saving…</span>}
            </>
          )}
        </div>
      )}
      {err && <div style={{ fontSize: c.fontXs, color: c.danger, marginTop: 4 }}>Could not save: {err}</div>}
    </div>
  );
}

// ── ComposeKeyPicker — the Job Composer's SSH-key section ────────────────────
// The controlled sibling of JobKeyField above: same rows, same apply-on-change
// picker, same runner-path caveat — but the chosen labels live in the COMPOSER's
// draft state rather than being written per change. The composer is draft-until-
// save for every other field, and a key that persisted while Create was never
// clicked would be a binding on a job that does not exist (create mode has no
// jobId to write against anyway). The composer PUTs the set after the job row
// saves, via the exported putBindings.
//
// Unlike the editor, this validates the DRAFT set: authoring-time feedback is the
// point here, exactly as the Run dialog's preflight validates its unsaved
// additions. The label is the host Field's, so this renders body only.
export function ComposeKeyPicker({
  keys,
  onChange,
  scope,
  executor,
  canManage,
}: {
  keys: string[];
  onChange: (keys: string[]) => void;
  scope?: string | null;
  /** The composer's executor choice; "" (auto) resolves at trigger — caveat-wise the same as null. */
  executor?: string | null;
  canManage: boolean;
}) {
  const labels = useKnownKeys();
  const sig = keys.join("|");
  const bindings = useMemo<ReferenceBinding[]>(
    () => keys.map((name) => ({ kind: "key", name, reference: derived("key", name) })),
    // keys is a fresh array each render; its joined signature is its identity.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [sig],
  );
  const effScope = scope === undefined ? null : (scope ?? "");
  const { byKey: verdicts } = useReferenceValidation(bindings, effScope);
  const unbound = labels.filter((l) => !keys.includes(l));
  const auto = !executor;

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 4 }}>
      {keys.length === 0 ? (
        <div style={{ fontSize: c.fontSm, color: c.textMuted, maxWidth: "70ch" }}>
          None declared — in-app SSH auth uses the host record's own key.
        </div>
      ) : (
        <ul style={{ listStyle: "none", margin: 0, padding: 0, display: "flex", flexDirection: "column", gap: 2 }}>
          {bindings.map((b) => (
            <ReferenceRow
              key={bkey(b)}
              b={b}
              v={verdicts[bkey(b)]}
              onRemove={canManage ? () => onChange(keys.filter((k) => k !== b.name)) : undefined}
            />
          ))}
        </ul>
      )}
      {/* KB — same consequence as JobKeyField: only the runner path materializes
          a declared key; a run that resolves to the in-app SSH executor is refused. */}
      {keys.length > 0 && executor !== "runner" && (
        <div style={{ fontSize: c.fontXs, color: c.warning, maxWidth: "70ch" }}>
          {auto
            ? "Refused if a run resolves to the in-app SSH executor (this job's executor resolves at trigger)"
            : "Refused on the in-app SSH executor — runs of this job are rejected unless overridden to a runner"}
          {" — SSH keys are delivered on the runner path only. To use the key on the target, bind it as a Secret and write the file in the job body."}
        </div>
      )}
      {canManage && (
        <div style={{ display: "flex", alignItems: "center", gap: 6, marginTop: 2 }}>
          {labels.length === 0 ? (
            <span style={{ fontSize: c.fontXs, color: c.textMuted }}>
              No stored SSH keys yet — add one under Env Vars → SSH Keys.
            </span>
          ) : (
            <select
              value=""
              disabled={unbound.length === 0}
              aria-label={keys.length === 0 ? "Assign an SSH key" : "Add another SSH key"}
              onChange={(e) => {
                const n = e.target.value;
                if (n && !keys.includes(n)) onChange([...keys, n]);
              }}
              style={{ fontSize: c.fontSm, padding: "3px 6px", borderRadius: c.radiusChip, background: c.panel2, color: c.text, border: `1px solid ${c.borderStrong}`, maxWidth: 260 }}
            >
              <option value="">
                {unbound.length === 0 ? "All stored keys bound" : keys.length === 0 ? "Assign a key…" : "Add another key…"}
              </option>
              {unbound.map((l) => (
                <option key={l} value={l}>
                  {l}
                </option>
              ))}
            </select>
          )}
        </div>
      )}
    </div>
  );
}

// ── ComposeReferencePicker — the Job Composer's secrets/variables section (JP-4a) ──
// The controlled sibling of ReferenceBindingsEditor, standing in the same relation
// to it as ComposeKeyPicker does to JobKeyField: same rows, same add controls, but
// the draft lives in the COMPOSER's state and is written by putBindings only after
// the job row exists. Job detail no longer edits these (JP-4b makes it a read-only
// effective-set display), so this is where a job's declared secrets and variables
// are authored.
//
// Like ComposeKeyPicker it validates the DRAFT set, not a server set: authoring-time
// feedback is the whole point, and an unsaved composer draft has no server state to
// validate instead. Deliberately no "Suggest from body" — that scans a SCRIPT body
// and belongs to the Scripts page, which keeps the full editor.
export function ComposeReferencePicker({
  bindings,
  onChange,
  scope,
  canManage,
}: {
  bindings: ReferenceBinding[];
  onChange: (bindings: ReferenceBinding[]) => void;
  scope?: string | null;
  canManage: boolean;
}) {
  const KINDS_HERE: Kind[] = ["secret", "var"];
  const [addKind, setAddKind] = useState<Kind>("secret");
  const [addName, setAddName] = useState("");
  const [addAs, setAddAs] = useState("");

  // Known names by kind, so a mistyped reference is caught while authoring rather
  // than at dispatch — the same two sources the editor uses.
  const secQ = useGet<unknown>(() => api.GET("/env-secrets"), []);
  const varQ = useGet<unknown>(() => api.GET("/env-vars"), []);
  const known = useMemo(() => {
    const m: Record<string, string[]> = { secret: [], var: [] };
    for (const s of rows<EnvSecret>(secQ.data)) if (s.key) m.secret.push(s.key);
    for (const v of rows<EnvVar>(varQ.data)) if (v.key) m.var.push(v.key);
    return m;
  }, [secQ.data, varQ.data]);

  const effScope = scope === undefined ? null : (scope ?? "");
  const { byKey: verdicts } = useReferenceValidation(bindings, effScope);

  const add = (kind: Kind, name: string, as?: string) => {
    const n = name.trim();
    if (!n) return;
    const a = (as ?? "").trim();
    // Dedupe on the FULL identity (kind+name+alias): the same row under two
    // destinations is two legitimate bindings, matching dispatch's dedupe key.
    if (bindings.some((b) => b.kind === kind && b.name === n && (b.as ?? "") === a)) return;
    onChange([...bindings, { kind, name: n, as: a || undefined, reference: derived(kind, n) }]);
    setAddName("");
    setAddAs("");
  };
  const remove = (b: ReferenceBinding) =>
    onChange(bindings.filter((x) => !(x.kind === b.kind && x.name === b.name && (x.as ?? "") === (b.as ?? ""))));
  const unknown = (b: ReferenceBinding): boolean => !(known[b.kind] ?? []).includes(b.name);

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 4 }}>
      {bindings.length === 0 ? (
        <div style={{ fontSize: c.fontSm, color: c.textMuted, maxWidth: "70ch" }}>
          None declared — this job's runs receive no stored secrets or variables.
        </div>
      ) : (
        <ul style={{ listStyle: "none", margin: 0, padding: 0, display: "flex", flexDirection: "column", gap: 2 }}>
          {bindings.map((b) => (
            <ReferenceRow
              key={bkey(b)}
              b={b}
              v={verdicts[bkey(b)]}
              onRemove={canManage ? () => remove(b) : undefined}
              // Every row here is unsaved by definition (draft-until-save), so the
              // ⚠ covers exactly what it was written for: a name no Env Vars row
              // carries yet. A verdict, when the validator returns one, is richer
              // and renders alongside.
              unsavedUnknown={unknown(b)}
            />
          ))}
        </ul>
      )}
      {canManage && (
        <div style={{ display: "flex", flexWrap: "wrap", gap: 6, alignItems: "center", marginTop: 2 }}>
          <select
            value={addKind}
            onChange={(e) => setAddKind(e.target.value as Kind)}
            aria-label="Reference kind"
            style={{ fontSize: c.fontSm, padding: "4px 6px", borderRadius: c.radiusChip, background: c.panel2, color: c.text, border: `1px solid ${c.borderStrong}` }}
          >
            {KINDS_HERE.map((k) => (
              <option key={k} value={k}>
                {KIND_LABEL[k]}
              </option>
            ))}
          </select>
          <input
            value={addName}
            list={`compose-known-${addKind}`}
            placeholder="reference name…"
            aria-label="Reference name"
            onChange={(e) => setAddName(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter") {
                e.preventDefault();
                add(addKind, addName, addAs);
              }
            }}
            style={{ fontSize: c.fontSm, padding: "4px 8px", borderRadius: c.radiusChip, background: c.panel2, color: c.text, border: `1px solid ${c.borderStrong}`, fontFamily: c.mono, minWidth: 180 }}
          />
          <datalist id={`compose-known-${addKind}`}>
            {(known[addKind] ?? []).map((n) => (
              <option key={n} value={n} />
            ))}
          </datalist>
          {/* RA-1 / RA-Q16 — the optional alias, same semantics as the editor's: a
              DESTINATION, never a selector, so one job body can consume any
              department's row under the single name it reads. */}
          <input
            value={addAs}
            placeholder="inject as… (optional)"
            aria-label="Alias — the name this reference is injected under (optional)"
            title="Optional. The bare destination name the value is injected under, so one job body can consume any department's row. Leave empty to inject under the row's own name."
            onChange={(e) => setAddAs(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter") {
                e.preventDefault();
                add(addKind, addName, addAs);
              }
            }}
            style={{ fontSize: c.fontSm, padding: "4px 8px", borderRadius: c.radiusChip, background: c.panel2, color: c.text, border: `1px solid ${c.border}`, fontFamily: c.mono, minWidth: 150 }}
          />
          {addAs.trim() && (
            <span style={{ fontSize: c.fontXs, color: c.textSec, fontFamily: c.mono }}>
              &rarr; {derived(addKind, addAs.trim())}
            </span>
          )}
          <Btn small onClick={() => add(addKind, addName, addAs)}>
            Add
          </Btn>
        </div>
      )}
    </div>
  );
}

// ── JobEffectiveReferences — job detail's read-only view (JP-4b) ─────────────
// Job detail used to EDIT the job's secret/var bindings. Authoring moved to the
// composer (ComposeReferencePicker above), on the principle that a job's content
// is set when the job is created or edited — so what belongs here is the truth
// the operator cannot get anywhere else: what this job's runs ACTUALLY receive.
//
// That is the union dispatch computes (collectRunBindings): the job's own
// declared bindings PLUS the referenced script's. The script half was never
// editable from this page and was never shown here either, so this display is
// strictly more informative than the editor it replaces. Each row carries its
// provenance, because where a reference came from is where you go to change it.
//
// Renders NOTHING (no empty section) when the union is empty — the old editor
// left "No secrets or variables declared." plus an add-row on every job that
// simply does not use references, which is most of them.
//
// SSH keys: the job's own keys are JobKeyField's (promoted beside Executor, EV-6)
// and are deliberately NOT repeated here. A key declared on the SCRIPT is shown,
// because nothing else on this page would tell you a run receives it.
export function JobEffectiveReferences({
  jobId,
  scriptRef,
  scope,
  action,
}: {
  jobId?: number;
  scriptRef?: string | null;
  scope?: string | null;
  /** Optional trailing control for the section header — job detail passes an
   *  "Edit in Composer" link for callers who may compose (JP-Q8). */
  action?: React.ReactNode;
}) {
  const jobQ = useGet<{ bindings?: ReferenceBinding[] }>(
    () =>
      jobId != null
        ? api.GET("/job-reference-bindings/{jobId}", { params: { path: { jobId } } })
        : Promise.resolve({ data: { bindings: [] } }),
    [jobId],
  );
  const scriptQ = useGet<{ bindings?: ReferenceBinding[] }>(
    () =>
      scriptRef
        ? api.GET("/script-reference-bindings/{name}", { params: { path: { name: scriptRef } } })
        : Promise.resolve({ data: { bindings: [] } }),
    [scriptRef ?? ""],
  );

  // Union, deduped exactly as dispatch dedupes (kind+name+alias). The job's own
  // row wins a tie so the provenance shown is the one the operator can act on
  // from the composer.
  const effective = useMemo(() => {
    const out: { b: ReferenceBinding; fromScript: boolean }[] = [];
    const seen = new Set<string>();
    for (const b of jobQ.data?.bindings ?? []) {
      if (b.kind === "key") continue; // JobKeyField owns the job's own keys.
      if (seen.has(bkey(b))) continue;
      seen.add(bkey(b));
      out.push({ b, fromScript: false });
    }
    for (const b of scriptQ.data?.bindings ?? []) {
      if (seen.has(bkey(b))) continue;
      seen.add(bkey(b));
      out.push({ b, fromScript: true });
    }
    return out;
  }, [jobQ.data, scriptQ.data]);

  const refs = useMemo(() => effective.map((e) => e.b), [effective]);
  const effScope = scope === undefined ? null : (scope ?? "");
  const { byKey: verdicts } = useReferenceValidation(refs, effScope);

  if (jobQ.loading || scriptQ.loading) return null;
  if (effective.length === 0) return null;

  return (
    <Section
      title={`Secrets & variables (${effective.length})`}
      info={
        <>
          What this job's runs receive at dispatch — its own declared references plus any the script it uses
          declares. Names only; values are resolved at dispatch and never shown. Each is checked against this
          job's scope; the Run dialog re-checks against the scope a run actually targets and can add more for a
          single run. To change the job's own references, edit the job; a reference marked{" "}
          <em>from script</em> is declared on the Scripts page and applies to every job using that script. SSH
          keys the job connects with are the same mechanism but live in their own field above.
        </>
      }
      actions={action}
    >
      <ul style={{ listStyle: "none", margin: 0, padding: 0, display: "flex", flexDirection: "column", gap: 2 }}>
        {effective.map(({ b, fromScript }) => (
          <ReferenceRow
            key={bkey(b)}
            b={b}
            v={verdicts[bkey(b)]}
            trailing={
              fromScript ? (
                <span
                  title={`Declared on ${scriptRef} — every job using that script receives it.`}
                  style={{
                    padding: "1px 7px",
                    borderRadius: c.radiusChip,
                    fontSize: c.fontXs,
                    background: c.panel2,
                    border: `1px solid ${c.border}`,
                    color: c.textSec,
                    whiteSpace: "nowrap",
                    flexShrink: 0,
                  }}
                >
                  from script
                </span>
              ) : undefined
            }
          />
        ))}
      </ul>
    </Section>
  );
}

// useRunReferencePreflight is the Run dialog's reference check (agencies plan
// T1.7, closing G6). It is the ONLY surface that can answer the question
// correctly: a job whose own scope is "All" can be run against a specific scope
// via the dialog's override, and references resolve against the RUN's scope — so
// job detail cannot predict resolvability, and this can.
//
// The set it checks mirrors dispatch's collectRunBindings: the job's declared
// bindings PLUS the referenced script's, deduped by kind+name.
//
// It is a HOOK rather than a self-contained component because the dialog's "Where
// it runs" disclosure UNMOUNTS its children while collapsed. Owning the check
// inside the panel therefore meant it never ran until the operator opened the
// fold — and the whole point is that an unresolvable reference OPENS the fold. The
// dialog holds the state; only the presentation below lives inside the fold.
export function useRunReferencePreflight({
  jobId,
  scriptRef,
  scope,
  added,
}: {
  jobId?: number;
  scriptRef?: string | null;
  scope: string;
  /** V2-11 — the operator's per-run reference ADDITIONS (dialog state, not yet a
      run). Validated together with the declared set, so an unresolvable addition
      springs the fold open exactly like an unresolvable declared binding. */
  added?: ReferenceBinding[];
}) {
  const jobQ = useGet<{ bindings?: ReferenceBinding[] }>(
    () =>
      jobId != null
        ? api.GET("/job-reference-bindings/{jobId}", { params: { path: { jobId } } })
        : Promise.resolve({ data: { bindings: [] } }),
    [jobId],
  );
  const scriptQ = useGet<{ bindings?: ReferenceBinding[] }>(
    () =>
      scriptRef
        ? api.GET("/script-reference-bindings/{name}", { params: { path: { name: scriptRef } } })
        : Promise.resolve({ data: { bindings: [] } }),
    [scriptRef ?? ""],
  );
  // The job's + script's DECLARED bindings — what dispatch injects with no operator
  // action, rendered read-only in the dialog (additive-only: a run can widen the
  // set, never shrink it).
  const declared = useMemo(() => {
    const seen = new Set<string>();
    const out: ReferenceBinding[] = [];
    for (const b of [...(jobQ.data?.bindings ?? []), ...(scriptQ.data?.bindings ?? [])]) {
      if (seen.has(bkey(b))) continue;
      seen.add(bkey(b));
      out.push(b);
    }
    return out;
  }, [jobQ.data, scriptQ.data]);
  // Declared ∪ added, deduped the same way dispatch will (kind+name) — this is the
  // set the run actually injects, so it is the set the validator checks.
  const addedList = added ?? [];
  const refs = useMemo(() => {
    const seen = new Set(declared.map(bkey));
    return [...declared, ...addedList.filter((b) => !seen.has(bkey(b)))];
    // addedList is a fresh array each render; its key signature is its identity.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [declared, addedList.map(bkey).sort().join("|")]);

  const { byKey, loading, error } = useReferenceValidation(refs, scope);
  const verdicts = refs.map((b) => byKey[bkey(b)]).filter(Boolean) as ReferenceValidation[];
  return {
    refs,
    declared,
    byKey,
    verdicts,
    loading,
    error,
    // The count the dialog folds into its collapsed summary and uses to spring the
    // fold open — the same treatment an incompatible scope gets, so a collapsed
    // fold can never hide a run that will fail closed at dispatch.
    unresolved: verdicts.filter((v) => !v.ok).length,
  };
}

export type RunReferencePreflight = ReturnType<typeof useRunReferencePreflight>;

// ReferencePreflightPanel renders what the hook above computed: the declared
// bindings as read-only chips and — when the caller wires `added`/`onAdd`/
// `onRemove` (V2-11) — the operator's per-run additions as removable rows plus
// two dropdowns to add more, mirroring the job editor's pickers. Additions need
// ManageEnvVars (the binding-edit gate, enforced server-side too); without it the
// panel stays the read-only preflight it always was. The `added` state lives in
// the DIALOG, not here — the fold unmounts this panel while collapsed.
export function ReferencePreflightPanel({
  state,
  scope,
  added,
  onAdd,
  onRemove,
  executor,
}: {
  state: RunReferencePreflight;
  scope: string;
  added?: ReferenceBinding[];
  onAdd?: (b: ReferenceBinding) => void;
  onRemove?: (b: ReferenceBinding) => void;
  /** The dialog's chosen executor, for the key-binding refusal notice (KB). */
  executor?: "runner" | "ssh";
}) {
  const { refs, declared, byKey, verdicts, loading, error, unresolved } = state;
  const editable = !!onAdd;
  const addedList = added ?? [];
  const [canManage, setCanManage] = useState(false);
  useEffect(() => {
    if (editable) fetchCapabilities().then((caps) => setCanManage(caps.manageEnvVars));
  }, [editable]);

  // Known rows for the add dropdowns — the same three sources the job editor's
  // picker uses, so the two surfaces can never disagree about what is bindable.
  const secQ = useGet<unknown>(() => (editable ? api.GET("/env-secrets") : Promise.resolve({ data: [] })), [editable]);
  const varQ = useGet<unknown>(() => (editable ? api.GET("/env-vars") : Promise.resolve({ data: [] })), [editable]);
  const keyLabels = useKnownKeys();
  const known = useMemo(() => {
    const m: Record<Kind, string[]> = { secret: [], var: [], key: keyLabels };
    for (const s of rows<EnvSecret>(secQ.data)) if (s.key) m.secret.push(s.key);
    for (const v of rows<EnvVar>(varQ.data)) if (v.key) m.var.push(v.key);
    m.secret.sort((a, b) => a.localeCompare(b));
    m.var.sort((a, b) => a.localeCompare(b));
    return m;
  }, [secQ.data, varQ.data, keyLabels]);

  const [addKind, setAddKind] = useState<Kind>("var");
  const [addAs, setAddAs] = useState("");
  const present = useMemo(() => new Set(refs.map(bkey)), [refs]);
  // Filtering by the FULL identity: with an alias typed, a row already bound under
  // its own name is still addable under the new destination — which is the whole
  // point of a per-run alias ("run this job with THAT credential, under this name").
  const addable = (known[addKind] ?? []).filter((n) => !present.has(bkey({ kind: addKind, name: n, as: addAs.trim() })));
  const showAdd = editable && canManage;
  // KB — a key reference (declared OR added) makes a run that resolves to the
  // in-app SSH executor REFUSED (422 key_binding_requires_runner); say so where the
  // executor is being chosen rather than letting the ✓ imply delivery. The
  // validator cannot carry this: existence and delivery differ.
  const keyCaveat = executor === "ssh" && refs.some((b) => b.kind === "key");

  if (refs.length === 0 && !showAdd) return null;
  const selectStyle: CSSProperties = {
    fontSize: c.fontSm,
    padding: "4px 6px",
    borderRadius: c.radiusChip,
    background: c.panel2,
    color: c.text,
    border: `1px solid ${c.borderStrong}`,
  };
  return (
    <div style={{ marginTop: 14 }}>
      <label style={{ display: "block", fontSize: c.fontXs, color: c.textSec, marginBottom: 6 }}>
        References ({refs.length})
      </label>
      {declared.length > 0 && (
        <div style={{ display: "flex", flexWrap: "wrap", gap: 6 }}>
          {declared.map((b) => (
            <BindingChip key={bkey(b)} b={b} v={byKey[bkey(b)]} />
          ))}
        </div>
      )}
      {addedList.length > 0 && (
        <div style={{ marginTop: declared.length > 0 ? 8 : 0 }}>
          <div style={{ fontSize: c.fontXs, color: c.textMuted, marginBottom: 2 }}>Added for this run</div>
          <ul style={{ listStyle: "none", margin: 0, padding: 0, display: "flex", flexDirection: "column", gap: 2 }}>
            {addedList.map((b) => (
              <ReferenceRow key={bkey(b)} b={b} v={byKey[bkey(b)]} onRemove={onRemove ? () => onRemove(b) : undefined} />
            ))}
          </ul>
        </div>
      )}
      {showAdd && (
        <div style={{ display: "flex", flexWrap: "wrap", alignItems: "center", gap: 6, marginTop: refs.length > 0 ? 8 : 0 }}>
          <select value={addKind} onChange={(e) => setAddKind(e.target.value as Kind)} aria-label="Reference kind to add" style={selectStyle}>
            {KINDS.map((k) => (
              <option key={k} value={k}>
                {KIND_LABEL[k]}
              </option>
            ))}
          </select>
          {/* Apply-on-change, the JobKeyField shape: adding one stored row to this
              run is a single decision, so a draft + Add button would be ceremony. */}
          <select
            value=""
            disabled={addable.length === 0}
            aria-label={`Add a stored ${KIND_LABEL[addKind].toLowerCase()} to this run`}
            onChange={(e) => {
              const n = e.target.value;
              if (n) onAdd?.({ kind: addKind, name: n, as: addAs.trim() || undefined, reference: derived(addKind, n) });
              setAddAs("");
            }}
            style={{ ...selectStyle, maxWidth: 260 }}
          >
            <option value="">
              {addable.length === 0
                ? (known[addKind] ?? []).length === 0
                  ? `No stored ${KIND_LABEL[addKind].toLowerCase()}s yet`
                  : `All ${KIND_LABEL[addKind].toLowerCase()}s bound`
                : `Add for this run…`}
            </option>
            {addable.map((n) => (
              <option key={n} value={n}>
                {n}
              </option>
            ))}
          </select>
          {/* RA-Q16 — the per-run alias, and the surface the founding ask actually
              describes: "assign the agency credentials you want for THIS run". Typed
              BEFORE picking the row, because the pick is what commits the addition
              (apply-on-change, no Add button) — so the box sits to the right and is
              consumed by the next selection. */}
          <input
            value={addAs}
            placeholder="inject as… (optional)"
            aria-label="Alias — the name this reference is injected under for this run (optional)"
            title="Optional. Injects the chosen row under this name instead of its own, so a shared job body can consume your department's credential. Type it before choosing the row."
            onChange={(e) => setAddAs(e.target.value)}
            style={{ ...selectStyle, fontFamily: c.mono, minWidth: 150 }}
          />
          {addAs.trim() && (
            <span style={{ fontSize: c.fontXs, color: c.textSec, fontFamily: c.mono }}>
              &rarr; {derived(addKind, addAs.trim())}
            </span>
          )}
        </div>
      )}
      {refs.length > 0 && (
        <div style={{ fontSize: c.fontXs, marginTop: 6, color: unresolved > 0 ? c.warning : c.textSec }}>
          {loading
            ? "Checking references…"
            : error
              ? `Could not check references: ${error}`
              : unresolved > 0
                ? `${unresolved} reference${unresolved === 1 ? "" : "s"} will not resolve in ${scope || GLOBAL_SCOPE_LABEL} — the run will fail closed at dispatch.`
                : `All references resolve in ${scope || GLOBAL_SCOPE_LABEL}.`}
        </div>
      )}
      {keyCaveat && (
        <div style={{ fontSize: c.fontXs, color: c.warning, marginTop: 4, maxWidth: "70ch" }}>
          This run will be refused: it resolves to the in-app SSH executor, which cannot deliver SSH keys. Choose the runner executor, or bind the key as a Secret and write the file in the job body.
        </div>
      )}
      {!loading && !error && (
        <div style={{ marginTop: 6 }}>
          <UnresolvedList verdicts={verdicts} />
        </div>
      )}
    </div>
  );
}

// RunReferences is the read-only run-detail display of the reference NAMES a run
// injects (P1.8). Values are never shown. Renders nothing when a run injects none.
export function RunReferences({ traceId }: { traceId: string }) {
  const q = useGet<{ references?: ReferenceBinding[] }>(
    () => api.GET("/runs/{traceId}/references", { params: { path: { traceId } } }),
    [traceId],
  );
  // Distinguish an ERROR (403/500/network) from "injected nothing": both used to
  // render null, so a failed fetch looked identical to a run with no references.
  if (q.error) {
    return (
      <div style={{ fontSize: c.fontXs, color: c.danger }}>
        Could not load this run's injected references: {q.error}
      </div>
    );
  }
  const refs = q.data?.references ?? [];
  if (refs.length === 0) return null;
  return (
    <div>
      <div style={{ fontSize: c.fontXs, fontFamily: c.sansCond, fontWeight: 600, color: c.textMuted, textTransform: "uppercase", letterSpacing: 0.7, marginBottom: 6 }}>
        Injected references ({refs.length})
      </div>
      <div style={{ display: "flex", flexWrap: "wrap", gap: 6 }}>
        {refs.map((b) => (
          <BindingChip key={bkey(b)} b={b} />
        ))}
      </div>
      <div style={{ fontSize: c.fontXs, color: c.textMuted, marginTop: 6 }}>
        Secrets and variables injected into this run at dispatch, from the audit trail. Values are never shown.
      </div>
    </div>
  );
}

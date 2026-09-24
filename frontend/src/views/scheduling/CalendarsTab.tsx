// CAL-18/19 — the Calendars tab under Schedules. A working calendar is a named
// set of wall-clock dates; schedule entries bind them in two polarities, and a
// global calendar applies to every entry at once.
//
// Cronomicon ships NO holiday content in any form (CAL-Q8) — not a seeded row, not
// an importable file. The dates are always the operator's, so a wrong or stale
// list is never one we supplied. That is why the entry paths below (bulk paste,
// CSV, iCal) exist, why the empty state teaches rather than just states an
// absence, and why the expiry badges are load-bearing rather than polish: a skip
// calendar that runs out of days simply stops suppressing, and holiday runs
// resume with nothing else anywhere saying so.
//
// Uploads are read in the BROWSER (FileReader) — never a network fetch of a
// published holiday feed. Several target deployments are air-gapped, where an
// outbound call would fail closed in exactly the environment that needs this most.
import { useEffect, useState } from "react";
import { api, csrfHeader, errMsg, fetchCapabilities } from "../../api/client";
import { c } from "../../theme";
import { DOC_LINKS } from "../../components/docLinks";
import { Btn, SkeletonRows, TableSurface,
  DocLink,
} from "../../components/ui";
import {
  calendarExpiry,
  parseDayLines,
  parseICS,
  useCalendars,
  type Calendar,
  type CalendarDay,
  type ExpiryState,
} from "./calendars";
import { th, td } from "./ui";
import { useNameDisambiguator } from "../../utils/disambiguate";

// ExpiryBadge (CAL-16) — the only warning surface this feature has, since there
// is no server-side Doctor to hang an operator-facing check on.
function ExpiryBadge({ state, daysRemaining }: { state: ExpiryState; daysRemaining?: number | null }) {
  if (!state) return null;
  const expired = state === "expired";
  const tone = expired ? c.danger : c.warning;
  return (
    <span
      title={
        expired
          ? "This calendar has no future days — it no longer suppresses anything. Renew its dates."
          : "This calendar runs out of days soon. Renew its dates before it silently stops suppressing."
      }
      style={{
        fontSize: c.fontXs,
        fontWeight: 600,
        padding: "2px 8px",
        borderRadius: c.radiusPill,
        background: `${tone}18`,
        color: tone,
        border: `1px solid ${tone}40`,
        whiteSpace: "nowrap",
      }}
    >
      {expired ? "expired" : `${daysRemaining}d left`}
    </span>
  );
}

const GlobalBadge = () => (
  <span
    title="Applies to EVERY schedule entry, whether or not the entry names it."
    style={{
      fontSize: c.fontXs,
      fontWeight: 600,
      padding: "2px 8px",
      borderRadius: c.radiusChip,
      background: `${c.info}18`,
      color: c.info,
      border: `1px solid ${c.info}40`,
      whiteSpace: "nowrap",
    }}
  >
    global
  </span>
);

export function CalendarsTab() {
  const [refresh, setRefresh] = useState(0);
  const [canCompose, setCanCompose] = useState(false);
  const [editing, setEditing] = useState<string | null>(null); // calendar name, or "" for a new one
  const { calendars, expiryWarningDays, loading, error } = useCalendars(refresh);

  useEffect(() => {
    fetchCapabilities().then((caps) => setCanCompose(caps.compose));
  }, []);

  const reload = () => setRefresh((n) => n + 1);

  if (editing !== null) {
    return (
      <CalendarEditor
        name={editing}
        onClose={() => setEditing(null)}
        onSaved={() => {
          setEditing(null);
          reload();
        }}
      />
    );
  }

  return (
    <div>
      <div style={{ display: "flex", alignItems: "center", gap: 12, marginBottom: 16, flexWrap: "wrap" }}>
        <div style={{ fontSize: c.fontSm, color: c.textSec, flex: 1, minWidth: 280 }}>
          A working calendar is a named set of dates. Bind it to a schedule so it never runs on those days,
          or so it runs on no others. It can only ever <em>stop</em> a run, never cause one.{" "}
          <DocLink href={DOC_LINKS.calendars}>How the veto is applied</DocLink>
        </div>
        {canCompose && <Btn primary onClick={() => setEditing("")} style={{ padding: "8px 14px", fontSize: c.fontSm }}>+ New calendar</Btn>}
      </div>

      {loading && <div style={{ padding: 16 }}><SkeletonRows rows={4} /></div>}
      {error && <div style={{ color: c.danger }}>Error: {error}</div>}

      {!loading && !error && calendars.length === 0 && (
        // The teaching empty state (CAL-19): this is the only place a first-time
        // operator learns what a calendar is for, and that the dates are theirs.
        <div style={{ border: `1px solid ${c.border}`, borderRadius: c.radiusSurface, background: c.panel, padding: 20, maxWidth: 680 }}>
          <div style={{ fontSize: c.fontSm, color: c.text, fontWeight: 600, marginBottom: 8 }}>No working calendars yet.</div>
          <div style={{ fontSize: c.fontSm, color: c.textSec, lineHeight: 1.6 }}>
            <p style={{ marginTop: 0 }}>
              A calendar is a list of dates — your company holidays, a change freeze, your month-end close.
              Bind one to a schedule as a <strong>skip</strong> calendar and no fire lands on those days; bind it as a{" "}
              <strong>run-day</strong> calendar and the schedule fires on no others. Mark one <strong>global</strong> and
              it applies to every schedule at once, which is how a change freeze is one checkbox instead of N edits.
            </p>
            <p>
              <strong>Cronomicon ships no holiday dates</strong> — every date is yours to author, so no list of ours can be
              silently wrong for your organization. You do not have to type them one at a time: the editor takes a
              pasted block of <code>YYYY-MM-DD,label</code> lines, or a CSV or .ics file.
            </p>
            <p style={{ marginBottom: 0 }}>
              A calendar stops working when it runs out of days — a skip calendar simply stops suppressing, with nothing
              else to tell you. This list flags one that has expired or is close to it, so renewing next year's dates
              stays an annual task with an owner.
            </p>
          </div>
          {canCompose && (
            <div style={{ marginTop: 14 }}>
              <Btn small onClick={() => setEditing("")}>New calendar</Btn>
            </div>
          )}
          {!canCompose && (
            <div style={{ marginTop: 14, fontSize: c.fontSm, color: c.textMuted }}>
              Authoring a calendar requires the <strong>Compose</strong> capability.
            </div>
          )}
        </div>
      )}

      {!loading && !error && calendars.length > 0 && (
        <TableSurface>
          <table style={{ width: "100%", borderCollapse: "collapse", fontSize: c.fontSm }}>
            <thead>
              <tr style={{ color: c.textSec, borderBottom: `1px solid ${c.border}` }}>
                <th style={th()}>Calendar</th>
                <th style={th()}>Days</th>
                <th style={th()}>Coverage through</th>
                <th style={th()}>Bound to</th>
                <th style={th()} />
              </tr>
            </thead>
            <tbody>
              {calendars.map((cal) => {
                const expiry = calendarExpiry(cal, expiryWarningDays);
                const usedBy = cal.usedBy ?? [];
                return (
                  <tr key={cal.name} style={{ borderBottom: `1px solid ${c.border}` }}>
                    <td style={td}>
                      <div style={{ display: "flex", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
                        <span style={{ fontFamily: c.mono, fontWeight: 600, color: c.text }}>{cal.name}</span>
                        {cal.global && <GlobalBadge />}
                        <ExpiryBadge state={expiry} daysRemaining={cal.daysRemaining} />
                      </div>
                      {cal.description && (
                        <div style={{ fontSize: c.fontXs, color: c.textSec, marginTop: 2 }}>{cal.description}</div>
                      )}
                    </td>
                    <td style={{ ...td, color: c.textSec, whiteSpace: "nowrap" }}>{cal.dayCount ?? 0}</td>
                    <td style={{ ...td, color: c.textSec, whiteSpace: "nowrap" }}>{cal.lastDay ?? "—"}</td>
                    <td style={{ ...td, color: usedBy.length ? c.textSec : c.textMuted }}>
                      {usedBy.length === 0
                        ? "not bound"
                        : `${usedBy.length} ${usedBy.length === 1 ? "entry" : "entries"}`}
                    </td>
                    <td style={{ ...td, textAlign: "right" }}>
                      <Btn small onClick={() => setEditing(cal.name ?? "")}>{canCompose ? "Edit" : "View"}</Btn>
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </TableSurface>
      )}
    </div>
  );
}

// ── editor ───────────────────────────────────────────────────────────────────

const NAME_RE = /^[a-z0-9][a-z0-9_-]{0,63}$/;

function CalendarEditor({ name, onClose, onSaved }: { name: string; onClose: () => void; onSaved: () => void }) {
  const isEdit = !!name;
  const [canCompose, setCanCompose] = useState<boolean | null>(null);
  const [slug, setSlug] = useState(name);
  const [description, setDescription] = useState("");
  const [global, setGlobal] = useState(false);
  const [recordSuppressed, setRecordSuppressed] = useState(false);
  const [days, setDays] = useState<CalendarDay[]>([]);
  const [usedBy, setUsedBy] = useState<NonNullable<Calendar["usedBy"]>>([]);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [loadErr, setLoadErr] = useState<string | null>(null);
  const [pendingForce, setPendingForce] = useState(false);

  useEffect(() => {
    fetchCapabilities().then((caps) => setCanCompose(caps.compose));
  }, []);

  useEffect(() => {
    if (!isEdit) return;
    let cancelled = false;
    (async () => {
      const { data, error } = await api.GET("/calendars/{name}", { params: { path: { name } } });
      if (cancelled) return;
      if (error || !data) {
        setLoadErr(`Could not load calendar "${name}".`);
        return;
      }
      const cal = data as Calendar;
      setSlug(cal.name ?? name);
      setDescription(cal.description ?? "");
      setGlobal(!!cal.global);
      setRecordSuppressed(!!cal.recordSuppressed);
      setDays(cal.days ?? []);
      setUsedBy(cal.usedBy ?? []);
    })();
    return () => {
      cancelled = true;
    };
  }, [name, isEdit]);

  const readOnly = canCompose === false;
  // The API refuses global on a calendar bound as a run-day calendar anywhere —
  // mirror it here so the checkbox explains itself rather than 422ing on save.
  const boundAsOnly = usedBy.filter((b) => b.polarity === "only");
  // R2F-3 — this calendar's bindings are the visible set: an owner name shared
  // by two departments' jobs badges here, so the two chips are distinguishable.
  const bindingLabel = useNameDisambiguator(usedBy, (b) => ({
    uid: b.ownerUid,
    name: b.ownerName,
    source: b.ownerSource,
    agencies: b.ownerAgencies,
    group: b.ownerKind,
  }));

  const setDay = (i: number, patch: Partial<CalendarDay>) =>
    setDays((cur) => cur.map((d, j) => (j === i ? { ...d, ...patch } : d)));
  const removeDay = (i: number) => setDays((cur) => cur.filter((_, j) => j !== i));
  // Merge keeps the existing label when a re-import repeats a date, sorts, dedupes.
  const mergeDays = (incoming: CalendarDay[]) =>
    setDays((cur) => {
      const map = new Map(cur.map((d) => [d.day, d]));
      for (const d of incoming) if (!map.has(d.day)) map.set(d.day, d);
      return [...map.values()].sort((a, b) => (a.day < b.day ? -1 : 1));
    });

  async function save() {
    setErr(null);
    const nm = slug.trim();
    if (!NAME_RE.test(nm)) return setErr("Name must be a slug (a-z, 0-9, _-, ≤64 chars).");
    if (global && boundAsOnly.length > 0) {
      return setErr(`"${nm}" is bound as a run-day calendar by ${boundAsOnly.length} entr${boundAsOnly.length === 1 ? "y" : "ies"} — a global calendar may only ever skip.`);
    }
    setBusy(true);
    if (!isEdit) {
      // POST accepts days inline, so a calendar and its dates arrive together.
      const { response, error } = await api.POST("/calendars", {
        params: { header: csrfHeader },
        body: { name: nm, description: description.trim(), global, recordSuppressed, days },
      } as never);
      setBusy(false);
      if (error || !response.ok) return setErr(errMsg(error) || `Create failed (${response.status}).`);
      onSaved();
      return;
    }
    // PUT deliberately does NOT carry days — a metadata edit cannot wipe a year of
    // dates — so the day list is a second call to its own endpoint.
    const meta = await api.PUT("/calendars/{name}", {
      params: { path: { name }, header: csrfHeader },
      body: { description: description.trim(), global, recordSuppressed },
    } as never);
    if (meta.error || !meta.response.ok) {
      setBusy(false);
      return setErr(errMsg(meta.error) || `Save failed (${meta.response.status}).`);
    }
    const dayResp = await api.PUT("/calendars/{name}/days", {
      params: { path: { name }, header: csrfHeader },
      body: { days },
    } as never);
    setBusy(false);
    if (dayResp.error || !dayResp.response.ok) {
      return setErr(errMsg(dayResp.error) || `The calendar's settings were saved, but its days were not (${dayResp.response.status}).`);
    }
    onSaved();
  }

  async function del(force: boolean) {
    setErr(null);
    setBusy(true);
    const { response, error } = await api.DELETE("/calendars/{name}", {
      params: { path: { name }, query: force ? { force: true } : {}, header: csrfHeader },
    } as never);
    setBusy(false);
    if (response.status === 409 && !force) {
      setErr(errMsg(error) || "This calendar is bound by schedule entries.");
      setPendingForce(true);
      return;
    }
    if (error || !response.ok) {
      setPendingForce(false);
      return setErr(errMsg(error) || `Delete failed (${response.status}).`);
    }
    onSaved();
  }

  if (loadErr) {
    return (
      <div>
        <div style={{ color: c.danger, marginBottom: 12 }}>{loadErr}</div>
        <Btn small onClick={onClose}>← Back to calendars</Btn>
      </div>
    );
  }

  return (
    <div style={{ maxWidth: 720, display: "flex", flexDirection: "column", gap: 16 }}>
      <div style={{ fontSize: c.fontSm, color: c.textSec }}>
        {isEdit ? (
          <>Editing the working calendar <span style={{ fontFamily: c.mono }}>{name}</span>.</>
        ) : (
          <>A new working calendar: a named set of dates that schedule entries can be bound to.</>
        )}
      </div>

      <Field label="Name">
        <input
          style={{ ...input(), opacity: isEdit ? 0.6 : 1 }}
          value={slug}
          onChange={(e) => setSlug(e.target.value)}
          placeholder="company-holidays"
          disabled={isEdit || readOnly}
        />
        {isEdit && <div style={{ fontSize: c.fontXs, color: c.textMuted, marginTop: 4 }}>Name is the identity — immutable on edit.</div>}
      </Field>

      <Field label="Description (optional)">
        <input style={input()} value={description} onChange={(e) => setDescription(e.target.value)} disabled={readOnly} placeholder="Observed US federal holidays" />
      </Field>

      <div style={{ display: "flex", flexDirection: "column", gap: 10 }}>
        <label style={{ display: "flex", gap: 8, alignItems: "flex-start", fontSize: c.fontSm, color: c.text }}>
          <input type="checkbox" checked={global} onChange={(e) => setGlobal(e.target.checked)} disabled={readOnly || boundAsOnly.length > 0} style={{ marginTop: 3 }} />
          <span>
            Apply to every schedule (global)
            <div style={{ fontSize: c.fontXs, color: c.textMuted, marginTop: 2 }}>
              Its days are skipped by every entry, whether or not the entry names it — a change freeze as one checkbox
              instead of N schedule edits. Skip only: a global calendar can never be a run-day calendar.
              {boundAsOnly.length > 0 && (
                <span style={{ color: c.warning }}>
                  {" "}Unavailable — {boundAsOnly.length} entr{boundAsOnly.length === 1 ? "y binds" : "ies bind"} this as a run-day calendar.
                </span>
              )}
            </div>
          </span>
        </label>
        <label style={{ display: "flex", gap: 8, alignItems: "flex-start", fontSize: c.fontSm, color: c.text }}>
          <input type="checkbox" checked={recordSuppressed} onChange={(e) => setRecordSuppressed(e.target.checked)} disabled={readOnly} style={{ marginTop: 3 }} />
          <span>
            Record run-day (only) suppressions in History
            <div style={{ fontSize: c.fontXs, color: c.textMuted, marginTop: 2 }}>
              Skip-mode suppressions are always recorded — that is the compliance question. Leave this off unless you
              want an audit row every time a run-day calendar holds a fire back, which for a weekdays-only calendar is
              every weekend.
            </div>
          </span>
        </label>
      </div>

      <DayEditor days={days} readOnly={readOnly} onSetDay={setDay} onRemoveDay={removeDay} onMerge={mergeDays} onClear={() => setDays([])} />

      {usedBy.length > 0 && (
        <div>
          <div style={sectionLabel()}>Bound by {usedBy.length} schedule {usedBy.length === 1 ? "entry" : "entries"}</div>
          <div style={{ display: "flex", flexWrap: "wrap", gap: 6 }}>
            {usedBy.map((b, i) => (
              <span
                key={`${b.ownerKind}:${b.ownerName}:${b.scheduleName}:${i}`}
                style={{ fontFamily: c.mono, fontSize: c.fontXs, padding: "3px 8px", borderRadius: c.radiusChip, background: c.panel2, border: `1px solid ${c.border}`, color: c.textSec }}
              >
                {b.ownerKind}: {bindingLabel(b)} · {b.scheduleName} ({b.polarity})
              </span>
            ))}
          </div>
        </div>
      )}

      {err && <div style={{ color: pendingForce ? c.warning : c.danger, fontSize: c.fontSm }}>{err}</div>}

      <div style={{ display: "flex", gap: 10, flexWrap: "wrap", alignItems: "center" }}>
        {!readOnly && (
          <button onClick={save} disabled={busy} style={btn()}>
            {busy ? "Saving…" : isEdit ? "Save calendar" : "Create calendar"}
          </button>
        )}
        <button onClick={onClose} disabled={busy} style={btnGhost()}>
          {readOnly ? "← Back to calendars" : "Cancel"}
        </button>
        {isEdit && !readOnly && (
          <span style={{ marginLeft: "auto", display: "inline-flex", gap: 8, alignItems: "center" }}>
            {pendingForce ? (
              <>
                <button onClick={() => del(true)} disabled={busy} style={dangerBtn()}>
                  Delete anyway ({usedBy.length} binding{usedBy.length === 1 ? "" : "s"})
                </button>
                <button onClick={() => { setPendingForce(false); setErr(null); }} disabled={busy} style={btnGhost()}>Cancel</button>
              </>
            ) : (
              <button onClick={() => del(false)} disabled={busy} style={dangerBtn()}>Delete calendar</button>
            )}
          </span>
        )}
      </div>
      {pendingForce && (
        <div style={{ fontSize: c.fontXs, color: c.warning, maxWidth: 620 }}>
          Forcing leaves the bindings in place as dangling names. A dangling <strong>skip</strong> binding lets the
          schedule fire (a visible, correctable policy violation); a dangling <strong>run-day</strong> binding means it
          never fires again — silence. Both are flagged in the schedule editors.
        </div>
      )}
    </div>
  );
}

// ── day list + entry paths ───────────────────────────────────────────────────

function DayEditor({
  days,
  readOnly,
  onSetDay,
  onRemoveDay,
  onMerge,
  onClear,
}: {
  days: CalendarDay[];
  readOnly: boolean;
  onSetDay: (i: number, patch: Partial<CalendarDay>) => void;
  onRemoveDay: (i: number) => void;
  onMerge: (incoming: CalendarDay[]) => void;
  onClear: () => void;
}) {
  const [paste, setPaste] = useState("");
  const [pasteOpen, setPasteOpen] = useState(false);
  const [notes, setNotes] = useState<string[]>([]);
  const today = new Date().toISOString().slice(0, 10);

  const applyParsed = (parsed: { days: CalendarDay[]; errors: string[] }, source: string) => {
    if (parsed.days.length > 0) onMerge(parsed.days);
    const msgs: string[] = [];
    msgs.push(`${source}: added ${parsed.days.length} date${parsed.days.length === 1 ? "" : "s"}.`);
    msgs.push(...parsed.errors.slice(0, 5));
    if (parsed.errors.length > 5) msgs.push(`…and ${parsed.errors.length - 5} more problems.`);
    setNotes(msgs);
  };

  const onFile = (file: File) => {
    const reader = new FileReader();
    reader.onload = () => {
      const text = String(reader.result ?? "");
      const parsed = /\.ics$/i.test(file.name) || /BEGIN:VCALENDAR/i.test(text) ? parseICS(text) : parseDayLines(text);
      applyParsed(parsed, file.name);
    };
    reader.onerror = () => setNotes([`Could not read ${file.name}.`]);
    reader.readAsText(file);
  };

  return (
    <div>
      <div style={sectionLabel()}>Days ({days.length})</div>
      {!readOnly && (
        <div style={{ display: "flex", gap: 8, flexWrap: "wrap", alignItems: "center", marginBottom: 10 }}>
          <Btn small onClick={() => onMerge([{ day: today }])}>+ Add a day</Btn>
          <Btn small onClick={() => setPasteOpen((o) => !o)}>{pasteOpen ? "Hide bulk paste" : "Paste a list"}</Btn>
          <label style={{ ...chipLabel(), cursor: "pointer" }}>
            Upload CSV or .ics
            <input
              type="file"
              accept=".csv,.txt,.ics,text/csv,text/calendar,text/plain"
              style={{ display: "none" }}
              onChange={(e) => {
                const f = e.target.files?.[0];
                if (f) onFile(f);
                e.target.value = "";
              }}
            />
          </label>
          {days.length > 0 && <Btn small onClick={onClear}>Clear all</Btn>}
        </div>
      )}

      {pasteOpen && !readOnly && (
        <div style={{ marginBottom: 10 }}>
          <textarea
            value={paste}
            onChange={(e) => setPaste(e.target.value)}
            rows={6}
            placeholder={"2026-01-01,New Year's Day\n2026-07-03,Independence Day (observed)\n2026-12-25,Christmas Day"}
            style={{ ...input(), fontFamily: c.mono, resize: "vertical" }}
          />
          <div style={{ display: "flex", gap: 8, alignItems: "center", marginTop: 6 }}>
            <Btn
              small
              onClick={() => {
                applyParsed(parseDayLines(paste), "Pasted list");
                setPaste("");
              }}
            >
              Add these dates
            </Btn>
            <span style={{ fontSize: c.fontXs, color: c.textMuted }}>
              One <code>YYYY-MM-DD</code> per line, with an optional <code>,label</code>. The label is what the History
              row says when a run is suppressed.
            </span>
          </div>
        </div>
      )}

      {notes.length > 0 && (
        <div style={{ fontSize: c.fontXs, color: c.textSec, background: c.panel2, border: `1px solid ${c.border}`, borderRadius: c.radiusSurface, padding: "8px 10px", marginBottom: 10 }}>
          {notes.map((n, i) => (
            <div key={i} style={{ color: i === 0 ? c.textSec : c.warning }}>{n}</div>
          ))}
        </div>
      )}

      {days.length === 0 ? (
        <div style={{ fontSize: c.fontSm, color: c.textMuted }}>
          No dates yet. A calendar with no days suppresses nothing — paste a list to get a year in at once.
        </div>
      ) : (
        <div style={{ display: "flex", flexDirection: "column", gap: 6, maxHeight: 360, overflowY: "auto" }}>
          {days.map((d, i) => {
            const past = d.day < today;
            return (
              <div key={`${d.day}:${i}`} style={{ display: "flex", gap: 8, alignItems: "center" }}>
                <input
                  type="date"
                  value={d.day}
                  disabled={readOnly}
                  onChange={(e) => onSetDay(i, { day: e.target.value })}
                  style={{ ...input(), width: 170, fontFamily: c.mono, opacity: past ? 0.6 : 1 }}
                />
                <input
                  value={d.label ?? ""}
                  disabled={readOnly}
                  placeholder="label (shown as the suppression reason)"
                  onChange={(e) => onSetDay(i, { label: e.target.value })}
                  style={{ ...input(), flex: 1 }}
                />
                {!readOnly && <Btn small onClick={() => onRemoveDay(i)} title="Remove this day">✕</Btn>}
              </div>
            );
          })}
        </div>
      )}
    </div>
  );
}

// ── local styles (functions, never module consts: a module-level style object
// freezes the load-time palette and breaks the theme toggle — see theme.ts) ──

function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div>
      <div style={sectionLabel()}>{label}</div>
      {children}
    </div>
  );
}

const sectionLabel = (): React.CSSProperties => ({
  fontSize: c.fontXs,
  fontFamily: c.sansCond,
  fontWeight: 600,
  color: c.textMuted,
  textTransform: "uppercase",
  letterSpacing: 0.7,
  marginBottom: 6,
});
const chipLabel = (): React.CSSProperties => ({
  padding: "5px 12px",
  fontSize: c.fontSm,
  fontWeight: 500,
  border: `1px solid ${c.border}`,
  borderRadius: c.radiusChip,
  background: "transparent",
  color: c.text,
  fontFamily: "inherit",
});
const input = (): React.CSSProperties => ({
  width: "100%",
  boxSizing: "border-box",
  padding: "8px 10px",
  background: c.panel2,
  border: `1px solid ${c.borderStrong}`,
  borderRadius: c.radiusChip,
  color: c.text,
  fontSize: c.fontSm,
  fontFamily: c.sans,
});
const btn = (): React.CSSProperties => ({
  padding: "9px 18px",
  background: c.primary,
  color: c.onSolid,
  border: "none",
  borderRadius: c.radiusChip,
  fontSize: c.fontSm,
  fontWeight: 600,
  cursor: "pointer",
});
const btnGhost = (): React.CSSProperties => ({
  padding: "9px 18px",
  background: "transparent",
  color: c.text,
  border: `1px solid ${c.border}`,
  borderRadius: c.radiusChip,
  fontSize: c.fontSm,
  cursor: "pointer",
});
const dangerBtn = (): React.CSSProperties => ({
  padding: "9px 18px",
  background: c.danger,
  color: c.onSolid,
  border: "none",
  borderRadius: c.radiusChip,
  fontSize: c.fontSm,
  fontWeight: 600,
  cursor: "pointer",
});

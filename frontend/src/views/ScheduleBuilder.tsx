import { useEffect, useMemo, useState } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";
import { api, fetchCapabilities } from "../api/client";
import { c } from "../theme";
import type { components } from "../api/schema";
import {
  CRON_PRESETS,
  cronPreview,
  describeSpec,
  modeOf,
  nextSpecTimes,
  validateSpec,
  type ActivationWindow,
  type ScheduleSpec,
} from "./scheduling/cron";
import { WindowEditor } from "./scheduling/WindowEditor";
import { CalendarPicker, EMPTY_BINDING, bindingError, type CalendarBinding } from "./scheduling/CalendarPicker";
import { IntervalField, ModePicker } from "./scheduling/ModePicker";
import { EnvRowsEditor } from "../components/ui";

type Schedule = components["schemas"]["Schedule"];
type EnvRow = { key: string; value: string };

// In-app first-class Schedule authoring (schedule-builder.md) — the schedule analog
// of the Compose (Job) and Workflow Editor surfaces. Creates/edits an amadeus-source
// Schedule (name + cron + optional env + description); an edit propagates the new
// cron to every job/workflow that referenced it. Gated on the Compose capability
// (Admin-only in v20). Reached from the Schedules page (no top-nav item). ?name=
// selects edit mode.
export function ScheduleBuilder() {
  const navigate = useNavigate();
  const [params] = useSearchParams();
  const editName = params.get("name");
  const isEdit = !!editName;

  const [canCompose, setCanCompose] = useState<boolean | null>(null);
  useEffect(() => {
    fetchCapabilities().then((caps) => setCanCompose(caps.compose));
  }, []);

  const [name, setName] = useState("");
  const [cron, setCron] = useState("");
  const [description, setDescription] = useState("");
  const [env, setEnv] = useState<EnvRow[]>([]);
  // Activation window (AW-10): optional deferred start / expiry for this schedule.
  const [window, setWindow] = useState<ActivationWindow>({});
  // Phase 2 — the interval mode's duration ("7d"); empty in cron/once mode.
  const [interval, setInterval] = useState<string | null>(null);
  // CAL-13 — working-calendar bindings, both polarities. Read back on edit and
  // always re-sent: the update body is a full replace, so omitting them would
  // silently clear the policy on any unrelated save.
  const [calendarBinding, setCalendarBinding] = useState<CalendarBinding>(EMPTY_BINDING);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [loadErr, setLoadErr] = useState<string | null>(null);

  // Edit mode: load the existing amadeus schedule into the form.
  useEffect(() => {
    if (!editName) return;
    let cancelled = false;
    (async () => {
      const { data, error } = await api.GET("/schedule-defs/{name}", {
        params: { path: { name: editName }, query: { source: "amadeus" } },
      });
      if (cancelled) return;
      if (error || !data) {
        setLoadErr(`Could not load schedule "${editName}".`);
        return;
      }
      const s = data as Schedule;
      setName(s.name ?? editName);
      setCron(s.cron ?? "");
      setDescription(s.description ?? "");
      const e = s.env ?? {};
      setEnv(Object.keys(e).map((k) => ({ key: k, value: e[k] })));
      setWindow({ startAt: s.startAt ?? null, endAt: s.endAt ?? null });
      setInterval(s.interval ?? null);
      setCalendarBinding({ skipCalendars: s.skipCalendars ?? [], onlyCalendars: s.onlyCalendars ?? [] });
    })();
    return () => {
      cancelled = true;
    };
  }, [editName]);

  // The shared cron toolkit (cron.ts) speaks the classic 5-field subset; the backend
  // also accepts 6-field (sec min hr dom mon dow). cronPreview projects a 6-field
  // expression to its 5-field form for the live preview (seconds at minute resolution)
  // so a valid 6-field cron — what the seeds and most schedules use — still previews.
  // The spec is the single source of truth for mode, validation and preview, so
  // the form can never disagree with what the backend will do on save.
  const spec: ScheduleSpec = useMemo(
    () => ({ cron, interval, startAt: window.startAt, endAt: window.endAt }),
    [cron, interval, window],
  );
  const mode = modeOf(spec);
  // The preview is clamped to the activation window (AW-12) so a deferred
  // schedule previews its real first fire rather than one it will not perform.
  const preview = useMemo(() => cronPreview(cron, 3, window), [cron, window]);
  const nexts = useMemo(() => nextSpecTimes(spec), [spec]);

  async function submit() {
    setErr(null);
    if (!isEdit && !name.trim()) return setErr("Name is required.");
    const specErr = validateSpec(spec);
    if (specErr) return setErr(specErr);
    const calErr = bindingError(calendarBinding);
    if (calErr) return setErr(calErr);
    const envMap: Record<string, string> = {};
    for (const row of env) {
      const k = row.key.trim();
      if (k) envMap[k] = row.value;
    }
    const body: Record<string, unknown> = {
      // Exactly one of cron/interval carries the rule; the other is cleared so
      // the backend's exclusivity check sees the mode the form is showing.
      cron: mode === "cron" ? cron.trim() : "",
      interval: mode === "interval" ? (interval ?? "").trim() : null,
      description: description.trim(),
      env: envMap,
      startAt: window.startAt ?? null,
      endAt: window.endAt ?? null,
      skipCalendars: calendarBinding.skipCalendars,
      onlyCalendars: calendarBinding.onlyCalendars,
    };
    setBusy(true);
    let resp: Response;
    let error: unknown;
    if (isEdit) {
      ({ response: resp, error } = await api.PUT("/schedule-defs/{name}", {
        params: { path: { name: editName! } },
        body: body as never,
      }));
    } else {
      body.name = name.trim();
      ({ response: resp, error } = await api.POST("/schedule-defs", { body: body as never }));
    }
    setBusy(false);
    if (error || !resp.ok) {
      setErr(errMessage(error) || `${isEdit ? "Save" : "Create"} failed (${resp.status}).`);
      return;
    }
    navigate("/schedules");
  }

  if (canCompose === false) {
    return (
      <div style={{ color: c.textSec, maxWidth: 560 }}>
        In-app schedule authoring requires the <strong>Compose</strong> capability (Admin-only in v20).
        Git-defined schedules are authored through the GitLab publish flow.
      </div>
    );
  }

  if (loadErr) {
    return <div style={{ color: c.danger, maxWidth: 560 }}>{loadErr}</div>;
  }

  return (
    // Centered, carded form column (VC.12) — matches the Settings container
    // language instead of a hard-left column against bare page background.
    <div
      style={{
        maxWidth: 640,
        margin: "0 auto",
        width: "100%",
        display: "flex",
        flexDirection: "column",
        gap: 16,
        background: c.panel,
        border: `1px solid ${c.border}`,
        borderRadius: c.radiusSurface,
        padding: 24,
      }}
    >
      <div style={{ color: c.textSec, fontSize: c.fontSm }}>
        {isEdit ? (
          <>
            Editing the <strong>amadeus-source</strong> schedule <span style={{ fontFamily: c.mono }}>{editName}</span>.
            Saving propagates the new cron to every job and workflow that references it.
          </>
        ) : (
          <>
            Author an <strong>amadeus-source</strong> schedule: a reusable named cron that jobs and workflows
            can bind via <em>scheduleRefs</em> — no Git round-trip.
          </>
        )}
      </div>

      <Field label="Name">
        <input
          style={{ ...input(), opacity: isEdit ? 0.6 : 1 }}
          value={name}
          onChange={(e) => setName(e.target.value)}
          placeholder="nightly-window"
          disabled={isEdit}
        />
        {isEdit && <div style={{ fontSize: c.fontXs, color: c.textMuted, marginTop: 4 }}>Name is the identity — immutable on edit.</div>}
      </Field>

      <Field label="How it runs">
        <ModePicker
          value={spec}
          onChange={(patch) => {
            if (patch.cron !== undefined) setCron(patch.cron ?? "");
            if (patch.interval !== undefined) setInterval(patch.interval ?? null);
          }}
        />
      </Field>

      {mode === "cron" && (
        <Field label="Cron">
          <div style={{ display: "flex", gap: 8 }}>
            <input style={{ ...input(), flex: 1, fontFamily: c.mono }} value={cron} onChange={(e) => setCron(e.target.value)} placeholder="0 2 * * *" />
            <select
              style={{ ...input(), width: 180 }}
              value=""
              onChange={(e) => {
                if (e.target.value) setCron(e.target.value);
              }}
            >
              <option value="">presets…</option>
              {CRON_PRESETS.map((p) => (
                <option key={p.expr} value={p.expr}>
                  {p.label}
                </option>
              ))}
            </select>
          </div>
          {preview.raw && !preview.valid && (
            <div style={{ marginTop: 6, fontSize: c.fontSm, color: c.textMuted }}>
              Can’t preview this expression — the backend validates 5- and 6-field crons on save.
            </div>
          )}
          {preview.raw && preview.valid && preview.sixField && (
            <div style={{ marginTop: 6, fontSize: c.fontSm, color: c.textMuted }}>seconds shown at minute resolution</div>
          )}
        </Field>
      )}

      {mode === "interval" && (
        <Field label="Interval">
          <IntervalField value={interval} onChange={setInterval} hasAnchor={!!window.startAt} />
        </Field>
      )}

      {mode === "once" && (
        <div style={{ fontSize: c.fontSm, color: c.textSec }}>
          This schedule fires a single time, at the <strong>start</strong> date below, and never again.
          It stays listed afterwards so you can see that it ran.
        </div>
      )}

      <div style={{ fontSize: c.fontSm }}>
        <div style={{ color: c.textSec }}>{describeSpec(spec)}</div>
        {nexts.length > 0 && (
          <div style={{ color: c.textMuted, marginTop: 2 }}>
            Next fires (app zone): {nexts.join(" · ")}
            {preview.nextsTruncated ? " · + more beyond the preview horizon" : ""}
          </div>
        )}
        {nexts.length === 0 && preview.nextsTruncated && (
          <div style={{ color: c.textMuted, marginTop: 2 }}>
            No fire found within the preview horizon — the schedule itself is unaffected.
          </div>
        )}
      </div>

      <Field label="Active period (optional)">
        <WindowEditor value={window} onChange={setWindow} startIsFireInstant={mode === "once"} />
      </Field>

      <Field label="Working calendars (optional)">
        <CalendarPicker value={calendarBinding} onChange={setCalendarBinding} spec={spec} />
      </Field>

      <Field label="Env (optional)">
        <div style={{ fontSize: c.fontXs, color: c.textMuted, marginBottom: 8 }}>
          Plaintext values injected when this schedule fires — not redacted. Real secrets belong to the scope/secrets system.
        </div>
        <EnvRowsEditor rows={env} onChange={setEnv} addLabel="+ Add env var" />
      </Field>

      <Field label="Description (optional)">
        <input style={input()} value={description} onChange={(e) => setDescription(e.target.value)} />
      </Field>

      {err && <div style={{ color: c.danger, fontSize: c.fontSm }}>{err}</div>}

      <div style={{ display: "flex", gap: 10 }}>
        <button onClick={submit} disabled={busy} style={btn()}>
          {busy ? "Saving…" : isEdit ? "Save schedule" : "Create schedule"}
        </button>
        <button onClick={() => navigate("/schedules")} disabled={busy} style={btnGhost()}>
          Cancel
        </button>
      </div>
    </div>
  );
}

function errMessage(error: unknown): string {
  if (error && typeof error === "object" && "message" in error) {
    return String((error as { message?: unknown }).message ?? "");
  }
  return "";
}

function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div>
      <div style={{ fontSize: c.fontXs, fontFamily: c.sansCond, fontWeight: 600, color: c.textMuted, textTransform: "uppercase", letterSpacing: 0.7, marginBottom: 4 }}>
        {label}
      </div>
      {children}
    </div>
  );
}

// Functions, not module-level consts, so a theme toggle re-reads the active
// palette. Capturing c.* tokens at module load freezes the load-time theme
// (default: dark). See theme.ts: "never capture token values in module-level
// constants."
const input = (): React.CSSProperties => ({
  width: "100%",
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

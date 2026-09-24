import { useState } from "react";
import { api, csrfHeader } from "../../api/client";
import type { components } from "../../api/schema";
import { c } from "../../theme";
import { Btn, Card, Chip, SaveBtn, SettingRow, Status, errMsg, inputStyle, lblStyle, useSettingForm } from "./ui";

type AuditComplianceSettings = components["schemas"]["AuditComplianceSettings"];
type RetentionDays = NonNullable<AuditComplianceSettings["retentionDays"]>;

// The A4 defaults, mirroring defaultAuditCompliance() in the backend's
// internal/settings/audit_compliance.go. Needed as a concrete object because the
// generated type makes `retentionDays` optional while every field inside it is
// required — so there is nothing safe to spread until the GET has resolved.
const DEFAULT_DAYS: RetentionDays = {
  runs: 90,
  activity: 90,
  workflowRuns: 90,
  changeLog: 365,
  schedulePushes: 365,
  logFiles: 90,
  auditLogFiles: 730,
  recycleBin: 30,
  definitionRevisions: 365,
  runnerPlacementHistory: 30,
  archivedLogFiles: 0,
};

// Ordered runs → audit → disk, which is roughly increasing blast radius.
// VU-16: each hint says what the operator loses when the window closes, and
// where. Two corrections rather than restyling: Change Log no longer claims to
// be the longest window (Audit Stream Files is, 730 vs 365) and now names the
// sign-in events that share its knob — db/retention.go prunes auth_events on
// ChangeLogDays deliberately, and nothing said so.
const RETENTION_FIELDS: { k: keyof RetentionDays; label: string; hint: string }[] = [
  { k: "runs", label: "Run History", hint: "How long a job run stays visible in History." },
  { k: "activity", label: "Activity Log", hint: "How long an entry stays in the Activity feed." },
  { k: "workflowRuns", label: "Workflow Runs", hint: "How long a workflow's own run record is kept. The job runs inside it follow Run History." },
  { k: "changeLog", label: "Change Log", hint: "Who changed what, and when. Sign-in events are kept for the same window." },
  { k: "schedulePushes", label: "Schedule Pushes", hint: "How long the record of each schedule published to GitLab is kept." },
  {
    k: "logFiles",
    label: "Run Log Files",
    hint: "On-disk run output under the configured log path. Deleted by file age, so logs outlive their run record only until this window closes.",
  },
  {
    k: "auditLogFiles",
    label: "Audit Stream Files",
    hint: "The audit.log export and its daily generations. Keep this the longest — it is what preserves a tamper-evident trail after the rows above have been pruned.",
  },
  {
    k: "recycleBin",
    label: "Recycle Bin",
    hint: "How long a deleted in-app Job, Workflow or Schedule stays restorable before it is permanently purged. The only window here whose expiry destroys a definition rather than a record of one — and a binned definition keeps its name until then, so a replacement cannot be created.",
  },
  {
    k: "definitionRevisions",
    label: "Definition History",
    hint: "How long the snapshot history of in-app definitions is kept. Git-source definitions are unaffected — their history is Git's.",
  },
  {
    k: "runnerPlacementHistory",
    label: "Runner Placement History",
    hint: "How long a removed runner's agencies and tags stay available to offer back if it re-registers. Counted from when the runner was removed, not from when it went offline — so a runner reaped after the 14-day offline window is still restorable for this long after that.",
  },
  {
    k: "archivedLogFiles",
    label: "Archived Run Logs (S3)",
    hint: "How long a run log stays in the S3 archive after it was copied there. 0 keeps it forever, and Cronomicon never deletes from the bucket — use an S3 lifecycle rule if you want expiry without granting delete. A window here makes the archive sync delete expired logs itself, which needs s3:DeleteObject on the bucket. Local run log files follow Run Log Files, except that a log the sync has not copied yet is never deleted locally while the S3 backend is on.",
  },
];

// ── Data Retention ───────────────────────────────────────────────────────────
// These knobs were stored but read by nothing until the sweeper was wired to
// them (LU-2). That is why the card states plainly when a change takes effect:
// a retention control that silently does nothing is worse than no control.
function RetentionCard() {
  const s = useSettingForm<AuditComplianceSettings>(
    () => api.GET("/settings/audit-compliance"),
    (body) => api.PUT("/settings/audit-compliance", { params: { header: csrfHeader }, body }),
  );
  const { form, patch } = s;
  const days = form?.retentionDays ?? DEFAULT_DAYS;

  const setDay = (k: keyof RetentionDays, raw: string) => {
    const n = Number(raw);
    // Clamp at 0 rather than rejecting. The server treats a negative window as
    // "keep forever" (its prune gates on days <= 0), which is the opposite of
    // what someone typing -1 expects, so it rejects negatives outright — this
    // keeps the form from posting a value it already knows will 422.
    patch({ retentionDays: { ...days, [k]: Number.isFinite(n) && n > 0 ? Math.floor(n) : 0 } });
  };

  return (
    <Card title="Data Retention" action={<SaveBtn onClick={() => s.save()} saving={s.saving} saved={s.saved} disabled={!form} />}>
      <Status loading={s.loading} error={s.error} saveErr={s.saveErr} />
      {form && (
        <div style={{ fontSize: c.fontSm }}>
          <div style={{ fontSize: c.fontSm, color: c.textSec, marginBottom: 6, lineHeight: 1.6 }}>
            How long each kind of record is kept. <strong>0 means keep forever.</strong> Pruning runs during the nightly
            sweep, so a change here takes effect on the next sweep — no restart needed.
          </div>
          {RETENTION_FIELDS.map((f, i) => (
            <SettingRow key={f.k} label={f.label} hint={f.hint} last={i === RETENTION_FIELDS.length - 1}>
              <div style={{ display: "flex", alignItems: "center", gap: 6 }}>
                <input
                  type="number"
                  min={0}
                  aria-label={f.label}
                  value={days[f.k]}
                  onChange={(e) => setDay(f.k, e.target.value)}
                  // Left-aligned deliberately: Firefox always shows the native
                  // spin buttons at the right edge, and a right-aligned value
                  // sits flush against them (no CSS hook can open that gap).
                  style={{ ...inputStyle(), width: 90 }}
                />
                <span style={{ color: c.textSec, fontSize: c.fontSm, width: 34 }}>{days[f.k] === 0 ? "∞" : "days"}</span>
              </div>
            </SettingRow>
          ))}
        </div>
      )}
    </Card>
  );
}

type EventType = "executions" | "activity" | "configChanges" | "authEvents" | "schedulePushes";

const EVENT_TYPES: { k: EventType; l: string }[] = [
  { k: "executions", l: "Executions" },
  { k: "activity", l: "Activity" },
  { k: "configChanges", l: "Config Changes" },
  { k: "authEvents", l: "Auth Events" },
  { k: "schedulePushes", l: "Schedule Pushes" },
];

const isoDate = (d: Date) => d.toISOString().slice(0, 10);

export function AuditComplianceSection() {
  const [from, setFrom] = useState(isoDate(new Date(Date.now() - 30 * 86400000)));
  const [to, setTo] = useState(isoDate(new Date()));
  const [eventTypes, setEventTypes] = useState<EventType[]>(EVENT_TYPES.map((e) => e.k));
  const [format, setFormat] = useState<"csv" | "json">("csv");
  const [exporting, setExporting] = useState(false);
  const [exportErr, setExportErr] = useState<string | null>(null);
  const [exportedAt, setExportedAt] = useState<string | null>(null);

  const toggleType = (k: EventType) =>
    setEventTypes((ts) => (ts.includes(k) ? ts.filter((x) => x !== k) : [...ts, k]));

  async function doExport() {
    if (eventTypes.length === 0) {
      setExportErr("Pick at least one event type.");
      return;
    }
    setExporting(true);
    setExportErr(null);
    setExportedAt(null);
    const { data, error } = await api.GET("/audit/export", {
      params: { query: { from, to, eventTypes, format } },
      parseAs: "blob",
      // spec: eventTypes is comma-separated (explode: false)
      querySerializer: { array: { style: "form", explode: false } },
    });
    setExporting(false);
    if (error || !data) {
      setExportErr(error ? errMsg(error) : "export failed");
      return;
    }
    const blob = data as unknown as Blob;
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = `audit-export-${from}-to-${to}.${format}`;
    document.body.appendChild(a);
    a.click();
    a.remove();
    URL.revokeObjectURL(url);
    setExportedAt(new Date().toLocaleTimeString());
  }

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 16 }}>
      <RetentionCard />
      <Card title="Audit Export">
        <div style={{ fontSize: c.fontSm, color: c.textSec, marginBottom: 14, lineHeight: 1.6 }}>
          Export the audit trail for a date range. The file downloads as soon as it is ready.
        </div>

        <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: 12, marginBottom: 14 }}>
          <div>
            <div style={lblStyle()}>From</div>
            <input type="date" value={from} onChange={(e) => setFrom(e.target.value)} style={{ ...inputStyle(), width: "100%" }} />
          </div>
          <div>
            <div style={lblStyle()}>To</div>
            <input type="date" value={to} onChange={(e) => setTo(e.target.value)} style={{ ...inputStyle(), width: "100%" }} />
          </div>
        </div>

        <div style={{ marginBottom: 14 }}>
          <div style={lblStyle()}>Event Types</div>
          <div style={{ display: "flex", gap: 8, flexWrap: "wrap" }}>
            {EVENT_TYPES.map(({ k, l }) => (
              <Chip key={k} label={l} on={eventTypes.includes(k)} onClick={() => toggleType(k)} />
            ))}
          </div>
        </div>

        <div style={{ display: "flex", alignItems: "center", gap: 10, flexWrap: "wrap" }}>
          {/* A segmented format control, not a status — chip shape (B-3/VU-17). */}
          <div style={{ display: "flex", borderRadius: c.radiusChip, overflow: "hidden", border: `1px solid ${c.border}` }}>
            {(["csv", "json"] as const).map((fmt, i) => (
              <div
                key={fmt}
                onClick={() => setFormat(fmt)}
                style={{
                  padding: "6px 14px",
                  fontSize: c.fontXs,
                  cursor: "pointer",
                  userSelect: "none",
                  background: format === fmt ? c.primary : "transparent",
                  color: format === fmt ? c.onSolid : c.textSec,
                  fontWeight: format === fmt ? 600 : 400,
                  borderRight: i === 0 ? `1px solid ${c.border}` : "none",
                  textTransform: "uppercase",
                  letterSpacing: 0.4,
                }}
              >
                {fmt}
              </div>
            ))}
          </div>
          <Btn primary onClick={doExport} disabled={exporting}>
            {exporting ? "Preparing…" : "Export Audit Trail"}
          </Btn>
          {exportedAt && <span style={{ fontSize: c.fontSm, color: c.success, fontWeight: 600 }}>✓ Export downloaded at {exportedAt}</span>}
          {exportErr && <span style={{ fontSize: c.fontSm, color: c.danger }}>Export failed: {exportErr}</span>}
        </div>
      </Card>
    </div>
  );
}

import { Fragment, useEffect, useState } from "react";
import { api } from "../../api/client";
import type { components } from "../../api/schema";
import { useGet, rows, useTableSort } from "../../hooks";
import { type SortColumn } from "../../utils/sort";
import { c } from "../../theme";
import { Btn, Card, Chip, csrfHeader, errMsg, fmtDateTime, inputStyle, lblStyle, tdStyle, thStyle } from "./ui";
import { ConfirmDialog, InlineLoading, Rule, SkeletonRows, SortableLabel, statusLabel } from "../../components/ui";

type NotificationConfig = components["schemas"]["NotificationConfig"];
type AppriseTarget = NonNullable<NonNullable<NotificationConfig["apprise"]>["targets"]>[number];
type AlertRule = components["schemas"]["AlertRule"];
type AlertRuleInput = components["schemas"]["AlertRuleInput"];
type AlertChannel = AlertRuleInput["channels"][number];
type TestReport = components["schemas"]["NotificationTestReport"];

export function NotificationsSection() {
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 16 }}>
      <RunNotificationsCard />
      <AlertRulesCard />
    </div>
  );
}

// K-2 (VF-16) — when each transport last delivered.
//
// This replaced a per-destination "last fired" column in the deleted Alert
// Destinations table, which was stamped on every enabled destination whose type
// matched the transport that sent — so it could only ever mean what this says
// explicitly. Two transports, two facts, no claim about routing.
//
// "Never" is deliberately its own state rather than a blank: an operator
// checking why an alert did not arrive needs to tell "nothing has ever sent"
// from "the last send failed".
function LastSentLine({ lastSent }: { lastSent: NotificationConfig["lastSent"] }) {
  const entries = (
    [
      ["Email", lastSent?.email],
      ["Apprise", lastSent?.apprise],
    ] as const
  ).filter(([, v]) => v?.at);
  if (entries.length === 0) return null;
  return (
    <div style={{ display: "flex", gap: 16, flexWrap: "wrap", marginBottom: 14, fontSize: c.fontSm }}>
      {entries.map(([label, v]) => (
        <span key={label} style={{ color: c.textSec }}>
          <strong style={{ color: c.text }}>{label}</strong>
          {" — "}
          {v!.status === "error" ? (
            <span style={{ color: c.danger }}>last send failed</span>
          ) : (
            <span style={{ color: c.success }}>sent</span>
          )}{" "}
          {fmtDateTime(v!.at)}
        </span>
      ))}
    </div>
  );
}

// K-5 — the outcome of a test send, one line per transport.
//
// Three states, not two. "Skipped" is the common one on a half-configured
// install and carries the reason, so the line tells the operator what to fix
// rather than only that something did not happen.
function TestReportLines({ report }: { report: TestReport | null }) {
  if (!report?.results?.length) return null;
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 4, marginBottom: 14, fontSize: c.fontSm }}>
      {/* Labelled because the last-sent line above also begins "Email —": one is
          the transport's history, this is the attempt you just made. Without the
          label the two read as a contradiction. */}
      <div style={lblStyle()}>Test send</div>
      {report.results.map((res) => {
        const color = res.sent ? c.success : res.skipped ? c.textSec : c.danger;
        const verdict = res.sent ? "sent" : res.skipped ? "not attempted" : "failed";
        return (
          <div key={res.transport} style={{ color: c.textSec }}>
            <strong style={{ color: c.text }}>{res.transport === "email" ? "Email" : "Apprise"}</strong>
            {" — "}
            <span style={{ color, fontWeight: 600 }}>{verdict}</span>
            {res.detail ? ` · ${res.detail}` : null}
          </div>
        );
      })}
      <div style={{ fontSize: c.fontXs, color: c.textSec }}>
        A test uses the <strong>saved</strong> settings — save your changes first if you have edited anything above.
      </div>
    </div>
  );
}

// ── Run notifications (SMTP / Apprise) — GET/PUT /settings/notifications ──
function RunNotificationsCard() {
  const [bump, setBump] = useState(0);
  const { data, error, loading } = useGet<NotificationConfig>(() => api.GET("/settings/notifications"), [bump]);
  const [form, setForm] = useState<NotificationConfig | null>(null);
  const [recipientsText, setRecipientsText] = useState("");
  const [smtpPassword, setSmtpPassword] = useState("");
  const [saving, setSaving] = useState(false);
  const [saved, setSaved] = useState(false);
  const [saveErr, setSaveErr] = useState<string | null>(null);
  const [testing, setTesting] = useState(false);
  const [testReport, setTestReport] = useState<TestReport | null>(null);
  const [testErr, setTestErr] = useState<string | null>(null);

  useEffect(() => {
    if (data) {
      setForm(data);
      setRecipientsText((data.smtp?.recipients ?? []).join(", "));
      setSmtpPassword("");
    }
  }, [data]);

  const provider = form?.provider ?? "apprise";
  const patchSmtp = (p: Partial<NonNullable<NotificationConfig["smtp"]>>) =>
    setForm((f) => (f ? { ...f, smtp: { ...f.smtp, ...p } } : f));
  const patchApprise = (p: Partial<NonNullable<NotificationConfig["apprise"]>>) =>
    setForm((f) => (f ? { ...f, apprise: { ...f.apprise, ...p } } : f));
  const setTargets = (fn: (ts: AppriseTarget[]) => AppriseTarget[]) =>
    setForm((f) => (f ? { ...f, apprise: { ...f.apprise, targets: fn(f.apprise?.targets ?? []) } } : f));

  // K-5 — send a test over every configured transport and show what happened.
  // The report is per-transport rather than one verdict, because "skipped: no
  // recipients configured" is the answer most of the time on a half-set-up
  // install, and it tells the operator what to do next in a way that a bare
  // failure does not.
  async function sendTest() {
    setTesting(true);
    setTestReport(null);
    setTestErr(null);
    const { data, error: postErr } = await api.POST("/settings/notifications/test", { params: { header: csrfHeader } });
    setTesting(false);
    if (postErr) {
      setTestErr(errMsg(postErr));
      return;
    }
    setTestReport(data ?? null);
    // Re-read so the last-sent line reflects the send that just happened — a
    // successful test is exactly the fact that line reports.
    setBump((b) => b + 1);
  }

  async function save() {
    if (!form) return;
    setSaving(true);
    setSaveErr(null);
    const body: NotificationConfig = {
      ...form,
      smtp: {
        ...form.smtp,
        recipients: recipientsText.split(",").map((s) => s.trim()).filter(Boolean),
        // omit password unless the operator typed a new one (write-only field)
        ...(smtpPassword ? { password: smtpPassword } : {}),
      },
    };
    const { error: putErr } = await api.PUT("/settings/notifications", { params: { header: csrfHeader }, body });
    setSaving(false);
    if (putErr) {
      setSaveErr(errMsg(putErr));
    } else {
      setSaved(true);
      setTimeout(() => setSaved(false), 2200);
      setBump((b) => b + 1);
    }
  }

  return (
    <Card
      title="Run Notifications"
      action={
        <div style={{ display: "flex", gap: 8 }}>
          {/* K-5 — deliberately beside Save, not inside the form: it sends the
              SAVED config, so an operator who has edited without saving is
              testing the old settings. The helper text below says so. */}
          <Btn onClick={sendTest} disabled={!form || testing || saving}>
            {testing ? "Sending…" : "Send test"}
          </Btn>
          <Btn primary onClick={save} disabled={!form || saving} style={saved ? { background: c.success, borderColor: c.success } : undefined}>
            {saved ? "✓ Saved" : saving ? "Saving…" : "Save Changes"}
          </Btn>
        </div>
      }
    >
      {loading && <InlineLoading />}
      {error && <div style={{ color: c.danger }}>Error: {error}</div>}
      {saveErr && <div style={{ color: c.danger, marginBottom: 10, fontSize: c.fontSm }}>Save failed: {saveErr}</div>}
      {!loading && !error && form && (
        <>
          <div style={{ fontSize: c.fontSm, color: c.textSec, marginBottom: 14, lineHeight: 1.6 }}>
            Pick how Cronomicon notifies people when a job or workflow is triggered, completed, or flagged. Apprise fans one
            event out to many services via URL DSNs; SMTP relays through a single mail server you control.
          </div>

          <LastSentLine lastSent={form.lastSent} />
          {testErr && (
            <div style={{ color: c.danger, marginBottom: 12, fontSize: c.fontSm }}>Test failed: {testErr}</div>
          )}
          <TestReportLines report={testReport} />

          {/* Provider toggle */}
          <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: 10, marginBottom: 18 }}>
            {(
              [
                { key: "apprise", label: "Apprise", sub: "URL-based fan-out (email, Slack, Discord, …)" },
                { key: "smtp", label: "SMTP relay", sub: "Direct connection to your mail server" },
              ] as const
            ).map((opt) => {
              const active = provider === opt.key;
              return (
                <div
                  key={opt.key}
                  onClick={() => setForm((f) => (f ? { ...f, provider: opt.key } : f))}
                  style={{
                    padding: "12px 14px",
                    borderRadius: c.radiusSurface,
                    cursor: "pointer",
                    border: `2px solid ${active ? c.primary : c.border}`,
                    background: active ? `${c.primary}1a` : c.panel2,
                  }}
                >
                  <div style={{ fontSize: c.fontBody, fontWeight: 700, color: active ? c.primary : c.text, marginBottom: 4 }}>{opt.label}</div>
                  <div style={{ fontSize: c.fontSm, color: c.textSec }}>{opt.sub}</div>
                </div>
              );
            })}
          </div>

          {provider === "apprise" ? (
            <>
              <div style={{ display: "flex", alignItems: "center", justifyContent: "space-between", marginBottom: 12 }}>
                <div>
                  <div style={lblStyle()}>Apprise API URL</div>
                  <input
                    value={form.apprise?.apiUrl ?? ""}
                    onChange={(e) => patchApprise({ apiUrl: e.target.value })}
                    placeholder="http://apprise:8000/notify"
                    style={{ ...inputStyle(), width: 320, fontFamily: c.mono, fontSize: c.fontSm }}
                  />
                </div>
                <label style={{ display: "inline-flex", alignItems: "center", gap: 8, fontSize: c.fontSm, color: c.textSec, cursor: "pointer" }}>
                  <input
                    type="checkbox"
                    checked={form.apprise?.enabled ?? false}
                    onChange={(e) => patchApprise({ enabled: e.target.checked })}
                    style={{ cursor: "pointer", accentColor: c.primary }}
                  />
                  Enabled
                </label>
              </div>

              <div style={lblStyle()}>Targets</div>
              <div style={{ display: "flex", flexDirection: "column", gap: 8 }}>
                {/* A hairline plus leading, not a box per target (B-1/VU-5): these
                    rows sit inside a bordered Card and carry bordered inputs, so an
                    individual frame was the third level of nesting and grouped
                    nothing the spacing does not already group. */}
                {(form.apprise?.targets ?? []).map((t, i) => (
                  <Fragment key={t.id ?? i}>
                    {i > 0 && <Rule />}
                    <div
                      style={{
                        display: "flex",
                        alignItems: "center",
                        gap: 10,
                        padding: "2px 0",
                      }}
                    >
                      <input
                        type="checkbox"
                        checked={t.enabled ?? false}
                        onChange={() => setTargets((ts) => ts.map((x, j) => (j === i ? { ...x, enabled: !x.enabled } : x)))}
                        style={{ cursor: "pointer", accentColor: c.primary }}
                      />
                      <input
                        value={t.label ?? ""}
                        placeholder="Label"
                        onChange={(e) => setTargets((ts) => ts.map((x, j) => (j === i ? { ...x, label: e.target.value } : x)))}
                        style={{ ...inputStyle(), width: 130 }}
                      />
                      <input
                        value={t.service ?? ""}
                        placeholder="service"
                        onChange={(e) => setTargets((ts) => ts.map((x, j) => (j === i ? { ...x, service: e.target.value } : x)))}
                        style={{ ...inputStyle(), width: 90 }}
                      />
                      <input
                        value={t.url ?? ""}
                        placeholder="mailtos://user:pass@smtp.host/?to=team@co.com"
                        onChange={(e) => setTargets((ts) => ts.map((x, j) => (j === i ? { ...x, url: e.target.value } : x)))}
                        style={{ ...inputStyle(), flex: 1, fontFamily: c.mono, fontSize: c.fontXs }}
                      />
                      <Btn onClick={() => setTargets((ts) => ts.filter((_, j) => j !== i))}>Remove</Btn>
                    </div>
                  </Fragment>
                ))}
                {(form.apprise?.targets ?? []).length === 0 && (
                  <div style={{ color: c.textSec, fontSize: c.fontSm }}>
                    No targets yet. Choose <strong>+ Add target</strong> below to send notifications somewhere.
                  </div>
                )}
                <div>
                  <Btn onClick={() => setTargets((ts) => [...ts, { label: "", service: "email", url: "", enabled: true }])}>
                    + Add target
                  </Btn>
                </div>
                <div style={{ fontSize: c.fontXs, color: c.textSec }}>
                  Drop in any Apprise URL DSN — changes apply when you hit Save Changes.
                </div>
              </div>
            </>
          ) : (
            <div style={{ display: "flex", flexDirection: "column", gap: 10 }}>
              <div style={{ display: "grid", gridTemplateColumns: "2fr 1fr", gap: 10 }}>
                <div>
                  <div style={lblStyle()}>SMTP host</div>
                  <input
                    value={form.smtp?.host ?? ""}
                    onChange={(e) => patchSmtp({ host: e.target.value })}
                    placeholder="smtp.company.com"
                    style={{ ...inputStyle(), width: "100%" }}
                  />
                </div>
                <div>
                  <div style={lblStyle()}>Port</div>
                  <input
                    type="number"
                    value={form.smtp?.port ?? 587}
                    onChange={(e) => patchSmtp({ port: Number(e.target.value) || 587 })}
                    style={{ ...inputStyle(), width: "100%" }}
                  />
                </div>
              </div>
              <div>
                <div style={lblStyle()}>Encryption</div>
                <div style={{ display: "flex", gap: 6 }}>
                  {(["none", "starttls", "ssl"] as const).map((enc) => (
                    <Chip key={enc} label={enc.toUpperCase()} on={(form.smtp?.encryption ?? "starttls") === enc} onClick={() => patchSmtp({ encryption: enc })} />
                  ))}
                </div>
              </div>
              <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: 10 }}>
                <div>
                  <div style={lblStyle()}>Username</div>
                  <input value={form.smtp?.username ?? ""} onChange={(e) => patchSmtp({ username: e.target.value })} style={{ ...inputStyle(), width: "100%" }} />
                </div>
                <div>
                  <div style={lblStyle()}>Password</div>
                  <input
                    type="password"
                    value={smtpPassword}
                    placeholder={form.smtp?.passwordSet ? "•••••••••• (set — leave blank to keep)" : "Set password"}
                    onChange={(e) => setSmtpPassword(e.target.value)}
                    style={{ ...inputStyle(), width: "100%" }}
                  />
                </div>
              </div>
              <div style={{ display: "grid", gridTemplateColumns: "1fr 2fr", gap: 10 }}>
                <div>
                  <div style={lblStyle()}>From name</div>
                  <input value={form.smtp?.fromName ?? ""} onChange={(e) => patchSmtp({ fromName: e.target.value })} style={{ ...inputStyle(), width: "100%" }} />
                </div>
                <div>
                  <div style={lblStyle()}>From address</div>
                  <input value={form.smtp?.fromAddress ?? ""} onChange={(e) => patchSmtp({ fromAddress: e.target.value })} style={{ ...inputStyle(), width: "100%" }} />
                </div>
              </div>
              <div>
                <div style={lblStyle()}>Default recipients</div>
                <input
                  value={recipientsText}
                  onChange={(e) => setRecipientsText(e.target.value)}
                  placeholder="ops-team@company.com, ops-lead@company.com"
                  style={{ ...inputStyle(), width: "100%" }}
                />
                <div style={{ fontSize: c.fontXs, color: c.textSec, marginTop: 4 }}>Comma-separated. Run notifications CC every address here.</div>
              </div>
            </div>
          )}
        </>
      )}
    </Card>
  );
}

// ── Alert rules — GET/POST /alerts, PUT/DELETE /alerts/{alertId} ──
//
// J-2 (VF-15) — this form used to collect five things the dispatcher never read:
// `tag` targeting, the `n-failures-window` trigger with its count/window pair,
// `notifyTriggerer`, and three of its four channels. They are gone. What remains
// is exactly what notify.go acts on — see the J-6 guard, which fails the build
// if the two ever diverge again.
interface RuleForm {
  targetMode: "job" | "all";
  jobName: string;
  trigger: AlertRuleInput["trigger"];
  channels: AlertChannel[];
  recipients: string;
  enabled: boolean;
}

const EMPTY_RULE: RuleForm = {
  targetMode: "job",
  jobName: "",
  trigger: "failure",
  channels: ["email"],
  recipients: "",
  enabled: true,
};

// What the dropdown OFFERS. `triggerMatches` honours every run status plus
// `any`, and the spec now says so, but widening what an operator can create is a
// capability decision that VF-Q3 deliberately left alone.
//
// SL added the two non-run-outcome triggers, and they ARE offered: an alert the
// dispatcher honours but the UI cannot reach is the second half of the VF-15
// defect the enum-conformance test exists to catch.
// FX-D4 adds `skipped`. It was already accepted by the API and listed in the
// spec, and until FX-D4 nothing could ever emit one — so offering it here before
// would have been the FIRST half of the VF-15 defect (a rule that can be created
// and can never fire) exactly as omitting it now is the second.
const OFFERED_TRIGGERS = ["failure", "success", "skipped", "sla-breach", "missed-run"] as const;
const TRIGGER_LABELS: Record<string, string> = {
  failure: "Job fails",
  success: "Job succeeds",
  skipped: "Run suppressed (calendar, pause, or concurrency)",
  "sla-breach": "Job runs late (past its deadline)",
  "missed-run": "Scheduled run never happened",
};
// G-1 (A-19) — a rule's trigger is matched against the run STATUS
// (notify.go's triggerMatches), so a STORED trigger can be any status word, and
// the seeded `warning` rule is real and does fire. The offered set is narrower,
// so the table used to fall back to the raw value and print `warning` in a
// column where every other surface in the app says Warn.
const triggerLabel = (t: string): string =>
  TRIGGER_LABELS[t] ?? (t.toLowerCase() === "any" ? "Any run" : `Job result: ${statusLabel(t)}`);

const ALL_CHANNELS: AlertChannel[] = ["email", "apprise"];
// Wire values must not reach the screen (G-1 / A-19). The three legacy entries
// are here so an OLD rule's chips still read as words; they cannot be selected.
const CHANNEL_LABELS: Record<string, string> = {
  email: "Email",
  apprise: "Apprise",
  slack: "Slack",
  webhook: "Webhook",
  "in-app": "In-App",
};
const channelLabel = (ch: string): string => CHANNEL_LABELS[ch] ?? ch;
// Channels the dispatcher has no branch for. A rule carrying only these sends
// nothing, and always did — the row marks them rather than hiding them, so the
// operator can see which rules need re-pointing at Email or Apprise.
const INERT_CHANNELS = new Set(["slack", "webhook", "in-app"]);

// J-4 (VF-15) — a stored value the form no longer offers is PRESERVED, not eaten.
// `ruleToForm` used to assign r.trigger straight in; when that value matched no
// <option> the select rendered as unset and Save rewrote it to `failure`. The
// seeded `warning` rule is the live case, and it does fire, so that was silent
// data loss. Legacy targetMode (`tag`/`scope`) collapses to `job` instead —
// those rules matched no run either way, so nothing is lost by normalising them,
// and unlike the trigger there is no value worth round-tripping.
function ruleToForm(r: AlertRule): RuleForm {
  return {
    targetMode: r.targetMode === "all" ? "all" : "job",
    jobName: r.jobName ?? "",
    trigger: r.trigger,
    // Inert channels are dropped on edit. Unlike the trigger there is nothing to
    // preserve — they never dispatched — and the row (below) marks them as
    // ignored before you open it, so this reads as the cleanup it is.
    channels: (r.channels ?? []).filter((ch) => !INERT_CHANNELS.has(ch)),
    recipients: r.recipients ?? "",
    enabled: r.enabled ?? true,
  };
}

function formToInput(f: RuleForm): AlertRuleInput {
  return {
    targetMode: f.targetMode,
    jobName: f.targetMode === "job" ? f.jobName : null,
    trigger: f.trigger,
    channels: f.channels,
    recipients: f.recipients,
    enabled: f.enabled,
  };
}

function targetLabel(r: AlertRule): string {
  if (r.targetMode === "all") return "All jobs";
  // `tag` and `scope` never matched a run (notify.go's targetMatches). Say so.
  // (The tag list such rows once carried was dropped with migration 1140.)
  if (r.targetMode !== "job") return `${r.targetMode} (unsupported target)`;
  return r.jobName ?? "—";
}

function RuleFormFields(props: { form: RuleForm; onChange: (p: Partial<RuleForm>) => void; dropped?: string[] }) {
  const { form, onChange, dropped = [] } = props;
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 12 }}>
      <div>
        <div style={lblStyle()}>Target</div>
        <div style={{ display: "flex", gap: 6, marginBottom: 8 }}>
          {(
            [
              { v: "job", l: "Specific Job" },
              { v: "all", l: "All Jobs" },
            ] as const
          ).map((opt) => (
            <Chip key={opt.v} label={opt.l} on={form.targetMode === opt.v} onClick={() => onChange({ targetMode: opt.v })} />
          ))}
        </div>
        {form.targetMode === "job" && (
          <input
            value={form.jobName}
            onChange={(e) => onChange({ jobName: e.target.value })}
            placeholder="Job name"
            style={{ ...inputStyle(), width: "100%" }}
          />
        )}
      </div>

      <div>
        <div style={lblStyle()}>When</div>
        <select
          value={form.trigger}
          onChange={(e) => onChange({ trigger: e.target.value as RuleForm["trigger"] })}
          style={{ ...inputStyle(), width: "100%", cursor: "pointer" }}
        >
          {OFFERED_TRIGGERS.map((t) => (
            <option key={t} value={t}>
              {TRIGGER_LABELS[t]}
            </option>
          ))}
          {/* J-4 — an API-created or legacy trigger keeps its own option, so
              saving this rule cannot silently rewrite it to `failure`. */}
          {!OFFERED_TRIGGERS.includes(form.trigger as (typeof OFFERED_TRIGGERS)[number]) && (
            <option value={form.trigger}>{triggerLabel(form.trigger)}</option>
          )}
        </select>
      </div>

      <div>
        <div style={lblStyle()}>Channels</div>
        {/* Opening a rule whose channels were all inert leaves this empty, and
            Save then refuses with "Pick at least one channel" — correct, but
            baffling unless we say what was dropped and why (VF-15 / J-4). */}
        {dropped.length > 0 && (
          <div style={{ fontSize: c.fontSm, color: c.warning, marginBottom: 8 }}>
            {dropped.map(channelLabel).join(" and ")} {dropped.length > 1 ? "were" : "was"} removed in v0.52.23 — nothing
            was ever delivered over {dropped.length > 1 ? "them" : "it"}. Pick <strong>Email</strong> or{" "}
            <strong>Apprise</strong> (which reaches Slack, Discord and webhooks) to make this rule send.
          </div>
        )}
        <div style={{ display: "flex", gap: 6, flexWrap: "wrap" }}>
          {ALL_CHANNELS.map((ch) => (
            <Chip
              key={ch}
              label={channelLabel(ch)}
              on={form.channels.includes(ch)}
              onClick={() =>
                onChange({
                  channels: form.channels.includes(ch) ? form.channels.filter((x) => x !== ch) : [...form.channels, ch],
                })
              }
            />
          ))}
        </div>
      </div>

      <div>
        <div style={lblStyle()}>Recipients</div>
        <input
          value={form.recipients}
          onChange={(e) => onChange({ recipients: e.target.value })}
          placeholder="ops-team@company.com, #ops-alerts"
          style={{ ...inputStyle(), width: "100%" }}
        />
        <div style={{ display: "flex", gap: 16, marginTop: 8 }}>
          <label style={{ display: "flex", alignItems: "center", gap: 6, cursor: "pointer", fontSize: c.fontSm, color: c.textSec }}>
            <input
              type="checkbox"
              checked={form.enabled}
              onChange={(e) => onChange({ enabled: e.target.checked })}
              style={{ cursor: "pointer", accentColor: c.primary }}
            />
            Active
          </label>
        </div>
      </div>
    </div>
  );
}

// TS-17 (the sorting-update plan) — the header row was a literal string array
// keyed by index; a keyed column spec so a header can carry sort state.
// Channels and Recipients are multi-value (unsortable); the last column is the
// actions gutter.
const RULE_HEADERS: { key: string; label: string; sortable: boolean }[] = [
  { key: "target", label: "Target", sortable: true },
  { key: "when", label: "When", sortable: true },
  { key: "channels", label: "Channels", sortable: false },
  { key: "recipients", label: "Recipients", sortable: false },
  { key: "status", label: "Status", sortable: true },
  { key: "actions", label: "", sortable: false },
];

// Status sorts by rank on the derived enabled/disabled state — disabled first
// ascending (TS-Q5: the rule that sends nothing is the one needing attention).
const RULE_STATUS_RANK: Record<string, number> = { disabled: 0, enabled: 1 };
const RULE_SORT_COLS: SortColumn<AlertRule>[] = [
  { key: "target", get: (r) => targetLabel(r), type: "text" },
  { key: "when", get: (r) => triggerLabel(r.trigger), type: "text" },
  { key: "status", get: (r) => (r.enabled ? "enabled" : "disabled"), type: "rank", rank: RULE_STATUS_RANK },
];

function AlertRulesCard() {
  const [bump, setBump] = useState(0);
  const { data, error, loading } = useGet<unknown>(() => api.GET("/alerts"), [bump]);
  const items = rows<AlertRule>(data);
  const sort = useTableSort(items, RULE_SORT_COLS, { key: "target", dir: "asc" }, { tableId: "alert-rules" });

  const [showAdd, setShowAdd] = useState(false);
  const [addForm, setAddForm] = useState<RuleForm>({ ...EMPTY_RULE });
  const [editId, setEditId] = useState<string | null>(null);
  // FX-9 — the rule pending deletion. Deleting an alert rule was instant and
  // irreversible: one click and the thing that tells you a job failed is gone,
  // with nothing to say which rule went. The other destructive actions in the app
  // (job/workflow/runner delete) all confirm; this one simply never did.
  const [deleting, setDeleting] = useState<AlertRule | null>(null);
  const [editForm, setEditForm] = useState<RuleForm>({ ...EMPTY_RULE });
  const [busy, setBusy] = useState(false);
  const [actionErr, setActionErr] = useState<string | null>(null);

  function validate(f: RuleForm): string | null {
    if (f.targetMode === "job" && !f.jobName.trim()) return "Pick a job name for the rule target.";
    if (f.channels.length === 0) return "Pick at least one channel.";
    return null;
  }

  async function create() {
    const v = validate(addForm);
    if (v) {
      setActionErr(v);
      return;
    }
    setBusy(true);
    setActionErr(null);
    const { error: e } = await api.POST("/alerts", { params: { header: csrfHeader }, body: formToInput(addForm) });
    setBusy(false);
    if (e) {
      setActionErr(errMsg(e));
    } else {
      setShowAdd(false);
      setAddForm({ ...EMPTY_RULE });
      setBump((b) => b + 1);
    }
  }

  async function update(id: string) {
    const v = validate(editForm);
    if (v) {
      setActionErr(v);
      return;
    }
    setBusy(true);
    setActionErr(null);
    const { error: e } = await api.PUT("/alerts/{alertId}", {
      params: { path: { alertId: id }, header: csrfHeader },
      body: formToInput(editForm),
    });
    setBusy(false);
    if (e) {
      setActionErr(errMsg(e));
    } else {
      setEditId(null);
      setBump((b) => b + 1);
    }
  }

  async function remove(id: string) {
    setBusy(true);
    setActionErr(null);
    const { error: e } = await api.DELETE("/alerts/{alertId}", { params: { path: { alertId: id }, header: csrfHeader } });
    setBusy(false);
    if (e) setActionErr(errMsg(e));
    else {
      setEditId(null);
      setBump((b) => b + 1);
    }
  }

  return (
    <Card
      title="Alert Rules"
      noPad
      action={
        <Btn
          primary
          onClick={() => {
            setShowAdd((s) => !s);
            setEditId(null);
            setActionErr(null);
          }}
        >
          {showAdd ? "Cancel" : "+ Add Alert Rule"}
        </Btn>
      }
    >
      {actionErr && <div style={{ color: c.danger, fontSize: c.fontSm, padding: "10px 16px" }}>Error: {actionErr}</div>}
      {showAdd && (
        <div style={{ padding: 16, borderBottom: `1px solid ${c.border}`, background: `${c.primary}0d` }}>
          <RuleFormFields form={addForm} onChange={(p) => setAddForm((f) => ({ ...f, ...p }))} />
          <div style={{ display: "flex", justifyContent: "flex-end", marginTop: 12 }}>
            <Btn primary onClick={create} disabled={busy}>
              {busy ? "Saving…" : "Save Rule"}
            </Btn>
          </div>
        </div>
      )}
      {loading && <div style={{ padding: 16 }}><SkeletonRows rows={5} /></div>}
      {error && <div style={{ color: c.danger, padding: 16 }}>Error: {error}</div>}
      {!loading && !error && items.length === 0 && (
        <div style={{ color: c.textSec, padding: 16 }}>
          No alert rules yet. Choose <strong>+ Add Alert Rule</strong> above to be told when a job fails.
        </div>
      )}
      {!loading && !error && items.length > 0 && (
        <table style={{ width: "100%", borderCollapse: "collapse", fontSize: c.fontSm }}>
          <thead>
            <tr>
              {RULE_HEADERS.map((h) => {
                const active = h.sortable && sort.isActive(h.key);
                return (
                  <th
                    key={h.key}
                    onClick={h.sortable ? () => sort.toggle(h.key) : undefined}
                    title={h.sortable ? `Sort by ${h.label}` : undefined}
                    aria-sort={h.sortable ? sort.ariaSort(h.key) : undefined}
                    style={{
                      ...thStyle(),
                      cursor: h.sortable ? "pointer" : undefined,
                      color: h.sortable ? (active ? c.text : c.textSec) : undefined,
                    }}
                  >
                    {h.sortable ? <SortableLabel label={h.label} active={active} dir={sort.sortDir} /> : h.label}
                  </th>
                );
              })}
            </tr>
          </thead>
          <tbody>
            {sort.sorted.map((r) => {
              // FX-12 — keyed on the id itself. It used to compare `editId` (set
              // from `r.id ?? null`) against `r.id ?? ""`, so a row with no id
              // could never match: `null === ""` is false, and the button did
              // nothing, forever. An alert rule always HAS an id — alert_config.id
              // is the primary key, settings.AlertRule.ID is a non-pointer string
              // so a null would fail the scan, and the spec marks it readOnly —
              // which is why the guard was never exercised and never noticed. The
              // typed id is optional all the same, so the impossible case is now
              // handled by not offering an action that cannot work (the Phase B
              // house rule: irrelevance hides).
              const editing = r.id != null && editId === r.id;
              return (
                <FragmentRow key={r.id ?? targetLabel(r)}>
                  <tr style={{ borderBottom: `1px solid ${c.border}`, background: editing ? `${c.primary}0d` : "transparent" }}>
                    <td style={{ ...tdStyle(), fontWeight: 600 }}>{targetLabel(r)}</td>
                    <td style={{ ...tdStyle(), color: c.textSec, fontSize: c.fontSm }}>
                      {triggerLabel(r.trigger)}
                    </td>
                    <td style={tdStyle()}>
                      <div style={{ display: "flex", gap: 4, flexWrap: "wrap" }}>
                        {(r.channels ?? []).map((ch) => {
                          // A legacy channel the dispatcher has no branch for is
                          // shown struck through rather than hidden — the rule
                          // sends nothing over it and the operator needs to know
                          // which rules to re-point (VF-15).
                          const inert = INERT_CHANNELS.has(ch);
                          return (
                            <span
                              key={ch}
                              title={inert ? `${channelLabel(ch)} is not delivered — re-point this rule at Email or Apprise.` : undefined}
                              style={{
                                padding: "2px 8px",
                                borderRadius: c.radiusChip,
                                fontSize: c.fontXs,
                                fontWeight: 600,
                                background: inert ? "transparent" : `${c.primary}1a`,
                                color: inert ? c.textSec : c.primary,
                                border: inert ? `1px dashed ${c.border}` : undefined,
                                textDecoration: inert ? "line-through" : undefined,
                              }}
                            >
                              {channelLabel(ch)}
                            </span>
                          );
                        })}
                      </div>
                    </td>
                    <td style={{ ...tdStyle(), fontSize: c.fontXs, color: c.textSec }}>
                      {(r.recipients ?? "").split(",")[0]?.trim() || "—"}
                    </td>
                    <td style={tdStyle()}>
                      <span style={{ color: r.enabled ? c.success : c.textSec, fontWeight: 600, fontSize: c.fontSm }}>
                        {r.enabled ? "Active" : "Inactive"}
                      </span>
                    </td>
                    <td style={tdStyle()}>
                      {r.id != null && (
                        <Btn
                          onClick={() => {
                            if (editing) {
                              setEditId(null);
                            } else {
                              setEditId(r.id!);
                              setEditForm(ruleToForm(r));
                              setShowAdd(false);
                              setActionErr(null);
                            }
                          }}
                        >
                          {editing ? "Close" : "Edit"}
                        </Btn>
                      )}
                    </td>
                  </tr>
                  {editing && (
                    <tr style={{ borderBottom: `1px solid ${c.border}` }}>
                      <td colSpan={6} style={{ padding: 16, background: `${c.primary}0d` }}>
                        <RuleFormFields
                          form={editForm}
                          onChange={(p) => setEditForm((f) => ({ ...f, ...p }))}
                          dropped={(r.channels ?? []).filter((ch) => INERT_CHANNELS.has(ch))}
                        />
                        <div style={{ display: "flex", justifyContent: "flex-end", gap: 8, marginTop: 12 }}>
                          <Btn danger onClick={() => setDeleting(r)} disabled={busy}>
                            Delete
                          </Btn>
                          <Btn primary onClick={() => update(r.id!)} disabled={busy}>
                            {busy ? "Saving…" : "Save"}
                          </Btn>
                        </div>
                      </td>
                    </tr>
                  )}
                </FragmentRow>
              );
            })}
          </tbody>
        </table>
      )}
      {deleting && (
        <ConfirmDialog
          title="Delete alert rule?"
          message={
            <>
              <strong>{targetLabel(deleting)}</strong> — {triggerLabel(deleting.trigger)}, over{" "}
              {(deleting.channels ?? []).map(channelLabel).join(" and ") || "no channel"}.
              {deleting.enabled ? (
                <> This rule is <strong>active</strong>: nothing will be sent for those runs once it is gone.</>
              ) : (
                <> This rule is inactive, so nothing is being sent for it today.</>
              )}{" "}
              Deleting is permanent — the rule has to be recreated by hand.
            </>
          }
          confirmLabel="Delete rule"
          busy={busy}
          onCancel={() => setDeleting(null)}
          onConfirm={() => {
            const id = deleting.id;
            setDeleting(null);
            if (id != null) void remove(id);
          }}
        />
      )}
    </Card>
  );
}

// Tiny helper so table rows + expanded editors can share a key without extra DOM.
function FragmentRow(props: { children: React.ReactNode }) {
  return <>{props.children}</>;
}

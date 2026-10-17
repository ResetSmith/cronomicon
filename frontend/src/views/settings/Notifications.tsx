import { Fragment, useEffect, useState } from "react";
import { api } from "../../api/client";
import type { components } from "../../api/schema";
import { useGet } from "../../hooks";
import { c } from "../../theme";
import { globalOnly } from "../../api/globalAdmin";
import { Btn, Card, Chip, ReadOnlyFields, csrfHeader, errMsg, fmtDateTime, inputStyle, lblStyle } from "./ui";
import { InlineLoading, Rule } from "../../components/ui";

type NotificationConfig = components["schemas"]["NotificationConfig"];
type AppriseTarget = NonNullable<NonNullable<NotificationConfig["apprise"]>["targets"]>[number];
type TestReport = components["schemas"]["NotificationTestReport"];

// `canWrite` — global administrator for configureApp (GC, v2.2.2). Saving the
// transports and sending a test (it delivers to real people over the stored
// config) are install-wide. The alert rules, which were here too, are one
// agency's since 2.5.0 C4 and live on Settings → Alerts.
export function NotificationsSection({ canWrite }: { canWrite: boolean }) {
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 16 }}>
      <RunNotificationsCard canWrite={canWrite} />
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
function RunNotificationsCard({ canWrite }: { canWrite: boolean }) {
  const why = globalOnly(canWrite);
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
          <Btn onClick={sendTest} disabled={!form || testing || saving || !!why} title={why || undefined}>
            {testing ? "Sending…" : "Send test"}
          </Btn>
          <Btn primary onClick={save} disabled={!form || saving || !!why} title={why || undefined} style={saved ? { background: c.success, borderColor: c.success } : undefined}>
            {saved ? "✓ Saved" : saving ? "Saving…" : "Save Changes"}
          </Btn>
        </div>
      }
    >
      {loading && <InlineLoading />}
      {error && <div style={{ color: c.danger }}>Error: {error}</div>}
      {saveErr && <div style={{ color: c.danger, marginBottom: 10, fontSize: c.fontSm }}>Save failed: {saveErr}</div>}
      {!loading && !error && form && (
        <ReadOnlyFields readOnly={!canWrite}>
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
                  // Not a form control, so the read-only fieldset cannot reach it.
                  aria-disabled={!canWrite || undefined}
                  onClick={canWrite ? () => setForm((f) => (f ? { ...f, provider: opt.key } : f)) : undefined}
                  style={{
                    padding: "12px 14px",
                    borderRadius: c.radiusSurface,
                    cursor: canWrite ? "pointer" : "default",
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
                      <Btn onClick={() => setTargets((ts) => ts.filter((_, j) => j !== i))} disabled={!canWrite}>Remove</Btn>
                    </div>
                  </Fragment>
                ))}
                {(form.apprise?.targets ?? []).length === 0 && (
                  <div style={{ color: c.textSec, fontSize: c.fontSm }}>
                    No targets yet. Choose <strong>+ Add target</strong> below to send notifications somewhere.
                  </div>
                )}
                <div>
                  <Btn onClick={() => setTargets((ts) => [...ts, { label: "", service: "email", url: "", enabled: true }])} disabled={!canWrite}>
                    + Add target
                  </Btn>
                </div>
                {/* 2.5.0 C4: these are Global's push targets (the save matches
                    them by URL, keeping the rules that name them); each agency's
                    are on Settings → Alerts. A changed URL matches nothing, so
                    the save makes a new target and deletes the old one with its
                    rules' links; Alerts edits a target by id and keeps them. */}
                <div style={{ fontSize: c.fontXs, color: c.textSec }}>
                  Drop in any Apprise URL DSN — changes apply when you hit Save Changes. These are Global's targets: Global's
                  alert rules pick from them, and each agency keeps its own under Alerts.
                </div>
                <div style={{ fontSize: c.fontXs, color: c.warning }}>
                  Changing a URL here makes a new target that no rule names; to change a URL and keep its rules, edit it
                  under Alerts while working in Global.
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
                {/* Question 80 (2.5.0 C4): an agency's rule mails its own recipients only. */}
                <div style={{ fontSize: c.fontXs, color: c.textSec, marginTop: 4 }}>
                  Comma-separated. Added to the mail of Global's alert rules only; an agency's rules mail their own recipients.
                </div>
              </div>
            </div>
          )}
        </ReadOnlyFields>
      )}
    </Card>
  );
}

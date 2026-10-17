import { useState } from "react";
import { api } from "../../api/client";
import type { components } from "../../api/schema";
import { useGet, rows, useTableSort } from "../../hooks";
import { type SortColumn } from "../../utils/sort";
import { c } from "../../theme";
import { GLOBAL_AGENCY, agenciesFor, useMyAccess } from "../../api/access";
import { useAgencyOptional } from "../../agency";
import { Btn, Card, Chip, csrfHeader, errMsg, inputStyle, lblStyle, tdStyle, thStyle } from "./ui";
import { ConfirmDialog, SkeletonRows, SortableLabel, statusLabel } from "../../components/ui";

type AlertRule = components["schemas"]["AlertRule"];
type AlertRuleInput = components["schemas"]["AlertRuleInput"];
type AlertChannel = AlertRuleInput["channels"][number];
type AlertTarget = components["schemas"]["AlertTarget"];
type AlertTargetInput = components["schemas"]["AlertTargetInput"];

// Alerts — one agency's alert rules and push targets (2.5.0 C4, AL-1 to AL-3).
//
// Until 2.5.0 every rule was Global's and every push went to every target, so
// both lived on Settings → Notifications, a global administrator's. A rule and
// a target now belong to ONE agency: its administrators (configureApp there)
// change them, and a rule sends to its own agency's targets only (question 79).
// The relay that carries the mail and the pushes stays the installation's, on
// Notifications.
//
// The lists are the agency in use's (the switcher narrows both routes), so one
// sentence says whether this caller may change what is shown. A person who only
// reads the agency sees it with every write disabled and the reason.
//
// `isGlobal` — the caller is a global administrator for configureApp, so
// changes every agency's, Global's included.
export function AlertsSection({ isGlobal }: { isGlobal: boolean }) {
  const ag = useAgencyOptional();
  const access = useMyAccess();
  const mine = agenciesFor(access ?? null, "configureApp");
  // The agency the rows here are, and a new one is created in. Outside the
  // switcher (a test) it is where the server would place the row; "" when it
  // cannot be told, and then the server decides.
  const agency = ag?.agency ?? (isGlobal ? GLOBAL_AGENCY : mine.length === 1 ? mine[0].id : "");
  const agencyName =
    ag?.agencyName ?? (agency === GLOBAL_AGENCY ? "Global" : mine.find((a) => a.id === agency)?.name ?? agency);
  // While /me/access loads, or if it cannot be read, nothing here claims the
  // agency is somebody else's: the controls stay live and the server decides.
  const known = isGlobal || access != null;
  const why =
    !known || !agency || isGlobal || mine.some((a) => a.id === agency)
      ? ""
      : agency === GLOBAL_AGENCY
        ? "Global's alerts are a global administrator's (a role on every agency) to change."
        : `Only ${agencyName}'s administrators can change its alerts.`;

  const [bump, setBump] = useState(0);
  const refresh = () => setBump((b) => b + 1);
  const rulesQ = useGet<unknown>(() => api.GET("/alerts"), [bump]);
  const targetsQ = useGet<unknown>(() => api.GET("/alert-targets"), [bump]);
  const rules = rows<AlertRule>(rulesQ.data);
  // null until read, and when the read failed: the rule form then offers no
  // picker, so a save cannot cut a rule's targets it never saw.
  const targets = targetsQ.loading || targetsQ.error ? null : rows<AlertTarget>(targetsQ.data);

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 16 }}>
      <div style={{ fontSize: c.fontSm, color: c.textSec, lineHeight: 1.6 }}>
        The alert rules and push targets of <strong style={{ color: c.text }}>{agencyName || "your agency"}</strong>.{" "}
        {agency === GLOBAL_AGENCY
          ? "Global's rules match the runs of every agency."
          : "Its rules match its own runs only, and push to its own targets."}{" "}
        To change another agency's, switch to it. The mail server and the Apprise gateway are the installation's, on
        Notifications.
      </div>
      <PushTargetsCard
        agency={agency}
        why={why}
        targets={targets}
        loading={targetsQ.loading}
        error={targetsQ.error}
        rules={rules}
        onChanged={refresh}
      />
      <AlertRulesCard
        agency={agency}
        why={why}
        rules={rules}
        loading={rulesQ.loading}
        error={rulesQ.error}
        targets={targets}
        onChanged={refresh}
      />
    </div>
  );
}

const ownerOf = (x: { ownerAgency?: string | null }): string => x.ownerAgency || GLOBAL_AGENCY;

// ── Push targets — GET/POST /alert-targets, PUT/DELETE /alert-targets/{targetId} ──
//
// An Apprise service URL one agency's rules push to (AL-2). Its URL is often
// the credential, so it is read only by those who may change the target
// (question 81); anyone else sees that one is set, and is offered no edit.
interface TargetForm {
  label: string;
  service: string;
  url: string;
  enabled: boolean;
}

const EMPTY_TARGET: TargetForm = { label: "", service: "", url: "", enabled: true };

const targetToForm = (t: AlertTarget): TargetForm => ({
  label: t.label ?? "",
  service: t.service ?? "",
  url: t.url ?? "",
  enabled: t.enabled ?? true,
});

function TargetFormFields({ form, onChange }: { form: TargetForm; onChange: (p: Partial<TargetForm>) => void }) {
  return (
    <div style={{ display: "flex", alignItems: "center", gap: 10, flexWrap: "wrap" }}>
      <input value={form.label} placeholder="Label" onChange={(e) => onChange({ label: e.target.value })} style={{ ...inputStyle(), width: 150 }} />
      <input value={form.service} placeholder="service" onChange={(e) => onChange({ service: e.target.value })} style={{ ...inputStyle(), width: 100 }} />
      <input
        value={form.url}
        placeholder="slack://tokenA/tokenB/tokenC"
        onChange={(e) => onChange({ url: e.target.value })}
        style={{ ...inputStyle(), flex: 1, minWidth: 240, fontFamily: c.mono, fontSize: c.fontXs }}
      />
      <label style={{ display: "inline-flex", alignItems: "center", gap: 6, fontSize: c.fontSm, color: c.textSec, cursor: "pointer" }}>
        <input
          type="checkbox"
          checked={form.enabled}
          onChange={(e) => onChange({ enabled: e.target.checked })}
          style={{ cursor: "pointer", accentColor: c.primary }}
        />
        Enabled
      </label>
    </div>
  );
}

const TARGET_HEADERS = ["Label", "Service", "URL", "Rules", "Status", ""];

function PushTargetsCard(props: {
  agency: string;
  why: string;
  targets: AlertTarget[] | null;
  loading: boolean;
  error: string | null;
  rules: AlertRule[];
  onChanged: () => void;
}) {
  const { agency, why, targets, loading, error, rules, onChanged } = props;
  const [showAdd, setShowAdd] = useState(false);
  const [addForm, setAddForm] = useState<TargetForm>({ ...EMPTY_TARGET });
  const [editId, setEditId] = useState<string | null>(null);
  const [editForm, setEditForm] = useState<TargetForm>({ ...EMPTY_TARGET });
  const [deleting, setDeleting] = useState<AlertTarget | null>(null);
  const [busy, setBusy] = useState(false);
  const [actionErr, setActionErr] = useState<string | null>(null);

  // The rules that push to a target: what deleting it changes.
  const namedBy = (id?: string) => rules.filter((r) => id != null && (r.targetIds ?? []).includes(id));

  async function create() {
    if (!addForm.url.trim()) {
      setActionErr("A push target needs its Apprise URL.");
      return;
    }
    setBusy(true);
    setActionErr(null);
    const body: AlertTargetInput = { ...addForm, ...(agency ? { ownerAgency: agency } : {}) };
    const { error: e } = await api.POST("/alert-targets", { params: { header: csrfHeader }, body });
    setBusy(false);
    if (e) {
      setActionErr(errMsg(e));
    } else {
      setShowAdd(false);
      setAddForm({ ...EMPTY_TARGET });
      onChanged();
    }
  }

  async function update(id: string) {
    if (!editForm.url.trim()) {
      setActionErr("A push target needs its Apprise URL.");
      return;
    }
    setBusy(true);
    setActionErr(null);
    const { error: e } = await api.PUT("/alert-targets/{targetId}", {
      params: { path: { targetId: id }, header: csrfHeader },
      body: editForm,
    });
    setBusy(false);
    if (e) {
      setActionErr(errMsg(e));
    } else {
      setEditId(null);
      onChanged();
    }
  }

  async function remove(id: string) {
    setBusy(true);
    setActionErr(null);
    const { error: e } = await api.DELETE("/alert-targets/{targetId}", { params: { path: { targetId: id }, header: csrfHeader } });
    setBusy(false);
    if (e) {
      setActionErr(errMsg(e));
    } else {
      setEditId(null);
      onChanged();
    }
  }

  const list = targets ?? [];
  return (
    <Card
      title="Push Targets"
      noPad
      action={
        <Btn
          primary
          disabled={!!why}
          title={why || undefined}
          onClick={() => {
            setShowAdd((s) => !s);
            setEditId(null);
            setActionErr(null);
          }}
        >
          {showAdd ? "Cancel" : "+ Add target"}
        </Btn>
      }
    >
      {actionErr && <div style={{ color: c.danger, fontSize: c.fontSm, padding: "10px 16px" }}>Error: {actionErr}</div>}
      {showAdd && (
        <div style={{ padding: 16, borderBottom: `1px solid ${c.border}`, background: `${c.primary}0d` }}>
          <TargetFormFields form={addForm} onChange={(p) => setAddForm((f) => ({ ...f, ...p }))} />
          <div style={{ display: "flex", justifyContent: "flex-end", marginTop: 12 }}>
            <Btn primary onClick={create} disabled={busy}>
              {busy ? "Saving…" : "Save target"}
            </Btn>
          </div>
        </div>
      )}
      {loading && <div style={{ padding: 16 }}><SkeletonRows rows={3} /></div>}
      {error && <div style={{ color: c.danger, padding: 16 }}>Error: {error}</div>}
      {!loading && !error && list.length === 0 && (
        <div style={{ color: c.textSec, padding: 16 }}>
          No push targets yet. Add one, then pick it on a rule that sends over Apprise: a rule pushes to the targets it
          names and nowhere else.
        </div>
      )}
      {!loading && !error && list.length > 0 && (
        <table style={{ width: "100%", borderCollapse: "collapse", fontSize: c.fontSm }}>
          <thead>
            <tr>
              {TARGET_HEADERS.map((h, i) => (
                <th key={i} style={thStyle()}>
                  {h}
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {list.map((t) => {
              const editing = t.id != null && editId === t.id;
              // Question 81: the URL is withheld from anyone who may not change
              // the target, and a save needs it, so such a row is not editable.
              const hidden = !t.url && !!t.urlSet;
              const users = namedBy(t.id);
              return (
                <FragmentRow key={t.id ?? t.label}>
                  <tr style={{ borderBottom: `1px solid ${c.border}`, background: editing ? `${c.primary}0d` : "transparent" }}>
                    <td style={{ ...tdStyle(), fontWeight: 600 }}>{t.label || "—"}</td>
                    <td style={{ ...tdStyle(), color: c.textSec }}>{t.service || "—"}</td>
                    <td style={{ ...tdStyle(), fontFamily: c.mono, fontSize: c.fontXs, color: c.textSec, wordBreak: "break-all" }}>
                      {t.url ? t.url : t.urlSet ? <span title="Only this agency's administrators read a target's URL.">set</span> : "—"}
                    </td>
                    <td style={{ ...tdStyle(), color: c.textSec }}>{users.length}</td>
                    <td style={tdStyle()}>
                      <span style={{ color: t.enabled ? c.success : c.textSec, fontWeight: 600, fontSize: c.fontSm }}>
                        {t.enabled ? "Active" : "Off"}
                      </span>
                    </td>
                    <td style={tdStyle()}>
                      {t.id != null && !hidden && (
                        <Btn
                          disabled={!!why}
                          title={why || undefined}
                          onClick={() => {
                            if (editing) {
                              setEditId(null);
                            } else {
                              setEditId(t.id!);
                              setEditForm(targetToForm(t));
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
                      <td colSpan={TARGET_HEADERS.length} style={{ padding: 16, background: `${c.primary}0d` }}>
                        <TargetFormFields form={editForm} onChange={(p) => setEditForm((f) => ({ ...f, ...p }))} />
                        <div style={{ display: "flex", justifyContent: "flex-end", gap: 8, marginTop: 12 }}>
                          <Btn danger onClick={() => setDeleting(t)} disabled={busy}>
                            Delete
                          </Btn>
                          <Btn primary onClick={() => update(t.id!)} disabled={busy}>
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
          title="Delete push target?"
          message={(() => {
            const n = namedBy(deleting.id).length;
            return (
              <>
                <strong>{deleting.label || deleting.service || "This target"}</strong>
                {n === 0 ? (
                  <> is named by no rule.</>
                ) : (
                  <>
                    {" "}
                    is named by {n === 1 ? "one rule" : `${n} rules`}: {n === 1 ? "it stops" : "they stop"} pushing to it.
                  </>
                )}{" "}
                Deleting is permanent.
              </>
            );
          })()}
          confirmLabel="Delete target"
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
  // The push targets the rule names (AL-2): its own agency's.
  targetIds: string[];
}

const EMPTY_RULE: RuleForm = {
  targetMode: "job",
  jobName: "",
  trigger: "failure",
  channels: ["email"],
  recipients: "",
  enabled: true,
  targetIds: [],
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
// The dispatcher's push channel, under either of its names (notify.go).
const pushes = (channels: readonly string[] | undefined) => (channels ?? []).some((ch) => ch === "apprise" || ch === "push");

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
    channels: (r.channels ?? []).filter((ch) => !INERT_CHANNELS.has(ch)) as AlertChannel[],
    recipients: r.recipients ?? "",
    enabled: r.enabled ?? true,
    targetIds: r.targetIds ?? [],
  };
}

// `offered` is the targets the picker showed, or null when it was not shown.
// The API reads an absent `targetIds` on an edit as "keep the rule's" and `[]`
// as "none", so the key is sent only when the person saw the choice — never
// from a form that hid it (Email only, or the targets could not be read).
function formToInput(f: RuleForm, offered: AlertTarget[] | null, ownerAgency?: string): AlertRuleInput {
  return {
    targetMode: f.targetMode,
    jobName: f.targetMode === "job" ? f.jobName : null,
    trigger: f.trigger,
    channels: f.channels,
    recipients: f.recipients,
    enabled: f.enabled,
    ...(ownerAgency ? { ownerAgency } : {}),
    ...(offered ? { targetIds: f.targetIds.filter((id) => offered.some((t) => t.id === id)) } : {}),
  };
}

// The targets a rule of `agency` may name: its own agency's (question 79).
// null when they could not be read; the form then shows no picker.
const targetsOf = (targets: AlertTarget[] | null, agency: string): AlertTarget[] | null =>
  targets == null ? null : targets.filter((t) => t.id != null && ownerOf(t) === agency);

// The picker is shown, and `targetIds` sent, when the rule pushes and the
// targets were read.
const pickerFor = (f: RuleForm, offered: AlertTarget[] | null): AlertTarget[] | null =>
  offered != null && pushes(f.channels) ? offered : null;

function targetLabel(r: AlertRule): string {
  if (r.targetMode === "all") return "All jobs";
  // `tag` and `scope` never matched a run (notify.go's targetMatches). Say so.
  // (The tag list such rows once carried was dropped with migration 1140.)
  if (r.targetMode !== "job") return `${r.targetMode} (unsupported target)`;
  return r.jobName ?? "—";
}

function RuleFormFields(props: {
  form: RuleForm;
  onChange: (p: Partial<RuleForm>) => void;
  dropped?: string[];
  // The rule's agency's targets; null when they could not be read.
  offered: AlertTarget[] | null;
  isGlobalRule: boolean;
}) {
  const { form, onChange, dropped = [], offered, isGlobalRule } = props;
  const picker = pickerFor(form, offered);
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

      {/* AL-2 — a push goes to the targets the rule names, and nowhere else. */}
      {picker && (
        <div>
          <div style={lblStyle()}>Push to</div>
          {picker.length === 0 ? (
            <div style={{ fontSize: c.fontSm, color: c.warning }}>
              This agency has no push targets yet. Add one under <strong>Push Targets</strong> above; until this rule names
              one, Apprise sends nothing for it.
            </div>
          ) : (
            <>
              <div style={{ display: "flex", gap: 14, flexWrap: "wrap" }}>
                {picker.map((t) => (
                  <label key={t.id} style={{ display: "inline-flex", alignItems: "center", gap: 6, cursor: "pointer", fontSize: c.fontSm, color: c.text }}>
                    <input
                      type="checkbox"
                      checked={form.targetIds.includes(t.id!)}
                      onChange={(e) =>
                        onChange({
                          targetIds: e.target.checked ? [...form.targetIds, t.id!] : form.targetIds.filter((x) => x !== t.id),
                        })
                      }
                      style={{ cursor: "pointer", accentColor: c.primary }}
                    />
                    {t.label || t.service || "Unnamed target"}
                    {!t.enabled && <span style={{ color: c.textSec, fontSize: c.fontXs }}>(off)</span>}
                  </label>
                ))}
              </div>
              {!picker.some((t) => form.targetIds.includes(t.id!)) && (
                <div style={{ fontSize: c.fontXs, color: c.warning, marginTop: 4 }}>
                  No target picked: Apprise sends nothing for this rule.
                </div>
              )}
            </>
          )}
        </div>
      )}

      <div>
        <div style={lblStyle()}>Recipients</div>
        <input
          value={form.recipients}
          onChange={(e) => onChange({ recipients: e.target.value })}
          placeholder="ops-team@company.com, #ops-alerts"
          style={{ ...inputStyle(), width: "100%" }}
        />
        {/* Question 80 — the relay's default recipients are on Global rules' mail only. */}
        <div style={{ fontSize: c.fontXs, color: c.textSec, marginTop: 4 }}>
          {isGlobalRule
            ? "Email goes to these addresses and to the relay's default recipients."
            : "Email goes to these addresses only."}
        </div>
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
// Channels, Push to and Recipients are multi-value (unsortable); the last column
// is the actions gutter.
const RULE_HEADERS: { key: string; label: string; sortable: boolean }[] = [
  { key: "target", label: "Target", sortable: true },
  { key: "when", label: "When", sortable: true },
  { key: "channels", label: "Channels", sortable: false },
  { key: "pushTo", label: "Push to", sortable: false },
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

// What a rule's push reaches: its targets' labels, or a warning when it pushes
// and names none (every rule made before C4.3's picker existed).
function PushToCell({ r, targets }: { r: AlertRule; targets: AlertTarget[] | null }) {
  if (!pushes(r.channels)) return <>—</>;
  const ids = r.targetIds ?? [];
  if (ids.length === 0) {
    return (
      <span style={{ color: c.warning }} title="Apprise sends nothing for this rule until it names a push target.">
        no target
      </span>
    );
  }
  if (targets == null) return <>{ids.length === 1 ? "1 target" : `${ids.length} targets`}</>;
  return <>{ids.map((id) => targets.find((t) => t.id === id)).map((t, i) => t?.label || t?.service || ids[i]).join(", ")}</>;
}

function AlertRulesCard(props: {
  agency: string;
  why: string;
  rules: AlertRule[];
  loading: boolean;
  error: string | null;
  targets: AlertTarget[] | null;
  onChanged: () => void;
}) {
  // AL-3 (2.5.0 C4) — POST/PUT/DELETE /alerts need configureApp on the rule's
  // agency, and the list is the agency in use's. So the rules stay visible to
  // whoever reads them, and Add/Edit — the only doors to Save and Delete — are
  // disabled with the reason for anyone who may not change them.
  const { agency, why, rules: items, loading, error, targets, onChanged } = props;
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

  // A new rule is the agency in use's; when that cannot be told (outside the
  // switcher) the picker is not offered and the server places the rule.
  const addOffered = agency ? targetsOf(targets, agency) : null;

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
    const { error: e } = await api.POST("/alerts", {
      params: { header: csrfHeader },
      body: formToInput(addForm, pickerFor(addForm, addOffered), agency || undefined),
    });
    setBusy(false);
    if (e) {
      setActionErr(errMsg(e));
    } else {
      setShowAdd(false);
      setAddForm({ ...EMPTY_RULE });
      onChanged();
    }
  }

  async function update(r: AlertRule) {
    const v = validate(editForm);
    if (v) {
      setActionErr(v);
      return;
    }
    setBusy(true);
    setActionErr(null);
    const { error: e } = await api.PUT("/alerts/{alertId}", {
      params: { path: { alertId: r.id! }, header: csrfHeader },
      // The rule keeps its agency; the API ignores ownerAgency on an edit.
      body: formToInput(editForm, pickerFor(editForm, targetsOf(targets, ownerOf(r)))),
    });
    setBusy(false);
    if (e) {
      setActionErr(errMsg(e));
    } else {
      setEditId(null);
      onChanged();
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
      onChanged();
    }
  }

  return (
    <Card
      title="Alert Rules"
      noPad
      action={
        <Btn
          primary
          disabled={!!why}
          title={why || undefined}
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
          <RuleFormFields
            form={addForm}
            onChange={(p) => setAddForm((f) => ({ ...f, ...p }))}
            offered={addOffered}
            isGlobalRule={agency === GLOBAL_AGENCY}
          />
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
                      <PushToCell r={r} targets={targets} />
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
                          disabled={!!why}
                          title={why || undefined}
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
                      <td colSpan={RULE_HEADERS.length} style={{ padding: 16, background: `${c.primary}0d` }}>
                        <RuleFormFields
                          form={editForm}
                          onChange={(p) => setEditForm((f) => ({ ...f, ...p }))}
                          dropped={(r.channels ?? []).filter((ch) => INERT_CHANNELS.has(ch))}
                          offered={targetsOf(targets, ownerOf(r))}
                          isGlobalRule={ownerOf(r) === GLOBAL_AGENCY}
                        />
                        <div style={{ display: "flex", justifyContent: "flex-end", gap: 8, marginTop: 12 }}>
                          <Btn danger onClick={() => setDeleting(r)} disabled={busy}>
                            Delete
                          </Btn>
                          <Btn primary onClick={() => update(r)} disabled={busy}>
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

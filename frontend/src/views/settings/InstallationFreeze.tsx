import { useState } from "react";
import { api } from "../../api/client";
import { c } from "../../theme";
import { appZoneLocalToISO, appZoneName, fmtInAppZone } from "../../utils/datetime";
import { FREEZE_CHANGED_EVENT, freezeState, freezeWhen, freezeWindowWords, useInstallationFreeze } from "../../components/InstallationFreezeBanner";
import { AlertBanner, Modal } from "../../components/ui";
import { Btn, Card, SettingRow, csrfHeader, errMsg, inputStyle } from "./ui";

// 2.5.0 C11, defect 84 (question 105): the switch for the freeze of the whole
// installation. It stops every run from starting, in every agency: schedules,
// Run, tokens, reactions, deferred runs and file arrivals (the gate matrix's
// globalFreeze, FX-Q2: a freeze a click can walk through is not one; C12,
// defect 93, corrected the copy that said manual runs go on). It takes a
// reason, which every skipped run records. Until C11 only `PUT /settings/freeze`
// set it, though the changelog and the manual said Settings.
//
// 2.5.1 (the owner, 2026-10-21): the card is a global administrator's alone,
// and is not rendered for anyone else (everyone still sees the banner while a
// freeze is on); freezing asks first, loudly, and needs an acknowledgement.
//
// 2.5.2 (FZ-1 to FZ-6): a freeze may be scheduled — one window, no recurrence.
// "Now" or "Schedule…"; the times are typed in the APP's timezone and the
// labels say which (a datetime-local input speaks the browser's clock, so the
// value is read through the app zone). The confirmation names the window in
// words. Lifting ends a running window early, or cancels one not begun.
export function InstallationFreezeCard({ canWrite }: { canWrite: boolean }) {
  const freeze = useInstallationFreeze();
  const [reason, setReason] = useState("");
  const [when, setWhen] = useState<"now" | "schedule">("now");
  const [fromLocal, setFromLocal] = useState("");
  const [untilLocal, setUntilLocal] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [confirming, setConfirming] = useState(false);
  const [understood, setUnderstood] = useState(false);
  if (!canWrite) return null;
  const state = freezeState(freeze);
  const zone = appZoneName();
  // The window as it will be sent (UTC), and in words for the confirmation.
  const from = when === "schedule" && fromLocal ? appZoneLocalToISO(fromLocal) : null;
  const until = untilLocal ? appZoneLocalToISO(untilLocal) : null;
  const words = freezeWindowWords({ from: from ?? undefined, until: until ?? undefined });
  const missing =
    reason.trim() === "" ? "Give the reason first" : when === "schedule" && !from ? "Give the time the freeze begins" : "";

  async function set(next: boolean) {
    setBusy(true);
    setErr(null);
    const body: { on: boolean; reason: string; from?: string; until?: string } = { on: next, reason: next ? reason.trim() : "" };
    if (next && from) body.from = from;
    if (next && until) body.until = until;
    const { error } = await api.PUT("/settings/freeze", { params: { header: csrfHeader }, body });
    setBusy(false);
    setConfirming(false);
    setUnderstood(false);
    if (error) {
      setErr(errMsg(error));
      return;
    }
    setReason("");
    setFromLocal("");
    setUntilLocal("");
    setWhen("now");
    window.dispatchEvent(new Event(FREEZE_CHANGED_EVENT));
  }

  const setBy = `${freeze?.setBy ? ` — set by ${freeze.setBy}` : ""}${freeze?.setAt ? ` at ${fmtInAppZone(freeze.setAt)}` : ""}`;
  const timeInput = (label: string, value: string, onChange: (v: string) => void) => (
    <label style={{ display: "flex", flexDirection: "column", gap: 4, fontSize: c.fontXs, color: c.textSec }}>
      {label} ({zone})
      <input type="datetime-local" value={value} onChange={(e) => onChange(e.target.value)} disabled={busy} style={{ ...inputStyle(), width: 220 }} />
    </label>
  );

  return (
    <div style={{ marginTop: 16 }}>
    <Card title="Installation freeze">
      <div style={{ color: c.textSec, fontSize: c.fontSm, marginBottom: 12 }}>
        While the installation is frozen, no run starts in any agency: schedules, Run, tokens, reactions, deferred runs
        and file arrivals are all skipped, with the reason. Runs already going finish. An agency freezes its own schedules
        with a freeze calendar of its own (Schedules → Calendars).
      </div>
      {freeze == null ? null : state === "in_effect" ? (
        <SettingRow label="Frozen" hint={`${freeze.reason ?? ""}${freeze.until ? ` — until ${freezeWhen(freeze.until)} (${zone})` : ""}${setBy}`}>
          <Btn onClick={() => void set(false)} disabled={busy}>
            Lift the freeze
          </Btn>
        </SettingRow>
      ) : state === "scheduled" ? (
        <SettingRow label="Scheduled" hint={`${freezeWindowWords(freeze)}: ${freeze.reason ?? ""}${setBy}`}>
          <Btn onClick={() => void set(false)} disabled={busy}>
            Cancel the scheduled freeze
          </Btn>
        </SettingRow>
      ) : (
        <SettingRow label="Not frozen" hint="The reason is shown on every page while the freeze is on, and recorded on every fire it skips.">
          <div style={{ display: "flex", flexDirection: "column", gap: 10 }}>
            <div style={{ display: "flex", gap: 8 }}>
              <input
                aria-label="Reason for the freeze"
                value={reason}
                onChange={(e) => setReason(e.target.value)}
                placeholder="Year-end change freeze"
                disabled={busy}
                maxLength={500}
                style={{ ...inputStyle(), flex: 1 }}
              />
              <Btn danger onClick={() => setConfirming(true)} disabled={busy || !!missing} title={missing || undefined}>
                {when === "schedule" ? "Schedule the freeze" : "Freeze every agency's schedules"}
              </Btn>
            </div>
            <div role="radiogroup" aria-label="When the freeze begins" style={{ display: "flex", gap: 16, fontSize: c.fontSm, color: c.textSec }}>
              <label style={{ display: "inline-flex", alignItems: "center", gap: 6 }}>
                <input type="radio" name="freeze-when" checked={when === "now"} onChange={() => setWhen("now")} disabled={busy} />
                Now
              </label>
              <label style={{ display: "inline-flex", alignItems: "center", gap: 6 }}>
                <input type="radio" name="freeze-when" checked={when === "schedule"} onChange={() => setWhen("schedule")} disabled={busy} />
                Schedule…
              </label>
            </div>
            <div style={{ display: "flex", gap: 12, flexWrap: "wrap" }}>
              {when === "schedule" && timeInput("Begins", fromLocal, setFromLocal)}
              {timeInput("Ends (optional: until lifted)", untilLocal, setUntilLocal)}
            </div>
          </div>
        </SettingRow>
      )}
      {err && <div style={{ color: c.danger, fontSize: c.fontSm, marginTop: 8 }}>{err}</div>}
    </Card>

    {confirming && (
      <Modal
        title={when === "schedule" ? "Schedule a freeze of the whole installation" : "Freeze the whole installation"}
        onClose={() => {
          setConfirming(false);
          setUnderstood(false);
        }}
        footer={
          <div style={{ display: "flex", justifyContent: "flex-end", gap: 8 }}>
            <Btn
              onClick={() => {
                setConfirming(false);
                setUnderstood(false);
              }}
              disabled={busy}
            >
              Cancel
            </Btn>
            <Btn danger onClick={() => void set(true)} disabled={busy || !understood} title={understood ? undefined : "Tick the statement first"}>
              {busy ? "Freezing…" : when === "schedule" ? "Schedule the freeze" : "Freeze the installation"}
            </Btn>
          </div>
        }
      >
        <AlertBanner type="danger">
          {when === "now" && !until ? (
            <>
              <strong>No run will start in any agency until a global administrator lifts the freeze.</strong> This is
              every agency's work, not only one agency's.
            </>
          ) : (
            <>
              <strong>No run will start in any agency {words}.</strong> This is every agency's work, not only one agency's.
            </>
          )}
        </AlertBanner>
        <ul style={{ fontSize: c.fontSm, color: c.textSec, lineHeight: 1.6, margin: "12px 0", paddingLeft: 18 }}>
          <li>Scheduled fires, reactions, deferred runs and file arrivals are skipped, each with the reason.</li>
          <li>Run and service-account tokens are refused too, for everyone.</li>
          <li>Runs already going finish.</li>
          <li>Every person sees a banner with the reason, your name and the time, on every page{when === "schedule" ? ", from now" : ""}.</li>
        </ul>
        <div style={{ fontSize: c.fontSm, color: c.textSec, marginBottom: 6 }}>
          When: <strong data-testid="freeze-window-words" style={{ color: c.text }}>{words}</strong>
        </div>
        <div style={{ fontSize: c.fontSm, color: c.textSec, marginBottom: 12 }}>
          Reason: <strong style={{ color: c.text }}>{reason.trim()}</strong>
        </div>
        <label style={{ display: "flex", gap: 8, alignItems: "flex-start", fontSize: c.fontSm }}>
          <input type="checkbox" checked={understood} onChange={(e) => setUnderstood(e.target.checked)} disabled={busy} />
          <span>I understand that every agency's runs stop while the freeze is in effect.</span>
        </label>
      </Modal>
    )}
    </div>
  );
}

import { useState } from "react";
import { api } from "../../api/client";
import { c } from "../../theme";
import { fmtInAppZone } from "../../utils/datetime";
import { FREEZE_CHANGED_EVENT, useInstallationFreeze } from "../../components/InstallationFreezeBanner";
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
export function InstallationFreezeCard({ canWrite }: { canWrite: boolean }) {
  const freeze = useInstallationFreeze();
  const [reason, setReason] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [confirming, setConfirming] = useState(false);
  const [understood, setUnderstood] = useState(false);
  if (!canWrite) return null;
  const on = !!freeze?.on;

  async function set(next: boolean) {
    setBusy(true);
    setErr(null);
    const { error } = await api.PUT("/settings/freeze", { params: { header: csrfHeader }, body: { on: next, reason: next ? reason.trim() : "" } });
    setBusy(false);
    setConfirming(false);
    setUnderstood(false);
    if (error) {
      setErr(errMsg(error));
      return;
    }
    setReason("");
    window.dispatchEvent(new Event(FREEZE_CHANGED_EVENT));
  }

  return (
    <div style={{ marginTop: 16 }}>
    <Card title="Installation freeze">
      <div style={{ color: c.textSec, fontSize: c.fontSm, marginBottom: 12 }}>
        While the installation is frozen, no run starts in any agency: schedules, Run, tokens, reactions, deferred runs
        and file arrivals are all skipped, with the reason. Runs already going finish. An agency freezes its own schedules
        with a global calendar of its own (Schedules → Calendars).
      </div>
      {freeze == null ? null : on ? (
        <SettingRow label="Frozen" hint={`${freeze.reason ?? ""}${freeze.setBy ? ` — set by ${freeze.setBy}` : ""}${freeze.setAt ? ` at ${fmtInAppZone(freeze.setAt)}` : ""}`}>
          <Btn onClick={() => void set(false)} disabled={busy}>
            Lift the freeze
          </Btn>
        </SettingRow>
      ) : (
        <SettingRow label="Not frozen" hint="The reason is shown on every page while the freeze is on, and recorded on every fire it skips.">
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
            <Btn
              danger
              onClick={() => setConfirming(true)}
              disabled={busy || reason.trim() === ""}
              title={reason.trim() === "" ? "Give the reason first" : undefined}
            >
              Freeze every agency's schedules
            </Btn>
          </div>
        </SettingRow>
      )}
      {err && <div style={{ color: c.danger, fontSize: c.fontSm, marginTop: 8 }}>{err}</div>}
    </Card>

    {confirming && (
      <Modal
        title="Freeze the whole installation"
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
              {busy ? "Freezing…" : "Freeze the installation"}
            </Btn>
          </div>
        }
      >
        <AlertBanner type="danger">
          <strong>No run will start in any agency until a global administrator lifts the freeze.</strong> This is every
          agency's work, not only Global's.
        </AlertBanner>
        <ul style={{ fontSize: c.fontSm, color: c.textSec, lineHeight: 1.6, margin: "12px 0", paddingLeft: 18 }}>
          <li>Scheduled fires, reactions, deferred runs and file arrivals are skipped, each with the reason.</li>
          <li>Run and service-account tokens are refused too, for everyone.</li>
          <li>Runs already going finish.</li>
          <li>Every person sees a banner with the reason, your name and the time, on every page.</li>
        </ul>
        <div style={{ fontSize: c.fontSm, color: c.textSec, marginBottom: 12 }}>
          Reason: <strong style={{ color: c.text }}>{reason.trim()}</strong>
        </div>
        <label style={{ display: "flex", gap: 8, alignItems: "flex-start", fontSize: c.fontSm }}>
          <input type="checkbox" checked={understood} onChange={(e) => setUnderstood(e.target.checked)} disabled={busy} />
          <span>I understand that every agency's runs stop until the freeze is lifted.</span>
        </label>
      </Modal>
    )}
    </div>
  );
}

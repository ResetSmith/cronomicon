import { useState } from "react";
import { api } from "../../api/client";
import { c } from "../../theme";
import { fmtInAppZone } from "../../utils/datetime";
import { FREEZE_CHANGED_EVENT, useInstallationFreeze } from "../../components/InstallationFreezeBanner";
import { Btn, Card, SettingRow, csrfHeader, errMsg, inputStyle } from "./ui";

// 2.5.0 C11, defect 84 (question 105): the switch for the freeze of the whole
// installation. It stops every run from starting, in every agency: schedules,
// Run, tokens, reactions, deferred runs and file arrivals (the gate matrix's
// globalFreeze, FX-Q2: a freeze a click can walk through is not one; C12,
// defect 93, corrected the copy that said manual runs go on). It takes a
// reason, which every skipped run records. A global administrator's;
// anyone else reads it. Until C11 only `PUT /settings/freeze` set it, though the
// changelog and the manual said Settings.
export function InstallationFreezeCard({ canWrite }: { canWrite: boolean }) {
  const freeze = useInstallationFreeze();
  const [reason, setReason] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const on = !!freeze?.on;

  async function set(next: boolean) {
    setBusy(true);
    setErr(null);
    const { error } = await api.PUT("/settings/freeze", { params: { header: csrfHeader }, body: { on: next, reason: next ? reason.trim() : "" } });
    setBusy(false);
    if (error) {
      setErr(errMsg(error));
      return;
    }
    setReason("");
    window.dispatchEvent(new Event(FREEZE_CHANGED_EVENT));
  }

  return (
    <div style={{ marginBottom: 16 }}>
    <Card title="Installation freeze">
      <div style={{ color: c.textSec, fontSize: c.fontSm, marginBottom: 12 }}>
        While the installation is frozen, no run starts in any agency: schedules, Run, tokens, reactions, deferred runs
        and file arrivals are all skipped, with the reason. Runs already going finish. An agency freezes its own schedules
        with a global calendar of its own (Schedules → Calendars).
      </div>
      {freeze == null ? null : on ? (
        <SettingRow label="Frozen" hint={`${freeze.reason ?? ""}${freeze.setBy ? ` — set by ${freeze.setBy}` : ""}${freeze.setAt ? ` at ${fmtInAppZone(freeze.setAt)}` : ""}`}>
          <Btn onClick={() => void set(false)} disabled={!canWrite || busy} title={canWrite ? undefined : "Only a global administrator lifts the freeze."}>
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
              disabled={!canWrite || busy}
              maxLength={500}
              style={{ ...inputStyle(), flex: 1 }}
            />
            <Btn
              primary
              onClick={() => void set(true)}
              disabled={!canWrite || busy || reason.trim() === ""}
              title={!canWrite ? "Only a global administrator freezes the installation." : reason.trim() === "" ? "Give the reason first" : undefined}
            >
              Freeze every agency's schedules
            </Btn>
          </div>
        </SettingRow>
      )}
      {err && <div style={{ color: c.danger, fontSize: c.fontSm, marginTop: 8 }}>{err}</div>}
    </Card>
    </div>
  );
}

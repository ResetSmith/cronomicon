import { useState } from "react";
import { Link } from "react-router-dom";
import { api } from "../../api/client";
import type { components } from "../../api/schema";
import { useGet } from "../../hooks";
import { c } from "../../theme";
import { AlertBanner, ConfirmDialog, InlineLoading } from "../../components/ui";
import { globalOnly } from "../../api/globalAdmin";
import { Btn, Card, SettingRow, csrfHeader, errMsg, inputStyle } from "./ui";
import { globalAgencyName } from "../../globalName";

type LocalRunner = components["schemas"]["LocalRunner"];

// The Local runner card (LR-1, LR-17, LR-43, LR-44; v2.3.0).
//
// The server can run shell jobs itself, over SSH, from inside its own process.
// That is the LOCAL RUNNER: one row in the runner list, Global's, off unless it
// is turned on here. Turning it on makes this server hold SSH private keys and
// open connections to job targets, so both directions are confirmed, and each
// says what changes for runs that are queued or running.
//
// It serves Global and nothing else since 2.4.0 (§0a, IN-3; migration 1380):
// the server refuses any other serve list (422 local_runner_global_only), so
// the card shows it and offers nothing to change (R8). Until then it was the
// one runner whose serve list was chosen here (MA-14).
//
// `canWrite` — a global administrator for configureApp. The section is only
// listed for one; the flag is honoured anyway, control by control.
export function LocalRunnerSection({ canWrite }: { canWrite: boolean }) {
  const why = globalOnly(canWrite);
  const [bump, setBump] = useState(0);
  const q = useGet<LocalRunner>(() => api.GET("/local-runner"), [bump]);
  const [confirm, setConfirm] = useState<"on" | "off" | null>(null);
  const [saving, setSaving] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [conc, setConc] = useState<string | null>(null);
  const busy = saving || q.loading;

  if (q.loading && !q.data) return <InlineLoading what="the local runner" />;
  if (q.error || !q.data) {
    return <AlertBanner type="danger">Could not read the local runner: {q.error ?? "no data"}</AlertBanner>;
  }
  const lr = q.data;
  const serves = lr.serves ?? [];

  const put = async (body: { enabled?: boolean; maxConcurrent?: number }) => {
    setSaving(true);
    setErr(null);
    const { error } = await api.PUT("/local-runner", { params: { header: csrfHeader }, body });
    setSaving(false);
    setConfirm(null);
    if (error) setErr(errMsg(error));
    else setConc(null);
    // Read it back either way: a refusal can still have saved the setting
    // (500 apply_failed), and the card must not go on showing the old state.
    setBump((n) => n + 1);
  };
  const concValue = conc ?? String(lr.maxConcurrent);
  const concNum = Number(concValue);
  const concOk = Number.isInteger(concNum) && concNum >= 1 && concNum <= 64;
  const state = lr.forbidden ? "Forbidden on this host" : lr.enabled ? "On" : "Off";
  const stateColor = lr.forbidden ? c.textSec : lr.enabled ? c.success : c.textSec;

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 16 }}>
      {err && (
        <AlertBanner type="danger" onDismiss={() => setErr(null)}>
          {err}
        </AlertBanner>
      )}
      <Card title="Local runner">
        <p style={{ margin: "0 0 12px", fontSize: c.fontSm, color: c.textSec, lineHeight: 1.6 }}>
          This server can run shell jobs itself (bash, perl, powershell, python), connecting to the target hosts over SSH. It
          is listed with the other runners and claims what it is placed to serve, like them. It has no Ansible or Terraform:
          those need a runner agent. Turned on, <strong style={{ color: c.text }}>this server holds SSH private keys and opens
          connections to job targets</strong>.
        </p>
        <SettingRow label="State" hint={lr.forbidden ? "CRONOMICON_LOCAL_RUNNER=forbid is set on the host, so it cannot be turned on here." : undefined}>
          <div style={{ display: "flex", alignItems: "center", gap: 12, flexWrap: "wrap" }}>
            <span style={{ fontWeight: 600, color: stateColor }}>{state}</span>
            {!lr.forbidden &&
              (lr.enabled ? (
                <Btn onClick={() => setConfirm("off")} disabled={busy || !!why} title={why || undefined}>
                  Turn off…
                </Btn>
              ) : (
                <Btn primary onClick={() => setConfirm("on")} disabled={busy || !!why} title={why || undefined}>
                  Turn on…
                </Btn>
              ))}
            <Link to="/runners" style={{ fontSize: c.fontSm, color: c.primary }}>
              See it on Runners
            </Link>
          </div>
        </SettingRow>
        <SettingRow label="Runs at once" hint="How many runs it executes at the same time. Each run opens SSH sessions from this server.">
          <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
            <input
              aria-label="Runs at once"
              type="number"
              min={1}
              max={64}
              value={concValue}
              disabled={!!why}
              title={why || undefined}
              onChange={(e) => setConc(e.target.value)}
              style={{ ...inputStyle(), width: 90 }}
            />
            {conc !== null && concValue !== String(lr.maxConcurrent) && (
              <Btn
                primary
                onClick={() => put({ maxConcurrent: concNum })}
                disabled={busy || !concOk || !!why}
                title={!concOk ? "A whole number from 1 to 64" : why || undefined}
              >
                Save
              </Btn>
            )}
          </div>
        </SettingRow>
        <SettingRow
          label="Serves"
          hint="Global: the jobs with no scope and Global's own scopes. An agency's runs are claimed by an agent enrolled for that agency. Narrow a scope to particular runners by binding it, on the Scopes page."
          last
        >
          <span style={{ fontWeight: 600, color: c.text }}>{serves.map((a) => a.name).join(", ") || globalAgencyName()}</span>
        </SettingRow>
      </Card>

      {confirm === "on" && (
        <ConfirmDialog
          title="Turn the local runner on?"
          danger={false}
          busy={busy}
          message={
            <>
              This server will hold SSH private keys in memory and open connections to job targets. It starts claiming at once:
              queued shell runs of {serves.map((a) => a.name).join(", ") || globalAgencyName()} that no other runner has taken
              will run from here. The change is recorded with your name.
            </>
          }
          confirmLabel="Turn on"
          onCancel={() => setConfirm(null)}
          onConfirm={() => put({ enabled: true })}
        />
      )}
      {confirm === "off" && (
        <ConfirmDialog
          title="Turn the local runner off?"
          busy={busy}
          message={
            <>
              Runs it is running now will finish. It claims nothing more: queued runs it would have taken wait until it is turned
              on again or another runner takes them, and a scope bound only to it stays closed. Nothing is failed. The change is
              recorded with your name.
            </>
          }
          confirmLabel="Turn off"
          onCancel={() => setConfirm(null)}
          onConfirm={() => put({ enabled: false })}
        />
      )}
    </div>
  );
}

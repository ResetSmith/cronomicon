import { useState } from "react";
import { Link } from "react-router-dom";
import { api } from "../../api/client";
import type { components } from "../../api/schema";
import { GLOBAL_AGENCY } from "../../api/access";
import { useGet } from "../../hooks";
import { c } from "../../theme";
import { AlertBanner, ConfirmDialog, InlineLoading } from "../../components/ui";
import { globalOnly } from "../../api/globalAdmin";
import { Btn, Card, SettingRow, csrfHeader, errMsg, inputStyle } from "./ui";

type LocalRunner = components["schemas"]["LocalRunner"];
type Agency = { id: string; name: string };

// The Local runner card (LR-1, LR-17, LR-43, LR-44; v2.3.0).
//
// The server can run shell jobs itself, over SSH, from inside its own process.
// That is the LOCAL RUNNER: one row in the runner list, off unless it is turned
// on here. Turning it on makes this server hold SSH private keys and open
// connections to job targets, so both directions are confirmed, and each says
// what changes for runs that are queued or running.
//
// 2.5.3 (CA-8 to CA-10): it belongs to one agency, chosen here, and serves that
// agency alone; or to none ("Unassigned"), and is off and cannot be turned on
// (422 local_runner_unassigned). Changing its agency is refused while it runs a
// run (local_runner_busy) or while scopes are bound to it (local_runner_bound),
// clears its host-key approvals (the new agency approves the keys of its own
// hosts), and turns it off.
//
// `canWrite` — a global administrator for configureApp. The section is only
// listed for one; the flag is honoured anyway, control by control.
export function LocalRunnerSection({ canWrite }: { canWrite: boolean }) {
  const why = globalOnly(canWrite);
  const [bump, setBump] = useState(0);
  const q = useGet<LocalRunner>(() => api.GET("/local-runner"), [bump]);
  const agenciesQ = useGet<Agency[]>(() => api.GET("/agencies"), []);
  const [confirm, setConfirm] = useState<"on" | "off" | null>(null);
  const [moveTo, setMoveTo] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [conc, setConc] = useState<string | null>(null);
  const busy = saving || q.loading;

  if (q.loading && !q.data) return <InlineLoading what="the local runner" />;
  if (q.error || !q.data) {
    return <AlertBanner type="danger">Could not read the local runner: {q.error ?? "no data"}</AlertBanner>;
  }
  const lr = q.data;
  const agencies = (Array.isArray(agenciesQ.data) ? agenciesQ.data : []).filter((a) => a.id !== GLOBAL_AGENCY);
  const owner = lr.owner && lr.owner.id !== GLOBAL_AGENCY ? lr.owner : null;
  const ownerName = owner?.name ?? "";
  const nameOf = (id: string) => agencies.find((a) => a.id === id)?.name ?? id;

  const put = async (body: { ownerAgency?: string; enabled?: boolean; maxConcurrent?: number }) => {
    setSaving(true);
    setErr(null);
    const { error } = await api.PUT("/local-runner", { params: { header: csrfHeader }, body: body as never });
    setSaving(false);
    setConfirm(null);
    setMoveTo(null);
    if (error) setErr(errMsg(error));
    else setConc(null);
    // Read it back either way: a refusal can still have saved the setting
    // (500 apply_failed), and the card must not go on showing the old state.
    setBump((n) => n + 1);
  };
  const concValue = conc ?? String(lr.maxConcurrent);
  const concNum = Number(concValue);
  const concOk = Number.isInteger(concNum) && concNum >= 1 && concNum <= 64;
  const state = lr.forbidden ? "Forbidden on this host" : lr.enabled ? "On" : owner ? "Off" : "Off: it belongs to no agency";
  const stateColor = lr.forbidden ? c.textSec : lr.enabled ? c.success : c.textSec;
  const onWhy = why || (!owner ? "Choose its agency first: with no agency it is off." : "");

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
          belongs to one agency and claims that agency's shell runs, like an agent of it. It has no Ansible or Terraform:
          those need a runner agent. Turned on, <strong style={{ color: c.text }}>this server holds SSH private keys and opens
          connections to job targets</strong>.
        </p>
        <SettingRow
          label="Agency"
          hint="The agency whose shell runs it claims. Its administrators approve the host keys of its hosts. With no agency it is off and cannot be turned on."
        >
          <select
            aria-label="Agency"
            value={owner?.id ?? ""}
            disabled={busy || !!why}
            title={why || undefined}
            onChange={(e) => setMoveTo(e.target.value)}
            style={{ ...inputStyle(), width: 240 }}
          >
            <option value="">Unassigned (off)</option>
            {owner && !agencies.some((a) => a.id === owner.id) && <option value={owner.id}>{owner.name}</option>}
            {agencies.map((a) => (
              <option key={a.id} value={a.id}>
                {a.name}
              </option>
            ))}
          </select>
        </SettingRow>
        <SettingRow label="State" hint={lr.forbidden ? "CRONOMICON_LOCAL_RUNNER=forbid is set on the host, so it cannot be turned on here." : undefined}>
          <div style={{ display: "flex", alignItems: "center", gap: 12, flexWrap: "wrap" }}>
            <span style={{ fontWeight: 600, color: stateColor }}>{state}</span>
            {!lr.forbidden &&
              (lr.enabled ? (
                <Btn onClick={() => setConfirm("off")} disabled={busy || !!why} title={why || undefined}>
                  Turn off…
                </Btn>
              ) : (
                <Btn primary onClick={() => setConfirm("on")} disabled={busy || !!onWhy} title={onWhy || undefined}>
                  Turn on…
                </Btn>
              ))}
            <Link to="/runners" style={{ fontSize: c.fontSm, color: c.primary }}>
              See it on Runners
            </Link>
          </div>
        </SettingRow>
        <SettingRow label="Runs at once" hint="How many runs it executes at the same time. Each run opens SSH sessions from this server." last>
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
      </Card>

      {moveTo !== null && (
        <ConfirmDialog
          title={moveTo ? `Give the local runner to ${nameOf(moveTo)}?` : "Take the local runner from its agency?"}
          danger={false}
          busy={busy}
          message={
            moveTo ? (
              <>
                It will claim {nameOf(moveTo)}'s shell runs{owner ? ` instead of ${ownerName}'s` : ""}. Its host-key approvals are
                cleared, so {nameOf(moveTo)}'s administrators approve the keys of its hosts, and it is turned off: turn it on again
                when they are ready. It is refused while it is running a run, or while scopes are bound to it. The change is
                recorded with your name.
              </>
            ) : (
              <>
                It will belong to no agency, and is turned off: with no agency it cannot run. Its host-key approvals are cleared.
                It is refused while it is running a run, or while scopes are bound to it. The change is recorded with your name.
              </>
            )
          }
          confirmLabel={moveTo ? `Give it to ${nameOf(moveTo)}` : "Unassign it"}
          onCancel={() => setMoveTo(null)}
          onConfirm={() => put({ ownerAgency: moveTo })}
        />
      )}
      {confirm === "on" && (
        <ConfirmDialog
          title="Turn the local runner on?"
          danger={false}
          busy={busy}
          message={
            <>
              This server will hold SSH private keys in memory and open connections to job targets. It starts claiming at once:
              queued shell runs of {ownerName} that no other runner has taken will run from here. The change is recorded with
              your name.
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

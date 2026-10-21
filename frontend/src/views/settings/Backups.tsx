import { useState } from "react";
import { api } from "../../api/client";
import type { components } from "../../api/schema";
import { useGet } from "../../hooks";
import { c } from "../../theme";
import { AlertBanner, ConfirmDialog, InlineLoading, Modal } from "../../components/ui";
import { globalOnly } from "../../api/globalAdmin";
import { fmtInAppZone } from "../../utils/datetime";
import { Btn, Card, SettingRow, csrfHeader, errMsg, inputStyle } from "./ui";

type BackupSettings = components["schemas"]["BackupSettings"];

// Settings → Backups (2.5.1 SG1.4).
//
// The nightly backup can be encrypted with age to public keys pasted here. It
// is optional (SG-4): with no key, backups are written unencrypted, as before,
// and nothing here warns about it. The server holds public keys only, so it can
// seal a backup and never open one; opening takes a private key that is kept
// off the server. That is the whole point, and the whole risk: if every
// private key is lost, every encrypted backup is unreadable. So turning
// encryption on is a loud step (SG-8): a danger banner and three statements to
// tick before the button works, and the server refuses the first key without
// the acknowledgement too. While only one key is set, a warning stays on the
// card. Adding another key is a plain confirmation showing the whole key, to
// compare with what age-keygen printed (question 125).
export function BackupsSection({ canWrite }: { canWrite: boolean }) {
  const why = globalOnly(canWrite);
  const [bump, setBump] = useState(0);
  const q = useGet<BackupSettings>(() => api.GET("/settings/backup"), [bump]);
  const [draft, setDraft] = useState("");
  const [adding, setAdding] = useState(false);
  const [pending, setPending] = useState<{ kind: "first" | "add" | "remove"; key: string } | null>(null);
  const [ticks, setTicks] = useState([false, false, false]);
  const [saving, setSaving] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  if (q.loading && !q.data) return <InlineLoading what="the backup settings" />;
  if (q.error || !q.data) return <AlertBanner type="danger">Could not read the backup settings: {q.error ?? "no data"}</AlertBanner>;
  const s = q.data;
  const keys = s.recipients ?? [];

  const put = async (recipients: string[], extra: { acknowledgedKeyCustody?: boolean; confirmUnencrypted?: boolean }) => {
    setSaving(true);
    setErr(null);
    const { error } = await api.PUT("/settings/backup", { params: { header: csrfHeader }, body: { recipients, ...extra } });
    setSaving(false);
    setPending(null);
    setTicks([false, false, false]);
    if (error) {
      setErr(errMsg(error));
      return;
    }
    setDraft("");
    setAdding(false);
    setBump((n) => n + 1);
  };

  const proposeAdd = () => {
    const key = draft.trim();
    if (!key) return;
    setErr(null);
    setPending({ kind: keys.length === 0 ? "first" : "add", key });
  };
  const confirmPending = () => {
    if (!pending) return;
    const current = keys.map((k) => k.key);
    if (pending.kind === "first") void put([pending.key], { acknowledgedKeyCustody: true });
    else if (pending.kind === "add") void put([...current, pending.key], {});
    else {
      const next = current.filter((k) => k !== pending.key);
      void put(next, next.length === 0 ? { confirmUnencrypted: true } : {});
    }
  };

  const last = s.lastBackup;
  const fpOf = (key: string) => keys.find((k) => k.key === key)?.fingerprint ?? "";
  // The local copies only this key opens (SG-8): removing it does not change
  // them, but its private key is then the only way into them.
  const onlyThisKey = (fp: string) => (s.localBackups ?? []).filter((b) => b.recipients.length === 1 && b.recipients[0] === fp);

  return (
    <div>
      <div style={{ marginBottom: 16 }}>
        <Card title="Backups">
          <SettingRow label="Schedule" hint="The daily backup, after the retention sweep. Set by CRONOMICON_BACKUP_AT.">
            <span style={{ fontFamily: c.mono, fontVariantNumeric: "tabular-nums" }}>{s.schedule} UTC</span>
          </SettingRow>
          <SettingRow label="Kept" hint={`On the server's volume for ${s.localKeepDays} days, in ${s.localDirectory}.`}>
            <span>{s.bucket ? <>Uploaded to the bucket <code style={{ fontFamily: c.mono }}>{s.bucket}</code></> : "On the server only (no bucket is configured)"}</span>
          </SettingRow>
          <SettingRow label="Last backup" last>
            {last ? (
              <span>
                <code style={{ fontFamily: c.mono }}>{last.file}</code>
                {last.at ? <> at {fmtInAppZone(last.at)}</> : null} —{" "}
                {last.recipients.length > 0 ? `encrypted to ${last.recipients.length === 1 ? "1 key" : `${last.recipients.length} keys`} (${last.recipients.join(", ")})` : "not encrypted"}
              </span>
            ) : s.lastSuccessAt ? (
              <span>{fmtInAppZone(s.lastSuccessAt)}</span>
            ) : (
              <span style={{ color: c.textSec }}>None yet</span>
            )}
          </SettingRow>
        </Card>
      </div>

      <Card title="Encryption">
        {keys.length === 0 ? (
          <div style={{ color: c.textSec, fontSize: c.fontSm, lineHeight: 1.6, marginBottom: 12 }}>
            Backups are <strong>not encrypted</strong>. Encryption is optional: with it, each backup is encrypted to the public keys
            you add here, and only their private keys open it. The private keys never go to this server.
          </div>
        ) : (
          <>
            {keys.length === 1 && (
              <div style={{ marginBottom: 12 }}>
                <AlertBanner type="warning">
                  One key is set. If its private key is lost, every encrypted backup is unreadable. Add a second key, held by
                  another person.
                </AlertBanner>
              </div>
            )}
            <div style={{ color: c.textSec, fontSize: c.fontSm, marginBottom: 8 }}>
              Each backup is encrypted to every key below; any one of their private keys opens it.
              {s.lastModifiedBy ? ` Last changed by ${s.lastModifiedBy}${s.lastModifiedAt ? ` at ${fmtInAppZone(s.lastModifiedAt)}` : ""}.` : ""}
            </div>
            {keys.map((k) => (
              <SettingRow key={k.key} label={k.fingerprint} hint={k.key}>
                <Btn small danger onClick={() => setPending({ kind: "remove", key: k.key })} disabled={!!why || saving} title={why}>
                  Remove
                </Btn>
              </SettingRow>
            ))}
          </>
        )}

        {adding || keys.length > 0 ? (
          <div style={{ marginTop: 12 }}>
            <div style={{ color: c.textSec, fontSize: c.fontSm, lineHeight: 1.6, marginBottom: 8 }}>
              Generate the key on your own machine, never on this server: <code style={{ fontFamily: c.mono }}>age-keygen -o backup-key.txt</code>.
              Paste the public key it prints (<code style={{ fontFamily: c.mono }}>age1…</code>). Keep the private key file off this
              server, apart from the KEK and apart from the backup bucket: restoring needs both the private key and the KEK.
            </div>
            <div style={{ display: "flex", gap: 8 }}>
              <input
                aria-label="Public key to add"
                value={draft}
                onChange={(e) => setDraft(e.target.value)}
                placeholder="age1…"
                disabled={!!why || saving}
                style={{ ...inputStyle(), flex: 1, fontFamily: c.mono }}
              />
              <Btn primary onClick={proposeAdd} disabled={!!why || saving || draft.trim() === ""} title={why || (draft.trim() === "" ? "Paste a public key first" : undefined)}>
                Add key
              </Btn>
            </div>
          </div>
        ) : (
          <Btn primary onClick={() => setAdding(true)} disabled={!!why} title={why}>
            Encrypt backups
          </Btn>
        )}
        {err && <div style={{ color: c.danger, fontSize: c.fontSm, marginTop: 8 }}>{err}</div>}
      </Card>

      {pending?.kind === "first" && (
        <Modal
          title="Turn on encrypted backups"
          onClose={() => setPending(null)}
          footer={
            <div style={{ display: "flex", justifyContent: "flex-end", gap: 8 }}>
              <Btn onClick={() => setPending(null)} disabled={saving}>
                Cancel
              </Btn>
              <Btn
                danger
                onClick={confirmPending}
                disabled={saving || ticks.some((t) => !t)}
                title={ticks.some((t) => !t) ? "Tick all three statements first" : undefined}
              >
                {saving ? "Working…" : "Encrypt backups"}
              </Btn>
            </div>
          }
        >
          <AlertBanner type="danger">
            <strong>If the private key is lost, every encrypted backup is unreadable.</strong> Nobody can recover it: not
            Cronomicon, not this server, not its administrators.
          </AlertBanner>
          <div style={{ fontSize: c.fontSm, color: c.textSec, lineHeight: 1.6, margin: "12px 0" }}>
            From the next backup on, each one is encrypted to this key:
            <div style={{ fontFamily: c.mono, color: c.text, wordBreak: "break-all", margin: "6px 0" }}>{pending.key}</div>
            Check it is the public key <code style={{ fontFamily: c.mono }}>age-keygen</code> printed. Then add a second key,
            held by another person, so that losing one is not losing the backups.
          </div>
          {[
            "The private key is stored off this server.",
            "The KEK is stored apart from the private key, and apart from the backup bucket.",
            "I know a restore needs both the private key and the KEK.",
          ].map((text, i) => (
            <label key={text} style={{ display: "flex", gap: 8, alignItems: "flex-start", fontSize: c.fontSm, marginBottom: 6 }}>
              <input
                type="checkbox"
                checked={ticks[i]}
                onChange={(e) => setTicks((t) => t.map((v, j) => (j === i ? e.target.checked : v)))}
                disabled={saving}
              />
              <span>{text}</span>
            </label>
          ))}
        </Modal>
      )}
      {pending?.kind === "add" && (
        <ConfirmDialog
          title="Add a backup key"
          danger={false}
          busy={saving}
          confirmLabel="Add key"
          message={
            <>
              From the next backup on, each one is also encrypted to:
              <div style={{ fontFamily: c.mono, color: c.text, wordBreak: "break-all", margin: "6px 0" }}>{pending.key}</div>
              Check it is the public key <code style={{ fontFamily: c.mono }}>age-keygen</code> printed, and that its private key is
              kept off this server.
            </>
          }
          onConfirm={confirmPending}
          onCancel={() => setPending(null)}
        />
      )}
      {pending?.kind === "remove" && (
        <ConfirmDialog
          title={keys.length === 1 ? "Turn off encrypted backups" : "Remove a backup key"}
          busy={saving}
          confirmLabel={keys.length === 1 ? "Remove the last key" : "Remove key"}
          message={
            <>
              {keys.length === 1 ? (
                <p style={{ marginTop: 0 }}>This is the last key: from the next backup on, backups are written unencrypted.</p>
              ) : (
                <p style={{ marginTop: 0 }}>From the next backup on, backups are not encrypted to this key.</p>
              )}
              <p>
                The backups already made stay as they are. Those encrypted to <strong>this key alone</strong> open only with its
                private key: keep it while you may need them.
                {onlyThisKey(fpOf(pending.key)).length > 0
                  ? ` ${onlyThisKey(fpOf(pending.key)).length} of the copies on this server are (${onlyThisKey(fpOf(pending.key))
                      .map((b) => b.file)
                      .join(", ")}), and the bucket's copies of the same days are too.`
                  : " None of the copies on this server is; check the bucket's older copies."}
              </p>
            </>
          }
          onConfirm={confirmPending}
          onCancel={() => setPending(null)}
        />
      )}
    </div>
  );
}

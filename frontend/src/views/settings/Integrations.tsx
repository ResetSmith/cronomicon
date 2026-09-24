import { useState } from "react";
import { api } from "../../api/client";
import type { components } from "../../api/schema";
import { c } from "../../theme";
import { Btn, Card, SaveBtn, SettingRow, Status, Toggle, csrfHeader, errMsg, fmtDateTime, inputStyle, useSettingForm } from "./ui";
import { Rule } from "../../components/ui";
import { fmtInAppZone } from "../../utils/datetime";

type GitlabConfig = components["schemas"]["GitlabConfig"];
type VaultConfig = components["schemas"]["VaultConfig"];
type LogStorageConfig = components["schemas"]["LogStorageConfig"];
type ObservabilityConfig = components["schemas"]["ObservabilityConfig"];

const wide = (): React.CSSProperties => ({ ...inputStyle(), width: 280 });

// G-1 (A-19) — Vault's status arrives as a wire enum (ok | degraded |
// unconfigured) and was rendered raw, so a settings page read `unconfigured` in
// an app that shows an operator no other wire value anywhere. These are the
// words for it. Not statusLabel's business: this is a connection state, not a
// run result, and folding an unrelated vocabulary into that function is how the
// two would start drifting into each other.
const VAULT_STATUS: Record<string, string> = {
  ok: "Connected",
  degraded: "Degraded",
  unconfigured: "Not configured",
};

// useSettingForm / SaveBtn / Status moved to ./ui when the retention card became
// their third consumer — same behaviour, one copy.

// ── GitLab Connection ────────────────────────────────────────────────────────
export function GitlabSection() {
  const s = useSettingForm<GitlabConfig>(() => api.GET("/settings/gitlab"), (body) =>
    api.PUT("/settings/gitlab", { params: { header: csrfHeader }, body }),
  );
  // PAT is masked on read; only send when the operator types a new one.
  const [newPat, setNewPat] = useState("");
  const [rotating, setRotating] = useState(false);
  const [rotateMsg, setRotateMsg] = useState<string | null>(null);
  const { form, patch } = s;

  // LB7: the receiver URL operators paste into GitLab. Fixed route, derived from
  // wherever the UI is being served, so it's correct behind any proxy/host.
  const webhookUrl = `${window.location.origin}/api/v1/webhooks/gitlab`;
  const [copied, setCopied] = useState(false);
  async function copyWebhookUrl() {
    try {
      await navigator.clipboard.writeText(webhookUrl);
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    } catch {
      // Clipboard API unavailable (e.g. insecure context); the field stays selectable.
    }
  }

  async function rotate() {
    setRotating(true);
    setRotateMsg(null);
    const { error: err } = await api.POST("/settings/gitlab/webhook-secret/rotate", { params: { header: csrfHeader } });
    setRotating(false);
    setRotateMsg(err ? `Rotate failed: ${errMsg(err)}` : "Webhook secret rotated.");
    if (!err) s.refetch();
  }

  return (
    <Card title="GitLab Connection" action={<SaveBtn onClick={() => s.save({ ...form!, ...(newPat ? { pat: newPat } : {}) })} saving={s.saving} saved={s.saved} disabled={!form} />}>
      <Status loading={s.loading} error={s.error} saveErr={s.saveErr} />
      {form && (
        <div style={{ fontSize: c.fontSm }}>
          <SettingRow label="Repository URL">
            <input value={form.repoUrl ?? ""} onChange={(e) => patch({ repoUrl: e.target.value })} style={wide()} />
          </SettingRow>
          <SettingRow label="Bot PAT" hint="Stored masked. Leave blank to keep the current token.">
            <input type="password" value={newPat} onChange={(e) => setNewPat(e.target.value)} placeholder="•••• (set)" style={wide()} />
          </SettingRow>
          <SettingRow label="Bot Name">
            <input value={form.botName ?? ""} onChange={(e) => patch({ botName: e.target.value })} style={wide()} />
          </SettingRow>
          <SettingRow label="Bot Email">
            <input value={form.botEmail ?? ""} onChange={(e) => patch({ botEmail: e.target.value })} style={wide()} />
          </SettingRow>
          {/* G-3 (VF-12) — it was "Write Branch", which named half of what it does:
              writeBranch (gitlab/sync.go:618) resolves the branch for sync-READ
              as well as publish-write, so an operator reading the old label had
              no reason to think changing it would re-point where definitions are
              read from. VU-16 corrected the hint; the label is the other half.
              The wire field stays `writeBranch` — that is the API, not the UI. */}
          <SettingRow label="Working Branch" hint="The branch Cronomicon syncs definitions from, and writes schedule changes back to.">
            <input value={form.writeBranch ?? ""} onChange={(e) => patch({ writeBranch: e.target.value })} style={{ ...inputStyle(), width: 160 }} />
          </SettingRow>
          {/* F2-1 — "Token Expiry Notice" stood here, promising a warning this
              many days before the PAT expires. It was stored
              (tokenExpiryNotifyDays) and there is no warning code anywhere: no
              banner, no notification, no scheduled check. The field and its
              schema stay so nothing stored is lost; re-add the control with the
              feature that honours it. */}
          <SettingRow label="Webhook URL" hint="Set this as the URL in GitLab → Settings → Webhooks">
            <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
              <input readOnly value={webhookUrl} onFocus={(e) => e.target.select()} style={wide()} />
              <Btn onClick={copyWebhookUrl}>{copied ? "✓ Copied" : "Copy"}</Btn>
            </div>
          </SettingRow>
          <SettingRow label="Webhook Enabled" hint="Pick up repository changes as soon as they are pushed to GitLab.">
            <Toggle on={form.webhookEnabled ?? false} onChange={() => patch({ webhookEnabled: !form.webhookEnabled })} />
          </SettingRow>
          <SettingRow label="Webhook Events">
            <div style={{ display: "flex", gap: 14, fontSize: c.fontSm, color: c.textSec }}>
              {(["push", "mr", "tag"] as const).map((ev) => (
                <label key={ev} style={{ display: "inline-flex", alignItems: "center", gap: 5, cursor: "pointer" }}>
                  <input
                    type="checkbox"
                    checked={form.webhookEvents?.[ev] ?? false}
                    onChange={() => patch({ webhookEvents: { ...form.webhookEvents, [ev]: !form.webhookEvents?.[ev] } })}
                  />
                  {ev}
                </label>
              ))}
            </div>
          </SettingRow>
          <SettingRow label="Webhook Secret" hint="Shared secret GitLab signs deliveries with" last>
            <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
              {form.webhookSecretEnvPinned ? (
                <span style={{ fontSize: c.fontSm, color: c.textSec }}>
                  Pinned by <code style={{ fontFamily: c.mono }}>AMADEUS_GITLAB_WEBHOOK_SECRET</code> — unset it to rotate via the API.
                </span>
              ) : (
                rotateMsg && <span style={{ fontSize: c.fontSm, color: rotateMsg.startsWith("Rotate failed") ? c.danger : c.success }}>{rotateMsg}</span>
              )}
              <Btn onClick={rotate} disabled={rotating || !!form.webhookSecretEnvPinned}>{rotating ? "Rotating…" : "Rotate secret"}</Btn>
            </div>
          </SettingRow>
        </div>
      )}
    </Card>
  );
}

// ── Vault ────────────────────────────────────────────────────────────────────
export function VaultSection() {
  const s = useSettingForm<VaultConfig>(() => api.GET("/settings/vault"), (body) =>
    api.PUT("/settings/vault", { params: { header: csrfHeader }, body }),
  );
  const [newSecretId, setNewSecretId] = useState("");
  const { form, patch } = s;
  const statusColor = form?.status === "ok" ? c.success : form?.status === "degraded" ? c.warning : c.textMuted;

  return (
    <Card title="Vault" action={<SaveBtn onClick={() => s.save({ ...form!, ...(newSecretId ? { secretId: newSecretId } : {}) })} saving={s.saving} saved={s.saved} disabled={!form} />}>
      <Status loading={s.loading} error={s.error} saveErr={s.saveErr} />
      {form && (
        <div style={{ fontSize: c.fontSm }}>
          <SettingRow label="Status">
            <span style={{ display: "inline-flex", alignItems: "center", gap: 6, fontSize: c.fontSm, fontWeight: 600, color: statusColor }}>
              <span style={{ width: 7, height: 7, borderRadius: "50%", background: statusColor }} />
              {VAULT_STATUS[form.status ?? "unconfigured"] ?? "Not configured"}
            </span>
          </SettingRow>
          <SettingRow label="Vault Address">
            <input value={form.addr ?? ""} onChange={(e) => patch({ addr: e.target.value })} placeholder="https://vault.internal:8200" style={wide()} />
          </SettingRow>
          <SettingRow label="Auth Method">
            <select value={form.authMethod ?? "approle"} onChange={(e) => patch({ authMethod: e.target.value as VaultConfig["authMethod"] })} style={{ ...inputStyle(), width: 160, cursor: "pointer" }}>
              <option value="approle">approle</option>
              <option value="token">token</option>
            </select>
          </SettingRow>
          {form.authMethod !== "token" && (
            <SettingRow label="Role ID">
              <input value={form.roleId ?? ""} onChange={(e) => patch({ roleId: e.target.value })} style={wide()} />
            </SettingRow>
          )}
          {/* One write-only field, two meanings: the AppRole secret_id, or the Vault
              token itself. The server stores both in the same slot and clears it when
              the auth method changes, so say that rather than let a stale credential
              look like it still applies. */}
          <SettingRow
            label={form.authMethod === "token" ? "Token" : "Secret ID"}
            hint={`Never returned on read. Leave blank to keep the current ${form.authMethod === "token" ? "token" : "value"}. Changing the auth method requires entering it again.`}
          >
            <input type="password" value={newSecretId} onChange={(e) => setNewSecretId(e.target.value)} placeholder={form.secretIdSet ? "•••• (set)" : "not set"} style={wide()} />
          </SettingRow>
          <SettingRow label="Namespace" hint="Vault Enterprise namespace (optional)" last>
            <input value={form.namespace ?? ""} onChange={(e) => patch({ namespace: e.target.value || null })} style={wide()} />
          </SettingRow>
        </div>
      )}
    </Card>
  );
}

// ── Log Storage ──────────────────────────────────────────────────────────────
// SL-5 (the s3-logging plan). `backend: s3` is "local + S3 archive":
// local disk stays the only write target during a run, and a scheduled sync
// copies sealed logs to the bucket afterwards. The card names that contract in
// the dropdown, states the exposure the timetable creates (logs completed since
// the last sync exist only on this host's disk), and offers Sync now — disabled
// with the reason, never hidden (FX-7).
//
// Sync presets are seconds on the wire; "Daily at…" reveals an HH:MM input
// that is UTC, and the label says so (the AR-scheduler seam: name the zone,
// never leave the offset silent).
const SYNC_PRESETS: { value: string; label: string }[] = [
  { value: "300", label: "Every 5 minutes" },
  { value: "900", label: "Every 15 minutes" },
  { value: "3600", label: "Every hour" },
  { value: "21600", label: "Every 6 hours" },
  { value: "daily", label: "Daily at…" },
];

type LogSync = NonNullable<LogStorageConfig["sync"]>;

// The select's value for a stored timetable. An interval outside the presets
// (a hand-set 120s, or the server's clamp) is shown as the nearest preset at or
// above it, so the control never renders blank; saving then writes that preset.
function syncSelectValue(sync?: LogSync | null): string {
  if (!sync) return "900";
  if (sync.mode === "daily") return "daily";
  const secs = sync.intervalSeconds ?? 900;
  for (const p of SYNC_PRESETS) {
    if (p.value !== "daily" && Number(p.value) >= secs) return p.value;
  }
  return "21600";
}

export function LogStorageSection() {
  const s = useSettingForm<LogStorageConfig>(() => api.GET("/settings/log-storage"), (body) =>
    api.PUT("/settings/log-storage", { params: { header: csrfHeader }, body }),
  );
  const [newKey, setNewKey] = useState("");
  const [syncing, setSyncing] = useState(false);
  const [syncMsg, setSyncMsg] = useState<string | null>(null);
  const { form, patch } = s;

  function buildBody(): LogStorageConfig {
    if (form?.backend === "s3" && newKey) return { ...form, s3: { ...form.s3, secretKey: newKey } };
    return form!;
  }

  const sync: LogSync = form?.sync ?? { mode: "interval", intervalSeconds: 900 };
  const setSyncPreset = (v: string) => {
    if (v === "daily") patch({ sync: { ...sync, mode: "daily", at: sync.at ?? "02:00" } });
    else patch({ sync: { ...sync, mode: "interval", intervalSeconds: Number(v) } });
  };

  const isS3 = form?.backend === "s3";
  const archive = form?.archive;
  // Sync now gates on what the SERVER holds, not the unsaved form: a backend
  // switched in the dropdown but not yet saved has no store to sync with.
  const savedS3 = s.data?.backend === "s3" && !s.saving;
  const syncReason = !isS3
    ? "Select the S3 archive backend and save first"
    : archive?.inProgress
      ? "A sync is running"
      : !savedS3
        ? "Save the S3 settings first"
        : "";

  async function syncNow() {
    setSyncing(true);
    setSyncMsg(null);
    const { error: err, response } = await api.POST("/settings/log-storage/sync", { params: { header: csrfHeader } });
    setSyncing(false);
    if (err) setSyncMsg(response?.status === 409 ? "A sync is already running." : `Sync failed: ${errMsg(err)}`);
    else setSyncMsg("Sync started. Refresh the card to follow it.");
    s.refetch();
  }

  return (
    <Card title="Log Storage" action={<SaveBtn onClick={() => s.save(buildBody())} saving={s.saving} saved={s.saved} disabled={!form} />}>
      <Status loading={s.loading} error={s.error} saveErr={s.saveErr} />
      {form && (
        <div style={{ fontSize: c.fontSm }}>
          <SettingRow label="Backend">
            <select value={form.backend ?? "local"} onChange={(e) => patch({ backend: e.target.value as LogStorageConfig["backend"] })} style={{ ...inputStyle(), width: 200, cursor: "pointer" }}>
              <option value="local">Local volume</option>
              <option value="s3">Local + S3 archive</option>
            </select>
          </SettingRow>
          <SettingRow
            label="Storage Path"
            hint="Applies immediately — the next run writes here, no restart. Runs already in progress finish writing to the old path, and existing logs are not moved."
            last={!isS3}
          >
            <input value={form.local?.path ?? ""} onChange={(e) => patch({ local: { ...form.local, path: e.target.value } })} placeholder="/var/lib/amadeus/logs" style={wide()} />
          </SettingRow>
          {isS3 && (
            <>
              <SettingRow label="Endpoint" hint="host[:port]. Leave empty for AWS S3 in the region below.">
                <input value={form.s3?.endpoint ?? ""} onChange={(e) => patch({ s3: { ...form.s3, endpoint: e.target.value } })} placeholder="minio.internal:9000" style={wide()} />
              </SettingRow>
              <SettingRow label="Use SSL" hint="Off for a plain-HTTP MinIO on the LAN.">
                <Toggle on={form.s3?.useSsl ?? true} onChange={() => patch({ s3: { ...form.s3, useSsl: !(form.s3?.useSsl ?? true) } })} />
              </SettingRow>
              {(form.s3?.useSsl ?? true) && (
                <SettingRow label="CA Bundle" hint="For a private S3 node signed by an internal CA: paste the PEM certificate(s) to trust. Leave empty to use the host's trust store. Takes effect on save, no restart.">
                  <textarea
                    aria-label="CA bundle (PEM)"
                    value={form.s3?.caPem ?? ""}
                    onChange={(e) => patch({ s3: { ...form.s3, caPem: e.target.value } })}
                    placeholder={"-----BEGIN CERTIFICATE-----\n…"}
                    rows={4}
                    spellCheck={false}
                    style={{ ...inputStyle(), width: 280, fontFamily: c.mono, fontSize: c.fontXs, resize: "vertical" }}
                  />
                </SettingRow>
              )}
              <SettingRow label="Bucket">
                <input value={form.s3?.bucket ?? ""} onChange={(e) => patch({ s3: { ...form.s3, bucket: e.target.value } })} style={wide()} />
              </SettingRow>
              <SettingRow label="Region" hint="Required when Endpoint is empty.">
                <input value={form.s3?.region ?? ""} onChange={(e) => patch({ s3: { ...form.s3, region: e.target.value } })} style={{ ...inputStyle(), width: 160 }} />
              </SettingRow>
              <SettingRow label="Key Prefix">
                <input value={form.s3?.prefix ?? ""} onChange={(e) => patch({ s3: { ...form.s3, prefix: e.target.value } })} placeholder="amadeus/" style={wide()} />
              </SettingRow>
              <SettingRow label="Access Key" hint="Leave both keys empty to use the host's AWS credentials (env, profile, or IAM role).">
                <input value={form.s3?.accessKey ?? ""} onChange={(e) => patch({ s3: { ...form.s3, accessKey: e.target.value } })} style={wide()} />
              </SettingRow>
              <SettingRow label="Secret Key" hint="Leave blank to keep the current value.">
                <input type="password" value={newKey} onChange={(e) => setNewKey(e.target.value)} placeholder="•••• (set)" style={wide()} />
              </SettingRow>
              <SettingRow
                label="Sync"
                hint="Logs completed since the last sync exist only on this host's disk. Syncs never overlap — a shorter interval is safe; it just runs more often."
                last
              >
                <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
                  <select aria-label="Sync schedule" value={syncSelectValue(sync)} onChange={(e) => setSyncPreset(e.target.value)} style={{ ...inputStyle(), width: 180, cursor: "pointer" }}>
                    {SYNC_PRESETS.map((p) => (
                      <option key={p.value} value={p.value}>
                        {p.label}
                      </option>
                    ))}
                  </select>
                  {sync.mode === "daily" && (
                    <>
                      {/* A text field, not type="time": the native picker renders in the
                          BROWSER's locale (a 12-hour "02:00 AM" in en-US) for a value that is
                          24-hour UTC, and it clips at this width. The server validates HH:MM. */}
                      <input
                        aria-label="Daily sync time (UTC)"
                        value={sync.at ?? "02:00"}
                        onChange={(e) => patch({ sync: { ...sync, at: e.target.value } })}
                        placeholder="HH:MM"
                        maxLength={5}
                        style={{ ...inputStyle(), width: 72, fontFamily: c.mono, fontVariantNumeric: "tabular-nums", textAlign: "center" }}
                      />
                      <span style={{ color: c.textSec, fontSize: c.fontXs }}>UTC</span>
                    </>
                  )}
                </div>
              </SettingRow>
            </>
          )}
          {(isS3 || (archive?.count ?? 0) > 0) && (
            <LogArchiveStatusLine archive={archive} syncing={syncing} reason={syncReason} onSync={syncNow} message={syncMsg} />
          )}
          {form.stats && <LogStorageUsage stats={form.stats} archive={archive} />}
        </div>
      )}
    </Card>
  );
}

type LogArchive = NonNullable<LogStorageConfig["archive"]>;

// The archive tier's status and the Sync now button. `reason` non-empty means
// the button is disabled and the tooltip says why (FX-7: a precondition
// disables with an explanation; only irrelevance hides — and this line is only
// rendered when the tier is on or still holds objects).
function LogArchiveStatusLine({
  archive,
  syncing,
  reason,
  onSync,
  message,
}: {
  archive?: LogArchive | null;
  syncing: boolean;
  reason: string;
  onSync: () => void;
  message: string | null;
}) {
  const last = archive?.lastSyncFinishedAt
    ? `Last sync ${fmtDateTime(archive.lastSyncFinishedAt)}`
    : archive?.lastSyncStartedAt
      ? `Sync started ${fmtDateTime(archive.lastSyncStartedAt)}`
      : "Never synced";
  const pending = archive?.pending ?? 0;
  return (
    <div style={{ display: "flex", alignItems: "center", justifyContent: "space-between", gap: 12, padding: "12px 0 0", fontSize: c.fontXs, color: c.textMuted }}>
      <div style={{ lineHeight: 1.7 }}>
        <div>
          {archive?.inProgress ? "Sync running" : last}
          {" · "}
          {pending === 0 ? "nothing pending" : `${pending} pending`}
        </div>
        {archive?.lastSyncError && <div style={{ color: c.danger }}>Last error: {archive.lastSyncError}</div>}
        {message && (
          <div role="status" aria-live="polite" style={{ color: c.textSec }}>
            {message}
          </div>
        )}
      </div>
      <Btn small onClick={onSync} disabled={!!reason || syncing} title={reason || "Run one archive sync now"}>
        {syncing ? "Starting…" : "Sync now"}
      </Btn>
    </div>
  );
}

// ── Log storage usage (LU-11) ────────────────────────────────────────────────
// The headline used to be a single "N log files · X" that silently counted the
// process log, its rotated generations and the per-folder sidecars alongside run
// output. An operator asking "what is filling my disk?" got a number that could
// not distinguish 5 GB of run history from one runaway process log.
//
// So: run logs stay the headline (that is the number that tracks the retention
// window), and every other class is listed underneath — but only when it is
// actually present, so a deployment with no audit log doesn't read a permanent
// "Audit log 0 B" that looks like something is broken.
//
// SL-5: the archived tier is listed beneath a Rule, apart from the disk classes,
// because it is not part of the on-disk total and must not read as if it were.
type LogStats = NonNullable<LogStorageConfig["stats"]>;
type LogClass = NonNullable<NonNullable<LogStats["classes"]>[keyof NonNullable<LogStats["classes"]>]>;

function LogStorageUsage({ stats, archive }: { stats: LogStats; archive?: LogArchive | null }) {
  const cls = stats.classes ?? {};
  const others: { label: string; v?: LogClass }[] = [
    { label: "Process log", v: cls.processLog },
    { label: "Audit log", v: cls.auditLog },
    { label: "Other", v: cls.other },
  ];
  const present = others.filter((o) => (o.v?.fileCount ?? 0) > 0);
  const archived = archive && (archive.count ?? 0) > 0 ? archive : null;

  return (
    <div style={{ marginTop: 12, fontSize: c.fontXs, color: c.textMuted, lineHeight: 1.7 }}>
      <div>
        {stats.fileCount ?? 0} run logs · {fmtBytes(cls.runLogs?.totalSizeBytes ?? stats.totalSizeBytes)}
        {stats.oldestLogAt
          ? ` · oldest ${fmtInAppZone(stats.oldestLogAt, { year: "numeric", month: "numeric", day: "numeric" })}`
          : ""}
      </div>
      {present.length > 0 && (
        <div>
          {present.map((o, i) => (
            <span key={o.label}>
              {i > 0 ? " · " : ""}
              {o.label} {fmtBytes(o.v?.totalSizeBytes)}
            </span>
          ))}
          {" · "}
          <strong>{fmtBytes(stats.totalSizeBytes)}</strong> total
        </div>
      )}
      {archived && (
        <>
          <Rule style={{ margin: "6px 0" }} />
          <div>
            Archived (S3) {archived.count} run logs · {fmtBytes(archived.bytes)}
          </div>
        </>
      )}
    </div>
  );
}

// ── Observability ────────────────────────────────────────────────────────────
export function ObservabilitySection() {
  const s = useSettingForm<ObservabilityConfig>(() => api.GET("/settings/observability"), (body) =>
    api.PUT("/settings/observability", { params: { header: csrfHeader }, body }),
  );
  const [newToken, setNewToken] = useState("");
  const { form, patch } = s;

  return (
    <Card title="Observability" action={<SaveBtn onClick={() => s.save({ ...form!, ...(newToken ? { bearerToken: newToken } : {}) })} saving={s.saving} saved={s.saved} disabled={!form} />}>
      <Status loading={s.loading} error={s.error} saveErr={s.saveErr} />
      {form && (
        <div style={{ fontSize: c.fontSm }}>
          <SettingRow label="Metrics Enabled" hint="Let Prometheus scrape metrics from the path below. While this is off, that path returns 404.">
            <Toggle on={form.enabled ?? false} onChange={() => patch({ enabled: !form.enabled })} />
          </SettingRow>
          {/* VU-16: the restart caveat is not guessable from the field. The
              enabled/auth gate is read per scrape, but the path is resolved once
              at startup (settings.ResolveMetricsPath, wired in api/router.go). */}
          <SettingRow label="Metrics Path" hint="Takes effect after a restart.">
            <input value={form.path ?? "/metrics"} onChange={(e) => patch({ path: e.target.value })} style={{ ...inputStyle(), width: 160 }} />
          </SettingRow>
          <SettingRow label="Auth Type">
            <select value={form.authType ?? "none"} onChange={(e) => patch({ authType: e.target.value as ObservabilityConfig["authType"] })} style={{ ...inputStyle(), width: 160, cursor: "pointer" }}>
              <option value="none">None</option>
              <option value="bearer">Bearer token</option>
            </select>
          </SettingRow>
          {form.authType === "bearer" && (
            <SettingRow label="Bearer Token" hint="Masked on read. Leave blank to keep the current value." last>
              <input type="password" value={newToken} onChange={(e) => setNewToken(e.target.value)} placeholder="•••• (set)" style={wide()} />
            </SettingRow>
          )}
        </div>
      )}
    </Card>
  );
}

function fmtBytes(n?: number): string {
  if (!n) return "0 B";
  const u = ["B", "KB", "MB", "GB", "TB"];
  let i = 0;
  let v = n;
  while (v >= 1024 && i < u.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(i ? 1 : 0)} ${u[i]}`;
}

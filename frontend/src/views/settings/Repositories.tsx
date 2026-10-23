import { useCallback, useEffect, useState } from "react";
import { api } from "../../api/client";
import type { components } from "../../api/schema";
import { agenciesFor, useMyAccess } from "../../api/access";
import { c } from "../../theme";
import { Btn, Card, SettingRow, Toggle, csrfHeader, errMsg, fmtDateTime, inputStyle } from "./ui";
import { globalAgencyName } from "../../globalName";

type GitRepo = components["schemas"]["GitRepo"];
type GitlabConfig = components["schemas"]["GitlabConfig"];
type Agency = { id: string; name: string };

// Repositories (2.4.0 Phase R8; §0a IN-2; the owner's answer to question 47).
//
// Every agency has a repository of definitions, Global included. This section,
// in the Agency group, lists them: an agency's administrator sees and edits
// their own agency's, a global administrator every agency's and the hosts a
// repository may be on (gitAllowedHosts). It replaces the Installation group's
// "GitLab Connection", which edited Global's alone through a route that is
// deprecated since 2.4.0.
//
// Each repository: its connection (URL, token, branch, the bot's name and
// email, the webhook), a Test that says whether the remote answers and whether
// the token may push — and what that cannot see (a protected branch) — its
// webhook URL (one per repository), a secret rotation, and what its last sync
// had to say about its files. A repository is changed, never disconnected: it
// goes with its agency when the agency is deleted (I3).

const wide = (): React.CSSProperties => ({ ...inputStyle(), width: 300 });

export function RepositoriesSection({ isGlobal }: { isGlobal: boolean }) {
  const access = useMyAccess();
  const [repos, setRepos] = useState<GitRepo[] | null>(null);
  const [agencies, setAgencies] = useState<Agency[]>([]);
  const [err, setErr] = useState<string | null>(null);

  const load = useCallback(async () => {
    const [r, a] = await Promise.all([api.GET("/git-repos"), api.GET("/agencies")]);
    if (r.error) {
      setErr(errMsg(r.error));
      return;
    }
    setErr(null);
    setRepos(r.data?.items ?? []);
    setAgencies(((a.data ?? []) as Agency[]).map((x) => ({ id: x.id, name: x.name })));
  }, []);
  useEffect(() => {
    load();
  }, [load]);

  // The agencies the caller may connect a repository for and has none: from
  // before 2.4.0 (agency_no_repository says which). A global administrator,
  // every agency; anyone else, those they administer.
  const mine = isGlobal ? agencies : agenciesFor(access ?? null, "configureApp");
  // An agency with a row (Global's has one even unconfigured) is edited on
  // that row's card; only one with none gets a Connect card (from R8's review).
  const connected = new Set((repos ?? []).map((r) => r.agencyId));
  const unconnected = mine.filter((a) => !connected.has(a.id));

  return (
    <div>
      {err && <div role="alert" style={{ color: c.danger, fontSize: c.fontSm, marginBottom: 10 }}>{err}</div>}
      {repos == null && !err && <div style={{ color: c.textSec, fontSize: c.fontSm }}>Loading…</div>}
      {repos?.map((r) => <RepoCard key={r.id} repo={r} onSaved={load} />)}
      {unconnected.map((a) => (
        <RepoCard key={`new-${a.id}`} repo={{ id: "", agencyId: a.id, agencyName: a.name, supplies: { jobs: 0, workflows: 0, scripts: 0, schedules: 0, scopes: 0 } } as GitRepo} onSaved={load} unconnected />
      ))}
      {isGlobal && <AllowedHostsCard />}
    </div>
  );
}

function RepoCard({ repo, onSaved, unconnected }: { repo: GitRepo; onSaved: () => void; unconnected?: boolean }) {
  const [open, setOpen] = useState(!!unconnected);
  const [form, setForm] = useState<GitlabConfig>(() => ({
    repoUrl: repo.repoUrl ?? "",
    writeBranch: repo.writeBranch ?? "main",
    botName: repo.botName ?? "",
    botEmail: repo.botEmail ?? "",
    webhookEnabled: repo.webhookEnabled ?? true,
    webhookEvents: repo.webhookEvents ?? { push: true, mr: true, tag: true },
    // A write replaces every field: one left out would be reset (from R8's review).
    ...(repo.tokenExpiryNotifyDays != null ? { tokenExpiryNotifyDays: repo.tokenExpiryNotifyDays } : {}),
  }));
  const [newPat, setNewPat] = useState("");
  const [saving, setSaving] = useState(false);
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null);
  const [test, setTest] = useState<components["schemas"]["GitlabConfig"] & Record<string, unknown> | null>(null);
  const [testing, setTesting] = useState(false);
  const [secret, setSecret] = useState<string | null>(null);
  const [problems, setProblems] = useState<{ path: string; kind: string; severity: string; message: string; firstSeen?: string }[] | null>(null);
  const patch = (p: Partial<GitlabConfig>) => setForm((f) => ({ ...f, ...p }));
  const agencyId = repo.agencyId;
  const label = repo.agencyId === "global" ? globalAgencyName() : (repo.agencyName ?? repo.agencyId);

  useEffect(() => {
    if (!repo.id) return;
    api.GET("/git-repos/{repoId}/problems", { params: { path: { repoId: repo.id } } }).then(({ data }) => setProblems(data?.items ?? []));
  }, [repo.id]);

  // The webhook field is sent always: a write replaces every field, and one
  // left out is OFF (R6).
  const body = (): GitlabConfig => ({ ...form, ...(newPat ? { pat: newPat } : {}) });

  async function save() {
    setSaving(true);
    setMsg(null);
    const { error, response } = await api.PUT("/agencies/{agencyId}/git-repo", { params: { path: { agencyId }, header: csrfHeader }, body: body() });
    setSaving(false);
    if (error) {
      setMsg({ ok: false, text: errMsg(error) });
      return;
    }
    setMsg({ ok: true, text: response.status === 201 ? "Connected; its first sync has started." : "Saved." });
    setNewPat("");
    onSaved();
  }

  async function runTest() {
    setTesting(true);
    setTest(null);
    const { data, error } = await api.POST("/agencies/{agencyId}/git-repo/test", { params: { path: { agencyId }, header: csrfHeader }, body: body() });
    setTesting(false);
    if (error) {
      setMsg({ ok: false, text: errMsg(error) });
      return;
    }
    setTest(data as never);
  }

  async function rotate() {
    setSecret(null);
    const { data, error } = await api.POST("/agencies/{agencyId}/git-repo/webhook-secret/rotate", {
      params: { path: { agencyId }, header: csrfHeader },
      body: { updateGitlab: true },
    });
    if (error) {
      setMsg({ ok: false, text: `Rotate failed: ${errMsg(error)}` });
      return;
    }
    setSecret((data as { secret?: string })?.secret ?? null);
  }

  const webhookUrl = repo.id ? `${window.location.origin}/api/v1/webhooks/gitlab/${repo.id}` : "";
  const t = test as { ok?: boolean; branchFound?: boolean; canPush?: boolean; error?: string; pushError?: string; pushNote?: string; branches?: string[] } | null;
  const errors = problems?.filter((p) => p.severity === "error").length ?? 0;

  return (
    <Card
      title={`${label}'s repository`}
      action={<Btn small onClick={() => setOpen((o) => !o)}>{open ? "Close" : "Edit"}</Btn>}
    >
      <div style={{ fontSize: c.fontSm, color: c.textSec, lineHeight: 1.7 }}>
        {unconnected ? (
          <span role="note" style={{ color: c.warning }}>
            No repository is connected. Every agency has one: it is where its definitions live, and where its publishes and the
            assignments it is given are committed. Connect it below.
          </span>
        ) : (
          <>
            <div>
              <code style={{ fontFamily: c.mono }}>{repo.repoUrl || "(no URL)"}</code> · branch <strong>{repo.writeBranch || "main"}</strong>
            </div>
            <div>
              Last sync: {repo.lastStatus ? `${repo.lastStatus}${repo.lastSyncedAt ? `, ${fmtDateTime(repo.lastSyncedAt)}` : ""}` : "never"} · supplies{" "}
              {repo.supplies.jobs} jobs, {repo.supplies.workflows} workflows, {repo.supplies.scripts} scripts, {repo.supplies.schedules} schedules,{" "}
              {repo.supplies.scopes} scopes
            </div>
            {problems && problems.length > 0 && (
              <details>
                <summary style={{ cursor: "pointer", color: errors > 0 ? c.danger : c.warning }}>
                  {problems.length} problem{problems.length === 1 ? "" : "s"} at its last sync ({errors} error{errors === 1 ? "" : "s"})
                </summary>
                <ul style={{ margin: "6px 0 0 18px", padding: 0 }}>
                  {problems.map((p, i) => (
                    <li key={i}>
                      <code style={{ fontFamily: c.mono }}>{p.path || "(the repository)"}</code> — {p.severity} ({p.kind}): {p.message}
                      {p.firstSeen ? ` · since ${fmtDateTime(p.firstSeen)}` : ""}
                    </li>
                  ))}
                </ul>
              </details>
            )}
          </>
        )}
      </div>
      {open && (
        <div style={{ fontSize: c.fontSm, marginTop: 10 }}>
          <SettingRow label="Repository URL" hint={repo.agencyId === "global" ? undefined : "Its host must be on the allowed hosts."}>
            <input aria-label="Repository URL" value={form.repoUrl ?? ""} onChange={(e) => patch({ repoUrl: e.target.value })} style={wide()} />
          </SettingRow>
          <SettingRow label="Token" hint="It must be able to push. Stored masked; leave blank to keep the current token.">
            <input aria-label="Token" type="password" value={newPat} onChange={(e) => setNewPat(e.target.value)} placeholder={unconnected ? "" : "•••• (set)"} style={wide()} />
          </SettingRow>
          <SettingRow label="Working Branch" hint="The branch Cronomicon syncs definitions from, and publishes to.">
            <input aria-label="Working Branch" value={form.writeBranch ?? ""} onChange={(e) => patch({ writeBranch: e.target.value })} style={{ ...inputStyle(), width: 160 }} />
          </SettingRow>
          <SettingRow label="Bot Name">
            <input aria-label="Bot Name" value={form.botName ?? ""} onChange={(e) => patch({ botName: e.target.value })} style={wide()} />
          </SettingRow>
          <SettingRow label="Bot Email">
            <input aria-label="Bot Email" value={form.botEmail ?? ""} onChange={(e) => patch({ botEmail: e.target.value })} style={wide()} />
          </SettingRow>
          {webhookUrl && (
            <SettingRow label="Webhook URL" hint="This repository's own; set it in GitLab → Settings → Webhooks.">
              <input readOnly value={webhookUrl} onFocus={(e) => e.target.select()} style={wide()} />
            </SettingRow>
          )}
          <SettingRow label="Webhook Enabled">
            <Toggle on={form.webhookEnabled ?? false} onChange={() => patch({ webhookEnabled: !form.webhookEnabled })} />
          </SettingRow>
          {form.webhookEnabled && (
            // The events the webhook starts a sync on (2.4.2, question 54: the
            // single form had them, and the Repositories card sent them unseen).
            <SettingRow label="Webhook Events" hint="The events that start a sync of this repository.">
              <div style={{ display: "flex", gap: 14 }}>
                {(
                  [
                    ["push", "Push"],
                    ["mr", "Merge request"],
                    ["tag", "Tag"],
                  ] as const
                ).map(([key, text]) => (
                  <label key={key} style={{ display: "inline-flex", alignItems: "center", gap: 4 }}>
                    <input
                      type="checkbox"
                      checked={!!form.webhookEvents?.[key]}
                      onChange={(e) => patch({ webhookEvents: { push: true, mr: true, tag: true, ...form.webhookEvents, [key]: e.target.checked } })}
                    />
                    {text}
                  </label>
                ))}
              </div>
            </SettingRow>
          )}
          {!unconnected && (
            <SettingRow label="Webhook Secret" hint="Rotating it updates the hook in GitLab; the new secret is shown once.">
              <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
                {secret && <code style={{ fontFamily: c.mono }}>{secret}</code>}
                <Btn small onClick={rotate}>Rotate secret</Btn>
              </div>
            </SettingRow>
          )}
          <SettingRow label="Connection" last>
            <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
              <Btn small onClick={runTest} disabled={testing}>{testing ? "Testing…" : "Test"}</Btn>
              <Btn small onClick={save} disabled={saving}>{saving ? "Saving…" : unconnected ? "Connect" : "Save"}</Btn>
            </div>
          </SettingRow>
          {t && (
            <div role="status" style={{ marginTop: 8, lineHeight: 1.7 }}>
              {t.ok ? (
                <>
                  <div style={{ color: c.success }}>The repository answered{t.branchFound ? `; branch ${form.writeBranch} is there.` : `, but has no branch ${form.writeBranch}.`}</div>
                  {t.canPush ? (
                    <div style={{ color: c.success }}>The token may push. {t.pushNote}</div>
                  ) : (
                    <div style={{ color: c.danger }}>The token may not push: {t.pushError}. Saving will be refused.</div>
                  )}
                </>
              ) : (
                <div style={{ color: c.danger }}>The repository did not answer: {t.error}</div>
              )}
            </div>
          )}
          {msg && <div role={msg.ok ? "status" : "alert"} style={{ marginTop: 8, color: msg.ok ? c.success : c.danger }}>{msg.text}</div>}
          {repo.agencyId !== "global" && !unconnected && (
            // A repository is not disconnected (IN-2): it goes with its agency,
            // when the agency is empty (IN-13). One place deletes, and names
            // what keeps it (2.4.2, question 54).
            <div style={{ marginTop: 10, fontSize: c.fontXs, color: c.textSec }}>
              To remove this repository, delete {label} under{" "}
              <a href="/scopes?tab=agencies" style={{ color: c.accent }}>
                Scopes → Agencies
              </a>
              : an agency is deleted when it is empty, and its repository with it.
            </div>
          )}
        </div>
      )}
    </Card>
  );
}

// The hosts an agency's repository may be on (GR-10): a global
// administrator's, in the general settings.
function AllowedHostsCard() {
  const [general, setGeneral] = useState<Record<string, unknown> | null>(null);
  const [text, setText] = useState("");
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null);
  useEffect(() => {
    api.GET("/settings/general").then(({ data }) => {
      if (!data) return;
      setGeneral(data as Record<string, unknown>);
      setText(((data as { gitAllowedHosts?: string[] }).gitAllowedHosts ?? []).join("\n"));
    });
  }, []);
  async function save() {
    if (!general) return;
    const hosts = text.split("\n").map((h) => h.trim()).filter(Boolean);
    const { error } = await api.PUT("/settings/general", { params: { header: csrfHeader }, body: { ...general, gitAllowedHosts: hosts } as never });
    setMsg(error ? { ok: false, text: errMsg(error) } : { ok: true, text: "Saved." });
  }
  return (
    <Card title="Allowed hosts" action={<Btn small onClick={save} disabled={!general}>Save</Btn>}>
      <div style={{ fontSize: c.fontSm, color: c.textSec, marginBottom: 8 }}>
        The hosts an agency's repository may be on, one per line (Global's own is exempt). A connection to any other is refused.
      </div>
      <textarea aria-label="Allowed hosts" value={text} onChange={(e) => setText(e.target.value)} rows={4} style={{ ...inputStyle(), width: 300, fontFamily: c.mono }} />
      {msg && <div role={msg.ok ? "status" : "alert"} style={{ color: msg.ok ? c.success : c.danger }}>{msg.text}</div>}
    </Card>
  );
}

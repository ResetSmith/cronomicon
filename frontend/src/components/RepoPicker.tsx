import { useEffect, useState } from "react";
import { api, fetchCapabilities } from "../api/client";
import { agenciesFor, useMyAccess } from "../api/access";
import { useGet } from "../hooks";
import { c } from "../theme";

/**
 * The repositories of definitions (2.4.0 I3: every agency has one, Global's
 * included), for the screens that work on one at a time (R8): History → Git
 * sync, Schedule pushes, Scripts, Publish.
 *
 * useRepos reads GET /git-repo-names: the repositories whose history the
 * caller may read (Global's for everyone, an agency's for its people), ids and
 * names only. useRepoConnections reads GET /git-repos, the connections the
 * caller may configure (configureApp), with their URLs.
 */
export interface RepoRow {
  id: string;
  agencyId: string;
  agencyName?: string;
  repoUrl?: string;
  writeBranch?: string;
}

/** The label a repository goes by on the screens: its agency's name. */
export const repoLabel = (r: RepoRow) => (r.agencyName || r.agencyId) + (r.agencyId === "global" ? "" : "'s");

const byGlobalThenName = (a: RepoRow, b: RepoRow) =>
  a.agencyId === "global" ? -1 : b.agencyId === "global" ? 1 : (a.agencyName ?? "").localeCompare(b.agencyName ?? "");

export function useRepos(): { repos: RepoRow[]; loading: boolean } {
  const q = useGet<{ items?: RepoRow[] }>(() => api.GET("/git-repo-names") as never, []);
  return { repos: [...(q.data?.items ?? [])].sort(byGlobalThenName), loading: q.loading };
}

export function useRepoConnections(): { repos: RepoRow[]; loading: boolean } {
  const q = useGet<{ items?: RepoRow[] }>(() => api.GET("/git-repos") as never, []);
  return { repos: [...(q.data?.items ?? [])].sort(byGlobalThenName), loading: q.loading };
}

/**
 * useRepoAdmin answers whether the caller administers a repository: a global
 * administrator every one; anyone else, those of the agencies they hold
 * configureApp on (GR-9). Global's is a global administrator's.
 */
export function useRepoAdmin(): (agencyId: string | undefined) => boolean {
  const access = useMyAccess();
  const [global, setGlobal] = useState(false);
  useEffect(() => {
    let cancelled = false;
    void fetchCapabilities().then((caps) => {
      if (!cancelled) setGlobal(!!caps.configureAppGlobal);
    });
    return () => {
      cancelled = true;
    };
  }, []);
  const mine = new Set(agenciesFor(access ?? null, "configureApp").map((a) => a.id));
  return (agencyId) => global || (!!agencyId && agencyId !== "global" && mine.has(agencyId));
}

/**
 * RepoSelect — "Repository: [Global ▾]". With `all`, a first option for every
 * repository ("" as its value). Hidden when there is one repository and no
 * `all`: there is nothing to choose.
 */
export function RepoSelect({
  repos,
  value,
  onChange,
  all,
  label = "Repository",
}: {
  repos: RepoRow[];
  value: string;
  onChange: (id: string) => void;
  all?: string;
  label?: string;
}) {
  if (repos.length <= 1 && !all) return null;
  return (
    <label style={{ display: "inline-flex", alignItems: "center", gap: 6, fontSize: c.fontXs, color: c.textSec, whiteSpace: "nowrap" }}>
      {label}
      <select
        aria-label={label}
        value={value}
        onChange={(e) => onChange(e.target.value)}
        style={{
          padding: "7px 10px",
          borderRadius: c.radiusChip,
          border: `1px solid ${c.border}`,
          background: c.panelInput,
          color: c.text,
          fontSize: c.fontXs,
          fontFamily: c.sans,
        }}
      >
        {all && <option value="">{all}</option>}
        {repos.map((r) => (
          <option key={r.id} value={r.id}>
            {repoLabel(r)}
          </option>
        ))}
      </select>
    </label>
  );
}

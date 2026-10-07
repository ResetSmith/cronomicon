// GC (v2.2.2, "gate closing") — the words for a control that needs a GLOBAL
// administrator.
//
// Before 2.2.2 most install-wide writes asked "does the caller hold this
// permission on ANY agency". They now ask for one access grant that covers every
// agency AND carries the permission, and GET /capabilities reports that as the
// five optional flags `configureAppGlobal`, `manageRolesGlobal`,
// `manageEnvVarsGlobal`, `publishScheduleGlobal` and `composeAdmin`. The flat
// flags beside them (`configureApp`, `compose`, …) still mean "somewhere" and are
// still what decides whether a control is RELEVANT to the caller at all.
//
// The house rule (FX-7) is that a precondition disables with an explanation and
// only irrelevance hides. So a caller who holds the flat permission and lacks
// the global one sees the control disabled, with one of these sentences as its
// tooltip or inline note. They live here, once, so the ten surfaces that say it
// cannot drift into ten slightly different explanations of the same rule.

/**
 * The one sentence, with the verb phrase swappable for a control where "change
 * this" would read wrong (a button that STARTS something). Every variant keeps
 * the same subject and the same parenthesis, so an operator who has met it once
 * recognises it everywhere.
 */
export const globalAdminOnly = (action: string = "change this"): string =>
  `Only a global administrator (a role on every agency) can ${action}.`;

/** The default explanation: an install-wide setting or catalog. */
export const GLOBAL_ADMIN_ONLY = globalAdminOnly();

/** For a surface whose READ is refused too (the GitLab and Vault connections). */
export const GLOBAL_ADMIN_VIEW_ONLY = globalAdminOnly("view or change this");

/** POST /git/sync and POST /scopes/resync — they re-read every agency's definitions. */
export const GLOBAL_ADMIN_SYNC_ONLY = globalAdminOnly("start a sync from GitLab");

/** Reusable schedules, calendars, reactions and the recycle bin (`composeAdmin`). */
export const COMPOSE_ADMIN_ONLY =
  "Reusable schedules, calendars, reactions and the recycle bin are shared by every agency — only a global administrator (a role on every agency) can change them.";

/** Choosing the Vault source for a secret, editing a Vault-backed one, Migrate to Vault. */
export const VAULT_SECRET_GLOBAL_ONLY =
  "A Vault-backed secret names a path on the installation's one Vault connection — only a global administrator can create, edit or migrate one.";

/**
 * globalOnly turns a "…Global" capability flag into the reason a control is
 * disabled: "" when the caller may act, the explanation otherwise. `undefined`
 * (an older server, or capabilities still loading) reads as NOT allowed —
 * withholding is the safe way to be wrong, since the server refuses anyway.
 *
 *   const why = globalOnly(caps.configureAppGlobal);
 *   <Btn disabled={!!why || busy} title={why || undefined}>…</Btn>
 */
export function globalOnly(allowed: boolean | undefined | null, reason: string = GLOBAL_ADMIN_ONLY): string {
  return allowed ? "" : reason;
}

import { useCallback, useMemo, useRef } from "react";

// Name disambiguation (R2F-3).
//
// Since 1.2.0 two departments may own a definition of the same name, so a
// surface that prints a bare name can be showing either of two things. This is
// the ONE place that decides when a name earns a qualifier, so twelve surfaces
// cannot drift into twelve slightly different rules.
//
// **The rule: a name gets its agency suffix when — and ONLY when — it is
// ambiguous within the viewer's visible set.** Unconditional badges would make
// the common single-agency install noisier to solve a problem it does not have.
//
// Two properties follow, and both are load-bearing:
//
//   · No oracle. The "visible set" is the dataset already on screen, which the
//     backend filtered by scope before sending. A collision entirely outside it
//     produces no badge, so a badge can never tell one department that another
//     has a definition by this name.
//   · No false precision. A row with no identity (`uid`) cannot be told apart
//     from a same-named row, so it never badges AND never makes another row
//     badge — history predating the identity backfill says the bare name, which
//     is the honest answer rather than an invented one.
//
// Deliberately free of theme imports: pure functions over rows, unit-testable
// without a DOM, and incapable of capturing a load-time palette.

/** Any row that displays a definition's name. Every field is optional because
 *  the surfaces differ: run rows carry a frozen agency snapshot, catalog rows
 *  carry a derived one, and older history rows carry neither. */
export interface NamedRef {
  /** The definition's permanent identity. Absent ⇒ this row never badges. */
  uid?: string | null;
  name?: string | null;
  /** git | amadeus — the last-resort qualifier when colliding rows have no agencies. */
  source?: string | null;
  agencies?: string[] | null;
  /**
   * The namespace a name must be unique WITHIN. Surfaces that mix kinds — the
   * reaction picker's jobs and workflows, the activity feed — set this to the
   * kind, so a job and a workflow that happen to share a name do not badge each
   * other: the `kind ·` prefix beside them already tells those apart, and a
   * badge there would be noise dressed as information. Omit when the rows are
   * all one kind.
   */
  group?: string | null;
}

/** The ambiguity key: a name is only ever compared within its own group. */
const groupKey = (r: NamedRef) => `${r.group ?? ""}\u0000${r.name ?? ""}`;

/**
 * The names that belong to more than one DISTINCT identity in `rows`.
 *
 * Rows without a uid are skipped entirely rather than keyed on their name: a
 * row that cannot be told apart is not evidence that two things exist, and
 * treating it as evidence would badge a name on the strength of a row that
 * cannot say which definition it refers to.
 */
export function ambiguousNames(rows: readonly NamedRef[]): Set<string> {
  const identities = new Map<string, Set<string>>();
  for (const r of rows) {
    if (!r?.name || !r.uid) continue;
    const key = groupKey(r);
    const ids = identities.get(key) ?? new Set<string>();
    ids.add(r.uid);
    identities.set(key, ids);
  }
  const out = new Set<string>();
  for (const [key, ids] of identities) if (ids.size > 1) out.add(key);
  return out;
}

/**
 * The agency half of a disambiguated label (R2F-Q1): " · FIN", " · FIN/DSS", or
 * " · 3 agencies" past two — a definition in many agencies is disambiguated by
 * the fact that it is, not by an unreadable list. "" when there is nothing to
 * say. The middot matches the existing `kind · name` chip idiom.
 */
export function agencySuffix(agencies?: readonly string[] | null): string {
  const a = (agencies ?? []).filter(Boolean);
  if (a.length === 0) return "";
  if (a.length > 2) return ` · ${a.length} agencies`;
  return ` · ${a.join("/")}`;
}

/** A row's display name: bare, unless its name is ambiguous in the visible set. */
export function disambiguate(row: NamedRef, ambiguous: ReadonlySet<string>): string {
  const name = row?.name ?? "";
  if (!name || !row.uid || !ambiguous.has(groupKey(row))) return name;
  const suffix = agencySuffix(row.agencies);
  // A collision whose sides have no agencies to tell them apart must still not
  // render as two identical rows — fall back to the source, the other axis a
  // reference can be qualified on (A11). Better a weak qualifier than none.
  return suffix ? `${name}${suffix}` : row.source ? `${name} · ${row.source}` : name;
}

/**
 * The hook every surface uses: pass the rows currently on screen plus how to
 * read a name reference out of one, get back a labeller for those same rows.
 *
 * `select` is required rather than inferred because the surfaces genuinely
 * disagree about field names — a run row says jobUid/jobName, a schedule row
 * ownerUid/ownerName, a catalog row uid/name. Naming the mapping at each call
 * site is one line, and it is the line that says WHICH of a row's several
 * possible names is being disambiguated.
 *
 * Recomputed only when the dataset changes, so a page of a few hundred rows
 * costs one pass per fetch rather than one per render.
 */
export function useNameDisambiguator<T>(
  rows: readonly T[],
  select: (row: T) => NamedRef,
): (row: T) => string {
  // select is held in a ref, not a dependency: call sites pass an inline arrow
  // (which is the readable way to write it), and a fresh identity every render
  // would recompute the scan every render — the one thing the memo exists to
  // avoid. Its BEHAVIOUR is stable at every real call site; only its identity
  // is not.
  const selectRef = useRef(select);
  selectRef.current = select;
  const ambiguous = useMemo(() => ambiguousNames(rows.map((r) => selectRef.current(r))), [rows]);
  return useCallback((row: T) => disambiguate(selectRef.current(row), ambiguous), [ambiguous]);
}

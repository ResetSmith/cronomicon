import { useEffect, useRef, useState } from "react";
import { api } from "../api/client";
import { OVERVIEW, useAgency, type AgencyChoice } from "../agency";
import { GLOBAL_AGENCY } from "../api/access";
import { c } from "../theme";

// 2.5.3 CA-12: Central runs nothing; among the choices it reads as what it is.
const label = (id: string, name: string) => (id === GLOBAL_AGENCY ? `${name} · administration` : name);

// The agency switcher (2.4.4 F1), under the wordmark. One agency at a time:
// the menu lists the person's choices — All agencies (the Dashboard across
// every agency, 2.5.2 OP-21) first for
// those who may see it, then Global, then their agencies — each with what it
// has waiting (SW-14), so that a person of several agencies is not blind to
// the ones they are not viewing. A person with a single choice sees a label.

type Badge = { agencyId: string; openNotices: number; failedRuns24h: number };

// The Dashboard's pace: the badges are a glance, not a live feed.
const BADGE_POLL_MS = 30_000;
// Past this many choices the menu offers a search.
const SEARCH_FROM = 8;

function useBadges(): Record<string, Badge> {
  const [badges, setBadges] = useState<Record<string, Badge>>({});
  useEffect(() => {
    let cancelled = false;
    const read = () =>
      void api.GET("/me/agency-badges").then((res) => {
        if (cancelled || !Array.isArray(res.data)) return;
        const out: Record<string, Badge> = {};
        for (const b of res.data as Badge[]) out[b.agencyId] = b;
        setBadges(out);
      });
    read();
    const t = window.setInterval(read, BADGE_POLL_MS);
    return () => {
      cancelled = true;
      window.clearInterval(t);
    };
  }, []);
  return badges;
}

function BadgeChips({ badge }: { badge?: Badge }) {
  if (!badge || (badge.openNotices === 0 && badge.failedRuns24h === 0)) return null;
  const chip = (n: number, colour: string, title: string) => (
    <span
      title={title}
      style={{
        minWidth: 16,
        padding: "0 4px",
        borderRadius: c.radiusChip,
        background: `${colour}22`,
        border: `1px solid ${colour}55`,
        color: colour,
        fontSize: c.fontXs,
        fontWeight: 700,
        fontFamily: c.mono,
        lineHeight: "16px",
        textAlign: "center",
      }}
    >
      {n}
    </span>
  );
  return (
    <span style={{ display: "inline-flex", gap: 4 }}>
      {badge.failedRuns24h > 0 &&
        chip(badge.failedRuns24h, c.danger, `${badge.failedRuns24h} failed run${badge.failedRuns24h === 1 ? "" : "s"} in the last 24 hours`)}
      {badge.openNotices > 0 &&
        chip(badge.openNotices, c.warning, `${badge.openNotices} open notice${badge.openNotices === 1 ? "" : "s"}`)}
    </span>
  );
}

export function AgencySwitcher({ collapsed }: { collapsed: boolean }) {
  const ag = useAgency();
  const badges = useBadges();
  const [open, setOpen] = useState(false);
  const [q, setQ] = useState("");
  const ref = useRef<HTMLDivElement>(null);
  // The menu is placed against the window, not inside the sidebar, which
  // clips what overflows it (the walkthrough found the badges cut off).
  const [at, setAt] = useState<{ top: number; left: number } | null>(null);
  const toggle = (e: { currentTarget: HTMLElement }) => {
    const r = e.currentTarget.getBoundingClientRect();
    setAt(collapsed ? { top: r.top, left: r.right + 6 } : { top: r.bottom + 4, left: r.left });
    setOpen((o) => !o);
  };
  useEffect(() => {
    if (!open) return;
    const close = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false);
    };
    const esc = (e: KeyboardEvent) => e.key === "Escape" && setOpen(false);
    document.addEventListener("mousedown", close);
    document.addEventListener("keydown", esc);
    return () => {
      document.removeEventListener("mousedown", close);
      document.removeEventListener("keydown", esc);
    };
  }, [open]);

  const currentId = ag.onOverview ? OVERVIEW : ag.agency;
  const single = ag.choices.length <= 1;
  // A person whose one choice is Central (no grant at all) sees its name alone.
  const currentName = ag.onOverview ? "All agencies" : single ? ag.agencyName : label(ag.agency, ag.agencyName);
  const elsewhere = Object.values(badges).some(
    (b) => b.agencyId !== ag.agency && (b.openNotices > 0 || b.failedRuns24h > 0),
  );

  const box = {
    display: "flex",
    alignItems: "center",
    gap: 6,
    width: collapsed ? undefined : "100%",
    justifyContent: collapsed ? "center" : "space-between",
    padding: collapsed ? "6px" : "7px 10px",
    borderRadius: c.radiusChip,
    border: `1px solid ${c.border}`,
    background: "transparent",
    color: c.sidebarText,
    fontSize: c.fontSm,
    textAlign: "left" as const,
  };

  if (single) {
    return (
      <div data-testid="agency-label" title={`You work in ${currentName}`} style={{ ...box, cursor: "default" }}>
        {collapsed ? currentName.slice(0, 1).toUpperCase() : <span style={{ fontWeight: 600 }}>{currentName}</span>}
      </div>
    );
  }

  const shown: AgencyChoice[] =
    ag.choices.length >= SEARCH_FROM && q.trim()
      ? ag.choices.filter((ch) => label(ch.id, ch.name).toLowerCase().includes(q.trim().toLowerCase()))
      : ag.choices;

  return (
    <div ref={ref} style={{ position: "relative" }}>
      <button
        type="button"
        data-testid="agency-switcher"
        aria-haspopup="listbox"
        aria-expanded={open}
        aria-label={`Agency: ${currentName}. Switch agency`}
        title={collapsed ? `${currentName} (switch agency)` : "Switch agency"}
        onClick={toggle}
        style={{ ...box, cursor: "pointer" }}
      >
        {collapsed ? (
          <span style={{ fontWeight: 700 }}>{currentName.slice(0, 1).toUpperCase()}</span>
        ) : (
          <>
            <span style={{ minWidth: 0, overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap", fontWeight: 600 }}>
              {currentName}
            </span>
            <span style={{ display: "inline-flex", alignItems: "center", gap: 6, flexShrink: 0 }}>
              {elsewhere && (
                <span title="Another of your agencies has something waiting" style={{ width: 7, height: 7, borderRadius: "50%", background: c.warning }} />
              )}
              <span aria-hidden style={{ opacity: 0.6 }}>▾</span>
            </span>
          </>
        )}
      </button>
      {open && (
        <div
          role="listbox"
          aria-label="Agencies"
          style={{
            position: "fixed",
            top: at?.top ?? 0,
            left: at?.left ?? 0,
            zIndex: 40,
            minWidth: 240,
            maxHeight: 420,
            overflowY: "auto",
            background: c.panel,
            // A floating overlay is the documented exception to "a surface gets
            // a border OR a shadow, never both".
            border: `1px solid ${c.border}`,
            borderRadius: c.radiusSurface,
            boxShadow: c.shadow,
            padding: 6,
          }}
        >
          {ag.choices.length >= SEARCH_FROM && (
            <input
              autoFocus
              value={q}
              onChange={(e) => setQ(e.target.value)}
              placeholder="Find an agency"
              aria-label="Find an agency"
              style={{
                width: "100%",
                boxSizing: "border-box",
                marginBottom: 6,
                padding: "6px 8px",
                borderRadius: c.radiusChip,
                border: `1px solid ${c.border}`,
                background: c.panelInput,
                color: c.text,
                fontSize: c.fontSm,
              }}
            />
          )}
          {shown.map((ch) => {
            const selected = ch.id === currentId;
            return (
              <button
                key={ch.id}
                type="button"
                role="option"
                aria-selected={selected}
                onClick={() => {
                  setOpen(false);
                  setQ("");
                  if (!selected) ag.switchTo(ch.id);
                }}
                style={{
                  display: "flex",
                  alignItems: "center",
                  justifyContent: "space-between",
                  gap: 8,
                  width: "100%",
                  padding: "7px 10px",
                  borderRadius: c.radiusChip,
                  border: "none",
                  borderBottom: ch.kind === "overview" ? `1px solid ${c.borderLight}` : "none",
                  background: selected ? c.panelHover : "transparent",
                  color: c.text,
                  fontSize: c.fontSm,
                  fontWeight: selected ? 600 : 400,
                  cursor: "pointer",
                  textAlign: "left",
                }}
              >
                <span style={{ minWidth: 0, overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>{label(ch.id, ch.name)}</span>
                <span style={{ display: "inline-flex", alignItems: "center", gap: 6, flexShrink: 0 }}>
                  {ch.kind === "agency" && <BadgeChips badge={badges[ch.id]} />}
                  {ag.effectiveDefault === ch.id && (
                    <span style={{ fontSize: c.fontXs, color: c.textMuted }} title="Where you start">
                      Default
                    </span>
                  )}
                </span>
              </button>
            );
          })}
          {shown.length === 0 && <div style={{ padding: "6px 10px", fontSize: c.fontSm, color: c.textMuted }}>No agency of that name</div>}
        </div>
      )}
    </div>
  );
}

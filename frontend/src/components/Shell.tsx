import { useEffect, useRef, useState } from "react";
import { NavLink, Outlet, useLocation } from "react-router-dom";
import type { CSSProperties, ReactNode } from "react";
import { c } from "../theme";
import { useTheme } from "../theme-context";
import { useAuth } from "../auth";
import { logout, fetchVersion, type BuildInfo } from "../api/client";
import { useSidebarCollapsed } from "../hooks";
// ONE emblem asset, both themes (LG-Q2). The wordmark used to be baked into the
// bitmap, which forced two assets — a white-lettered one for the dark rail and a
// navy-lettered one for the light rail (the white mark on the true light rail of
// VU-Q2 would be white-on-white, and the brand would vanish). Two assets meant
// two things to keep in sync, and they had already drifted: different canvas
// padding, different aspect, and even different taglines, which is why the light
// mark rendered at under half the dark one's size from identical CSS.
//
// The wordmark is now real DOM text (Wordmark below), so the emblem artwork —
// which is identical in both themes, a colour badge with dark outlines and a
// gold ring that reads on white and on navy alike — is the only bitmap left.
// That also retires the collapsed rail's hand-tuned crop window: collapsing now
// just hides the text. The rail shows emblem + name only; the tagline is an
// opt-in line that Login renders and the sidebar deliberately does not.
import { Wordmark } from "./Wordmark";
// EP-3 — one refresh glyph for the whole app; the copy that lived here is gone.
import { IcRefresh } from "./ui";

// Sidebar geometry. Collapsed is wide enough for a centred 16px icon inside its
// 8px-padded hit target; expanded is the original width the nav labels were
// sized against.
export const SIDEBAR_WIDTH = 195;
export const SIDEBAR_WIDTH_COLLAPSED = 60;

// displayVersion renders a clean `vX.Y.Z` from whatever the build stamped: a
// release tag (`v0.26.0`), the source default (`0.26.0`), or a git-describe dev
// string (`v0.26.0-3-gabc-dirty`) all collapse to the semver. The full string is
// kept for the hover title so a dev/dirty build is still identifiable.
function displayVersion(v: string): string {
  const m = v.match(/\d+\.\d+\.\d+/);
  // Semver builds get the conventional "v" prefix; anything else (e.g. a
  // release-candidate label like "RC1.0") is shown verbatim.
  return m ? "v" + m[0] : v;
}

// Route metadata drives the header bar title/subtitle so individual views no
// longer render their own <h1>. Keep keys in sync with App.tsx routes.
export const meta: Record<string, { label: string; subtitle: string; icon: ReactNode }> = {
  "/": { label: "Dashboard", subtitle: "Jobs, workflows and recent activity at a glance", icon: <IcGrid /> },
  "/jobs": { label: "Jobs", subtitle: "Run your scripts on a schedule, across your hosts", icon: <IcList /> },
  "/compose": { label: "Compose", subtitle: "Build an in-app job from a script, schedules and a scope", icon: <IcCode /> },
  "/jobs/publish": { label: "Publish to GitLab", subtitle: "Author a job's schedule and publish it to GitLab", icon: <IcCode /> },
  "/scripts": { label: "Scripts", subtitle: "Reusable executable units referenced by jobs", icon: <IcCode /> },
  "/schedules": { label: "Schedules", subtitle: "Schedule catalog, live inventory and upcoming runs", icon: <IcCal /> },
  "/schedule-builder": { label: "Schedule Builder", subtitle: "Author an in-app schedule referenced by jobs and workflows", icon: <IcCal /> },
  "/workflows": { label: "Workflows", subtitle: "Multi-step job pipelines", icon: <IcFlow /> },
  "/workflow-editor": { label: "Workflow Editor", subtitle: "Compose an in-app workflow from job steps", icon: <IcFlow /> },
  "/activity": { label: "Activity", subtitle: "Recent events across the system", icon: <IcPulse /> },
  "/runs": { label: "History", subtitle: "Execution and change history", icon: <IcClock /> },
  "/scopes": { label: "Scopes", subtitle: "Hosts & execution environments", icon: <IcScope /> },
  "/env-vars": { label: "Env Vars", subtitle: "Variables and secrets", icon: <IcKey /> },
  "/runners": { label: "Runners", subtitle: "Registered execution runners", icon: <IcServer /> },
  "/settings": { label: "Settings", subtitle: "Configuration and access", icon: <IcGear /> },
};

export const nav = [
  { to: "/", end: true },
  { to: "/jobs" },
  { to: "/workflows" },
  { to: "/scripts" },
  { to: "/schedules" },
  { to: "/scopes" },
  { to: "/env-vars" },
  { to: "/runners" },
  { to: "/runs" },
  { to: "/activity" },
  { to: "/settings" },
];

export function Shell() {
  const { me } = useAuth();
  const { mode, toggle } = useTheme();
  const { pathname } = useLocation();
  const current = meta[pathname] ?? { label: "Cronomicon", subtitle: "", icon: null };
  const [collapsed, toggleCollapsed] = useSidebarCollapsed();
  const [build, setBuild] = useState<BuildInfo | null>(null);
  useEffect(() => {
    void fetchVersion().then(setBuild);
  }, []);

  // Shared top-bar control style (Refresh + theme toggle). Defined in render so
  // it reads the live, mutable theme tokens in `c` rather than freezing them at
  // module load — otherwise the buttons wouldn't recolour on a light/dark switch.
  const topBarBtn: CSSProperties = {
    display: "flex",
    alignItems: "center",
    gap: 7,
    padding: "8px 12px",
    borderRadius: c.radiusChip,
    border: `1px solid ${c.border}`,
    background: c.panelInput,
    color: c.textSec,
    fontSize: c.fontSm,
    cursor: "pointer",
  };

  return (
    <div style={{ display: "flex", height: "100vh", background: c.bg, color: c.text, fontFamily: c.sans }}>
      <aside
        data-testid="sidebar"
        data-collapsed={collapsed ? "true" : "false"}
        style={{
          width: collapsed ? SIDEBAR_WIDTH_COLLAPSED : SIDEBAR_WIDTH,
          flexShrink: 0,
          background: c.sidebar,
          borderRight: `1px solid ${c.border}`,
          display: "flex",
          flexDirection: "column",
          color: c.sidebarText,
          transition: "width 0.15s ease",
          overflow: "hidden",
        }}
      >
        <div style={{ padding: collapsed ? "12px 6px 16px" : "10px 6px 16px" }}>
          <Wordmark collapsed={collapsed} />
        </div>
        <nav style={{ display: "flex", flexDirection: "column", gap: 2, padding: collapsed ? "0 8px" : "0 10px" }}>
          {nav.map((n) => (
            <NavItem
              key={n.to}
              to={n.to}
              end={n.end}
              label={meta[n.to].label}
              icon={meta[n.to].icon}
              collapsed={collapsed}
            />
          ))}
        </nav>
        <div
          style={{
            marginTop: "auto",
            padding: collapsed ? "16px 8px" : "16px 20px",
            display: "flex",
            flexDirection: "column",
            alignItems: collapsed ? "center" : "stretch",
            gap: 10,
          }}
        >
          {!collapsed && (
            <div style={{ fontSize: c.fontXs, color: c.sidebarText }}>
              {me?.email}
              {me?.roles?.length ? <div style={{ opacity: 0.7 }}>{me.roles.join(", ")}</div> : null}
            </div>
          )}
          <button
            onClick={() => void logout()}
            title={collapsed ? `Sign out${me?.email ? ` (${me.email})` : ""}` : undefined}
            style={{
              alignSelf: collapsed ? "center" : "flex-start",
              display: "flex",
              alignItems: "center",
              justifyContent: "center",
              gap: 7,
              background: "transparent",
              color: c.sidebarText,
              border: `1px solid ${c.border}`,
              borderRadius: c.radiusChip,
              padding: collapsed ? "6px" : "5px 10px",
              fontSize: c.fontSm,
              cursor: "pointer",
            }}
          >
            <IcSignOut />
            {!collapsed && "Sign out"}
          </button>
          <button
            onClick={toggleCollapsed}
            aria-label={collapsed ? "Expand sidebar" : "Collapse sidebar"}
            aria-expanded={!collapsed}
            title={collapsed ? "Expand sidebar" : "Collapse sidebar"}
            style={{
              alignSelf: collapsed ? "center" : "flex-start",
              display: "flex",
              alignItems: "center",
              justifyContent: "center",
              gap: 7,
              background: "transparent",
              color: c.sidebarText,
              border: `1px solid ${c.border}`,
              borderRadius: c.radiusChip,
              padding: collapsed ? "6px" : "5px 10px",
              fontSize: c.fontXs,
              cursor: "pointer",
            }}
          >
            {collapsed ? <IcChevronRight /> : <IcChevronLeft />}
            {!collapsed && "Collapse"}
          </button>
          {!collapsed && (
            <div
              style={{ fontSize: c.fontXs, opacity: 0.3 }}
              title={build ? `${build.version} · ${build.commit} · ${build.date}` : undefined}
            >
              {build ? displayVersion(build.version) : ""}
            </div>
          )}
        </div>
      </aside>

      <div style={{ flex: 1, display: "flex", flexDirection: "column", overflow: "hidden" }}>
        <header
          style={{
            display: "flex",
            justifyContent: "space-between",
            alignItems: "center",
            padding: "14px 28px",
            borderBottom: `1px solid ${c.border}`,
            background: c.panel,
            flexShrink: 0,
          }}
        >
          <div>
            {/* The page title is the step the scale was missing (VU-7): the app's
                type ran 10–14px, so a page title and a table cell were nearly the
                same size and hierarchy fell entirely to colour. 24px at 600 —
                Plex Sans, not the display face, which stays on the brand
                surfaces (theme.ts `display`). */}
            <div style={{ fontSize: c.fontTitle, fontWeight: 600, letterSpacing: -0.2, color: c.text, lineHeight: 1.15 }}>{current.label}</div>
            {current.subtitle && <div style={{ fontSize: c.fontSm, color: c.textMuted, marginTop: 3 }}>{current.subtitle}</div>}
          </div>
          <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
            <HelpMenu btnStyle={topBarBtn} />
            {/* EP-Q2 — "Reload page", not "Refresh". Behaviour is unchanged (a
                hard reload), but the word is not: since EP-3 every expanded
                panel carries a surgical Refresh that re-fetches its data and
                keeps your expansions, filters and drafts. One word per concept
                — this control throws all of that away, and now says so. */}
            <button
              onClick={() => window.location.reload()}
              title="Reload the whole page (discards expansions, filters and unsaved drafts)"
              style={topBarBtn}
            >
              <IcRefresh />
              Reload page
            </button>
            <button
              onClick={toggle}
              title={mode === "dark" ? "Switch to light mode" : "Switch to dark mode"}
              style={topBarBtn}
            >
              {mode === "dark" ? <IcSun /> : <IcMoon />}
              {mode === "dark" ? "Light" : "Dark"}
            </button>
          </div>
        </header>

        {/* No key: a light/dark toggle RE-RENDERS the outlet subtree (App subscribes
            to the theme context and recreates the route elements), which repaints
            every view from the mutable `c` tokens WITHOUT remounting — so the active
            tab, search, pagination and expanded rows survive the toggle. */}
        <main style={{ flex: 1, overflow: "auto", padding: 28 }}>
          <Outlet />
        </main>
      </div>
    </div>
  );
}

function NavItem({
  to,
  end,
  label,
  icon,
  collapsed,
}: {
  to: string;
  end?: boolean;
  label: string;
  icon: ReactNode;
  collapsed?: boolean;
}) {
  const [hover, setHover] = useState(false);
  return (
    <NavLink
      to={to}
      end={end}
      // Collapsed rows drop their visible text, so the label moves to the native
      // tooltip and to aria-label — the nav stays navigable by screen reader.
      title={collapsed ? label : undefined}
      aria-label={collapsed ? label : undefined}
      onMouseEnter={() => setHover(true)}
      onMouseLeave={() => setHover(false)}
      style={({ isActive }) => ({
        display: "flex",
        alignItems: "center",
        justifyContent: collapsed ? "center" : "flex-start",
        gap: 10,
        padding: collapsed ? "10px 0" : "10px 12px",
        borderRadius: c.radiusChip,
        textDecoration: "none",
        fontSize: c.fontSm,
        whiteSpace: "nowrap",
        color: isActive ? c.sidebarTextActive : c.sidebarText,
        background: isActive ? c.sidebarActive : hover ? c.sidebarHover : "transparent",
        fontWeight: isActive ? 600 : 400,
        transition: "background 0.15s, color 0.15s",
      })}
    >
      {({ isActive }: { isActive: boolean }) => (
        <>
          <span style={{ opacity: isActive ? 1 : 0.65, display: "flex" }}>{icon}</span>
          {!collapsed && label}
        </>
      )}
    </NavLink>
  );
}

// ── Icons (16px stroke SVGs ported from the prototype's Ic* set) ─────────────
function svg(children: ReactNode) {
  return (
    <svg width={16} height={16} viewBox="0 0 18 18" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round">
      {children}
    </svg>
  );
}
function IcGrid() { return svg(<><rect x="2.5" y="2.5" width="5" height="5" rx="1" /><rect x="10.5" y="2.5" width="5" height="5" rx="1" /><rect x="2.5" y="10.5" width="5" height="5" rx="1" /><rect x="10.5" y="10.5" width="5" height="5" rx="1" /></>); }
function IcList() { return svg(<><path d="M6 4.5h9M6 9h9M6 13.5h9" /><circle cx="3" cy="4.5" r="0.6" /><circle cx="3" cy="9" r="0.6" /><circle cx="3" cy="13.5" r="0.6" /></>); }
function IcCode() { return svg(<path d="M6 6 3 9l3 3m6-6 3 3-3 3M10.5 4.5l-3 9" />); }
function IcFlow() { return svg(<><rect x="2" y="6.5" width="5" height="5" rx="1" /><rect x="11" y="2.5" width="5" height="4" rx="1" /><rect x="11" y="11" width="5" height="4" rx="1" /><path d="M7 9h2.5M9.5 9V4.5H11M9.5 9v4.5H11" /></>); }
function IcPulse() { return svg(<path d="M2 9h3l2-5 3 10 2-5h4" />); }
function IcClock() { return svg(<><circle cx="9" cy="9" r="6.5" /><path d="M9 5.5V9l2.5 1.5" /></>); }
function IcCal() { return svg(<><rect x="2.5" y="3.5" width="13" height="12" rx="1.5" /><path d="M2.5 7h13M6 2v3M12 2v3" /></>); }
function IcKey() { return svg(<><circle cx="6" cy="6" r="3.5" /><path d="M8.5 8.5l6 6M12 12l1.5-1.5M13.5 13.5l1.5-1.5" /></>); }
function IcScope() { return svg(<><circle cx="9" cy="9" r="6.5" /><circle cx="9" cy="9" r="2" /><path d="M9 1v3M9 14v3M1 9h3M14 9h3" /></>); }
function IcServer() { return svg(<><rect x="2.5" y="3" width="13" height="5" rx="1" /><rect x="2.5" y="10" width="13" height="5" rx="1" /><path d="M5 5.5h0M5 12.5h0" /></>); }
function IcGear() { return svg(<path fill="currentColor" stroke="none" fillRule="evenodd" d="M17.5,9 L14.6,12.2 L13.2,16.4 L9,15.5 L4.8,16.4 L3.4,12.2 L0.5,9 L3.4,5.8 L4.8,1.6 L9,2.5 L13.2,1.6 L14.6,5.8 Z M6,9 a3,3 0 1,0 6,0 a3,3 0 1,0 -6,0" />); }
function IcSun() { return svg(<><circle cx="9" cy="9" r="3.2" /><path d="M9 2.5v2m0 9v2m-6.5-6.5h2m9 0h2M4.4 4.4l1.4 1.4m6.4 6.4l1.4 1.4M4.4 13.6l1.4-1.4m6.4-6.4l1.4-1.4" /></>); }
function IcMoon() { return svg(<path d="M14 10.5A5.5 5.5 0 017.5 4a5.5 5.5 0 106.5 6.5z" />); }
// The Help button used to be a bare link to the user manual, which made the
// administrator manual and the two training courses reachable only from inside
// a doc page's own sidebar (TR-5's "link from the app wherever the manuals are
// linked"). It is now a small popover of the four documents; the guides stay
// discoverable through every doc page's sidebar rather than swelling this menu
// to ten rows. Dismissal is the shipped TagFilterSelect/ColumnsMenu pattern —
// click-outside + Esc-returns-focus — no new overlay vocabulary.
const HELP_LINKS: { group: string; items: { href: string; label: string }[] }[] = [
  {
    group: "Manuals",
    items: [
      { href: "/user-manual.html", label: "User Manual" },
      { href: "/administrator-manual.html", label: "Administrator Manual" },
    ],
  },
  {
    group: "Training courses",
    items: [
      { href: "/training-operator.html", label: "Operator Course" },
      { href: "/training-admin.html", label: "Administrator Course" },
    ],
  },
];

export function HelpMenu({ btnStyle }: { btnStyle: CSSProperties }) {
  const [open, setOpen] = useState(false);
  const ref = useRef<HTMLDivElement>(null);
  const triggerRef = useRef<HTMLButtonElement>(null);

  useEffect(() => {
    if (!open) return;
    const onDoc = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false);
    };
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        e.stopPropagation();
        setOpen(false);
        triggerRef.current?.focus();
      }
    };
    document.addEventListener("mousedown", onDoc);
    document.addEventListener("keydown", onKey, true);
    return () => {
      document.removeEventListener("mousedown", onDoc);
      document.removeEventListener("keydown", onKey, true);
    };
  }, [open]);

  return (
    <div ref={ref} style={{ position: "relative", display: "inline-flex" }}>
      <button
        ref={triggerRef}
        onClick={() => setOpen((o) => !o)}
        aria-expanded={open}
        aria-haspopup="true"
        title="Manuals & training courses (each opens in a new tab)"
        style={btnStyle}
      >
        <IcBook />
        Help
      </button>
      {open && (
        <div
          role="dialog"
          aria-label="Documentation"
          style={{
            position: "absolute",
            top: "100%",
            right: 0,
            zIndex: 30,
            marginTop: 4,
            minWidth: 210,
            background: c.panel,
            // A floating overlay is the documented exception to "a surface gets
            // a border OR a shadow, never both".
            border: `1px solid ${c.border}`,
            borderRadius: c.radiusSurface,
            boxShadow: c.shadow,
            padding: 8,
          }}
        >
          {HELP_LINKS.map((g) => (
            <div key={g.group}>
              <div
                style={{
                  fontFamily: c.sansCond,
                  textTransform: "uppercase",
                  letterSpacing: 0.6,
                  fontSize: c.fontXs,
                  color: c.textMuted,
                  padding: "6px 10px 4px",
                }}
              >
                {g.group}
              </div>
              {g.items.map((l) => (
                <a
                  key={l.href}
                  href={l.href}
                  target="_blank"
                  rel="noopener noreferrer"
                  onClick={() => setOpen(false)}
                  style={{
                    display: "block",
                    padding: "7px 10px",
                    borderRadius: c.radiusChip,
                    color: c.text,
                    fontSize: c.fontSm,
                    textDecoration: "none",
                  }}
                  onMouseEnter={(e) => { e.currentTarget.style.background = c.panelHover; }}
                  onMouseLeave={(e) => { e.currentTarget.style.background = "transparent"; }}
                >
                  {l.label}
                </a>
              ))}
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

function IcBook() { return svg(<><path d="M9 4.5C7.4 3.6 5.2 3.4 3 3.8v9.7c2.2-0.4 4.4-0.2 6 0.7" /><path d="M9 4.5c1.6-0.9 3.8-1.1 6-0.7v9.7c-2.2-0.4-4.4-0.2-6 0.7" /></>); }
function IcChevronLeft() { return svg(<path d="M11 3.5 5.5 9l5.5 5.5" />); }
function IcChevronRight() { return svg(<path d="M7 3.5 12.5 9 7 14.5" />); }
function IcSignOut() { return svg(<><path d="M7 15.5H4a1.5 1.5 0 0 1-1.5-1.5V4A1.5 1.5 0 0 1 4 2.5h3" /><path d="M11.5 12 15 9l-3.5-3M15 9H6.5" /></>); }

import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState } from "react";
import type { ReactNode } from "react";
import { useLocation, useNavigate } from "react-router-dom";
import { AGENCY_REFRESH_EVENT, api, csrfHeader, errMsg, setRequestAgency } from "./api/client";
import { useToast } from "./hooks";
import { Toast } from "./components/ui";
import { c } from "./theme";
import { setGlobalAgencyName } from "./globalName";

// The agency switcher (2.4.4; plan-2.4.4 §3, SW-1 to SW-19).
//
// A person works in ONE agency at a time: every list shows that agency's rows,
// and what they create is that agency's. The agency lives in the address
// (?agency=<id>), so a link carries it; a visit without one starts where
// GET /me/context says. The API client sends it on every request
// (X-Cronomicon-Agency, api/client.ts); the server narrows each list by it and
// still enforces every grant, so this is a view, not a permission.
//
// All agencies (2.5.2 OP-21, the Operations overview until then) is not an
// agency: it is the Dashboard across every agency, at /?agency=overview, for
// those whose access reaches every agency. On it the switcher reads "All
// agencies"; every other page, and every list, uses the working agency.

export const OVERVIEW = "overview";
export const AGENCY_PARAM = "agency";
/** The built-in agency's id: fixed, whatever it is called (2.5.2 GR). */
const GLOBAL_AGENCY_ID = "global";

/**
 * VIEW_PARAMS are the query parameters that say how a page is shown, and are
 * kept when the person switches agency; every other one names an object of the
 * agency left, and goes (2.5.0 C11, defect 85).
 */
export const VIEW_PARAMS = new Set(["tab", "triggerKind", "result", "stopped", "theme"]);

/** EDITOR_LIST: an editor whose object went with a switch shows its list. */
export const EDITOR_LIST: Record<string, string> = {
  "/compose": "/jobs",
  "/workflow-editor": "/workflows",
  "/schedule-builder": "/schedules",
};

export type AgencyChoice = { id: string; name: string; kind: "overview" | "agency" };

type MeContext = {
  choices: AgencyChoice[];
  storedDefault?: string | null;
  effectiveDefault: string;
  workingAgency: string;
  canOverview: boolean;
  /** 2.5.2 GR — the built-in agency's name now (id "global"); absent from an older server. */
  globalName?: string;
};

type AgencyCtx = {
  /** The agency the lists use (never "overview"). */
  agency: string;
  agencyName: string;
  onOverview: boolean;
  choices: AgencyChoice[];
  canOverview: boolean;
  /** The built-in agency's name now (2.5.2 GR); read it with useGlobalName. */
  globalName: string;
  /** The person's own default, as stored (null when none). */
  storedDefault: string | null;
  /** Where a visit with no agency in its address starts. */
  effectiveDefault: string;
  nameOf: (id: string) => string;
  /**
   * Switch to an agency, or to the overview. The page's object (a run, a job
   * being edited) is dropped and the page's list shown, unless keepPage says
   * the switch is TO that object's agency (useEnsureAgency).
   */
  switchTo: (id: string, notice?: string, opts?: { keepPage?: boolean }) => void;
  /** Store (or clear, with null) the person's own default. */
  setDefault: (id: string | null) => Promise<string | null>;
};

const Ctx = createContext<AgencyCtx | null>(null);

export function useAgency(): AgencyCtx {
  const v = useContext(Ctx);
  if (!v) throw new Error("useAgency outside AgencyProvider");
  return v;
}

/** useAgencyOptional is useAgency for a component that may render outside the
 * provider (a test, the login screen): null there. */
export function useAgencyOptional(): AgencyCtx | null {
  return useContext(Ctx);
}

/**
 * useGlobalName — the built-in agency's name now (2.5.2 GR): "Central" on a new
 * installation, "Global" on an older one until a global administrator renames
 * it. Read it here, never write the literal: an owner name compared with
 * "Global" stops matching the day the agency is renamed. "Global" outside the
 * provider (a test) or from a server that does not say.
 */
export function useGlobalName(): string {
  return useContext(Ctx)?.globalName ?? "Global";
}

/**
 * useEnsureAgency switches to the agency an object belongs to when it is not
 * the one being viewed, and says so (SW-8): a link into another agency's run,
 * job, workflow or schedule. Nothing happens for an owner the person may not
 * choose (the page answers as the server does).
 */
export function useEnsureAgency(owner: string | null | undefined) {
  const ctx = useAgencyOptional();
  useEffect(() => {
    if (!ctx || !owner) return;
    if (owner === ctx.agency && !ctx.onOverview) return;
    if (!ctx.choices.some((ch) => ch.id === owner && ch.kind === "agency")) return;
    ctx.switchTo(owner, `Switched to ${ctx.nameOf(owner)}`, { keepPage: true });
    // Only when the object's owner, or the agency viewed, changes.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [owner, ctx?.agency]);
}

export function AgencyProvider({ children }: { children: ReactNode }) {
  const location = useLocation();
  const navigate = useNavigate();
  const [me, setMe] = useState<MeContext | null>(null);
  const [failed, setFailed] = useState<string | null>(null);
  const [agency, setAgency] = useState<string>("");
  const [toast, fire] = useToast(4000);
  const started = useRef(false);
  // The agency a switch from the switcher is on its way to (C11, defect 85):
  // the address is changed first, and the effect below takes the agency from
  // it, without the "Switched to" notice a link's switch gets.
  const quietSwitch = useRef<string | null>(null);

  const load = useCallback(async () => {
    const { data, error } = await api.GET("/me/context");
    if (error || !data) {
      setFailed(errMsg(error));
      return null;
    }
    setMe(data as MeContext);
    return data as MeContext;
  }, []);

  useEffect(() => {
    void load();
    const again = () => void load();
    window.addEventListener(AGENCY_REFRESH_EVENT, again);
    return () => window.removeEventListener(AGENCY_REFRESH_EVENT, again);
  }, [load]);

  const isChoice = useCallback(
    (id: string | null) => !!me && !!id && me.choices.some((ch) => ch.id === id && ch.kind === "agency"),
    [me],
  );
  const nameOf = useCallback(
    (id: string) =>
      id === OVERVIEW
        ? "All agencies"
        : me?.choices.find((ch) => ch.id === id)?.name ?? (id === GLOBAL_AGENCY_ID ? (me?.globalName ?? "Global") : id),
    [me],
  );

  // Keep the agency and the address in step: an agency in the address is
  // taken (a link into another agency switches, and says so); one that is not
  // among the person's choices gives way to their default with a notice that
  // does not say why (SW-15); an address with none gets the current one.
  const urlAgency = new URLSearchParams(location.search).get(AGENCY_PARAM);
  useEffect(() => {
    if (!me) return;
    const params = new URLSearchParams(location.search);
    let next = agency;
    if (!started.current) {
      started.current = true;
      next = me.workingAgency;
      // A person whose start is the overview lands on the Dashboard with All
      // agencies chosen (2.5.2 OP-25).
      if (!urlAgency && me.effectiveDefault === OVERVIEW && location.pathname === "/") {
        setAgency(next);
        navigate({ pathname: "/", search: `?${AGENCY_PARAM}=${OVERVIEW}` }, { replace: true });
        return;
      }
    }
    // All agencies (2.5.2 OP-21) is the Dashboard's, for those whose access
    // reaches every agency: the address keeps it, and the agency the lists use
    // stays the working one (OP-24). Elsewhere, or for anyone else, it gives
    // way to that agency like any agency that is not a choice.
    if (urlAgency === OVERVIEW && me.canOverview && location.pathname === "/") {
      if (!isChoice(next)) next = me.workingAgency;
      if (next !== agency) setAgency(next);
      return;
    }
    if (urlAgency === OVERVIEW) {
      if (!me.canOverview) fire(`That agency is not available to you; showing ${nameOf(next || me.workingAgency)}`);
      if (!isChoice(next)) next = me.workingAgency;
    } else if (urlAgency && urlAgency !== agency) {
      if (isChoice(urlAgency)) {
        if (agency && quietSwitch.current !== urlAgency) fire(`Switched to ${nameOf(urlAgency)}`);
        quietSwitch.current = null;
        next = urlAgency;
      } else {
        fire(`That agency is not available to you; showing ${nameOf(next)}`);
      }
    } else if (!isChoice(next)) {
      // Grants changed, and the agency in use is no longer the person's.
      next = me.workingAgency;
    }
    if (next !== agency) setAgency(next);
    if (params.get(AGENCY_PARAM) !== next) {
      params.set(AGENCY_PARAM, next);
      navigate({ pathname: location.pathname, search: "?" + params.toString(), hash: location.hash }, { replace: true });
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [me, urlAgency, location.pathname]);

  const switchTo = useCallback(
    (id: string, notice?: string, opts?: { keepPage?: boolean }) => {
      if (id === OVERVIEW) {
        navigate({ pathname: "/", search: `?${AGENCY_PARAM}=${OVERVIEW}` });
        return;
      }
      if (!isChoice(id)) return;
      const params = new URLSearchParams(location.search);
      let path = location.pathname;
      if (opts?.keepPage) {
        setAgency(id);
      } else {
        // The address goes first and the agency follows it (the effect above):
        // a navigation is a transition (React Router 7), so setting the agency
        // here as well rendered the page in the new agency with the old
        // object still in the address, and the page switched back to it.
        quietSwitch.current = id;
        // 2.5.0 C11, defect 85: the page's object (?trace=, ?id=, ?agencyId=,
        // a folder ?path=) is another agency's once the switch is made, and the
        // page switched straight back to it (useEnsureAgency). Only what says
        // how a page is shown stays, and an editor gives way to its list.
        let dropped = false;
        for (const k of [...params.keys()]) {
          if (k !== AGENCY_PARAM && !VIEW_PARAMS.has(k)) {
            params.delete(k);
            dropped = true;
          }
        }
        if (dropped && EDITOR_LIST[path]) path = EDITOR_LIST[path];
      }
      params.set(AGENCY_PARAM, id);
      navigate({ pathname: path, search: "?" + params.toString() });
      if (notice) fire(notice);
    },
    [isChoice, location.pathname, location.search, navigate, fire],
  );

  const setDefault = useCallback(
    async (id: string | null) => {
      const { error } = await api.PUT("/me/preferences", {
        params: { header: csrfHeader },
        body: { defaultAgencyId: id },
      });
      if (error) return errMsg(error);
      await load();
      fire(id ? `${nameOf(id)} is now where you start` : "You start where your access says");
      return null;
    },
    [load, fire, nameOf],
  );

  // The Dashboard with All agencies chosen (2.5.2 OP-21).
  const onOverview = location.pathname === "/" && urlAgency === OVERVIEW && !!me?.canOverview;
  const value = useMemo<AgencyCtx | null>(() => {
    if (!me || !agency) return null;
    return {
      agency,
      agencyName: nameOf(agency),
      onOverview,
      choices: me.choices,
      canOverview: me.canOverview,
      globalName: me.globalName ?? "Global",
      storedDefault: me.storedDefault ?? null,
      effectiveDefault: me.effectiveDefault,
      nameOf,
      switchTo,
      setDefault,
    };
  }, [me, agency, onOverview, nameOf, switchTo, setDefault]);

  if (failed && !me) {
    return (
      <div style={{ height: "100vh", display: "grid", placeItems: "center", background: c.bg, color: c.danger }}>
        Your agencies could not be read ({failed}). Reload the page to try again.
      </div>
    );
  }
  // No list is rendered, so none is fetched, before the agency is known (F1):
  // a fetch without the header would show every agency's rows for a moment.
  if (!value) {
    return <div style={{ height: "100vh", display: "grid", placeItems: "center", background: c.bg, color: c.textSec }}>Loading…</div>;
  }
  // Set during render, before any child's effect fetches, so a list never
  // asks without the agency.
  setRequestAgency(value.agency);
  // 2.5.2 GR: the built-in agency's name for plain functions and labels (globalName.ts).
  setGlobalAgencyName(value.globalName);
  return (
    <Ctx.Provider value={value}>
      {children}
      <Toast message={toast} />
    </Ctx.Provider>
  );
}

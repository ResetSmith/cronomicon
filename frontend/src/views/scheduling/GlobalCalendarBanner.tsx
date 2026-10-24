// CAL-28 — the active-global banner. A global calendar unions its days into every
// schedule entry's skip set of ITS agency (since 2.4.0, §0a), so one covering
// today means that agency's schedules are silent; the banner names whose. A global suppression nobody can see is the worst failure mode this
// feature has, which is why this is not optional polish: it names the calendar
// and the day's label wherever an operator would otherwise just see nothing run.
//
// The expiring/expired GLOBAL case folds in here too (CAL-16), since a global
// calendar that ran out of days stops freezing anything — a change freeze that
// silently lifted is the same class of invisible failure, pointing the other way.
import { c } from "../../theme";
import { calendarDaysKey, calendarExpiry, dayInAppZone, useCalendarDays, useCalendars } from "./calendars";
import { api } from "../../api/client";
import { useGet, rows } from "../../hooks";
import { globalAgencyName } from "../../globalName";

export function GlobalCalendarBanner({ onGoToCalendars }: { onGoToCalendars?: () => void }) {
  const { calendars, expiryWarningDays } = useCalendars();
  const globals = calendars.filter((cal) => cal.global);
  // Each read by its name AND its owner: two agencies may each have a global
  // calendar of a name, and by the name alone one's days were read for both.
  const { days } = useCalendarDays(globals.map((cal) => ({ name: cal.name ?? "", owner: cal.ownerAgency })));
  const today = dayInAppZone();
  const catalogQ = useGet<unknown>(() => api.GET("/agencies"), []);
  const agencyName = (id?: string | null) =>
    !id || id === "global" ? globalAgencyName() : (rows<{ id: string; name: string }>(catalogQ.data).find((a) => a.id === id)?.name ?? id);

  // Covering today: the fleet is quiet right now, and this is the only place
  // that says why.
  const activeToday: { name: string; key: string; label?: string; owner: string }[] = [];
  for (const cal of globals) {
    const hit = (days[calendarDaysKey(cal.name ?? "", cal.ownerAgency)] ?? []).find((d) => d.day === today);
    if (hit) activeToday.push({ name: cal.name ?? "", key: calendarDaysKey(cal.name ?? "", cal.ownerAgency), label: hit.label, owner: agencyName(cal.ownerAgency) });
  }
  const owners = [...new Set(activeToday.map((g) => g.owner))];

  const lapsed = globals
    .map((cal) => ({ name: cal.name ?? "", key: calendarDaysKey(cal.name ?? "", cal.ownerAgency), state: calendarExpiry(cal, expiryWarningDays), daysRemaining: cal.daysRemaining }))
    .filter((g) => g.state !== null);

  if (activeToday.length === 0 && lapsed.length === 0) return null;

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 8, marginBottom: 14 }}>
      {activeToday.length > 0 && (
        <div style={bannerStyle(c.warning)}>
          <strong>
            Nothing of {owners.join(", ")} starts today
            {activeToday.length === 1 ? "" : ` by ${activeToday.length} freeze calendars`}.
          </strong>{" "}
          {activeToday.map((g, i) => (
            <span key={g.key}>
              {i > 0 ? " · " : ""}
              <span style={{ fontFamily: c.mono }}>{g.name}</span>
              {owners.length > 1 ? ` (${g.owner})` : ""}
              {g.label ? ` — ${g.label}` : ""}
            </span>
          ))}
          . A freeze stops a person's Run too; other agencies are unaffected.
          {onGoToCalendars && (
            <button type="button" onClick={onGoToCalendars} style={linkBtn()}>
              Review calendars
            </button>
          )}
        </div>
      )}
      {lapsed.map((g) => (
        <div key={g.key} style={bannerStyle(g.state === "expired" ? c.danger : c.warning)}>
          The freeze calendar <span style={{ fontFamily: c.mono }}>{g.name}</span>{" "}
          {g.state === "expired"
            ? "has no future days left, so it no longer suppresses anything — runs it used to hold back are firing again."
            : `${coverageEnds(g.daysRemaining)}. Renew its dates before it silently stops suppressing.`}
          {onGoToCalendars && (
            <button type="button" onClick={onGoToCalendars} style={linkBtn()}>
              Renew it
            </button>
          )}
        </div>
      ))}
    </div>
  );
}

// A calendar whose last day IS today is at zero days remaining — "runs out of
// days in 0 days" is both clumsy and easy to misread as "already expired", which
// is a different state with a different banner.
const coverageEnds = (daysRemaining?: number | null): string => {
  if (daysRemaining === 0) return "covers today and nothing after it";
  if (daysRemaining === 1) return "runs out of days tomorrow";
  return `runs out of days in ${daysRemaining} days`;
};

// Functions, not module consts: a module-level style object captures whatever the
// palette held at import time and never repaints on the theme toggle (theme.ts).
const bannerStyle = (tone: string): React.CSSProperties => ({
  padding: "10px 14px",
  borderRadius: c.radiusSurface,
  border: `1px solid ${tone}50`,
  background: `${tone}14`,
  color: c.text,
  fontSize: c.fontSm,
  lineHeight: 1.5,
});
const linkBtn = (): React.CSSProperties => ({
  marginLeft: 8,
  padding: 0,
  background: "none",
  border: "none",
  color: c.primary,
  fontSize: c.fontSm,
  fontFamily: "inherit",
  cursor: "pointer",
  textDecoration: "underline",
});

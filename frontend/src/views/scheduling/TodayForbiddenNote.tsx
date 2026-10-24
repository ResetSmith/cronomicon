import { c } from "../../theme";
import type { components } from "../../api/schema";

type CalendarToday = components["schemas"]["CalendarToday"];

// 2.5.3 CA-32: a job's own Forbid and Allow calendars gate every automatic
// start, not a person's Run. The Run dialog says when today is one the job's
// calendars stop (the server's calendarToday, present only then), and the run
// goes ahead if the person runs it.
export function TodayForbiddenNote({ today, allow }: { today?: CalendarToday | null; allow?: string[] | null }) {
  if (!today?.calendar) return null;
  const said =
    today.polarity === "only"
      ? `Today is not in ${(allow && allow.length > 0 ? allow : [today.calendar]).join(", ")}.`
      : `Today is forbidden by ${today.calendar}${today.label ? ` (${today.label})` : ""}.`;
  return (
    <div
      role="note"
      data-testid="today-forbidden"
      style={{
        marginTop: 12,
        padding: "8px 12px",
        borderRadius: c.radiusSurface,
        border: `1px solid ${c.warning}40`,
        background: c.warningBg,
        color: c.warning,
        fontSize: c.fontSm,
      }}
    >
      {said} Its schedules and triggers do not start it today; a Run from here still runs.
    </div>
  );
}

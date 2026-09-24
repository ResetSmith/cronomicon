// CAL-29 — the History calendar filter, shared by the Executions and Workflow
// Runs tabs so both ask the same question the same way.
//
// The wire value is the calendar NAME (or "*" for any suppression), matched by
// the server against the structured suppressed_by_calendar column — never a
// string match on the reason text, so rewording the message cannot silently
// break "every run suppressed by federal-holidays this fiscal year".
import { FilterSelect } from "../../components/ui";
import { useCalendars } from "./calendars";

const ALL = "All";
const ANY = "Any calendar";

export function CalendarFilterSelect({ value, onChange }: { value: string; onChange: (wire: string) => void }) {
  const { calendars } = useCalendars();
  // No calendars authored and no filter active: the dropdown could only offer
  // "All", so it is noise in the filter bar rather than a control.
  if (calendars.length === 0 && !value) return null;

  const names = calendars.map((cal) => cal.name ?? "").filter(Boolean);
  // A filter can outlive its calendar (force-delete leaves the name behind in
  // history rows), so keep the active value selectable rather than snapping the
  // dropdown back to "All" while the rows stay filtered.
  const options = [ALL, ANY, ...names];
  if (value && value !== "*" && !names.includes(value)) options.push(value);

  const label = value === "" ? ALL : value === "*" ? ANY : value;
  return (
    <FilterSelect
      label="Calendar"
      value={label}
      options={options}
      onChange={(v) => onChange(v === ALL ? "" : v === ANY ? "*" : v)}
    />
  );
}

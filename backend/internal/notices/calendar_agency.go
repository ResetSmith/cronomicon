package notices

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/ResetSmith/cronomicon/internal/agencyid"
	"github.com/ResetSmith/cronomicon/internal/calendar"
)

// KindCalendarOtherAgency — a schedule entry of an agency's definition, or a
// schedule an agency owns, names a calendar of another agency (2.4.0, §0a;
// SO-1). Until then every calendar was Global's and any entry named any.
// The binding still applies, and follows the calendar's edits (an entry does
// not start firing on its days), but it cannot be saved again as it is: an
// entry binds its own agency's calendars. The owner's choice of 2026-10-14:
// the link stays live until names are per agency (I2), whose migration gives
// each such agency a copy of the calendar under the same name; holiday
// calendars then reach an agency as a copy of Global's with "an update is
// available" (C10, IN-12).
//
// Subject: `<definition uid or schedule:uid>:<entry>:<calendar>`. Filed under
// the binding's agency. It clears when the binding names its own agency's.
const KindCalendarOtherAgency = "calendar_other_agency"

// KindGlobalCalendarScope — a calendar of Global's is global (2.4.0, §0a):
// until then it froze every agency's fires; now it freezes Global's only, and
// a freeze of the whole installation is the installation-wide freeze. Raised
// while there is an agency besides Global. Subject: the calendar's name. Filed
// under Global. It clears when the calendar is not global, or goes.
const KindGlobalCalendarScope = "global_calendar_scope"

func checkCalendarAgencies(ctx context.Context, database *sql.DB) error {
	owner := map[string]string{}
	rows, err := database.QueryContext(ctx, `SELECT name, COALESCE(owner_agency, 'global'), global FROM calendars`)
	if err != nil {
		return err
	}
	var globals []string
	for rows.Next() {
		var name, o string
		var g int
		if err := rows.Scan(&name, &o, &g); err != nil {
			rows.Close()
			return err
		}
		owner[name] = o
		if g == 1 && o == agencyid.Global {
			globals = append(globals, name)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	// Bindings: definitions' entries, and schedules (a schedule's calendars
	// are copied into the entries bound to it, and are its owner's to name).
	type binding struct{ agency, subject, what, cal string }
	var bindings []binding
	collect := func(agency, subject, what, skip, only string) {
		for _, c := range append(calendar.ParseNames(skip), calendar.ParseNames(only)...) {
			if o, ok := owner[c]; ok && o != agency {
				bindings = append(bindings, binding{agency, subject + ":" + c, what, c})
			}
		}
	}
	erows, err := database.QueryContext(ctx, `
		SELECT owner_uid, owner_kind, owner_name, name, COALESCE(skip_calendars, ''), COALESCE(only_calendars, '')
		  FROM definition_schedules
		 WHERE COALESCE(owner_uid, '') <> '' AND (COALESCE(skip_calendars, '') <> '' OR COALESCE(only_calendars, '') <> '')`)
	if err != nil {
		return err
	}
	type entry struct{ uid, kind, owner, name, skip, only string }
	var entries []entry
	for erows.Next() {
		var e entry
		if err := erows.Scan(&e.uid, &e.kind, &e.owner, &e.name, &e.skip, &e.only); err != nil {
			erows.Close()
			return err
		}
		entries = append(entries, e)
	}
	erows.Close()
	if err := erows.Err(); err != nil {
		return err
	}
	for _, e := range entries {
		var agency sql.NullString
		if err := database.QueryRowContext(ctx, `SELECT `+agencyid.DefinitionSQL, e.uid, e.kind).Scan(&agency); err != nil {
			return err
		}
		if !agency.Valid {
			continue
		}
		collect(agency.String, e.uid+":"+e.name, fmt.Sprintf("the schedule entry %s of the %s %s", e.name, e.kind, e.owner), e.skip, e.only)
	}
	srows, err := database.QueryContext(ctx, `
		SELECT uid, name, owner_agency, COALESCE(skip_calendars, ''), COALESCE(only_calendars, '')
		  FROM schedules WHERE deleted_at IS NULL AND (COALESCE(skip_calendars, '') <> '' OR COALESCE(only_calendars, '') <> '')`)
	if err != nil {
		return err
	}
	for srows.Next() {
		var uid, name, agency, skip, only string
		if err := srows.Scan(&uid, &name, &agency, &skip, &only); err != nil {
			srows.Close()
			return err
		}
		collect(agency, "schedule:"+uid, "the schedule "+name, skip, only)
	}
	srows.Close()
	if err := srows.Err(); err != nil {
		return err
	}
	var found []Finding
	for _, b := range bindings {
		found = append(found, Finding{
			AgencyID: b.agency,
			Subject:  b.subject,
			Detail: fmt.Sprintf("%s names the calendar %s, which is another agency's. It still applies, and follows "+
				"that calendar's edits, but it cannot be saved again as it is: an entry names its own agency's calendars. "+
				"Give this agency a calendar of its own (a copy of the other's) and name it instead. This notice clears "+
				"when it is done.", capitalize(b.what), b.cal),
		})
	}
	if err := Reconcile(ctx, database, KindCalendarOtherAgency, found); err != nil {
		return err
	}

	var others int
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM agencies WHERE id <> ?`, agencyid.Global).Scan(&others); err != nil {
		return err
	}
	var scope []Finding
	if others > 0 {
		for _, c := range globals {
			scope = append(scope, Finding{
				AgencyID: agencyid.Global,
				Subject:  c,
				Detail: fmt.Sprintf("The calendar %s is global and Global's. Until 2.4.0 a global calendar of Global's "+
					"froze every agency's fires; now a calendar freezes its own agency's only, so it freezes Global's. "+
					"To stop every agency's fires, use the installation-wide freeze (Settings); to give agencies these "+
					"days, give each a copy of the calendar. This notice clears when the calendar is not global.", c),
			})
		}
	}
	return Reconcile(ctx, database, KindGlobalCalendarScope, scope)
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	if s[0] >= 'a' && s[0] <= 'z' {
		return string(s[0]-'a'+'A') + s[1:]
	}
	return s
}

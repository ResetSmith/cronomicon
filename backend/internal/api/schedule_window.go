package api

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Schedule activation windows (AW, the schedule-update plan).
//
// A schedule entry may carry an optional start_at/end_at pair bounding WHEN its
// cron is allowed to fire, without touching the expression itself. This file
// owns the shared parse/validate/serialize helpers used by every write surface
// (schedule-def compose, job compose, workflow compose) and every read
// projection, so all of them agree on the wire format and the error text.
//
// Wire format is RFC3339. Values are normalized to UTC on the way in, so the
// stored string is canonical regardless of the offset the client sent.

// errWindowOrder is returned when the bounds are present but inverted. A window
// that closes before it opens can never fire, which is far more likely a typo
// than an intent worth persisting.
var errWindowOrder = errors.New("startAt must be before endAt")

// parseWindowInput validates and normalizes an optional RFC3339 bound from a
// request body. An absent, null, or blank value yields (nil, nil) — meaning
// unbounded on that side, which is the pre-window behavior.
func parseWindowInput(field string, v *string) (*string, error) {
	if v == nil {
		return nil, nil
	}
	raw := strings.TrimSpace(*v)
	if raw == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil, errors.New("invalid " + field + " (want an RFC3339 timestamp, e.g. 2026-08-05T17:00:00Z)")
	}
	norm := t.UTC().Format(time.RFC3339)
	return &norm, nil
}

// parseWindowPair validates both bounds together, enforcing ordering. The
// returned values are canonical UTC strings ready to bind into a query.
func parseWindowPair(startAt, endAt *string) (*string, *string, error) {
	start, err := parseWindowInput("startAt", startAt)
	if err != nil {
		return nil, nil, err
	}
	end, err := parseWindowInput("endAt", endAt)
	if err != nil {
		return nil, nil, err
	}
	if start != nil && end != nil {
		st, _ := time.Parse(time.RFC3339, *start)
		et, _ := time.Parse(time.RFC3339, *end)
		if !st.Before(et) {
			return nil, nil, errWindowOrder
		}
	}
	return start, end, nil
}

// deref returns the pointed-to string, or "" for nil — for callers that take a
// plain string (the content-hash input, which distinguishes only set-vs-unset).
func deref(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

// windowArg converts an optional bound to a bind value, so a nil bound writes a
// real SQL NULL rather than an empty string (which would parse as unbounded but
// read back as "set").
func windowArg(v *string) any {
	if v == nil {
		return nil
	}
	return *v
}

// nullStringOf lifts an optional canonical bound back into a sql.NullString, so
// the window helpers can be reused on values that never touched the database.
func nullStringOf(v *string) sql.NullString {
	if v == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: *v, Valid: true}
}

// windowStrings converts scanned columns to optional response fields.
func windowStrings(startAt, endAt sql.NullString) (*string, *string) {
	var start, end *string
	if startAt.Valid && startAt.String != "" {
		s := startAt.String
		start = &s
	}
	if endAt.Valid && endAt.String != "" {
		e := endAt.String
		end = &e
	}
	return start, end
}

// windowTimes parses scanned columns into instants for projection. Unparseable
// values degrade to unbounded — the same tolerance the scheduler applies, so a
// malformed row projects exactly as it fires.
func windowTimes(startAt, endAt sql.NullString) (*time.Time, *time.Time) {
	return windowBound(startAt), windowBound(endAt)
}

func windowBound(v sql.NullString) *time.Time {
	if !v.Valid || v.String == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, v.String)
	if err != nil {
		return nil
	}
	return &t
}

// nullableOf lifts a plain string to an optional field, treating empty as absent
// so a blank interval writes SQL NULL rather than an empty string (the two would
// otherwise be indistinguishable when reading the mode back).
func nullableOf(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

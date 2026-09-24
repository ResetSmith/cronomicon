package gitlab

import (
	"errors"
	"strings"
	"time"
)

// Schedule activation window normalization (AW-8, the schedule-update plan).
//
// A schedule entry — inline on a job/workflow, or a first-class schedules/*.yaml
// doc — may declare `startAt` / `endAt` RFC3339 bounds constraining when its cron
// is allowed to fire. This mirrors the API-side validation so a window authored
// in Git is held to exactly the same contract as one authored in-app, and stores
// the same canonical UTC form either way.

// NormalizeWindow validates a start/end pair and returns them canonicalized to
// UTC RFC3339. Empty strings pass through as empty (unbounded on that side),
// which is the pre-window behavior for every existing definition.
func NormalizeWindow(startAt, endAt string) (string, string, error) {
	start, err := normalizeBound("startAt", startAt)
	if err != nil {
		return "", "", err
	}
	end, err := normalizeBound("endAt", endAt)
	if err != nil {
		return "", "", err
	}
	if start != "" && end != "" {
		st, _ := time.Parse(time.RFC3339, start)
		et, _ := time.Parse(time.RFC3339, end)
		if !st.Before(et) {
			return "", "", errors.New("startAt must be before endAt")
		}
	}
	return start, end, nil
}

// parseRFC3339Ptr lifts a canonical bound string to an instant for spec
// validation, returning nil for an empty or unparseable value (the same
// tolerance the scheduler applies).
func parseRFC3339Ptr(v string) *time.Time {
	if v == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return nil
	}
	return &t
}

func normalizeBound(field, v string) (string, error) {
	raw := strings.TrimSpace(v)
	if raw == "" {
		return "", nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return "", errors.New("invalid " + field + " (want an RFC3339 timestamp, e.g. 2026-08-05T17:00:00Z)")
	}
	return t.UTC().Format(time.RFC3339), nil
}

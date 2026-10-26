package notices

// The notices of a calendar an agency copied from Central's catalogue (2.5.3,
// CA-27, CA-28). They are written where the copy changes (the calendar
// routes), not by RunChecks, as git_sync_problems is by the sync: whether an
// update can be applied is the calendar routes' rule (CAL-27, the agency's
// bindings), and a check that reconciled would resolve what it cannot judge.
//
// Subject: `calendar:<the copy's uid>`. Filed under the copy's agency.
const (
	// KindCalendarUpdateAvailable — the catalogue calendar a copy was taken
	// from has changed since: the copy waits for its agency's Update (update
	// mode Ask), or an automatic update was refused by the agency's bindings
	// and says why. It resolves when the copy is up to date, made the agency's
	// own, or deleted.
	KindCalendarUpdateAvailable = "calendar_update_available"
	// KindCalendarNoLongerOffered — the catalogue calendar a copy was taken
	// from was deleted: the copy stays as it is, read-only, until it is made
	// the agency's own or deleted, which resolves it.
	KindCalendarNoLongerOffered = "calendar_no_longer_offered"
)

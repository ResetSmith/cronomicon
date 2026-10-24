package calendar

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
)

// Content is what a copy of a catalogue calendar takes from it (2.5.3, CA-26
// to CA-28): its description, its two flags and its days with their labels.
// Not its name, which the agency adding a copy may change.
type Content struct {
	Description      string
	Global           bool
	RecordSuppressed bool
	Days             DaySet
}

// ContentHash identifies a Content: two calendars with the same description,
// flags, days and labels have the same hash, whatever their names. A copy
// stores the hash it holds (calendars.origin_hash, migration 1700); it is
// behind while the catalogue calendar's differs.
func ContentHash(c Content) string {
	h := sha256.New()
	flag := func(b bool) string {
		if b {
			return "1"
		}
		return "0"
	}
	// Lines of tab-separated fields, each escaped, so no description or label
	// can make two contents read alike.
	esc := strings.NewReplacer(`\`, `\\`, "\t", `\t`, "\n", `\n`)
	h.Write([]byte("v1\n" + esc.Replace(c.Description) + "\n" + flag(c.Global) + "\t" + flag(c.RecordSuppressed) + "\n"))
	days := make([]string, 0, len(c.Days))
	for d := range c.Days {
		days = append(days, d)
	}
	sort.Strings(days)
	for _, d := range days {
		h.Write([]byte(esc.Replace(d) + "\t" + esc.Replace(c.Days[d]) + "\n"))
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// ContentDiff is what an update from the catalogue would change in a copy.
type ContentDiff struct {
	// DaysAdded and DaysRemoved are the days the update adds and removes, in
	// order; DaysRelabelled those it keeps with another label.
	DaysAdded, DaysRemoved, DaysRelabelled []string
	DescriptionChanged                     bool
	// GlobalChanged and RecordSuppressedChanged say a flag is changed; its new
	// value is the catalogue's.
	GlobalChanged, RecordSuppressedChanged bool
}

// Empty reports whether the update would change nothing.
func (d ContentDiff) Empty() bool {
	return len(d.DaysAdded) == 0 && len(d.DaysRemoved) == 0 && len(d.DaysRelabelled) == 0 &&
		!d.DescriptionChanged && !d.GlobalChanged && !d.RecordSuppressedChanged
}

// Diff is what taking `to` in place of `from` changes.
func Diff(from, to Content) ContentDiff {
	var d ContentDiff
	for day, label := range to.Days {
		was, ok := from.Days[day]
		switch {
		case !ok:
			d.DaysAdded = append(d.DaysAdded, day)
		case was != label:
			d.DaysRelabelled = append(d.DaysRelabelled, day)
		}
	}
	for day := range from.Days {
		if _, ok := to.Days[day]; !ok {
			d.DaysRemoved = append(d.DaysRemoved, day)
		}
	}
	sort.Strings(d.DaysAdded)
	sort.Strings(d.DaysRemoved)
	sort.Strings(d.DaysRelabelled)
	d.DescriptionChanged = from.Description != to.Description
	d.GlobalChanged = from.Global != to.Global
	d.RecordSuppressedChanged = from.RecordSuppressed != to.RecordSuppressed
	return d
}

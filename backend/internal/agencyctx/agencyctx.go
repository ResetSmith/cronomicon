// Package agencyctx is the agency a request works in (2.4.4; the 2.5.0 plan's
// C9, re-cut by the owner on 2026-10-16).
//
// The shell names the agency it works in on every request, in Header, and each
// list answers with that agency's rows only. It is a VIEW, not a permission:
// the clause is added to the filter a list has today, never in its place, so
// naming an agency narrows a list and never widens it. A request that names
// none answers exactly as before 2.4.4, which is what scripts, service
// accounts and the CI template send (SW-11).
//
// An agency the caller may not read narrows to nothing, with no error that
// would say whether the agency exists (the 2.5.0 plan's §9 gotcha). Global is
// everyone's (auth.Identity.ReadsAgency).
package agencyctx

import (
	"net/http"
	"strings"

	"github.com/ResetSmith/cronomicon/internal/agencyid"
	"github.com/ResetSmith/cronomicon/internal/auth"
)

// Header names the agency a request works in: an agency's id, or "global".
const Header = auth.AgencyHeader

// unreadable is the context of a request that named an agency its caller may
// not read. No row's owner is ever this value, so every list is empty.
const unreadable = "\x00unreadable"

// From is the agency the request works in, and whether it names one.
func From(r *http.Request) (string, bool) {
	v := strings.TrimSpace(r.Header.Get(Header))
	if v == "" {
		return "", false
	}
	id, ok := auth.IdentityFrom(r.Context())
	if !ok || !id.ReadsAgency(v) {
		return unreadable, true
	}
	return v, true
}

// Readable reports whether an agency From answered is one the caller may read
// (false for the stand-in of one they may not).
func Readable(agency string) bool { return agency != unreadable }

// Narrow is the SQL clause (" AND <owner> = ?") and its argument that narrow a
// list to the request's agency, or nothing when the request names none.
// ownerExpr must already follow the no-owner rule (agencyid.Owner).
func Narrow(r *http.Request, ownerExpr string) (string, []any) {
	a, ok := From(r)
	if !ok {
		return "", nil
	}
	return " AND " + ownerExpr + " = ?", []any{a}
}

// In reports whether a row owned by owner belongs in a list the request asks
// for: always when the request names no agency. owner follows the no-owner rule
// here: the empty string is Global's.
func In(r *http.Request, owner string) bool {
	a, ok := From(r)
	if !ok {
		return true
	}
	if owner == "" {
		owner = agencyid.Global
	}
	return owner == a
}

// Keep is In over a list: the rows whose owner belongs in the request's agency,
// or the list as it is when the request names none.
func Keep[T any](r *http.Request, list []T, owner func(T) string) []T {
	if _, ok := From(r); !ok {
		return list
	}
	out := make([]T, 0, len(list))
	for _, v := range list {
		if In(r, owner(v)) {
			out = append(out, v)
		}
	}
	return out
}

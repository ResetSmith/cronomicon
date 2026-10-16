package api

import (
	"net/http"
	"strings"

	"github.com/ResetSmith/cronomicon/internal/agencyctx"
	"github.com/ResetSmith/cronomicon/internal/agencyid"
	"github.com/ResetSmith/cronomicon/internal/httpx"
)

// Writes in an agency context (2.4.4 S3). A person works in one agency at a
// time, so what they create is that agency's: a create that names no owner
// takes the agency the request works in, and one that names another is
// refused (422 agency_context_mismatch) rather than placed elsewhere. A global
// administrator creates for another agency by switching to it. A request that
// names no agency (a script, a service account) is answered as before.

// ownerInContext is the owner of a row being created: given, or, when the
// request works in an agency and given is empty, that agency. false after a
// refusal is written.
func (s *Server) ownerInContext(w http.ResponseWriter, r *http.Request, given, label string) (string, bool) {
	ctxAgency, named := agencyctx.From(r)
	if !named {
		return given, true
	}
	if strings.TrimSpace(given) == "" {
		if !agencyctx.Readable(ctxAgency) {
			failContextMismatch(w, label)
			return "", false
		}
		return ctxAgency, true
	}
	return given, s.ownerMatchesContext(w, r, given, label)
}

// ownerMatchesContext reports whether a row of owner may be created in the
// agency the request works in: always when it names none. The answer is
// written on a refusal.
func (s *Server) ownerMatchesContext(w http.ResponseWriter, r *http.Request, owner, label string) bool {
	ctxAgency, named := agencyctx.From(r)
	if !named {
		return true
	}
	if strings.TrimSpace(owner) == "" {
		owner = agencyid.Global
	}
	if agencyctx.Readable(ctxAgency) && owner == ctxAgency {
		return true
	}
	failContextMismatch(w, label)
	return false
}

func failContextMismatch(w http.ResponseWriter, label string) {
	httpx.Fail(w, http.StatusUnprocessableEntity, "agency_context_mismatch",
		"this "+label+" would belong to another agency than the one you are working in: switch to that agency to create it there")
}

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/httpx"
	"github.com/ResetSmith/cronomicon/internal/workflow"
)

// Moves become repair (2.4.0 Phase I4; §0a, IN-7; the owner's answers to
// questions 42 to 45).
//
// An object is not moved between agencies: a scope is cloned (R6), a
// definition is assigned (IN-12), a credential or a host record is made again
// in the agency that needs it. What changes an existing object's agency — a
// scope's, a secret's, a variable's, a key's, a host record's or a bastion's
// owner or membership, a runner's serve list or owner — is a global
// administrator's REPAIR of a placement that was a mistake or of an object in
// no agency, and says so: `repair: true` and a reason, which is written to the
// audit log. A write that changes no agency is not a repair, and is held to
// the rules it always was. Until I4 an administrator of both agencies could
// move a secret, a variable, a key, a host record or a bastion (the two-sided
// rule, RF-1b and LR-7); a scope was already a global administrator's
// (bee2b35), and a runner was handed on by MA-12.

// repairInput is a repair's two fields, on every route that can change an
// object's agency.
type repairInput struct {
	Repair bool   `json:"repair"`
	Reason string `json:"reason"`
}

// repairReasonMax bounds the reason written to the audit row.
const repairReasonMax = 500

// requireRepair answers for a write that changes an object's agency: 403
// unless the caller is a global administrator (configureApp on every agency),
// 409 repair_required unless the request says `repair: true`, 422 unless it
// gives a reason of at most repairReasonMax characters. what names the object
// for the messages ("this secret").
func (s *Server) requireRepair(w http.ResponseWriter, r *http.Request, id auth.Identity, rep repairInput, what string) bool {
	if !id.GlobalAdmin(auth.PermConfigureApp) {
		s.denyEntityAgency(w, r, id, auth.PermConfigureApp, auth.AllScopes,
			what+" does not change agency: an agency gets another's by a copy (a scope's clone, a secret or a record "+
				"made again). Changing an object's agency is a global administrator's repair of a placement that was a mistake")
		return false
	}
	if !rep.Repair {
		httpx.Fail(w, http.StatusConflict, "repair_required",
			"this changes the agency of "+what+", which is a repair: send `repair: true` and a `reason`, which is written to the audit log")
		return false
	}
	reason := strings.TrimSpace(rep.Reason)
	if reason == "" || utf8.RuneCountInString(reason) > repairReasonMax {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed",
			"a repair needs a reason, of at most 500 characters: it is written to the audit log")
		return false
	}
	return true
}

// auditRepair writes a repair's row to the audit log: who, what was changed,
// how (change: "agency set to [ag:TAX]", "removed from agency ag:FIN"), and
// the reason given. A row that cannot be written is logged: the write it
// records has been made.
func (s *Server) auditRepair(ctx context.Context, actor, target, change string, rep repairInput) {
	if err := workflow.InsertChangeLog(ctx, s.db, actor, "Agencies", "Repaired", target,
		change+"; reason: "+strings.TrimSpace(rep.Reason)); err != nil && s.log != nil {
		s.log.Error("a repair was made and its audit row could not be written",
			"actor", actor, "target", target, "change", change, "reason", strings.TrimSpace(rep.Reason), "error", err)
	}
}

// agencySet renders an agency list for a repair's audit row.
func agencySet(agencies []string) string {
	return "agency set to [" + strings.Join(agencies, ", ") + "]"
}

// decodeRepairable reads the body of a route whose body is an array: the bare
// array (a write that is not a repair), or {repair, reason, assignments}. It
// answers 422 itself for a body that is neither.
func decodeRepairable[T any](w http.ResponseWriter, r *http.Request, list *[]T, rep *repairInput) bool {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_json", err.Error())
		return false
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '{' {
		var wrapped struct {
			repairInput
			Assignments *[]T `json:"assignments"`
		}
		if err := json.Unmarshal(trimmed, &wrapped); err != nil {
			httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_json", err.Error())
			return false
		}
		// An object without its assignments is not a wrapper: it is a row
		// sent where a list of rows was meant, and changes nothing (from the
		// review of I4: it was answered 200).
		if wrapped.Assignments == nil {
			httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_json",
				"the body is a list of rows, or {repair, reason, assignments: [rows]}")
			return false
		}
		*list, *rep = *wrapped.Assignments, wrapped.repairInput
		return true
	}
	if err := json.Unmarshal(trimmed, list); err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_json", err.Error())
		return false
	}
	return true
}

// sameAgencySet reports whether two agency lists name the same agencies.
func sameAgencySet(a, b []string) bool {
	x, y := slices.Clone(a), slices.Clone(b)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(slices.Compact(x), slices.Compact(y))
}

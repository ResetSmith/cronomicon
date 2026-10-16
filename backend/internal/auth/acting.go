package auth

import (
	"context"
	"net/http"
	"strings"

	"github.com/ResetSmith/cronomicon/internal/agencyid"
	"github.com/ResetSmith/cronomicon/internal/auditlog"
)

// AgencyHeader names the agency a request works in (2.4.4, the agency
// switcher): an agency's id, or "global". agencyctx reads it to narrow lists;
// it is defined here because RequireSession also reads it, for the audit.
const AgencyHeader = "X-Cronomicon-Agency"

// actingAgencyContext marks a global administrator's request that works in an
// agency other than Global (the 2.5.0 plan's IN-9): each change it writes to
// change_log records both identities, "X, acting in Y". Anyone else works in
// their own agencies, and their changes say so by what they change.
func actingAgencyContext(ctx context.Context, r *http.Request, id Identity) context.Context {
	a := strings.TrimSpace(r.Header.Get(AgencyHeader))
	if a == "" || a == agencyid.Global || !id.GlobalAdmin(PermConfigureApp) {
		return ctx
	}
	return auditlog.WithActingAgency(ctx, a)
}

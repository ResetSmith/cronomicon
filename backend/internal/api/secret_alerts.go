package api

import (
	"fmt"
	"net/http"

	"github.com/ResetSmith/cronomicon/internal/agencyid"
	"github.com/ResetSmith/cronomicon/internal/auditlog"
	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/notify"
)

// Alerts about secrets and the backup keys (2.5.1 SG2.7, SG-17). An agency's
// alert rules can now say "tell me when one of our secrets is revealed or
// changed"; its rules see its own rows only (an agency's rule matches its own
// agency's events), Global's see every agency's. The message names the actor
// and says when they are outside the agency, a global administrator included:
// that is the case an agency most wants to hear about. These events are not a
// job's, so only a rule whose target is "all" matches them.

// raiseSecretAlert raises trigger about a secret or key (noun: "Secret" or
// "SSH key") named name, owned by agency, after action ("revealed",
// "rotated", ...). Call it after the change is committed.
func (s *Server) raiseSecretAlert(r *http.Request, trigger, noun, name, agency, action string) {
	if agency == "" {
		agency = agencyid.Global
	}
	actor := "someone"
	if id, ok := auth.IdentityFrom(r.Context()); ok && id.Email != "" {
		actor = id.Email
	}
	agencyName := s.agencyNameOr(r, agency)
	who := actor
	switch auditlog.ActorRelation(r.Context(), agency) {
	case auditlog.RelationGlobalAdmin:
		who += " (a global administrator, outside " + agencyName + ")"
	case auditlog.RelationOutside:
		who += " (outside " + agencyName + ")"
	}
	subject := fmt.Sprintf("[Cronomicon] %s %s %s in %s by %s", noun, name, action, agencyName, who)
	detail := fmt.Sprintf("%s %s of %s was %s by %s.\nIts Usage view (Env Vars) shows what refers to it, which runs used it and every change.",
		noun, name, agencyName, action, who)
	notify.New(s.db, s.cfg, s.log).AlertRaised(notify.AlertEvent{
		Trigger: trigger, OwnerKind: "secret", Agency: agency, Subject: subject, Detail: detail,
	})
}

// raiseBackupKeyAlert tells Global's rules that the backup keys changed
// (SG-7): someone who can change them can point later backups at a key of
// their own.
func (s *Server) raiseBackupKeyAlert(r *http.Request, summary string) {
	actor := "someone"
	if id, ok := auth.IdentityFrom(r.Context()); ok && id.Email != "" {
		actor = id.Email
	}
	notify.New(s.db, s.cfg, s.log).AlertRaised(notify.AlertEvent{
		Trigger: notify.TriggerBackupKeyChanged, OwnerKind: "installation", Agency: agencyid.Global,
		Subject: "[Cronomicon] The backup keys were changed by " + actor,
		Detail:  summary + "\nEvery backup from the next one on is encrypted to these keys. If you did not expect this, check Settings → Backups.",
	})
}

// agencyNameOr is an agency's name, or its id when it cannot be read.
func (s *Server) agencyNameOr(r *http.Request, id string) string {
	var name string
	if err := s.db.QueryRowContext(r.Context(), `SELECT name FROM agencies WHERE id = ?`, id).Scan(&name); err != nil || name == "" {
		return id
	}
	return name
}

package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/httpx"
	"github.com/ResetSmith/cronomicon/internal/settings"
)

// The address alerts link to (2.5.2 OP2.1; settings.AlertLinkAddress). A
// global administrator's, like the rest of Settings → Notifications.
//
//	GET  /api/v1/settings/alert-link-address         the stored address and how it got there
//	PUT  /api/v1/settings/alert-link-address         {address}: set it ("" clears it, and it stays cleared)
//	POST /api/v1/settings/alert-link-address/detect  {address}: the browser's address, recorded only when none is stored
func (s *Server) mountAlertLink(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/settings/alert-link-address", s.requireGlobal(auth.PermConfigureApp)(http.HandlerFunc(s.getAlertLink)))
	mux.Handle("PUT /api/v1/settings/alert-link-address", s.requireGlobal(auth.PermConfigureApp)(http.HandlerFunc(s.putAlertLink)))
	mux.Handle("POST /api/v1/settings/alert-link-address/detect", s.requireGlobal(auth.PermConfigureApp)(http.HandlerFunc(s.detectAlertLink)))
}

func (s *Server) getAlertLink(w http.ResponseWriter, r *http.Request) {
	a, err := settings.ReadAlertLinkAddress(r.Context(), s.db)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	httpx.JSON(w, http.StatusOK, a)
}

// alertLinkBody reads {address}, required: a body without it must not clear
// the stored one.
func alertLinkBody(w http.ResponseWriter, r *http.Request) (string, bool) {
	var in struct {
		Address *string `json:"address"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_json", err.Error())
		return "", false
	}
	if in.Address == nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", `address is required: the address, or "" for no links`)
		return "", false
	}
	return *in.Address, true
}

func (s *Server) putAlertLink(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.IdentityFrom(r.Context())
	address, ok := alertLinkBody(w, r)
	if !ok {
		return
	}
	a, err := settings.SetAlertLinkAddress(r.Context(), s.db, address, id.Email)
	s.alertLinkResult(w, a, err)
}

func (s *Server) detectAlertLink(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.IdentityFrom(r.Context())
	address, ok := alertLinkBody(w, r)
	if !ok {
		return
	}
	a, _, err := settings.DetectAlertLinkAddress(r.Context(), s.db, address, id.Email)
	s.alertLinkResult(w, a, err)
}

func (s *Server) alertLinkResult(w http.ResponseWriter, a settings.AlertLinkAddress, err error) {
	switch {
	case errors.Is(err, settings.ErrInvalidAlertLinkAddress):
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", err.Error())
	case err != nil:
		httpx.Fail500(w, s.log, "db_error", err)
	default:
		httpx.JSON(w, http.StatusOK, a)
	}
}

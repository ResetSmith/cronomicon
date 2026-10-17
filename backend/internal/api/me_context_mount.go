package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/ResetSmith/cronomicon/internal/agencyid"
	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/httpx"
)

// The agency switcher's choices and defaults (2.4.4 S4; plan-2.4.4 §3, SW-2,
// SW-5, SW-6, SW-7, SW-9, SW-13).
//
//	GET /api/v1/me/context      → where the caller may work, and where they start
//	PUT /api/v1/me/preferences  → the caller's own default (clsSelf)

// overviewChoice is the Operations overview's id among the choices: a page,
// not an agency (SW-12).
const overviewChoice = "overview"

type agencyChoice struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"` // overview | agency
}

type myAgencyContext struct {
	Choices          []agencyChoice `json:"choices"`
	StoredDefault    *string        `json:"storedDefault"`
	EffectiveDefault string         `json:"effectiveDefault"`
	WorkingAgency    string         `json:"workingAgency"`
	CanOverview      bool           `json:"canOverview"`
}

func (s *Server) mountMeContext(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/me/context", s.auth.RequireSession(http.HandlerFunc(s.getMyAgencyContext)))
	mux.Handle("PUT /api/v1/me/preferences",
		s.auth.RequireSession(s.auth.RequireCSRF(http.HandlerFunc(s.putMyPreferences))))
}

// canSeeOverview reports whether the caller may open the Operations overview:
// a grant that reaches every agency (a global administrator's has one).
func canSeeOverview(id auth.Identity) bool {
	everywhere, _ := id.AgenciesRead()
	return everywhere
}

// agencyChoices is what the caller may switch to: the overview first for those
// who may see it, then Global, then the agencies they may read, by name and
// ignoring case.
func (s *Server) agencyChoices(ctx context.Context, id auth.Identity) ([]agencyChoice, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name FROM agencies`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var agencies []agencyChoice
	globalName := agencyid.GlobalName
	for rows.Next() {
		var c agencyChoice
		if err := rows.Scan(&c.ID, &c.Name); err != nil {
			return nil, err
		}
		if c.ID == agencyid.Global {
			globalName = c.Name
			continue
		}
		if id.ReadsAgency(c.ID) {
			c.Kind = "agency"
			agencies = append(agencies, c)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(agencies, func(i, j int) bool {
		a, b := strings.ToLower(agencies[i].Name), strings.ToLower(agencies[j].Name)
		if a != b {
			return a < b
		}
		return agencies[i].ID < agencies[j].ID
	})
	out := []agencyChoice{}
	if canSeeOverview(id) {
		out = append(out, agencyChoice{ID: overviewChoice, Name: "Overview: all agencies", Kind: "overview"})
	}
	// Global is a choice for those who read it (2.5.0 C2, question 77), and
	// the one choice of someone who holds no grant at all, so that the shell
	// has an agency to work in (its lists are empty to them).
	if id.ReadsAgency(agencyid.Global) || len(agencies) == 0 {
		out = append(out, agencyChoice{ID: agencyid.Global, Name: globalName, Kind: "agency"})
	}
	return append(out, agencies...), nil
}

// workingAgencyFor is the rule for the agency the lists start in, without the
// stored default and without the overview: Global for a global administrator;
// else the first, by name, of the agencies the caller holds a grant on; else
// Global. choices is agencyChoices' answer (sorted by name).
func workingAgencyFor(id auth.Identity, choices []agencyChoice) string {
	if id.GlobalAdmin(auth.PermConfigureApp) {
		return agencyid.Global
	}
	_, held := id.AgenciesRead()
	heldSet := map[string]bool{}
	for _, a := range held {
		heldSet[a] = true
	}
	for _, c := range choices {
		if c.Kind == "agency" && heldSet[c.ID] {
			return c.ID
		}
	}
	return agencyid.Global
}

// myAgencyContextFor is GET /me/context's answer for the caller.
func (s *Server) myAgencyContextFor(ctx context.Context, id auth.Identity) (myAgencyContext, error) {
	choices, err := s.agencyChoices(ctx, id)
	if err != nil {
		return myAgencyContext{}, err
	}
	out := myAgencyContext{Choices: choices, CanOverview: canSeeOverview(id)}
	var stored sql.NullString
	err = s.db.QueryRowContext(ctx, `SELECT default_agency FROM user_preferences WHERE email = ?`, prefKey(id)).Scan(&stored)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return myAgencyContext{}, err
	}
	if stored.Valid && stored.String != "" {
		v := stored.String
		out.StoredDefault = &v
	}
	working := workingAgencyFor(id, choices)
	switch {
	case out.StoredDefault != nil && isChoice(choices, *out.StoredDefault):
		out.EffectiveDefault = *out.StoredDefault
	case id.GlobalAdmin(auth.PermConfigureApp):
		out.EffectiveDefault = agencyid.Global
	case out.CanOverview:
		out.EffectiveDefault = overviewChoice
	default:
		out.EffectiveDefault = working
	}
	out.WorkingAgency = out.EffectiveDefault
	if out.WorkingAgency == overviewChoice {
		out.WorkingAgency = working
	}
	return out, nil
}

func isChoice(choices []agencyChoice, id string) bool {
	for _, c := range choices {
		if c.ID == id {
			return true
		}
	}
	return false
}

// prefKey is the key of the caller's row in user_preferences: their sign-in's
// email, as recent_logins keys it, in lower case.
func prefKey(id auth.Identity) string { return strings.ToLower(strings.TrimSpace(id.Email)) }

func (s *Server) getMyAgencyContext(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	out, err := s.myAgencyContextFor(r.Context(), id)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (s *Server) putMyPreferences(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok || prefKey(id) == "" {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	var in struct {
		DefaultAgencyID *string `json:"defaultAgencyId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_json", err.Error())
		return
	}
	var stored any
	if in.DefaultAgencyID != nil && strings.TrimSpace(*in.DefaultAgencyID) != "" {
		want := strings.TrimSpace(*in.DefaultAgencyID)
		choices, err := s.agencyChoices(r.Context(), id)
		if err != nil {
			httpx.Fail500(w, s.log, "db_error", err)
			return
		}
		// One answer whether the agency is not there or not the caller's.
		if !isChoice(choices, want) {
			httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_default_agency",
				"that is not an agency you can start in")
			return
		}
		stored = want
	}
	if _, err := s.db.ExecContext(r.Context(), `
		INSERT INTO user_preferences (email, default_agency, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(email) DO UPDATE SET default_agency = excluded.default_agency, updated_at = excluded.updated_at`,
		prefKey(id), stored, time.Now().UTC().Format(time.RFC3339)); err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	out, err := s.myAgencyContextFor(r.Context(), id)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

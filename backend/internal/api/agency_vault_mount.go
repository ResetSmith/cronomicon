package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/ResetSmith/cronomicon/internal/agencyid"
	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/httpx"
	"github.com/ResetSmith/cronomicon/internal/secrets"
	"github.com/ResetSmith/cronomicon/internal/secretstore"
	"github.com/ResetSmith/cronomicon/internal/settings"
)

// A Vault per agency (2.5.1 SG3.3). configureApp on the agency, as for its
// repository (requireRepoAgency); Global's Vault is the installation's.
//
//	GET    /api/v1/agencies/{agencyId}/vault          the connection (never a credential)
//	PUT    /api/v1/agencies/{agencyId}/vault          connect or change it
//	DELETE /api/v1/agencies/{agencyId}/vault          disconnect (409 vault_in_use while it holds Vault rows); its Vault path prefixes go too
//	POST   /api/v1/agencies/{agencyId}/vault/test     log in with a proposed connection
//	POST   /api/v1/agencies/{agencyId}/vault/preview  would each of the agency's Vault references resolve there
//	POST   /api/v1/settings/vault/test                log in with the installation's (a global administrator's)
//
// A save logs in first, and checks every Vault reference the agency owns
// against the new Vault (SG-25): one that would not resolve refuses the save
// (409 references_unresolved, with the list) unless confirmUnresolved is
// set. A value read for the check is discarded; none is ever returned.
func (s *Server) mountAgencyVaults(mux *http.ServeMux) {
	configure := s.requirePerm("configureApp", permConfigureApp)
	mux.Handle("GET /api/v1/agencies/{agencyId}/vault", configure(http.HandlerFunc(s.handleGetAgencyVault)))
	mux.Handle("PUT /api/v1/agencies/{agencyId}/vault", configure(http.HandlerFunc(s.handlePutAgencyVault)))
	mux.Handle("DELETE /api/v1/agencies/{agencyId}/vault", configure(http.HandlerFunc(s.handleDeleteAgencyVault)))
	mux.Handle("POST /api/v1/agencies/{agencyId}/vault/test", configure(http.HandlerFunc(s.handleTestAgencyVault)))
	mux.Handle("POST /api/v1/agencies/{agencyId}/vault/preview", configure(http.HandlerFunc(s.handlePreviewAgencyVault)))
	// 2.5.5 SM-10 (question 226): may the agency name this reference? For the
	// secret and key forms, before they save; the write judges it again.
	mux.Handle("POST /api/v1/agencies/{agencyId}/vault/validate-ref", s.requirePerm("manageEnvVars or configureApp",
		func(p rolePermissions) bool { return p.ManageEnvVars || p.ConfigureApp })(http.HandlerFunc(s.handleValidateStoreRef)))
	mux.Handle("POST /api/v1/settings/vault/test", s.requireGlobal(auth.PermConfigureApp)(http.HandlerFunc(s.handleTestInstallationVault)))
}

// agencyVaultBody is a PUT, test or preview body.
type agencyVaultBody struct {
	settings.AgencyVaultInput
	ConfirmUnresolved bool `json:"confirmUnresolved"`
	// ExampleRef is a reference to read with the proposed connection when the
	// provider has no login of its own and the agency names nothing in it
	// yet (2.5.5 SM-5a, question 227). Read and discarded.
	ExampleRef string `json:"exampleRef"`
}

// vaultRefResult is one reference in a preview.
type vaultRefResult struct {
	settings.AgencyVaultRef
	Resolves bool   `json:"resolves"`
	Problem  string `json:"problem,omitempty"`
}

// agencyVaultAgency gates the route and refuses Global. False: answered.
func (s *Server) agencyVaultAgency(w http.ResponseWriter, r *http.Request) (string, auth.Identity, bool) {
	agency := r.PathValue("agencyId")
	id, ok := s.requireRepoAgency(w, r, agency)
	if !ok {
		return "", id, false
	}
	if agency == agencyid.Global {
		httpx.Fail(w, http.StatusUnprocessableEntity, "use_installation_vault", agencyid.CurrentGlobalName(r.Context(), s.db)+"'s Vault is the installation's: Settings → Vault")
		return "", id, false
	}
	return agency, id, true
}

func (s *Server) handleGetAgencyVault(w http.ResponseWriter, r *http.Request) {
	agency, _, ok := s.agencyVaultAgency(w, r)
	if !ok {
		return
	}
	v, err := settings.GetAgencyVault(r.Context(), s.db, agency)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"agencyId": agency, "connected": v != nil, "vault": v})
}

// prepareAgencyVault decodes and validates a body and builds its client, one
// of its own: the live client in the cache is not touched (2.5.1 SG6.2).
func (s *Server) prepareAgencyVault(w http.ResponseWriter, r *http.Request, agency string) (agencyVaultBody, secrets.VaultClient, bool) {
	var in agencyVaultBody
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", "invalid request body")
		return in, nil, false
	}
	rt, err := settings.PrepareAgencyVault(r.Context(), s.db, s.cfg, agency, in.AgencyVaultInput)
	if err != nil {
		switch {
		case errors.Is(err, settings.ErrStoreProviderInUse):
			httpx.Fail(w, http.StatusConflict, "vault_in_use", err.Error()+
				": change them to stored values or references in the new store, or delete them, before changing the kind of store")
		case errors.Is(err, settings.ErrValidation):
			httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", err.Error())
		default:
			httpx.Fail500(w, s.log, "db_error", err)
		}
		return in, nil, false
	}
	vc, err := settings.NewAgencyVaultClient(r.Context(), s.db, s.cfg, rt)
	if err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", err.Error())
		return in, nil, false
	}
	return in, vc, true
}

// vaultLogin logs in with vc; the error is the sentence a caller is shown.
func vaultLogin(vc secrets.VaultClient) error {
	if l, ok := vc.(secrets.VaultLogin); ok {
		return l.Login()
	}
	return nil
}

// errNoReferenceToTest: a provider with no login of its own is tested by a
// read, and there is nothing to read.
var errNoReferenceToTest = errors.New("this kind of store is tested by reading a secret: the agency names none in it yet, so give an example reference")

// testConnection is the connection test (SM-5a): a login where the provider
// has one; otherwise a read of the example reference, else of the agency's
// first reference. A read that fails because the secret is missing still
// shows the credential works; one that says the store is unusable does not.
func (s *Server) testConnection(r *http.Request, agency string, vc secrets.VaultClient, example string) error {
	if _, ok := vc.(secrets.VaultLogin); ok {
		return vaultLogin(vc)
	}
	ref := strings.TrimSpace(example)
	if ref == "" {
		refs, err := settings.ListAgencyVaultRefs(r.Context(), s.db, agency)
		if err != nil {
			return err
		}
		if len(refs) == 0 {
			return errNoReferenceToTest
		}
		ref = refs[0].Path
	}
	if _, err := vc.Fetch(ref); err != nil && (errors.Is(err, secretstore.ErrStoreUnusable) || example != "") {
		return err
	}
	return nil
}

// previewRefs reads each of the agency's Vault references through vc and
// says whether it resolves. The value is discarded.
func (s *Server) previewRefs(r *http.Request, agency string, vc secrets.VaultClient) ([]vaultRefResult, error) {
	refs, err := settings.ListAgencyVaultRefs(r.Context(), s.db, agency)
	if err != nil {
		return nil, err
	}
	out := make([]vaultRefResult, 0, len(refs))
	for _, ref := range refs {
		res := vaultRefResult{AgencyVaultRef: ref, Resolves: true}
		if _, ferr := vc.Fetch(ref.Path); ferr != nil {
			res.Resolves, res.Problem = false, ferr.Error()
		}
		out = append(out, res)
	}
	return out, nil
}

func (s *Server) handleTestAgencyVault(w http.ResponseWriter, r *http.Request) {
	agency, _, ok := s.agencyVaultAgency(w, r)
	if !ok {
		return
	}
	in, vc, ok := s.prepareAgencyVault(w, r, agency)
	if !ok {
		return
	}
	if err := s.testConnection(r, agency, vc, in.ExampleRef); err != nil {
		httpx.JSON(w, http.StatusOK, map[string]any{"ok": false, "problem": err.Error()})
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handlePreviewAgencyVault(w http.ResponseWriter, r *http.Request) {
	agency, _, ok := s.agencyVaultAgency(w, r)
	if !ok {
		return
	}
	_, vc, ok := s.prepareAgencyVault(w, r, agency)
	if !ok {
		return
	}
	res, err := s.previewRefs(r, agency, vc)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"references": res})
}

func (s *Server) handlePutAgencyVault(w http.ResponseWriter, r *http.Request) {
	agency, id, ok := s.agencyVaultAgency(w, r)
	if !ok {
		return
	}
	in, vc, ok := s.prepareAgencyVault(w, r, agency)
	if !ok {
		return
	}
	if err := s.testConnection(r, agency, vc, in.ExampleRef); err != nil {
		if errors.Is(err, errNoReferenceToTest) {
			httpx.Fail(w, http.StatusUnprocessableEntity, "example_ref_required", err.Error())
			return
		}
		httpx.Fail(w, http.StatusUnprocessableEntity, "vault_login_failed", "the store refused the connection: "+err.Error())
		return
	}
	res, err := s.previewRefs(r, agency, vc)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	var unresolved []vaultRefResult
	for _, x := range res {
		if !x.Resolves {
			unresolved = append(unresolved, x)
		}
	}
	if len(unresolved) > 0 && !in.ConfirmUnresolved {
		httpx.JSON(w, http.StatusConflict, map[string]any{
			"code":       "references_unresolved",
			"message":    "some of the agency's Vault references would not resolve in this Vault; confirm to save anyway",
			"references": unresolved,
		})
		return
	}
	rt, err := settings.PrepareAgencyVault(r.Context(), s.db, s.cfg, agency, in.AgencyVaultInput)
	if err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", err.Error())
		return
	}
	v, err := settings.PutAgencyVault(r.Context(), s.db, s.cfg, agency, rt, id.Email)
	if err != nil {
		httpx.Fail500(w, s.log, "save_failed", err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"agencyId": agency, "connected": true, "vault": v})
}

func (s *Server) handleDeleteAgencyVault(w http.ResponseWriter, r *http.Request) {
	agency, id, ok := s.agencyVaultAgency(w, r)
	if !ok {
		return
	}
	found, err := settings.DeleteAgencyVault(r.Context(), s.db, agency, id.Email)
	switch {
	case errors.Is(err, settings.ErrAgencyVaultInUse):
		httpx.Fail(w, http.StatusConflict, "vault_in_use", err.Error()+": move them out of Vault, or delete them, first")
		return
	case err != nil:
		httpx.Fail500(w, s.log, "db_error", err)
		return
	case !found:
		httpx.Fail(w, http.StatusNotFound, "not_found", "the agency has no Vault of its own")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleTestInstallationVault(w http.ResponseWriter, r *http.Request) {
	vc := settings.InstallationVaultClient(r.Context(), s.db, s.cfg, s.log)
	if vc == nil {
		httpx.JSON(w, http.StatusOK, map[string]any{"ok": false, "problem": "the installation's Vault is not configured"})
		return
	}
	if err := vaultLogin(vc); err != nil {
		httpx.JSON(w, http.StatusOK, map[string]any{"ok": false, "problem": err.Error()})
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleValidateStoreRef answers whether the agency may name a reference
// (2.5.5 SM-10): its store's grammar and its prefixes, by the same judgement
// as the write (settings.JudgeStoreRef). manageEnvVars or configureApp on the
// agency (question 226). It writes nothing.
func (s *Server) handleValidateStoreRef(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	agency := r.PathValue("agencyId")
	if !id.CanAgency(auth.PermManageEnvVars, agency) && !id.CanAgency(auth.PermConfigureApp, agency) {
		s.denyEntityAgency(w, r, id, auth.PermManageEnvVars, agency,
			"you do not administer this agency, so which references it may name is not yours to ask")
		return
	}
	var in struct {
		Ref     string `json:"ref"`
		Current string `json:"current"`
		Label   string `json:"label"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Ref == "" {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", "ref is required")
		return
	}
	label := "secret"
	if in.Label == "key" {
		label = "SSH key"
	}
	if agency == agencyid.Global {
		store, err := settings.StoreProviderOf(r.Context(), s.db, agency)
		if err != nil {
			httpx.Fail500(w, s.log, "db_error", err)
			return
		}
		if err := store.ValidateRef(in.Ref); err != nil {
			httpx.JSON(w, http.StatusOK, settings.StoreRefVerdict{Code: "validation_failed", Reason: err.Error()})
			return
		}
		httpx.JSON(w, http.StatusOK, settings.StoreRefVerdict{OK: true})
		return
	}
	v, err := settings.JudgeStoreRef(r.Context(), s.db, agency, in.Ref, in.Current, label)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	httpx.JSON(w, http.StatusOK, v)
}

package api

import (
	"context"
	"encoding/csv"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/ResetSmith/cronomicon/internal/agencyid"
	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/httpx"
	"github.com/ResetSmith/cronomicon/internal/runref"
)

// An agency's secrets report (2.5.1 SG2.5, SG-15): every stored secret and
// SSH key the agency owns, with what each Usage view says in one row, for the
// agency to hand to its own auditors. No values, ever.
//
//	GET /api/v1/agencies/{agencyId}/secrets-report?format=csv|json&days=90
//
// Read by the agency's people and those who reach every agency (Global's by
// the latter only), as the Usage views are; a secret whose scope the caller
// cannot read is left out, as it is from the secrets list.
func (s *Server) mountSecretsReport(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/agencies/{agencyId}/secrets-report", s.auth.RequireSession(http.HandlerFunc(s.handleSecretsReport)))
	mux.Handle("GET /api/v1/agencies/{agencyId}/held-by-global", s.auth.RequireSession(http.HandlerFunc(s.handleHeldByGlobal)))
}

// Rows Global holds that an agency is listed on (2.5.1 SG2.6, SG-16): the
// secrets and SSH keys several agencies shared before 2.3.0, which stayed
// Global's and, since 2.4.0 (IN-6), only Global's runs use. They are the
// agency's former secrets that Global now uses, so the agency sees them, and
// their Usage, read only. This is the one exception to the reading rule
// (Global's rows are everywhere-readers'), and heldByGlobalFor is its one
// statement.
//
//	GET /api/v1/agencies/{agencyId}/held-by-global  the agency's people; 404 otherwise

// heldByGlobalItem is one such row.
type heldByGlobalItem struct {
	Kind  string `json:"kind"` // secret | key
	ID    string `json:"id"`
	Name  string `json:"name"`
	Scope string `json:"scope,omitempty"`
}

func (s *Server) handleHeldByGlobal(w http.ResponseWriter, r *http.Request) {
	agency := r.PathValue("agencyId")
	var known int
	if err := s.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM agencies WHERE id = ?`, agency).Scan(&known); err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	if known == 0 || !readsAgency(r, agency) {
		httpx.Fail(w, http.StatusNotFound, "not_found", "agency not found")
		return
	}
	out := []heldByGlobalItem{}
	if agency == agencyid.Global { // Global's own rows are on its lists already
		httpx.JSON(w, http.StatusOK, map[string]any{"items": out})
		return
	}
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT 'secret', t.id, t.key, COALESCE(t.scope, '') FROM secrets t
		 WHERE t.owner_agency = ? AND EXISTS (SELECT 1 FROM secret_agencies m WHERE m.secret_id = t.id AND m.agency_id = ?)
		UNION ALL
		SELECT 'key', t.id, t.label, '' FROM ssh_credentials t
		 WHERE t.owner_agency = ? AND EXISTS (SELECT 1 FROM ssh_credential_agencies m WHERE m.credential_id = t.id AND m.agency_id = ?)
		ORDER BY 1, 3`, agencyid.Global, agency, agencyid.Global, agency)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	defer rows.Close()
	id0, _ := auth.IdentityFrom(r.Context())
	for rows.Next() {
		var it heldByGlobalItem
		if err := rows.Scan(&it.Kind, &it.ID, &it.Name, &it.Scope); err != nil {
			httpx.Fail500(w, s.log, "db_error", err)
			return
		}
		// The secrets list's own scope rule (2.5.1 review).
		if it.Kind == "secret" && !auth.ScopeReadableIn(id0, agency, it.Scope) {
			continue
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": out})
}

// heldByGlobalFor reports whether the caller may read row id (of the secrets
// or ssh_credentials table, with its membership table and column) as one of
// its agencies' former secrets that Global now holds: the row is Global's and
// lists an agency (not Global) whose rows the caller reads. Unreadable
// membership is a no.
//
// A secret has a scope, and a grant limited to some of the agency's scopes
// reads the held row only when it reads that scope, as it would the agency's
// own secret there (2.5.1 review).
func (s *Server) heldByGlobalFor(r *http.Request, table, memberTable, memberCol, id string) bool {
	id0, ok := auth.IdentityFrom(r.Context())
	if !ok {
		return false
	}
	scopeExpr := "''"
	if table == "secrets" {
		scopeExpr = "COALESCE(t.scope, '')"
	}
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT m.agency_id, `+scopeExpr+` FROM `+memberTable+` m JOIN `+table+` t ON t.id = m.`+memberCol+`
		 WHERE m.`+memberCol+` = ? AND t.owner_agency = ? AND m.agency_id <> ?`, id, agencyid.Global, agencyid.Global) //nolint:gosec // fixed identifiers
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var a, scope string
		if rows.Scan(&a, &scope) == nil && readsAgency(r, a) && auth.ScopeReadableIn(id0, a, scope) {
			return true
		}
	}
	return false
}

// secretsReportRow is one secret or key.
type secretsReportRow struct {
	Kind             string `json:"kind"`
	ID               string `json:"id"`
	Name             string `json:"name"`
	Scope            string `json:"scope"`
	Source           string `json:"source"`
	VaultPath        string `json:"vaultPath,omitempty"`
	LastRotated      string `json:"lastRotated,omitempty"`
	LastUsed         string `json:"lastUsed,omitempty"`
	Uses             int    `json:"uses"`
	References       int    `json:"references"`
	Reveals          int    `json:"reveals"`
	OutsideChanges   int    `json:"outsideChanges"`
	LastOutsideActor string `json:"lastOutsideActor,omitempty"`
	RevealLocked     bool   `json:"revealLocked"` // 2.5.1 SG-19
}

func (s *Server) handleSecretsReport(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	agency := r.PathValue("agencyId")
	var known int
	if err := s.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM agencies WHERE id = ?`, agency).Scan(&known); err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	if known == 0 || !readsAgency(r, agency) {
		httpx.Fail(w, http.StatusNotFound, "not_found", "agency not found")
		return
	}
	days := 90
	if v := r.URL.Query().Get("days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 3650 {
			httpx.Fail(w, http.StatusBadRequest, "bad_days", "days is a number of days from 1 to 3650")
			return
		}
		days = n
	}
	format := r.URL.Query().Get("format")
	if format == "" {
		format = "json"
	}
	if format != "json" && format != "csv" {
		httpx.Fail(w, http.StatusBadRequest, "bad_format", "format is csv or json")
		return
	}
	rows, err := s.secretsReport(r.Context(), id, agency, days)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	if format == "json" {
		httpx.JSON(w, http.StatusOK, map[string]any{"agency": agency, "days": days, "items": rows})
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="secrets-report-%s-%s.csv"`, agency, time.Now().UTC().Format("20060102")))
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"kind", "id", "name", "scope", "source", "vault_path", "last_rotated", "last_used",
		fmt.Sprintf("uses_%dd", days), "references", fmt.Sprintf("reveals_%dd", days), fmt.Sprintf("outside_changes_%dd", days), "last_outside_actor", "reveal_locked"})
	for _, row := range rows {
		_ = cw.Write([]string{row.Kind, row.ID, csvSafe(row.Name), csvSafe(row.Scope), row.Source, csvSafe(row.VaultPath), row.LastRotated, row.LastUsed,
			strconv.Itoa(row.Uses), strconv.Itoa(row.References), strconv.Itoa(row.Reveals), strconv.Itoa(row.OutsideChanges), csvSafe(row.LastOutsideActor),
			strconv.FormatBool(row.RevealLocked)})
	}
	cw.Flush()
}

// csvSafe keeps a spreadsheet from reading a cell as a formula (a key name
// is operator text).
func csvSafe(v string) string {
	if v != "" && (v[0] == '=' || v[0] == '+' || v[0] == '-' || v[0] == '@') {
		return "'" + v
	}
	return v
}

func (s *Server) secretsReport(ctx context.Context, id auth.Identity, agency string, days int) ([]secretsReportRow, error) {
	since := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339)
	type item struct {
		kind  runref.Kind
		table string
		row   secretsReportRow
	}
	var items []item
	q, err := s.db.QueryContext(ctx, `
		SELECT 'secret', id, key, COALESCE(scope, ''), COALESCE(NULLIF(source, ''), 'stored'), COALESCE(vault_ref, ''), COALESCE(last_modified_at, created_at), reveal_locked
		  FROM secrets WHERE COALESCE(NULLIF(owner_agency, ''), 'global') = ?
		UNION ALL
		SELECT 'key', id, label, '', COALESCE(NULLIF(source, ''), 'stored'), COALESCE(vault_ref, ''), COALESCE(last_modified_at, created_at), 0
		  FROM ssh_credentials WHERE COALESCE(NULLIF(owner_agency, ''), 'global') = ?
		ORDER BY 1, 3`, agency, agency)
	if err != nil {
		return nil, err
	}
	for q.Next() {
		var it item
		var kind string
		if err := q.Scan(&kind, &it.row.ID, &it.row.Name, &it.row.Scope, &it.row.Source, &it.row.VaultPath, &it.row.LastRotated, &it.row.RevealLocked); err != nil {
			q.Close()
			return nil, err
		}
		it.row.Kind = kind
		if kind == "secret" {
			it.kind, it.table = runref.KindSecret, "secrets"
		} else {
			it.kind, it.table = runref.KindKey, "ssh_credentials"
		}
		items = append(items, it)
	}
	q.Close()
	if err := q.Err(); err != nil {
		return nil, err
	}
	out := []secretsReportRow{}
	for _, it := range items {
		// The secrets list's own scope rule: a scope the caller cannot read
		// hides its secrets here too.
		if it.kind == runref.KindSecret && !auth.ScopeReadableIn(id, agency, it.row.Scope) {
			continue
		}
		row := it.row
		category := "Secrets"
		if it.kind == runref.KindKey {
			category = "SSH Keys"
		}
		var lastUsed, lastOutside *string
		if err := s.db.QueryRowContext(ctx, `
			SELECT MAX(at), COALESCE(SUM(at >= ?), 0) FROM secret_uses WHERE kind = ? AND row_id = ? AND owner_agency = ?`,
			since, string(it.kind), row.ID, agency).Scan(&lastUsed, &row.Uses); err != nil {
			return nil, err
		}
		if lastUsed != nil {
			row.LastUsed = *lastUsed
		}
		idMatch := "id=" + row.ID
		if err := s.db.QueryRowContext(ctx, `
			SELECT COALESCE(SUM(action = 'revealed'), 0),
			       COALESCE(SUM(actor_relation IN ('outside', 'global_admin')), 0),
			       (SELECT actor FROM change_log c2
			         WHERE c2.category = ? AND c2.at >= ? AND COALESCE(c2.agency_id, 'global') = ?
			           AND c2.actor_relation IN ('outside', 'global_admin')
			           AND (c2.details = ? OR c2.details LIKE ?)
			         ORDER BY c2.at DESC LIMIT 1)
			  FROM change_log
			 WHERE category = ? AND at >= ? AND COALESCE(agency_id, 'global') = ? AND action <> 'injected'
			   AND (details = ? OR details LIKE ?)`,
			category, since, agency, idMatch, idMatch+" %",
			category, since, agency, idMatch, idMatch+" %").Scan(&row.Reveals, &row.OutsideChanges, &lastOutside); err != nil {
			return nil, err
		}
		if lastOutside != nil {
			row.LastOutsideActor = *lastOutside
		}
		if it.kind == runref.KindSecret {
			var rotated *string
			if err := s.db.QueryRowContext(ctx, `
				SELECT MAX(at) FROM change_log WHERE category = 'Secrets' AND action IN ('created', 'rotated', 'migrated-to-vault')
				   AND COALESCE(agency_id, 'global') = ? AND (details = ? OR details LIKE ?)`,
				agency, idMatch, idMatch+" %").Scan(&rotated); err == nil && rotated != nil {
				row.LastRotated = *rotated
			}
		}
		refs, err := s.rowReferences(ctx, it.kind, row.ID, row.Name, agency)
		if err != nil {
			return nil, err
		}
		row.References = len(refs)
		out = append(out, row)
	}
	return out, nil
}

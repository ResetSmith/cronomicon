package api

import (
	"context"
	"encoding/csv"
	"fmt"
	"net/http"
	"strconv"
	"time"

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
		fmt.Sprintf("uses_%dd", days), "references", fmt.Sprintf("reveals_%dd", days), fmt.Sprintf("outside_changes_%dd", days), "last_outside_actor"})
	for _, row := range rows {
		_ = cw.Write([]string{row.Kind, row.ID, csvSafe(row.Name), csvSafe(row.Scope), row.Source, csvSafe(row.VaultPath), row.LastRotated, row.LastUsed,
			strconv.Itoa(row.Uses), strconv.Itoa(row.References), strconv.Itoa(row.Reveals), strconv.Itoa(row.OutsideChanges), csvSafe(row.LastOutsideActor)})
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
		SELECT 'secret', id, key, COALESCE(scope, ''), COALESCE(NULLIF(source, ''), 'stored'), COALESCE(vault_ref, ''), COALESCE(last_modified_at, created_at)
		  FROM secrets WHERE COALESCE(NULLIF(owner_agency, ''), 'global') = ?
		UNION ALL
		SELECT 'key', id, label, '', COALESCE(NULLIF(source, ''), 'stored'), COALESCE(vault_ref, ''), COALESCE(last_modified_at, created_at)
		  FROM ssh_credentials WHERE COALESCE(NULLIF(owner_agency, ''), 'global') = ?
		ORDER BY 1, 3`, agency, agency)
	if err != nil {
		return nil, err
	}
	for q.Next() {
		var it item
		var kind string
		if err := q.Scan(&kind, &it.row.ID, &it.row.Name, &it.row.Scope, &it.row.Source, &it.row.VaultPath, &it.row.LastRotated); err != nil {
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
			SELECT MAX(at), COALESCE(SUM(at >= ?), 0) FROM secret_uses WHERE kind = ? AND row_id = ?`,
			since, string(it.kind), row.ID).Scan(&lastUsed, &row.Uses); err != nil {
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

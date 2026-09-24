package settings

import (
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// AuditRow is a unified row for export purposes.
type AuditRow struct {
	Source   string `json:"source"`
	At       string `json:"at"`
	Actor    string `json:"actor"`
	Category string `json:"category,omitempty"`
	Action   string `json:"action,omitempty"`
	Target   string `json:"target,omitempty"`
	Details  string `json:"details,omitempty"`
	Status   string `json:"status,omitempty"`
	TraceID  string `json:"traceId,omitempty"`
}

// ExportAudit streams the requested audit events to w as CSV or JSON. Rows are
// read and written one at a time so a full-table export (no from/to bounds)
// stays flat on memory (CC.10) instead of buffering every change_log / activity
// / runs / schedule_pushes row before marshaling. Sources are emitted in a fixed
// order, each ordered by its own timestamp column.
//
// Because the body is streamed, the caller must have already written the
// response status + headers: a mid-stream failure cannot roll the status back,
// so the returned error is for logging only (the partial body signals the
// truncation to the operator).
func ExportAudit(ctx context.Context, database *sql.DB, w io.Writer, from, to string, eventTypes []string, format string) error {
	typeSet := map[string]bool{}
	for _, t := range eventTypes {
		typeSet[t] = true
	}
	all := len(typeSet) == 0

	sources := []struct {
		include bool
		stream  func(context.Context, *sql.DB, string, string, func(AuditRow) error) error
	}{
		{all || typeSet["configChanges"], streamChangeLog},
		{all || typeSet["activity"], streamActivity},
		{all || typeSet["executions"], streamRuns},
		{all || typeSet["schedulePushes"], streamSchedulePushes},
		// LU-9: authEvents used to be a pure alias for configChanges — selecting it
		// alone returned every config change and not one auth event, because no
		// auth event was recorded anywhere. It now streams the table that actually
		// holds them.
		{all || typeSet["authEvents"], streamAuthEvents},
	}

	if format == "csv" {
		cw := csv.NewWriter(w)
		if err := cw.Write([]string{"source", "at", "actor", "category", "action", "target", "details", "status", "traceId"}); err != nil {
			return err
		}
		emit := func(r AuditRow) error {
			return cw.Write([]string{r.Source, r.At, r.Actor, r.Category, r.Action, r.Target, r.Details, r.Status, r.TraceID})
		}
		for _, src := range sources {
			if !src.include {
				continue
			}
			if err := src.stream(ctx, database, from, to, emit); err != nil {
				cw.Flush()
				return err
			}
		}
		cw.Flush()
		return cw.Error()
	}

	// JSON array, marshaled one row at a time (empty ⇒ "[]"). For a non-empty
	// export this is byte-identical to json.Marshal of the equivalent slice.
	if _, err := io.WriteString(w, "["); err != nil {
		return err
	}
	first := true
	emit := func(r AuditRow) error {
		b, err := json.Marshal(r)
		if err != nil {
			return err
		}
		if !first {
			if _, err := io.WriteString(w, ","); err != nil {
				return err
			}
		}
		first = false
		_, err = w.Write(b)
		return err
	}
	for _, src := range sources {
		if !src.include {
			continue
		}
		if err := src.stream(ctx, database, from, to, emit); err != nil {
			return err
		}
	}
	_, err := io.WriteString(w, "]")
	return err
}

// streamTable runs a date-filtered, date-ordered query over one audit table and
// calls emit for each row mapped to an AuditRow — the shared core that replaced
// the four copy-pasted query/scan functions.
func streamTable(ctx context.Context, database *sql.DB, table, dateCol, cols string,
	scan func(*sql.Rows) (AuditRow, error), from, to string, emit func(AuditRow) error) error {
	q := "SELECT " + cols + " FROM " + table
	args, where := buildDateRange(from, to, dateCol)
	if where != "" {
		q += " WHERE " + where
	}
	q += " ORDER BY " + dateCol
	rows, err := database.QueryContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("query %s: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		r, err := scan(rows)
		if err != nil {
			return err
		}
		if err := emit(r); err != nil {
			return err
		}
	}
	return rows.Err()
}

func streamChangeLog(ctx context.Context, database *sql.DB, from, to string, emit func(AuditRow) error) error {
	return streamTable(ctx, database, "change_log", "at", "at, actor, category, action, target, details",
		func(rows *sql.Rows) (AuditRow, error) {
			var r AuditRow
			var target, details, category sql.NullString
			if err := rows.Scan(&r.At, &r.Actor, &category, &r.Action, &target, &details); err != nil {
				return r, err
			}
			r.Source = "change_log"
			r.Category = category.String
			r.Target = target.String
			r.Details = details.String
			return r, nil
		}, from, to, emit)
}

func streamActivity(ctx context.Context, database *sql.DB, from, to string, emit func(AuditRow) error) error {
	return streamTable(ctx, database, "activity", "at", "at, actor, kind, summary, details, trace_id",
		func(rows *sql.Rows) (AuditRow, error) {
			var r AuditRow
			var summary, details, traceID sql.NullString
			if err := rows.Scan(&r.At, &r.Actor, &r.Category, &summary, &details, &traceID); err != nil {
				return r, err
			}
			r.Source = "activity"
			r.Action = summary.String
			r.Details = details.String
			r.TraceID = traceID.String
			return r, nil
		}, from, to, emit)
}

func streamRuns(ctx context.Context, database *sql.DB, from, to string, emit func(AuditRow) error) error {
	return streamTable(ctx, database, "runs", "created_at", "created_at, triggered_by, job_name, status, id",
		func(rows *sql.Rows) (AuditRow, error) {
			var r AuditRow
			if err := rows.Scan(&r.At, &r.Actor, &r.Target, &r.Status, &r.TraceID); err != nil {
				return r, err
			}
			r.Source = "runs"
			r.Category = "executions"
			r.Action = "run"
			return r, nil
		}, from, to, emit)
}

func streamSchedulePushes(ctx context.Context, database *sql.DB, from, to string, emit func(AuditRow) error) error {
	return streamTable(ctx, database, "schedule_pushes", "at", "at, actor, schedule_file, status, details",
		func(rows *sql.Rows) (AuditRow, error) {
			var r AuditRow
			var details sql.NullString
			if err := rows.Scan(&r.At, &r.Actor, &r.Target, &r.Status, &details); err != nil {
				return r, err
			}
			r.Source = "schedule_pushes"
			r.Category = "schedulePushes"
			r.Action = "push"
			r.Details = details.String
			return r, nil
		}, from, to, emit)
}

// streamAuthEvents exports the authentication/authorization trail (LU-9).
//
// The two address columns are folded into Details rather than given AuditRow
// columns of their own: AuditRow is a UNION shape across four dissimilar tables
// and its CSV header is a stable contract, so widening it for one source would
// add two permanently-empty columns to every other row. The addresses are the
// detail of an auth event, which is exactly what Details is for.
func streamAuthEvents(ctx context.Context, database *sql.DB, from, to string, emit func(AuditRow) error) error {
	return streamTable(ctx, database, "auth_events", "at",
		"at, actor, kind, outcome, reason, target, remote_addr, client_ip, user_agent, details",
		func(rows *sql.Rows) (AuditRow, error) {
			var r AuditRow
			var actor, reason, target, remoteAddr, clientIP, userAgent, details sql.NullString
			if err := rows.Scan(&r.At, &actor, &r.Action, &r.Status, &reason, &target,
				&remoteAddr, &clientIP, &userAgent, &details); err != nil {
				return r, err
			}
			r.Source = "auth_events"
			r.Category = "authEvents"
			// A NULL actor is legal and meaningful here — a failed login before any
			// identity was established. Rendering it as an explicit marker beats an
			// empty cell an auditor has to guess at.
			r.Actor = actor.String
			if r.Actor == "" {
				r.Actor = "(unauthenticated)"
			}
			r.Target = target.String
			r.Details = joinDetails(
				kv("reason", reason.String),
				kv("remoteAddr", remoteAddr.String),
				kv("clientIp", clientIP.String),
				kv("userAgent", userAgent.String),
				details.String,
			)
			return r, nil
		}, from, to, emit)
}

func kv(k, v string) string {
	if v == "" {
		return ""
	}
	return k + "=" + v
}

func joinDetails(parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, " ")
}

func buildDateRange(from, to, col string) ([]any, string) {
	var args []any
	var clauses []string
	if from != "" {
		clauses = append(clauses, col+" >= ?")
		args = append(args, from)
	}
	if to != "" {
		clauses = append(clauses, col+" < ?")
		args = append(args, to)
	}
	return args, strings.Join(clauses, " AND ")
}

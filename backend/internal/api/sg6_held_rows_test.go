package api_test

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"
)

// 2.5.1 SG6.1 — what the pre-release review found about rows Global holds
// (the review's A1, A2, A3, A10; SG-42; question 140).
//
// A held row is a Global-owned secret or key that lists member agencies: the
// rows several agencies shared before 2.3.0. Its member agencies read it, and
// its Usage (SG-16), read only. The reveal, lock and write gates asked for
// membership, so any member could reveal it, make it unrevealable for good, or
// delete a key Global's runs use.

// createSecret creates a stored, unscoped secret as a global administrator in
// one agency and answers its id.
func createSecret(t *testing.T, h http.Handler, key, value, agency string) string {
	t.Helper()
	rec := gateReq(t, h, http.MethodPost, "/api/v1/env-secrets", gRoot,
		`{"key":"`+key+`","source":"stored","value":"`+value+`","agencies":["`+agency+`"]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("creating %s in %s = %d (%s)", key, agency, rec.Code, rec.Body)
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.ID == "" {
		t.Fatalf("creating %s: no id in %s", key, rec.Body)
	}
	return out.ID
}

func TestSG6_AHeldRowIsReadOnlyToItsMembers(t *testing.T) {
	h, pool := gateServer(t)
	exec := mustExec(t, pool)
	const at = "2026-01-01T00:00:00Z"

	sid := createSecret(t, h, "HELD_SECRET", "held-secret-value", "global")
	exec(`INSERT INTO secret_agencies (secret_id, agency_id) VALUES (?, 'ag:FIN'), (?, 'ag:TAX')`, sid, sid)
	exec(`INSERT INTO ssh_credentials (id,label,source,created_by,last_modified_by,created_at,last_modified_at,owner_agency)
	      VALUES ('pk-held','HELD_KEY','stored','x','x',?,?,'global')`, at, at)
	exec(`DELETE FROM ssh_credential_agencies WHERE credential_id = 'pk-held'`)
	exec(`INSERT INTO ssh_credential_agencies (credential_id, agency_id) VALUES ('pk-held','ag:FIN'), ('pk-held','ag:TAX')`)
	// A record of TAX's that connects with the held key: a forced delete would
	// clear it.
	exec(`INSERT INTO ssh_hosts (id, hostname, port, source, auth_credential_id, created_by, created_at, last_modified_by, last_modified_at, owner_agency)
	      VALUES ('h-tax','tax-db.internal',22,'cronomicon','pk-held','seed',?,'seed',?,'ag:TAX')`, at, at)

	state := func() string {
		var s string
		if err := pool.QueryRow(`SELECT
		    (SELECT COUNT(*) || '|' || COALESCE(MAX(reveal_locked), '') || '|' || COALESCE(MAX(last_modified_by), '') || '|' ||
		            COALESCE(MAX(tags), '') || '|' || COALESCE(MAX(source), '') FROM secrets WHERE id = ?) || '#' ||
		    (SELECT COUNT(*) || '|' || COALESCE(MAX(description), '') || '|' || COALESCE(MAX(tags), '') FROM ssh_credentials WHERE id = 'pk-held') || '#' ||
		    (SELECT COALESCE(auth_credential_id, '') FROM ssh_hosts WHERE id = 'h-tax')`, sid).Scan(&s); err != nil {
			t.Fatalf("state: %v", err)
		}
		return s
	}

	writes := []struct{ name, method, path, body string }{
		{"reveal", http.MethodPost, "/api/v1/env-secrets/" + sid + "/reveal", ``},
		{"lock-reveal", http.MethodPost, "/api/v1/env-secrets/" + sid + "/lock-reveal", ``},
		{"edit", http.MethodPut, "/api/v1/env-secrets/" + sid, `{"key":"HELD_SECRET","source":"stored","value":"by-a-member"}`},
		{"tags", http.MethodPut, "/api/v1/env-secret-tags/" + sid, `{"tags":["member"]}`},
		{"migrate", http.MethodPost, "/api/v1/env-secrets/" + sid + "/migrate-to-vault", `{"vaultPath":"secret/data/x#f"}`},
		{"delete", http.MethodDelete, "/api/v1/env-secrets/" + sid, ``},
		{"key edit", http.MethodPut, "/api/v1/ssh/credentials/pk-held", `{"label":"HELD_KEY","source":"stored","description":"by-a-member"}`},
		{"key tags", http.MethodPut, "/api/v1/ssh-credential-tags/pk-held", `{"tags":["member"]}`},
		{"key delete", http.MethodDelete, "/api/v1/ssh/credentials/pk-held?force=true", ``},
	}
	for _, who := range []string{gFinAdmin, gTaxAdmin} {
		for _, c := range writes {
			before := state()
			rec := gateReq(t, h, c.method, c.path, who, c.body)
			// The member reads the row (SG-16), so the refusal is a 403, not a 404.
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s's %s of a row Global holds = %d, want 403 (%s)", who, c.name, rec.Code, rec.Body)
			}
			if strings.Contains(rec.Body.String(), "held-secret-value") {
				t.Errorf("%s's %s of a row Global holds answered with its value", who, c.name)
			}
			if after := state(); after != before {
				t.Errorf("%s's refused %s changed the row:\n before %s\n after  %s", who, c.name, before, after)
			}
		}
	}

	// A member still reads it, and its Usage.
	for _, p := range []string{"/api/v1/env-secrets/" + sid + "/usage", "/api/v1/ssh/credentials/pk-held/usage", "/api/v1/agencies/ag:FIN/held-by-global"} {
		if rec := gateReq(t, h, http.MethodGet, p, gFinAdmin, ``); rec.Code != http.StatusOK {
			t.Errorf("FIN's administrator reading %s = %d, want 200 (%s)", p, rec.Code, rec.Body)
		}
	}

	// It is Global's: a global administrator reveals it and may lock it.
	if rec := gateReq(t, h, http.MethodPost, "/api/v1/env-secrets/"+sid+"/reveal", gRoot, ``); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), "held-secret-value") {
		t.Errorf("a global administrator's reveal of Global's secret = %d (%s)", rec.Code, rec.Body)
	}
	if rec := gateReq(t, h, http.MethodPost, "/api/v1/env-secrets/"+sid+"/lock-reveal", gRoot, ``); rec.Code != http.StatusOK {
		t.Errorf("a global administrator's lock of Global's secret = %d (%s)", rec.Code, rec.Body)
	}
	if rec := gateReq(t, h, http.MethodPut, "/api/v1/ssh/credentials/pk-held", gRoot, `{"label":"HELD_KEY","source":"stored","description":"by-global"}`); rec.Code != http.StatusOK {
		t.Errorf("a global administrator's edit of Global's key = %d (%s)", rec.Code, rec.Body)
	}
}

// secretKeyWriteProbes: every write route that names one secret or SSH key (as
// writeRouteGates names it), and the request on the object of an agency key.
type secretKeyWrite struct {
	method string
	path   func(obj map[string]string, key string) string
	body   string
}

var secretKeyWriteProbes = map[string]secretKeyWrite{
	"PUT /api/v1/env-secrets/{secretId}": {http.MethodPut, func(o map[string]string, k string) string { return "/api/v1/env-secrets/" + o["s:"+k] },
		`{"key":"PROBE","source":"stored","scope":"fin-hosts","value":"rewritten"}`},
	"DELETE /api/v1/env-secrets/{secretId}":                {http.MethodDelete, func(o map[string]string, k string) string { return "/api/v1/env-secrets/" + o["s:"+k] }, ``},
	"POST /api/v1/env-secrets/{secretId}/reveal":           {http.MethodPost, func(o map[string]string, k string) string { return "/api/v1/env-secrets/" + o["s:"+k] + "/reveal" }, ``},
	"POST /api/v1/env-secrets/{secretId}/lock-reveal":      {http.MethodPost, func(o map[string]string, k string) string { return "/api/v1/env-secrets/" + o["s:"+k] + "/lock-reveal" }, ``},
	"POST /api/v1/env-secrets/{secretId}/migrate-to-vault": {http.MethodPost, func(o map[string]string, k string) string { return "/api/v1/env-secrets/" + o["s:"+k] + "/migrate-to-vault" }, `{"vaultPath":"secret/data/x#f"}`},
	"PUT /api/v1/env-secret-tags/{secretId}":               {http.MethodPut, func(o map[string]string, k string) string { return "/api/v1/env-secret-tags/" + o["s:"+k] }, `{"tags":["probe"]}`},
	"PUT /api/v1/ssh/credentials/{credentialId}": {http.MethodPut, func(o map[string]string, k string) string { return "/api/v1/ssh/credentials/pk-" + k },
		`{"label":"PROBE_KEY","source":"stored","description":"rewritten"}`},
	"DELETE /api/v1/ssh/credentials/{credentialId}":  {http.MethodDelete, func(o map[string]string, k string) string { return "/api/v1/ssh/credentials/pk-" + k + "?force=true" }, ``},
	"PUT /api/v1/ssh-credential-tags/{credentialId}": {http.MethodPut, func(o map[string]string, k string) string { return "/api/v1/ssh-credential-tags/pk-" + k }, `{"tags":["probe"]}`},
}

func isSecretOrKeyObjectRoute(route string) bool {
	path := route[strings.Index(route, " ")+1:]
	return strings.Contains(path, "{") && (strings.HasPrefix(path, "/api/v1/env-secret") ||
		strings.HasPrefix(path, "/api/v1/ssh/credentials/") || strings.HasPrefix(path, "/api/v1/ssh-credential-tags/"))
}

// Question 140: C11's object-write probe (TestC11_EveryObjectWriteIsProbed)
// covers the visible, open and verb classes; the secret and key writes are
// clsObject and were never probed. Every one that names an object is listed
// here.
func TestSG6_EverySecretAndKeyWriteIsProbed(t *testing.T) {
	for route := range writeRouteGates {
		if isSecretOrKeyObjectRoute(route) {
			if _, ok := secretKeyWriteProbes[route]; !ok {
				t.Errorf("%s names a secret or key and is not in secretKeyWriteProbes: add it, so another agency's row is answered 404", route)
			}
		}
	}
	for route := range secretKeyWriteProbes {
		if _, ok := writeRouteGates[route]; !ok {
			t.Errorf("secretKeyWriteProbes names %s, which writeRouteGates does not", route)
		}
	}
}

// Question 140 (C11's decision 109 for these routes): FIN's administrator is
// answered 404 on TAX's and Global's unscoped secrets and keys, before any
// verb is judged, so no 403 says whose a row is; nothing changes. FIN's
// operator and viewer, who lack the verb everywhere, are refused by the route's
// own gate, which names no row. FIN's own row is the control.
func TestSG6_NoSecretOrKeyWriteAnswersAnotherAgency(t *testing.T) {
	h, pool := gateServer(t)
	exec := mustExec(t, pool)
	const at = "2026-01-01T00:00:00Z"
	// FIN's, made by FIN's administrator in its scope; TAX's with no scope (the
	// shape whose refusal named its owner: an empty scope passes the scope
	// check for everyone); Global's, made by a global administrator.
	rec := gateReq(t, h, http.MethodPost, "/api/v1/env-secrets", gFinAdmin, `{"key":"FIN_PROBE","source":"stored","scope":"fin-hosts","value":"fin-probe-value"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("FIN's administrator creating FIN's secret = %d (%s)", rec.Code, rec.Body)
	}
	var fin struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &fin)
	exec(`INSERT INTO secrets (id, key, scope, source, created_by, created_at, last_modified_by, last_modified_at, owner_agency)
	      VALUES ('s-tax0', 'TAX_PROBE', NULL, 'stored', 'x', ?, 'x', ?, 'ag:TAX')`, at, at)
	exec(`DELETE FROM secret_agencies WHERE secret_id = 's-tax0'`)
	exec(`INSERT INTO secret_agencies (secret_id, agency_id) VALUES ('s-tax0', 'ag:TAX')`)
	obj := map[string]string{
		"s:fin":  fin.ID,
		"s:tax":  "s-tax0",
		"s:glob": createSecret(t, h, "GLOB_PROBE", "glob-probe-value", "global"),
	}
	for _, k := range [][2]string{{"fin", "ag:FIN"}, {"tax", "ag:TAX"}, {"glob", "global"}} {
		exec(`INSERT INTO ssh_credentials (id,label,source,description,created_by,last_modified_by,created_at,last_modified_at,owner_agency)
		      VALUES (?,?,'stored','as-made','x','x',?,?,?)`, "pk-"+k[0], strings.ToUpper(k[0])+"_KEY", at, at, k[1])
		exec(`DELETE FROM ssh_credential_agencies WHERE credential_id = ?`, "pk-"+k[0])
		exec(`INSERT INTO ssh_credential_agencies (credential_id, agency_id) VALUES (?, ?)`, "pk-"+k[0], k[1])
	}
	state := func() string {
		var s string
		if err := pool.QueryRow(`SELECT
		    (SELECT COALESCE(group_concat(id || ':' || key || ':' || reveal_locked || ':' || COALESCE(tags, '') || ':' || source, ','), '') FROM secrets) || '#' ||
		    (SELECT COALESCE(group_concat(id || ':' || COALESCE(description, '') || ':' || COALESCE(tags, ''), ','), '') FROM ssh_credentials)`).Scan(&s); err != nil {
			t.Fatalf("state: %v", err)
		}
		return s
	}
	leaks := []string{"tax-probe-value", "glob-probe-value", "belongs to Global", "agency that owns", "TAX"}
	routes := make([]string, 0, len(secretKeyWriteProbes))
	for route := range secretKeyWriteProbes {
		routes = append(routes, route)
	}
	sort.Strings(routes)
	for _, route := range routes {
		p := secretKeyWriteProbes[route]
		for _, k := range []string{"tax", "glob"} {
			for _, who := range []string{gFinAdmin, gFinOperator, gFinViewer} {
				before := state()
				rec := gateReq(t, h, p.method, p.path(obj, k), who, p.body)
				if who == gFinAdmin && rec.Code != http.StatusNotFound {
					t.Errorf("%s by %s on %s's row = %d, want 404 (%s)", route, who, k, rec.Code, rec.Body)
				}
				if rec.Code < 400 {
					t.Errorf("%s by %s on %s's row = %d, want it refused (%s)", route, who, k, rec.Code, rec.Body)
				}
				for _, l := range leaks {
					if strings.Contains(rec.Body.String(), l) {
						t.Errorf("%s by %s on %s's row: the refusal says %q (%s)", route, who, k, l, rec.Body)
					}
				}
				if after := state(); after != before {
					t.Errorf("%s by %s on %s's row changed something:\n before %s\n after  %s", route, who, k, before, after)
				}
			}
		}
	}

	// The control, deletes last: FIN's administrator on FIN's own row is not
	// told it is absent.
	sort.SliceStable(routes, func(i, j int) bool {
		return !strings.HasPrefix(routes[i], "DELETE") && strings.HasPrefix(routes[j], "DELETE")
	})
	for _, route := range routes {
		p := secretKeyWriteProbes[route]
		if rec := gateReq(t, h, p.method, p.path(obj, "fin"), gFinAdmin, p.body); rec.Code == http.StatusNotFound || rec.Code == http.StatusForbidden {
			t.Errorf("%s by FIN's administrator on FIN's own row = %d, want it to pass the gate (%s)", route, rec.Code, rec.Body)
		}
	}
}

// The review's A3: a held key's Usage named every host record and bastion
// that connects with it, another agency's included. They are counted, not
// named, for a caller who does not read their agency.
func TestSG6_AHeldKeysUsageNamesOnlyRecordsTheCallerReads(t *testing.T) {
	h, pool := gateServer(t)
	exec := mustExec(t, pool)
	const at = "2026-01-01T00:00:00Z"
	exec(`INSERT INTO ssh_credentials (id,label,source,created_by,last_modified_by,created_at,last_modified_at,owner_agency)
	      VALUES ('pk-held','HELD_KEY','stored','x','x',?,?,'global')`, at, at)
	exec(`DELETE FROM ssh_credential_agencies WHERE credential_id = 'pk-held'`)
	exec(`INSERT INTO ssh_credential_agencies (credential_id, agency_id) VALUES ('pk-held','ag:FIN'), ('pk-held','ag:TAX')`)
	exec(`INSERT INTO ssh_hosts (id, hostname, port, source, auth_credential_id, created_by, created_at, last_modified_by, last_modified_at, owner_agency) VALUES
	      ('h-fin','fin-db.internal',22,'cronomicon','pk-held','seed',?,'seed',?,'ag:FIN'),
	      ('h-tax','tax-payroll-db.internal',22,'cronomicon','pk-held','seed',?,'seed',?,'ag:TAX')`, at, at, at, at)
	exec(`INSERT INTO bastions (id, name, hostname, address, port, created_at, owner_agency, auth_credential_id)
	      VALUES ('pb-tax','tax-jump-name','tax-jump.internal','10.9.9.9',22,?,'ag:TAX','pk-held')`, at)

	usage := func(who string) (string, int) {
		t.Helper()
		rec := gateReq(t, h, http.MethodGet, "/api/v1/ssh/credentials/pk-held/usage", who, ``)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s reading the held key's Usage = %d (%s)", who, rec.Code, rec.Body)
		}
		var u struct {
			OtherAgencyRecords int `json:"otherAgencyRecords"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &u)
		return rec.Body.String(), u.OtherAgencyRecords
	}

	for _, who := range []string{gFinViewer, gFinAdmin} {
		body, others := usage(who)
		for _, l := range []string{"tax-payroll-db", "tax-jump", "h-tax", "pb-tax"} {
			if strings.Contains(body, l) {
				t.Errorf("%s's view of the held key's Usage names TAX's record %q: %s", who, l, body)
			}
		}
		if !strings.Contains(body, "fin-db.internal") {
			t.Errorf("%s's view of the held key's Usage leaves out FIN's own record: %s", who, body)
		}
		if others != 2 {
			t.Errorf("%s's view counts %d records of other agencies, want 2", who, others)
		}
	}
	body, others := usage(gRoot)
	if !strings.Contains(body, "tax-payroll-db.internal") || !strings.Contains(body, "fin-db.internal") || others != 0 {
		t.Errorf("a global administrator's view of the held key's Usage = %d hidden, %s", others, body)
	}
}

// The review's A10: a secret moved by PUT /agencies/{id}/members raised no
// secret-changed alert; the per-kind route does (SG-17). Removing FIN's share
// of a Global-held secret leaves it TAX's, owner and member: TAX's rule hears
// it. The alert's delivery is seen on the transport's stamp (the SMTP host is
// unreachable, so the send fails and is recorded).
func TestSG6_AMoveThroughMembersAlerts(t *testing.T) {
	h, pool := gateServer(t)
	exec := mustExec(t, pool)
	exec(`INSERT INTO notification_config (id, smtp_host, smtp_port, smtp_from, smtp_encryption, smtp_recipients, apprise_enabled, apprise_url, apprise_targets, last_modified_at)
	      VALUES (1, '127.0.0.1', 1, 'cronomicon@example.com', 'starttls', '[]', 0, '', '[]', 'now')
	      ON CONFLICT(id) DO UPDATE SET smtp_host = excluded.smtp_host, smtp_port = excluded.smtp_port,
	        smtp_from = excluded.smtp_from, smtp_encryption = excluded.smtp_encryption, last_email_at = NULL`)
	exec(`INSERT INTO alert_config (id, target_mode, job_name, trigger, channels, recipients, enabled, created_at, owner_agency)
	      VALUES ('tax-secrets', 'all', NULL, 'secret-changed', '["email"]', 'tax@example.com', 1, 'now', 'ag:TAX')`)
	sid := createSecret(t, h, "MOVING", "moving-value", "global")
	exec(`DELETE FROM secret_agencies WHERE secret_id = ?`, sid)
	exec(`INSERT INTO secret_agencies (secret_id, agency_id) VALUES (?, 'ag:FIN'), (?, 'ag:TAX')`, sid, sid)
	sent := func() string {
		var s string
		_ = pool.QueryRow(`SELECT COALESCE(last_email_at, '') FROM notification_config WHERE id = 1`).Scan(&s)
		return s
	}
	if s := sent(); s != "" {
		t.Fatalf("an email was sent before the move: %s", s)
	}

	rec := gateReq(t, h, http.MethodPut, "/api/v1/agencies/ag:FIN/members", gRoot,
		asRepair(`{"members":[{"kind":"scope","id":"sc:fin"}]}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("a global administrator's repair taking FIN's share = %d (%s)", rec.Code, rec.Body)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM secrets WHERE id = ? AND owner_agency = 'ag:TAX'`, sid); n != 1 {
		t.Fatal("the secret did not become TAX's: nothing moved, so nothing to alert")
	}
	deadline := time.Now().Add(5 * time.Second)
	for sent() == "" && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if sent() == "" {
		t.Error("the secret moved to TAX through PUT /agencies/{id}/members and TAX's secret-changed rule heard nothing")
	}
}

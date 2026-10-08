package settings

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/ResetSmith/cronomicon/internal/hostkeys"
	"github.com/ResetSmith/cronomicon/internal/notices"
)

// How a runner reaches a bastion decides which machine's key it must trust. An
// agent dials the name a host record gives its bastion, and resolves it itself;
// the server hops through the bastion RECORD that name stands for, at that
// record's address and port. So the local runner's plan for a scope names the
// record's address — for the scan and for the coverage — and a bastion the
// server cannot resolve makes its hosts untrusted, which they are.
//
// And the standing notice: while the local runner is on, a scope whose runs it
// takes and whose hosts it has no approved key for is reported, with the hosts,
// until the keys are approved (or the scope goes to an agent).
func TestTheLocalRunnersPlanAndItsMissingKeysNotice(t *testing.T) {
	database := newTestDB(t)
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := database.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	localID, _, err := EnsureLocalRunner(ctx, database, true, 4)
	if err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO agencies (id, name, created_at) VALUES ('ag-fin', 'Finance', 't')`)
	exec(`INSERT INTO scopes (id, name, source, created_at) VALUES ('sc-fin', 'fin-prod', 'git', 't')`)
	exec(`DELETE FROM scope_agencies WHERE scope_id = 'sc-fin'`)
	exec(`INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('sc-fin', 'ag-fin')`)
	exec(`INSERT INTO runner_agencies (runner_id, agency_id) VALUES (?, 'ag-fin')`, localID)
	for _, h := range []string{"direct", "fronted", "lost", "norecord"} {
		exec(`INSERT INTO scope_hosts (scope_id, host) VALUES ('sc-fin', ?)`, h)
	}
	host := func(id, name, addr string, port int, via any) {
		exec(`INSERT INTO ssh_hosts (id, hostname, address, port, via, scope_id, source, created_by, created_at, last_modified_by, last_modified_at)
		      VALUES (?, ?, ?, ?, ?, 'sc-fin', 'git', 't', 't', 't', 't')`, id, name, addr, port, via)
	}
	host("h1", "direct", "10.1.0.5", 22, nil)
	host("h2", "fronted", "10.2.0.5", 22, "dmz-jump")
	host("h3", "lost", "10.3.0.5", 22, "no-such-bastion")
	// The bastion's NAME is not an address anyone can dial; its record says where it is.
	exec(`INSERT INTO bastions (id, hostname, name, address, port, owner_agency, created_by, created_at, last_modified_by, last_modified_at)
	      VALUES ('b1', 'dmz-jump', 'dmz-jump', '198.51.100.7', 2222, 'ag-fin', 't', 't', 't', 't')`)

	byHost := func(plan []hostkeys.ScopeHost) map[string]hostkeys.ScopeHost {
		out := map[string]hostkeys.ScopeHost{}
		for _, h := range plan {
			out[h.Host] = h
		}
		return out
	}
	agentPlan, err := hostkeys.PlanScopeFor(ctx, database, "fin-prod", false)
	if err != nil {
		t.Fatal(err)
	}
	serverPlan, err := hostkeys.PlanScopeForRunner(ctx, database, "fin-prod", localID)
	if err != nil {
		t.Fatal(err)
	}
	a, s := byHost(agentPlan), byHost(serverPlan)
	if a["fronted"].ViaPattern != "dmz-jump" || a["fronted"].ViaTarget != "dmz-jump" {
		t.Errorf("an agent's plan for the bastion = %+v, want the name it resolves itself", a["fronted"])
	}
	if s["fronted"].ViaPattern != "[198.51.100.7]:2222" || s["fronted"].ViaTarget != "198.51.100.7:2222" {
		t.Errorf("the server's plan for the bastion = %+v, want the record's address and port", s["fronted"])
	}
	if s["lost"].ViaPattern != hostkeys.UnresolvedBastion || s["lost"].ViaTarget != "" {
		t.Errorf("a host behind a bastion with no record = %+v, want it unresolved and not scannable", s["lost"])
	}
	if s["direct"].Target != "10.1.0.5" || s["direct"].Pattern != "10.1.0.5" {
		t.Errorf("a directly reached host = %+v", s["direct"])
	}

	newKey := func() ssh.PublicKey {
		pub, _, _ := ed25519.GenerateKey(rand.Reader)
		k, _ := ssh.NewPublicKey(pub)
		return k
	}
	trust := func(pattern string) {
		k := newKey()
		exec(`INSERT INTO host_key_ledger (batch_id, runner_id, runner_name, host, key_type, fingerprint, known_hosts_line,
		                                   decision, source, actor, decided_at, delivered_at, confirmed_at)
		      VALUES ('t', ?, 'Local runner', ?, ?, ?, ?, 'approved', 'pasted', 't', 't', 't', 't')`,
			localID, pattern, k.Type(), ssh.FingerprintSHA256(k), pattern+" "+strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k))))
	}
	notices.SetLocalRunnerHostKeysCheck(hostkeys.LocalRunnerMissingKeys)
	t.Cleanup(func() { notices.SetLocalRunnerHostKeysCheck(nil) })
	open := func() (notices.Notice, bool) {
		t.Helper()
		if err := notices.RunChecks(ctx, database); err != nil {
			t.Fatalf("checks: %v", err)
		}
		all, err := notices.ListOpen(ctx, database)
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range all {
			if n.Kind == notices.KindLocalRunnerHostKeys && n.Subject == "sc-fin" {
				return n, true
			}
		}
		return notices.Notice{}, false
	}

	// Off: it takes no run, so nothing fails for want of a key.
	if _, ok := open(); ok {
		t.Fatal("a missing-keys notice while the local runner is off")
	}
	exec(`UPDATE runners SET status = 'online' WHERE id = ?`, localID)
	n, ok := open()
	if !ok {
		t.Fatal("no notice although the local runner is on and trusts none of the scope's hosts")
	}
	// Three hosts it would dial (the one with no record is an agent's to reach by name).
	if n.AgencyID != "ag-fin" || !strings.Contains(n.Detail, "3 of the 3") || strings.Contains(n.Detail, "norecord") ||
		!strings.Contains(n.Detail, "direct") || !strings.Contains(n.Detail, "host_key_unverified") {
		t.Errorf("notice = %+v; want Finance's, naming the 3 hosts the server dials and the failure", n)
	}

	// The direct host's key is approved, and the target behind the bastion —
	// but not the bastion itself: that host is still not reachable.
	trust("10.1.0.5")
	trust("10.2.0.5")
	if n, ok := open(); !ok || !strings.Contains(n.Detail, "2 of the 3") || strings.Contains(n.Detail, "direct,") {
		t.Errorf("with the bastion's own key missing: %+v (open=%v), want 2 of 3 still missing", n, ok)
	}
	// A key under the bastion's NAME changes nothing: the server hops through the record's address.
	trust("dmz-jump")
	if n, ok := open(); !ok || !strings.Contains(n.Detail, "2 of the 3") {
		t.Errorf("a key approved under the bastion's name was counted: %+v (open=%v)", n, ok)
	}
	trust("[198.51.100.7]:2222")
	if n, ok := open(); !ok || !strings.Contains(n.Detail, "1 of the 3") || !strings.Contains(n.Detail, "lost") {
		t.Errorf("with the bastion's key approved: %+v (open=%v), want only the host behind the unresolvable bastion", n, ok)
	}
	// The scope goes to an agent: the local runner no longer takes its runs.
	exec(`INSERT INTO runners (id, name, status, registered_at, created_at, owner_agency) VALUES ('r-fin', 'r-fin', 'online', 't', 't', 'ag-fin')`)
	exec(`INSERT INTO scope_runners (scope_id, runner_id, runner_name, bound_at) VALUES ('sc-fin', 'r-fin', 'r-fin', 't')`)
	if _, ok := open(); ok {
		t.Error("still reported although the scope is bound to an agent")
	}
}

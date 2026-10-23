package runner

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ResetSmith/cronomicon/internal/hostkeys"
	"github.com/ResetSmith/cronomicon/internal/keyscan"
	"github.com/ResetSmith/cronomicon/internal/notices"
	"github.com/ResetSmith/cronomicon/internal/settings"
)

// localHK is the host-key fixture for the LOCAL runner: its row exists, as
// after a start, and the handlers are called for its id. It has no token and
// never uploads; its scans run in this process.
func localHK(t *testing.T) *hkFixture {
	t.Helper()
	svc := newTestService(t)
	id, _, err := settings.EnsureLocalRunner(context.Background(), svc.db, true, 4)
	if err != nil {
		t.Fatal(err)
	}
	return &hkFixture{t: t, svc: svc, as: authSvc(t, svc), id: id}
}

// stubScan stands in for the network: each target answers with the key given
// for it, and one with no key is unreachable. It is installed once per test;
// the returned function changes what the hosts present (a re-key), under a
// lock, because a scan runs on goroutines that outlive the request that
// started it.
func stubScan(t *testing.T, keys map[string]ssh.PublicKey) (rekey func(map[string]ssh.PublicKey)) {
	t.Helper()
	var mu sync.Mutex
	present := keys
	old := scanHost
	scanHost = func(_ context.Context, target string, _ time.Duration) (keyscan.Key, error) {
		mu.Lock()
		k, ok := present[target]
		mu.Unlock()
		if !ok {
			return keyscan.Key{}, errors.New("connection refused")
		}
		return keyscan.Key{Host: target, KeyType: k.Type(), Fingerprint: ssh.FingerprintSHA256(k),
			KnownHostsLine: renderKnownHostsLine(hostkeys.Pattern(target), k)}, nil
	}
	t.Cleanup(func() { scanHost = old })
	return func(next map[string]ssh.PublicKey) {
		mu.Lock()
		present = next
		mu.Unlock()
	}
}

func (f *hkFixture) pendingFor(target string) string {
	f.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var id string
		err := f.svc.db.QueryRow(`
			SELECT id FROM pending_host_keys WHERE runner_id = ? AND host = ? AND approved_at IS NULL AND rejected_at IS NULL`,
			f.id, hostkeys.Pattern(target)).Scan(&id)
		if err == nil {
			return id
		}
		time.Sleep(20 * time.Millisecond)
	}
	f.t.Fatalf("no key for %s reached the review list: the local scan did not run", target)
	return ""
}

type ledgerState struct {
	fingerprint                                   string
	delivered, confirmed, superseded, untrustedAt sql.NullString
}

func (f *hkFixture) ledgerOf(pattern string) []ledgerState {
	f.t.Helper()
	rows, err := f.svc.db.Query(`
		SELECT fingerprint, delivered_at, confirmed_at, superseded_at, untrusted_at
		  FROM host_key_ledger WHERE runner_id = ? AND host = ? AND decision = 'approved' ORDER BY id`, f.id, pattern)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var out []ledgerState
	for rows.Next() {
		var l ledgerState
		if err := rows.Scan(&l.fingerprint, &l.delivered, &l.confirmed, &l.superseded, &l.untrustedAt); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, l)
	}
	return out
}

// The local runner's host keys are scanned by the server itself and reviewed
// like an agent's — and an approval is IN FORCE the moment it is made: there is
// no agent to poll for the line, no file to write it to and none to report. A
// key that replaces another makes the old one untrusted in the same moment.
func TestTheLocalRunnerScansInProcessAndItsApprovalsAreInForceAtOnce(t *testing.T) {
	f := localHK(t)
	first, second := testHostKey(t), testHostKey(t)
	rekey := stubScan(t, map[string]ssh.PublicKey{"10.0.0.5": first, "10.0.0.6:2222": first})

	// The owner queues a scan of two hosts and one that does not answer.
	rec := f.do(f.svc.HandleKeyscan, http.MethodPost, "/x", `{"hosts":["10.0.0.5","10.0.0.6:2222","10.0.0.7"]}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("keyscan = %d (%s)", rec.Code, rec.Body)
	}
	a, b := f.pendingFor("10.0.0.5"), f.pendingFor("10.0.0.6:2222")
	// The queue was taken by the server, not left for a poll that will never come.
	var queued sql.NullString
	_ = f.svc.db.QueryRow(`SELECT keyscan_requested FROM runners WHERE id = ?`, f.id).Scan(&queued)
	if queued.Valid && queued.String != "" {
		t.Errorf("the scan is still queued on the local runner's row: %s", queued.String)
	}

	// Nothing is trusted until it is approved.
	if n := len(f.ledgerOf("10.0.0.5")); n != 0 {
		t.Fatalf("a scanned key was trusted before review: %d ledger row(s)", n)
	}
	if rec := f.resolve([]string{a, b}, nil); rec.Code != http.StatusOK {
		t.Fatalf("approve = %d (%s)", rec.Code, rec.Body)
	}
	for _, pattern := range []string{"10.0.0.5", "[10.0.0.6]:2222"} {
		l := f.ledgerOf(pattern)
		if len(l) != 1 || !l[0].delivered.Valid || !l[0].confirmed.Valid || l[0].superseded.Valid {
			t.Fatalf("%s after approval = %+v; want one row, delivered and confirmed at once, in force", pattern, l)
		}
	}
	// Coverage reads it as trusted: no file is consulted for the local runner.
	trusted, err := hostkeys.LoadTrusted(context.Background(), f.svc.db, f.id)
	if err != nil {
		t.Fatal(err)
	}
	if got := trusted.State(hostkeys.ScopeHost{Host: "h", Pattern: "10.0.0.5"}); got != hostkeys.StateApproved {
		t.Errorf("the approved host reads %q for the local runner, want %q", got, hostkeys.StateApproved)
	}
	// Nothing is left "to deliver": the agent-side queues are empty for it.
	if lines := f.svc.takeTrustHosts(context.Background(), f.id); len(lines) != 0 {
		t.Errorf("lines waiting to be delivered to the local runner: %v", lines)
	}

	// The host is re-keyed: the scan finds another key, the review marks it
	// changed, and approving it retires the old one in the same step.
	rekey(map[string]ssh.PublicKey{"10.0.0.5": second})
	if rec := f.do(f.svc.HandleKeyscan, http.MethodPost, "/x", `{"hosts":["10.0.0.5"]}`); rec.Code != http.StatusAccepted {
		t.Fatalf("rescan = %d (%s)", rec.Code, rec.Body)
	}
	changed := f.pendingFor("10.0.0.5")
	var pending []pendingKeyRow
	_ = json.Unmarshal(f.do(f.svc.HandleListRunnerPendingHostKeys, http.MethodGet, "/x", "").Body.Bytes(), &pending)
	if len(pending) != 1 || pending[0].Status != keyStatusChanged || pending[0].PreviousSource != "runner" {
		t.Fatalf("the re-keyed host is shown as %+v, want changed against the local runner's own key", pending)
	}
	if rec := f.resolve([]string{changed}, nil, changed); rec.Code != http.StatusOK {
		t.Fatalf("approve the changed key = %d (%s)", rec.Code, rec.Body)
	}
	l := f.ledgerOf("10.0.0.5")
	if len(l) != 2 {
		t.Fatalf("ledger rows for the re-keyed host = %d, want 2", len(l))
	}
	if !l[0].superseded.Valid || !l[0].untrustedAt.Valid {
		t.Errorf("the replaced key = %+v; want superseded and untrusted at once", l[0])
	}
	if l[1].fingerprint != ssh.FingerprintSHA256(second) || l[1].superseded.Valid || !l[1].confirmed.Valid {
		t.Errorf("the new key = %+v; want in force, confirmed", l[1])
	}
	if lines := f.svc.takeUntrustHosts(context.Background(), f.id); len(lines) != 0 {
		t.Errorf("lines waiting to be removed from the local runner's (non-existent) file: %v", lines)
	}
}

// An agent's approval is unchanged by all this: queued until a poll delivers
// it, confirmed only by the agent's own report.
func TestAnAgentsApprovalStillWaitsForDelivery(t *testing.T) {
	f := newHKFixture(t, "agent")
	id := f.scan("10.0.0.5", testHostKey(t))
	if rec := f.resolve([]string{id}, nil); rec.Code != http.StatusOK {
		t.Fatalf("approve = %d (%s)", rec.Code, rec.Body)
	}
	l := f.ledgerOf("10.0.0.5")
	if len(l) != 1 || l[0].delivered.Valid || l[0].confirmed.Valid {
		t.Errorf("an agent's approval = %+v; want queued for delivery, not delivered or confirmed", l)
	}
}

// The keys the server had captured before 2.3.0 — parked by migration 1270
// when their columns were dropped — become the local runner's approved keys,
// under the address each record is dialled at, so the server goes on connecting
// to every host it was connecting to. Recorded as what they are (carried, by
// the upgrade), once, and never over a decision an operator has since made.
func TestCarryServerHostKeys(t *testing.T) {
	f := localHK(t)
	ctx := context.Background()
	exec := func(q string, a ...any) {
		t.Helper()
		if _, err := f.svc.db.Exec(q, a...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	k1, k2, k3, k4, decided := testHostKey(t), testHostKey(t), testHostKey(t), testHostKey(t), testHostKey(t)
	auth := func(k ssh.PublicKey) string { return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k))) }
	exec(`INSERT INTO agencies (id, name, created_at) VALUES ('ag-fin', 'Finance', ?)`, now())
	exec(`INSERT INTO scopes (id, name, source, created_at) VALUES ('sc-fin', 'fin-prod', 'git', ?)`, now())
	park := func(kind, id, name, hostname, address string, port int, scopeID, owner, key string) {
		exec(`INSERT INTO carried_server_host_keys (kind, record_id, name, hostname, address, port, scope_id, owner_agency, host_key)
		      VALUES (?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), ?, NULLIF(?, ''), ?, ?)`,
			kind, id, name, hostname, address, port, scopeID, owner, key)
	}
	park("host", "h1", "web1", "", "10.0.0.5", 22, "", "global", auth(k1))        // a global administrator's record
	park("host", "h2", "db1", "", "10.0.0.6", 2222, "sc-fin", "ag-fin", auth(k2)) // imported for Finance's scope, off port 22
	park("host", "h3", "named.example", "", "", 22, "", "ag-fin", auth(k3))       // Finance wrote it by hand; no address: dialled by name
	park("bastion", "b1", "jump", "jump.example", "10.0.0.9", 22, "", "global", auth(k4))
	park("host", "h4", "broken", "", "10.0.0.7", 22, "", "global", "not a key")
	park("host", "h5", "decided", "", "10.0.0.8", 22, "", "global", auth(k1))
	park("host", "h6", "web1-again", "", "10.0.0.5", 22, "sc-fin", "ag-fin", auth(k1)) // the same machine, imported for a scope too
	park("host", "h7", "other-zone", "", "10.0.0.5", 22, "", "global", auth(k2))       // the same ADDRESS, a different key
	// An operator has already approved a key for 10.0.0.8 since the upgrade.
	serverTrusts(t, f.svc, "10.0.0.8", decided, "", "root@example.com")

	n, err := CarryServerHostKeys(ctx, f.svc.db, f.svc.log)
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Errorf("carried %d record(s), want 4 (not the unreadable key, the one already decided, the duplicate or the conflict)", n)
	}
	type row struct{ fingerprint, source, actor, scopeID, hostName string }
	inForce := func(pattern string) []row {
		t.Helper()
		rows, err := f.svc.db.Query(`
			SELECT fingerprint, source, actor, COALESCE(scope_id, ''), COALESCE(host_name, '') FROM host_key_ledger
			 WHERE runner_id = ? AND host = ? AND decision = 'approved' AND superseded_at IS NULL
			   AND delivered_at IS NOT NULL AND confirmed_at IS NOT NULL ORDER BY id`, f.id, pattern)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.fingerprint, &r.source, &r.actor, &r.scopeID, &r.hostName); err != nil {
				t.Fatal(err)
			}
			out = append(out, r)
		}
		return out
	}
	fp := ssh.FingerprintSHA256
	for _, want := range []struct {
		pattern string
		row     row
	}{
		{"10.0.0.5", row{fp(k1), "carried", "upgrade", "", "web1"}},
		{"[10.0.0.6]:2222", row{fp(k2), "carried", "upgrade:agency:ag-fin", "sc-fin", "db1"}},
		{"named.example", row{fp(k3), "carried", "upgrade:agency:ag-fin", "", "named.example"}},
		// A bastion's key under its dial address — never under a name.
		{"10.0.0.9", row{fp(k4), "carried", "upgrade", "", "jump"}},
		// The operator's decision stands.
		{"10.0.0.8", row{fp(decided), "pasted", "root@example.com", "", ""}},
	} {
		got := inForce(want.pattern)
		if len(got) != 1 || got[0] != want.row {
			t.Errorf("in force for %s = %+v, want exactly %+v", want.pattern, got, want.row)
		}
	}
	if got := inForce("10.0.0.7"); len(got) != 0 {
		t.Errorf("an unreadable stored key was carried: %+v", got)
	}
	for _, name := range []string{"jump", "jump.example"} {
		if got := inForce(name); len(got) != 0 {
			t.Errorf("the bastion's key was recorded under the name %q: %+v", name, got)
		}
	}
	// Every parked row is handled, with the reason where it was not carried.
	var left int
	_ = f.svc.db.QueryRow(`SELECT COUNT(*) FROM carried_server_host_keys WHERE carried_at IS NULL`).Scan(&left)
	if left != 0 {
		t.Errorf("%d parked key(s) left unhandled", left)
	}
	notes := map[string]string{}
	nrows, err := f.svc.db.Query(`SELECT record_id, COALESCE(note, '') FROM carried_server_host_keys`)
	if err != nil {
		t.Fatal(err)
	}
	for nrows.Next() {
		var id, note string
		_ = nrows.Scan(&id, &note)
		notes[id] = note
	}
	nrows.Close()
	if !strings.Contains(notes["h4"], "could not be read") {
		t.Errorf("the unreadable key's note = %q", notes["h4"])
	}
	// The same key for the same address is nothing lost; a different key is a
	// conflict, recorded as one — the inbox reports it.
	if notes["h6"] != noteSameKey {
		t.Errorf("a second record with the SAME key for the address: note = %q, want %q", notes["h6"], noteSameKey)
	}
	if notes["h7"] != notices.CarriedKeyConflictNote || notes["h5"] != notices.CarriedKeyConflictNote {
		t.Errorf("records whose key differs from the one in force: notes = %q, %q; want the conflict note", notes["h7"], notes["h5"])
	}
	if got := inForce("10.0.0.5"); len(got) != 1 || got[0].fingerprint != fp(k1) {
		t.Errorf("in force for the contested address = %+v, want the first record's key alone", got)
	}
	if err := notices.RunChecks(ctx, f.svc.db); err != nil {
		t.Fatalf("checks: %v", err)
	}
	// No host or bastion RECORDS exist in this fixture, only parked keys: a
	// conflict is reported for a record that is still there.
	var conflicts int
	_ = f.svc.db.QueryRow(`SELECT COUNT(*) FROM notices WHERE kind = 'host_key_conflict' AND resolved_at IS NULL`).Scan(&conflicts)
	if conflicts != 0 {
		t.Errorf("%d conflict notice(s) for records that do not exist", conflicts)
	}
	exec(`INSERT INTO ssh_hosts (id, hostname, address, port, created_at) VALUES ('h7', 'other-zone', '10.0.0.5', 22, ?)`, now())
	if err := notices.RunChecks(ctx, f.svc.db); err != nil {
		t.Fatalf("checks: %v", err)
	}
	var detail string
	if err := f.svc.db.QueryRow(`SELECT detail FROM notices WHERE kind = 'host_key_conflict' AND subject = 'host:h7' AND resolved_at IS NULL`).Scan(&detail); err != nil {
		t.Fatalf("no conflict notice for the record whose key lost: %v", err)
	}
	if !strings.Contains(detail, "other-zone") || !strings.Contains(detail, fp(k2)) || !strings.Contains(detail, fp(k1)) {
		t.Errorf("the notice must name the record and both keys: %q", detail)
	}
	if !strings.Contains(detail, "dismiss this notice") {
		t.Errorf("the notice must say what clears it when nothing needs changing: %q", detail)
	}
	openConflict := func(subject string) int {
		t.Helper()
		var n int
		_ = f.svc.db.QueryRow(`SELECT COUNT(*) FROM notices WHERE kind = 'host_key_conflict' AND subject = ? AND resolved_at IS NULL`, subject).Scan(&n)
		return n
	}
	// The notice asks which of two keys is right, about a key the UPGRADE chose.
	// A key a person approved is not a question (2.3.2): h5's address was decided
	// by an operator before the carry, so its record raises nothing.
	exec(`INSERT INTO ssh_hosts (id, hostname, address, port, created_at) VALUES ('h5', 'decided', '10.0.0.8', 22, ?)`, now())
	if err := notices.RunChecks(ctx, f.svc.db); err != nil {
		t.Fatalf("checks: %v", err)
	}
	if openConflict("host:h5") != 0 {
		t.Error("a conflict was reported against a key an operator approved")
	}
	// And the open one resolves by its remedy: a person scans the host and
	// approves the key it has now. Until 2.3.2 it stayed, whatever was approved.
	exec(`UPDATE host_key_ledger SET superseded_at = ? WHERE host = '10.0.0.5' AND decision = 'approved' AND superseded_at IS NULL`, now())
	serverTrusts(t, f.svc, "10.0.0.5", decided, "", "root@example.com")
	if err := notices.RunChecks(ctx, f.svc.db); err != nil {
		t.Fatalf("checks: %v", err)
	}
	if openConflict("host:h7") != 0 {
		t.Error("the conflict notice stayed open after an operator approved a key for the address")
	}
	// It runs once.
	before := f.count(`SELECT COUNT(*) FROM host_key_ledger`)
	if n, err := CarryServerHostKeys(ctx, f.svc.db, f.svc.log); err != nil || n != 0 {
		t.Errorf("a second pass carried %d (%v), want nothing", n, err)
	}
	if after := f.count(`SELECT COUNT(*) FROM host_key_ledger`); after != before {
		t.Errorf("a second pass wrote %d ledger row(s)", after-before)
	}
}

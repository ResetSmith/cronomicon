package runner

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/hostkeys"
	"github.com/ResetSmith/cronomicon/internal/runnerproto"
)

// The host-key ledger (SB band; migrations 1200/1210, protocol 14). What these
// tests hold in place:
//
//   - the SERVER decides what a scanned line says: fingerprint and delivered
//     line come from the parsed key, bound to the one host that was scanned;
//   - a batch belongs to one runner, is all-or-nothing, and leaves one ledger
//     row per key and one change-log row for the lot;
//   - a changed key REPLACES the old one — the old line is removed from the
//     runner, not left trusted beside the new;
//   - nothing typed into the paste box reaches a runner's file except as one
//     rendered line per named host;
//   - the runner's report of its file is kept apart from what was approved,
//     and is the only thing that confirms a delivery.

func testHostKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	k, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("ssh key: %v", err)
	}
	return k
}

// scanLine is the known_hosts line an agent would upload for a scan target.
func scanLine(target string, key ssh.PublicKey) string {
	return renderKnownHostsLine(hostkeys.Pattern(target), key)
}

func scanBody(target, line string) string {
	b, _ := json.Marshal(map[string]any{"entries": []map[string]string{{"host": target, "knownHostsLine": line}}})
	return string(b)
}

// operator returns a request carrying an unrestricted operator's identity.
func operator(method, path, body string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	return req.WithContext(auth.WithIdentity(req.Context(),
		auth.Identity{Email: "op@example.com", AllowedScopes: []string{auth.AllScopes}}))
}

type hkFixture struct {
	t   *testing.T
	svc *Service
	as  *auth.Service
	id  string
	tok string
}

func newHKFixture(t *testing.T, name string) *hkFixture {
	t.Helper()
	svc := newTestService(t)
	f := &hkFixture{t: t, svc: svc, as: authSvc(t, svc), id: "runner-" + name, tok: "crn_run_" + name}
	insertRunner(t, svc, f.id, name, "online", []string{"bash"})
	bindRunnerToken(t, svc, f.tok, f.id)
	return f
}

// scan uploads one scanned key and returns its pending id.
func (f *hkFixture) scan(target string, key ssh.PublicKey) string {
	f.t.Helper()
	if code := uploadKeys(f.t, f.svc, f.as, f.id, f.tok, scanBody(target, scanLine(target, key))); code != http.StatusOK {
		f.t.Fatalf("upload %s: %d", target, code)
	}
	var id string
	if err := f.svc.db.QueryRow(`
		SELECT id FROM pending_host_keys WHERE runner_id = ? AND host = ? AND approved_at IS NULL AND rejected_at IS NULL`,
		f.id, hostkeys.Pattern(target)).Scan(&id); err != nil {
		f.t.Fatalf("pending row for %s: %v", target, err)
	}
	return id
}

func (f *hkFixture) do(h http.HandlerFunc, method, path, body string, pathVals ...string) *httptest.ResponseRecorder {
	f.t.Helper()
	req := operator(method, path, body)
	req.SetPathValue("id", f.id)
	for i := 0; i+1 < len(pathVals); i += 2 {
		req.SetPathValue(pathVals[i], pathVals[i+1])
	}
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func (f *hkFixture) resolve(approve, reject []string, acknowledgeChanged ...string) *httptest.ResponseRecorder {
	f.t.Helper()
	b, _ := json.Marshal(map[string]any{"approve": approve, "reject": reject, "acknowledgeChanged": acknowledgeChanged})
	return f.do(f.svc.HandleResolveHostKeyBatch, http.MethodPost, "/x", string(b))
}

func (f *hkFixture) count(query string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.svc.db.QueryRow(query, args...).Scan(&n); err != nil {
		f.t.Fatalf("count: %v", err)
	}
	return n
}

// control polls once and returns the control ops by name.
func (f *hkFixture) control() map[string]runnerproto.PollControl {
	f.t.Helper()
	// A claimable run makes the poll return at once instead of long-polling.
	insertQueuedRun(f.t, f.svc, "q-"+db.NewID(), "hk-job", "bash", "")
	pr, code := pollDecode(f.t, f.svc, f.as, f.id, f.tok, "settingsVersion=0")
	if code != http.StatusOK {
		f.t.Fatalf("poll: %d", code)
	}
	out := map[string]runnerproto.PollControl{}
	for _, c := range pr.Control {
		out[c.Op] = c
	}
	return out
}

func (f *hkFixture) view() runnerHostKeysView {
	f.t.Helper()
	rec := f.do(f.svc.HandleRunnerHostKeys, http.MethodGet, "/x", "")
	if rec.Code != http.StatusOK {
		f.t.Fatalf("view: %d %s", rec.Code, rec.Body.String())
	}
	var v runnerHostKeysView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		f.t.Fatalf("decode view: %v", err)
	}
	return v
}

func (f *hkFixture) report(entries ...knownHostsEntry) {
	f.t.Helper()
	b, _ := json.Marshal(knownHostsReport{Entries: entries})
	h := f.as.RequireRunner(http.HandlerFunc(f.svc.HandleUploadKnownHosts))
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(string(b)))
	req.SetPathValue("id", f.id)
	req.Header.Set("Authorization", "Bearer "+f.tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		f.t.Fatalf("report: %d %s", rec.Code, rec.Body.String())
	}
}

// The agent's claimed key type and fingerprint are ignored: what is stored, and
// later shown to the operator, is computed from the key in the line.
func TestUploadDerivesFingerprintFromTheLine(t *testing.T) {
	f := newHKFixture(t, "derive")
	key := testHostKey(t)
	body := fmt.Sprintf(`{"entries":[{"host":"db01:2222","keyType":"ssh-rsa","fingerprint":"SHA256:lie","knownHostsLine":%q}]}`,
		scanLine("db01:2222", key))
	if code := uploadKeys(t, f.svc, f.as, f.id, f.tok, body); code != http.StatusOK {
		t.Fatalf("upload: %d", code)
	}
	var host, keyType, fp, line string
	if err := f.svc.db.QueryRow(`SELECT host, key_type, fingerprint, known_hosts_line FROM pending_host_keys WHERE runner_id = ?`, f.id).
		Scan(&host, &keyType, &fp, &line); err != nil {
		t.Fatalf("pending row: %v", err)
	}
	if host != "[db01]:2222" || keyType != key.Type() || fp != ssh.FingerprintSHA256(key) {
		t.Fatalf("stored %q %q %q; want the known_hosts host and the key's own type and fingerprint", host, keyType, fp)
	}
	if !strings.HasPrefix(line, "[db01]:2222 ssh-ed25519 ") {
		t.Fatalf("line was not rendered for the scanned host: %q", line)
	}
}

// A line must be for the host it was scanned as, and for that host only.
func TestUploadRefusesALineForAnotherHost(t *testing.T) {
	f := newHKFixture(t, "other")
	key := testHostKey(t)
	for name, line := range map[string]string{
		"different host": scanLine("somewhere-else", key),
		"wildcard":       renderKnownHostsLine("*", key),
		"two hosts":      renderKnownHostsLine("web01,10.0.0.9", key),
		"revoked marker": "@revoked " + scanLine("web01", key),
		"not a key":      "web01 ssh-ed25519 AAAA",
	} {
		if code := uploadKeys(t, f.svc, f.as, f.id, f.tok, scanBody("web01", line)); code != http.StatusOK {
			t.Fatalf("%s: upload answered %d", name, code)
		}
		if n := f.count(`SELECT COUNT(*) FROM pending_host_keys WHERE runner_id = ?`, f.id); n != 0 {
			t.Fatalf("%s: stored %d pending row(s); want none", name, n)
		}
	}
}

func TestBatchResolveWritesOneBatch(t *testing.T) {
	f := newHKFixture(t, "batch")
	a, b, c := f.scan("web01", testHostKey(t)), f.scan("web02", testHostKey(t)), f.scan("web03", testHostKey(t))

	rec := f.resolve([]string{a, b}, []string{c})
	if rec.Code != http.StatusOK {
		t.Fatalf("resolve: %d %s", rec.Code, rec.Body.String())
	}
	var res batchResult
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if res.Approved != 2 || res.Rejected != 1 || res.BatchID == "" {
		t.Fatalf("result %+v; want 2 approved, 1 rejected, a batch id", res)
	}
	if n := f.count(`SELECT COUNT(*) FROM host_key_ledger WHERE batch_id = ? AND runner_id = ? AND runner_name = 'batch' AND actor = 'op@example.com'`, res.BatchID, f.id); n != 3 {
		t.Fatalf("ledger rows in the batch = %d; want 3, each naming runner and actor", n)
	}
	// A rejection is never in force.
	if n := f.count(`SELECT COUNT(*) FROM host_key_ledger WHERE decision = 'rejected' AND superseded_at IS NULL`); n != 0 {
		t.Fatalf("%d rejected row(s) left in force", n)
	}
	// One change-log row for the batch, naming the runner.
	var target, details string
	if err := f.svc.db.QueryRow(`SELECT target, details FROM change_log WHERE category = 'Host Keys'`).Scan(&target, &details); err != nil {
		t.Fatalf("change log: %v", err)
	}
	if n := f.count(`SELECT COUNT(*) FROM change_log WHERE category = 'Host Keys'`); n != 1 {
		t.Fatalf("%d change-log rows; want one for the batch", n)
	}
	if !strings.Contains(target, "runner:batch") || !strings.Contains(details, res.BatchID) ||
		!strings.Contains(details, "2 approved") || !strings.Contains(details, "1 rejected") {
		t.Fatalf("change log %q / %q does not name the runner, the counts and the batch", target, details)
	}
	// Only the approved lines are delivered.
	if got := f.control()["trust-hosts"].Entries; len(got) != 2 {
		t.Fatalf("delivered %d line(s); want the 2 approved", len(got))
	}
}

// A batch is for one runner. An id that belongs to another runner fails the
// whole batch, and nothing is written.
func TestBatchResolveIsPerRunnerAndAtomic(t *testing.T) {
	f := newHKFixture(t, "mine")
	insertRunner(t, f.svc, "runner-theirs", "theirs", "online", []string{"bash"})
	bindRunnerToken(t, f.svc, "crn_run_theirs", "runner-theirs")
	mine := f.scan("web01", testHostKey(t))
	if code := uploadKeys(t, f.svc, f.as, "runner-theirs", "crn_run_theirs", scanBody("web09", scanLine("web09", testHostKey(t)))); code != http.StatusOK {
		t.Fatalf("upload: %d", code)
	}
	var theirs string
	_ = f.svc.db.QueryRow(`SELECT id FROM pending_host_keys WHERE runner_id = 'runner-theirs'`).Scan(&theirs)

	if rec := f.resolve([]string{mine, theirs}, nil); rec.Code != http.StatusConflict {
		t.Fatalf("a batch naming another runner's key answered %d; want 409", rec.Code)
	}
	if n := f.count(`SELECT COUNT(*) FROM host_key_ledger`); n != 0 {
		t.Fatalf("%d ledger row(s) written by a refused batch", n)
	}
	if n := f.count(`SELECT COUNT(*) FROM pending_host_keys WHERE approved_at IS NOT NULL`); n != 0 {
		t.Fatalf("%d pending row(s) approved by a refused batch", n)
	}
}

// A re-scan that finds a different key is shown as changed, with the key it
// would replace; approving it removes the old line from the runner.
func TestChangedKeyReplacesTheOldOne(t *testing.T) {
	f := newHKFixture(t, "chg")
	oldKey, newKey := testHostKey(t), testHostKey(t)
	if rec := f.resolve([]string{f.scan("web01", oldKey)}, nil); rec.Code != http.StatusOK {
		t.Fatalf("first approve: %d", rec.Code)
	}
	if got := f.control()["trust-hosts"].Entries; len(got) != 1 {
		t.Fatalf("first key not delivered: %v", got)
	}

	second := f.scan("web01", newKey)
	rec := f.do(f.svc.HandleListRunnerPendingHostKeys, http.MethodGet, "/x", "")
	var pending []pendingKeyRow
	_ = json.Unmarshal(rec.Body.Bytes(), &pending)
	if len(pending) != 1 || pending[0].Status != keyStatusChanged ||
		pending[0].PreviousFingerprint != ssh.FingerprintSHA256(oldKey) || pending[0].PreviousSource != "runner" {
		t.Fatalf("pending = %+v; want one changed key naming the runner's previous fingerprint", pending)
	}

	// A key that replaces a trusted one is accepted knowingly or not at all:
	// approving it as if it were any other key is refused, and writes nothing.
	if rec := f.resolve([]string{second}, nil); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "review_stale") {
		t.Fatalf("approving a changed key without acknowledging it answered %d %s; want 409 review_stale", rec.Code, rec.Body.String())
	}
	if n := f.count(`SELECT COUNT(*) FROM host_key_ledger WHERE runner_id = ?`, f.id); n != 1 {
		t.Fatalf("a refused batch left %d ledger rows; want the original only", n)
	}
	if n := f.count(`SELECT COUNT(*) FROM pending_host_keys WHERE id = ? AND approved_at IS NULL`, second); n != 1 {
		t.Fatalf("a refused batch resolved the pending key")
	}
	if rec := f.resolve([]string{second}, nil, second); rec.Code != http.StatusOK {
		t.Fatalf("second approve: %d %s", rec.Code, rec.Body.String())
	}
	if n := f.count(`SELECT COUNT(*) FROM host_key_ledger WHERE runner_id = ? AND decision = 'approved' AND superseded_at IS NULL`, f.id); n != 1 {
		t.Fatalf("%d keys in force for one host; want 1", n)
	}
	var prev string
	_ = f.svc.db.QueryRow(`SELECT COALESCE(previous_fingerprint, '') FROM host_key_ledger WHERE fingerprint = ?`, ssh.FingerprintSHA256(newKey)).Scan(&prev)
	if prev != ssh.FingerprintSHA256(oldKey) {
		t.Fatalf("the replacement does not record what it replaced: %q", prev)
	}

	ctl := f.control()
	if got := ctl["untrust-hosts"].Entries; len(got) != 1 || got[0] != scanLine("web01", oldKey) {
		t.Fatalf("untrust-hosts = %v; want the old line", got)
	}
	if got := ctl["trust-hosts"].Entries; len(got) != 1 || got[0] != scanLine("web01", newKey) {
		t.Fatalf("trust-hosts = %v; want the new line", got)
	}
	if _, ok := ctl["known-hosts-report"]; !ok {
		t.Fatalf("no report requested after changing the file")
	}
	if again := f.control(); len(again["untrust-hosts"].Entries)+len(again["trust-hosts"].Entries) != 0 {
		t.Fatalf("host-key ops delivered twice: %+v", again)
	}
}

// Approving a key the runner already trusts records nothing new.
func TestApprovingATrustedKeyIsUnchanged(t *testing.T) {
	f := newHKFixture(t, "same")
	key := testHostKey(t)
	f.resolve([]string{f.scan("web01", key)}, nil)
	f.control()

	rec := f.resolve([]string{f.scan("web01", key)}, nil)
	var res batchResult
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if rec.Code != http.StatusOK || res.Unchanged != 1 || res.Approved != 0 || res.BatchID != "" {
		t.Fatalf("re-approving a trusted key: %d %+v; want unchanged=1 and no batch", rec.Code, res)
	}
	if n := f.count(`SELECT COUNT(*) FROM host_key_ledger WHERE runner_id = ?`, f.id); n != 1 {
		t.Fatalf("%d ledger rows; want the original only", n)
	}
	// Never confirmed by a report, so the operator's second approval sends it again.
	if got := f.control()["trust-hosts"].Entries; len(got) != 1 {
		t.Fatalf("an unconfirmed key was not re-sent: %v", got)
	}
}

// The server's own pin for a host is the second opinion: same key = match.
func TestClassifiesAgainstTheServerPin(t *testing.T) {
	f := newHKFixture(t, "pin")
	pinned, other := testHostKey(t), testHostKey(t)
	// What the server trusts is what is in force for the local runner (2.3.0).
	for _, host := range []string{"10.0.0.5", "10.0.0.6"} {
		serverTrusts(t, f.svc, host, pinned, "", "root@example.com")
	}
	f.scan("10.0.0.5", pinned) // what the server pinned
	f.scan("10.0.0.6", other)  // differs from the server's pin
	f.scan("10.0.0.7", other)  // the server knows nothing

	rec := f.do(f.svc.HandleListRunnerPendingHostKeys, http.MethodGet, "/x", "")
	var pending []pendingKeyRow
	_ = json.Unmarshal(rec.Body.Bytes(), &pending)
	got := map[string]pendingKeyRow{}
	for _, p := range pending {
		got[p.Host] = p
	}
	if p := got["10.0.0.5"]; p.Status != keyStatusMatch || !p.MatchedServerPin {
		t.Errorf("10.0.0.5 = %+v; want match", p)
	}
	if p := got["10.0.0.6"]; p.Status != keyStatusChanged || p.PreviousSource != "server" || p.PreviousFingerprint != ssh.FingerprintSHA256(pinned) {
		t.Errorf("10.0.0.6 = %+v; want changed against the server's pin", p)
	}
	if p := got["10.0.0.7"]; p.Status != keyStatusNew {
		t.Errorf("10.0.0.7 = %+v; want new", p)
	}
}

// provide runs the paste flow the way the screen does: a dry run, and — for a
// commit — a second call naming every reviewed row by host, type, fingerprint
// and the status it was shown with.
func (f *hkFixture) provide(dryRun bool, lines ...string) (*httptest.ResponseRecorder, candidatesResponse) {
	f.t.Helper()
	text := []string{strings.Join(lines, "\n")}
	b, _ := json.Marshal(provideRequest{Lines: text, DryRun: true})
	rec := f.do(f.svc.HandleProvideHostKeys, http.MethodPost, "/x", string(b))
	var out candidatesResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if dryRun {
		return rec, out
	}
	sel := []keySelection{}
	for _, c := range out.Candidates {
		sel = append(sel, keySelection{Host: c.Host, KeyType: c.KeyType, Fingerprint: c.Fingerprint, Status: c.Status})
	}
	b, _ = json.Marshal(provideRequest{Lines: text, Select: &sel})
	rec = f.do(f.svc.HandleProvideHostKeys, http.MethodPost, "/x", string(b))
	out = candidatesResponse{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func TestProvideDryRunWritesNothing(t *testing.T) {
	f := newHKFixture(t, "dry")
	key := testHostKey(t)
	rec, out := f.provide(true, "# a comment", "", renderKnownHostsLine("web01,10.0.0.5", key)+" trailing comment")
	if rec.Code != http.StatusOK || !out.Acceptable || len(out.Candidates) != 2 {
		t.Fatalf("dry run: %d %+v; want two acceptable candidates (one per host)", rec.Code, out)
	}
	for _, c := range out.Candidates {
		if c.Fingerprint != ssh.FingerprintSHA256(key) || c.Status != keyStatusNew {
			t.Errorf("candidate %+v; want the key's fingerprint, status new", c)
		}
	}
	if n := f.count(`SELECT COUNT(*) FROM host_key_ledger`); n != 0 {
		t.Fatalf("a dry run wrote %d ledger row(s)", n)
	}
}

func TestProvideCommitsOneLinePerHost(t *testing.T) {
	f := newHKFixture(t, "paste")
	key := testHostKey(t)
	rec, _ := f.provide(false, renderKnownHostsLine("web01,[10.0.0.5]:2222", key)+" root@somewhere")
	if rec.Code != http.StatusOK {
		t.Fatalf("provide: %d %s", rec.Code, rec.Body.String())
	}
	if n := f.count(`SELECT COUNT(*) FROM host_key_ledger WHERE runner_id = ? AND decision = 'approved' AND source = 'pasted' AND superseded_at IS NULL`, f.id); n != 2 {
		t.Fatalf("%d pasted keys in force; want 2", n)
	}
	got := f.control()["trust-hosts"].Entries
	want := map[string]bool{renderKnownHostsLine("web01", key): true, renderKnownHostsLine("[10.0.0.5]:2222", key): true}
	if len(got) != 2 || !want[got[0]] || !want[got[1]] {
		t.Fatalf("delivered %q; want one rendered line per host, no comment", got)
	}
}

// One bad line refuses the whole paste; and the lines that would mean more than
// "trust this key for this host" are all bad lines.
func TestProvideRefusesWhatItCannotReview(t *testing.T) {
	f := newHKFixture(t, "bad")
	key := testHostKey(t)
	good := renderKnownHostsLine("web01", key)
	for name, bad := range map[string]string{
		"wildcard":       renderKnownHostsLine("*.example.com", key),
		"negation":       renderKnownHostsLine("!web02", key),
		"revoked":        "@revoked " + renderKnownHostsLine("web02", key),
		"cert-authority": "@cert-authority " + renderKnownHostsLine("web02", key),
		"garbage":        "web02 ssh-ed25519 not-base64",
		"duplicate":      good,
	} {
		rec, _ := f.provide(false, good, bad)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: answered %d; want 422", name, rec.Code)
		}
		if n := f.count(`SELECT COUNT(*) FROM host_key_ledger`); n != 0 {
			t.Fatalf("%s: %d ledger row(s) written; a paste is all or nothing", name, n)
		}
	}
	// A control character inside a JSON string cannot hide a second line: the
	// paste is split on newlines, so it is simply two lines, each reviewed.
	_, out := f.provide(true, good+"\n"+renderKnownHostsLine("web02", testHostKey(t)))
	if len(out.Candidates) != 2 {
		t.Fatalf("an embedded newline produced %d candidate(s); want 2 separately reviewed", len(out.Candidates))
	}
}

func TestRemoveKeyUntrustsIt(t *testing.T) {
	f := newHKFixture(t, "rm")
	key := testHostKey(t)
	f.resolve([]string{f.scan("web01", key)}, nil)
	f.control()
	v := f.view()
	if len(v.InForce) != 1 {
		t.Fatalf("in force = %d; want 1", len(v.InForce))
	}
	rec := f.do(f.svc.HandleRemoveHostKey, http.MethodPost, "/x", "", "ledgerId", fmt.Sprint(v.InForce[0].ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("remove: %d %s", rec.Code, rec.Body.String())
	}
	v = f.view()
	if len(v.InForce) != 0 || len(v.History) != 2 {
		t.Fatalf("after removal: %d in force, %d history; want 0 and 2 (the approval and the removal)", len(v.InForce), len(v.History))
	}
	if got := f.control()["untrust-hosts"].Entries; len(got) != 1 || got[0] != scanLine("web01", key) {
		t.Fatalf("untrust-hosts = %v; want the removed line", got)
	}
	// Removing it twice is a conflict, not a second removal row.
	if rec := f.do(f.svc.HandleRemoveHostKey, http.MethodPost, "/x", "", "ledgerId", fmt.Sprint(v.History[1].ID)); rec.Code != http.StatusConflict {
		t.Fatalf("second removal answered %d; want 409", rec.Code)
	}
}

// The runner's report is its own list. It confirms deliveries, marks which of
// its lines were approved here, and never turns into an approval.
func TestKnownHostsReportIsKeptApart(t *testing.T) {
	f := newHKFixture(t, "rep")
	approved, seeded, lost := testHostKey(t), testHostKey(t), testHostKey(t)
	f.resolve([]string{f.scan("web01", approved), f.scan("web02", lost)}, nil)
	f.control() // both delivered

	if v := f.view(); v.KnownHosts.ReportedAt != nil || v.InForce[0].PresentInFile != nil {
		t.Fatalf("before any report the view must not claim to know the file: %+v", v.KnownHosts)
	}

	fp := ssh.FingerprintSHA256
	f.report(
		knownHostsEntry{Line: 1, Hosts: "web01", KeyType: approved.Type(), Fingerprint: fp(approved)},
		knownHostsEntry{Line: 2, Hosts: "legacy-box", KeyType: seeded.Type(), Fingerprint: fp(seeded)},
		knownHostsEntry{Line: 3, Hosts: "|1|c2FsdA==|aGFzaA==", Hashed: true, KeyType: seeded.Type(), Fingerprint: fp(seeded)},
		knownHostsEntry{Line: 4, Hosts: "*.old", Marker: "revoked", KeyType: lost.Type(), Fingerprint: fp(lost)},
	)
	v := f.view()
	if v.KnownHosts.ReportedAt == nil || len(v.KnownHosts.Entries) != 4 {
		t.Fatalf("file view = %+v; want 4 entries and a report time", v.KnownHosts)
	}
	byLine := map[int]knownHostsFileRow{}
	for _, e := range v.KnownHosts.Entries {
		byLine[e.Line] = e
	}
	if !byLine[1].ApprovedHere || byLine[2].ApprovedHere || byLine[3].ApprovedHere || byLine[4].ApprovedHere {
		t.Fatalf("approvedHere wrong: %+v", byLine)
	}
	if byLine[3].Hosts != "" || !byLine[3].Hashed {
		t.Fatalf("a hashed entry must be stored without its hash: %+v", byLine[3])
	}
	if byLine[4].Marker != "revoked" {
		t.Fatalf("a marker line must be shown as one: %+v", byLine[4])
	}
	// web01 is confirmed and present; web02's only appearance is a @revoked
	// line, which is the opposite of confirmation.
	for _, k := range v.InForce {
		present := k.PresentInFile != nil && *k.PresentInFile
		switch k.Host {
		case "web01":
			if k.ConfirmedAt == nil || !present {
				t.Errorf("web01 should be confirmed and present: %+v", k)
			}
		case "web02":
			if k.ConfirmedAt != nil || present {
				t.Errorf("web02 should be neither confirmed nor present: %+v", k)
			}
		}
	}
	// The hand-seeded key is in the file's list only. It is not in the ledger.
	if n := f.count(`SELECT COUNT(*) FROM host_key_ledger WHERE fingerprint = ?`, fp(seeded)); n != 0 {
		t.Fatalf("a reported key became a ledger row")
	}

	// A report replaces the previous one.
	f.report(knownHostsEntry{Line: 1, Hosts: "web01", KeyType: approved.Type(), Fingerprint: fp(approved)})
	if v := f.view(); len(v.KnownHosts.Entries) != 1 {
		t.Fatalf("a second report left %d entries; want 1", len(v.KnownHosts.Entries))
	}

	// Re-sending the key the file lacks queues it again.
	for _, k := range f.view().InForce {
		if k.Host == "web02" {
			if rec := f.do(f.svc.HandleResendHostKey, http.MethodPost, "/x", "", "ledgerId", fmt.Sprint(k.ID)); rec.Code != http.StatusAccepted {
				t.Fatalf("resend: %d", rec.Code)
			}
		}
	}
	if got := f.control()["trust-hosts"].Entries; len(got) != 1 || got[0] != scanLine("web02", lost) {
		t.Fatalf("resend delivered %v; want web02's line", got)
	}
}

func TestKnownHostsReportIsOwnershipGuarded(t *testing.T) {
	f := newHKFixture(t, "own")
	insertRunner(t, f.svc, "runner-other", "other", "online", []string{"bash"})
	h := f.as.RequireRunner(http.HandlerFunc(f.svc.HandleUploadKnownHosts))
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"entries":[]}`))
	req.SetPathValue("id", "runner-other")
	req.Header.Set("Authorization", "Bearer "+f.tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("a runner reporting for another answered %d; want 404", rec.Code)
	}
}

func TestRefreshRequestsOneReport(t *testing.T) {
	f := newHKFixture(t, "ref")
	if _, ok := f.control()["known-hosts-report"]; ok {
		t.Fatalf("a report was requested with nothing asking for one")
	}
	if rec := f.do(f.svc.HandleRequestKnownHosts, http.MethodPost, "/x", ""); rec.Code != http.StatusAccepted {
		t.Fatalf("refresh: %d", rec.Code)
	}
	if _, ok := f.control()["known-hosts-report"]; !ok {
		t.Fatalf("the requested report was not delivered")
	}
	if _, ok := f.control()["known-hosts-report"]; ok {
		t.Fatalf("the request was delivered twice")
	}
}

// seedScope builds a scope with three hosts: one direct on a non-default port,
// one behind a bastion, and one with no SSH host record.
func (f *hkFixture) seedScope(name string) string {
	f.t.Helper()
	scopeID := db.NewID()
	insertScopeRow(f.t, f.svc, scopeID, name)
	for _, h := range []string{"direct", "fronted", "norecord"} {
		if _, err := f.svc.db.Exec(`INSERT INTO scope_hosts(scope_id, host) VALUES (?, ?)`, scopeID, h); err != nil {
			f.t.Fatalf("scope host: %v", err)
		}
	}
	for _, h := range []struct {
		host, addr string
		port       int
		via        any
	}{{"direct", "10.1.0.5", 2222, nil}, {"fronted", "10.2.0.5", 22, "bastion-a"}} {
		if _, err := f.svc.db.Exec(`
			INSERT INTO ssh_hosts(id, hostname, address, port, username, via, created_at)
			VALUES (?, ?, ?, ?, 'deploy', ?, ?)`, db.NewID(), h.host, h.addr, h.port, h.via, now()); err != nil {
			f.t.Fatalf("ssh host: %v", err)
		}
	}
	return scopeID
}

func TestScopeScanExpandsAndNamesWhatItSkips(t *testing.T) {
	f := newHKFixture(t, "scope")
	scopeID := f.seedScope("dmz")

	rec := f.do(f.svc.HandleKeyscan, http.MethodPost, "/x", `{"scopeId":"`+scopeID+`"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("scope scan: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Hosts   []string          `json:"hosts"`
		Skipped []skippedScanHost `json:"skipped"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	// The directly reachable host, and the bastion — which is itself dialled
	// directly, and whose key the runner checks on the way to the host behind it.
	if want := []string{"10.1.0.5:2222", "bastion-a"}; !slices.Equal(out.Hosts, want) {
		t.Fatalf("queued %v; want %v", out.Hosts, want)
	}
	reasons := map[string]string{}
	for _, s := range out.Skipped {
		reasons[s.Host] = s.Reason
	}
	if !strings.Contains(reasons["fronted"], "not scannable, provide the key") || !strings.Contains(reasons["norecord"], "no SSH host record") {
		t.Fatalf("skipped = %+v; want both unscannable hosts, each with its reason", out.Skipped)
	}
	// The runner is told addresses, and nothing about the scope.
	if got := f.control()["keyscan"].Hosts; !slices.Equal(got, []string{"10.1.0.5:2222", "bastion-a"}) {
		t.Fatalf("keyscan op carried %v", got)
	}

	// The key that comes back is filed under the scope and the host's name.
	key := testHostKey(t)
	id := f.scan("10.1.0.5:2222", key)
	if rec := f.resolve([]string{id}, nil); rec.Code != http.StatusOK {
		t.Fatalf("approve: %d", rec.Code)
	}
	var scope, hostName, host string
	if err := f.svc.db.QueryRow(`SELECT scope_name, host_name, host FROM host_key_ledger WHERE runner_id = ?`, f.id).
		Scan(&scope, &hostName, &host); err != nil {
		t.Fatalf("ledger row: %v", err)
	}
	if scope != "dmz" || hostName != "direct" || host != "[10.1.0.5]:2222" {
		t.Fatalf("ledger row = scope %q host name %q host %q", scope, hostName, host)
	}
	var target string
	_ = f.svc.db.QueryRow(`SELECT target FROM change_log WHERE category = 'Host Keys'`).Scan(&target)
	if !strings.Contains(target, "scope dmz") {
		t.Fatalf("change log target %q does not name the scope", target)
	}
}

func TestScopeScanNeedsAnEligibleRunnerAndAReadableScope(t *testing.T) {
	f := newHKFixture(t, "elig")
	scopeID := f.seedScope("tax-hosts")
	// The scope belongs to an agency the runner is not a member of.
	if _, err := f.svc.db.Exec(`INSERT INTO agencies(id, name, created_at) VALUES ('ag-tax', 'tax', ?)`, now()); err != nil {
		t.Fatalf("agency: %v", err)
	}
	if _, err := f.svc.db.Exec(`INSERT INTO scope_agencies(scope_id, agency_id) VALUES (?, 'ag-tax')`, scopeID); err != nil {
		t.Fatalf("scope agency: %v", err)
	}
	if rec := f.do(f.svc.HandleKeyscan, http.MethodPost, "/x", `{"scopeId":"`+scopeID+`"}`); rec.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(rec.Body.String(), "runner_not_eligible") {
		t.Fatalf("an ineligible runner scanning a scope answered %d %s", rec.Code, rec.Body.String())
	}
	// A caller who cannot read the scope is told it does not exist.
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"scopeId":"`+scopeID+`"}`))
	req = req.WithContext(auth.WithIdentity(req.Context(), auth.Identity{Email: "x@example.com", AllowedScopes: []string{"elsewhere"}}))
	req.SetPathValue("id", f.id)
	rec := httptest.NewRecorder()
	f.svc.HandleKeyscan(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("an unreadable scope answered %d; want 404", rec.Code)
	}
	if n := f.count(`SELECT COUNT(*) FROM host_key_scan_targets`); n != 0 {
		t.Fatalf("a refused scope scan recorded %d target(s)", n)
	}
}

func TestKeyscanRefusesATargetThatIsNotOneHost(t *testing.T) {
	f := newHKFixture(t, "tgt")
	for _, bad := range []string{"web01,web02", "*.example.com", "web 01", "!web01", "|1|abc|def"} {
		b, _ := json.Marshal(map[string]any{"hosts": []string{bad}})
		if rec := f.do(f.svc.HandleKeyscan, http.MethodPost, "/x", string(b)); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%q answered %d; want 422", bad, rec.Code)
		}
	}
	if rec := f.do(f.svc.HandleKeyscan, http.MethodPost, "/x", `{"hosts":["a"],"scopeId":"s"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("hosts and scopeId together answered %d; want 400", rec.Code)
	}
}

func TestScopeCoverage(t *testing.T) {
	f := newHKFixture(t, "cov")
	scopeID := f.seedScope("dmz")
	bindScope(t, f.svc, scopeID, f.id, "cov")
	bindScope(t, f.svc, scopeID, "runner-gone", "gone")

	// direct: approved here. fronted: its own key is in the runner's file — but
	// not, yet, the key of the bastion in front of it.
	directKey := testHostKey(t)
	direct := knownHostsEntry{Line: 1, Hosts: "[10.1.0.5]:2222", KeyType: directKey.Type(), Fingerprint: ssh.FingerprintSHA256(directKey)}
	seeded := knownHostsEntry{Line: 2, Hosts: "10.2.0.*", KeyType: "ssh-ed25519", Fingerprint: "SHA256:seeded"}
	if rec, _ := f.provide(false, renderKnownHostsLine("[10.1.0.5]:2222", directKey)); rec.Code != http.StatusOK {
		t.Fatalf("provide: %d %s", rec.Code, rec.Body.String())
	}
	missing := func() int {
		t.Helper()
		n, total, err := hostkeys.Missing(context.Background(), f.svc.db, "dmz", f.id)
		if err != nil || total != 3 {
			t.Fatalf("Missing: %d of %d (%v)", n, total, err)
		}
		return n
	}
	// Approved but not yet sent: on its way, so not counted as missing.
	if n := missing(); n != 2 {
		t.Fatalf("with the key queued, missing = %d; want 2 (the fronted host and the one with no record)", n)
	}
	f.control() // delivered
	// The runner's own report contradicts the ledger: the key is NOT in its file.
	// Coverage must believe the file, or the scope reads as covered while the
	// runner refuses the host.
	f.report(seeded)
	if n := missing(); n != 3 {
		t.Fatalf("with the approved key absent from the reported file, missing = %d; want 3", n)
	}
	// In the file now — but the fronted host still lacks its bastion's key.
	f.report(direct, seeded)
	if n := missing(); n != 2 {
		t.Fatalf("with the bastion's key missing, missing = %d; want 2", n)
	}
	f.report(direct, seeded, knownHostsEntry{Line: 3, Hosts: "bastion-a", KeyType: "ssh-ed25519", Fingerprint: "SHA256:bastion"})

	rec := f.do(f.svc.HandleScopeHostKeyCoverage, http.MethodGet, "/x", "", "scopeId", scopeID)
	if rec.Code != http.StatusOK {
		t.Fatalf("coverage: %d %s", rec.Code, rec.Body.String())
	}
	var cov scopeCoverage
	_ = json.Unmarshal(rec.Body.Bytes(), &cov)
	if len(cov.Hosts) != 3 || len(cov.Runners) != 2 {
		t.Fatalf("coverage = %d hosts × %d runners; want 3 × 2", len(cov.Hosts), len(cov.Runners))
	}
	// The local runner bound to the same scope is in this table like an agent
	// (2.3.0, Phase C): what it trusts is what is in force in its ledger —
	// there is no file to compare with, so approved means trusted.
	serverTrusts(t, f.svc, "10.1.0.5", directKey, "", "root@example.com") // not this host's pattern: it listens on 2222
	serverTrusts(t, f.svc, "[10.1.0.5]:2222", directKey, "", "root@example.com")
	var localID string
	_ = f.svc.db.QueryRow(`SELECT id FROM runners WHERE kind = 'server'`).Scan(&localID)
	if _, err := f.svc.db.Exec(`INSERT INTO scope_runners (scope_id, runner_id, runner_name, bound_by, bound_at) VALUES (?, ?, 'Local runner', 'test', 't')`, scopeID, localID); err != nil {
		t.Fatal(err)
	}
	var withLocal scopeCoverage
	_ = json.Unmarshal(f.do(f.svc.HandleScopeHostKeyCoverage, http.MethodGet, "/x", "", "scopeId", scopeID).Body.Bytes(), &withLocal)
	if len(withLocal.Runners) != 3 {
		t.Fatalf("coverage with the local runner bound lists %d runners, want 3", len(withLocal.Runners))
	}
	for _, r := range withLocal.Runners {
		if r.RunnerID != localID {
			continue
		}
		states := map[string]string{}
		for i, h := range withLocal.Hosts {
			states[h.Host] = r.States[i]
		}
		if states["direct"] != hostkeys.StateApproved || r.Missing != 2 {
			t.Errorf("the local runner's coverage = %v missing %d; want the host it has an approved key for, and 2 missing", states, r.Missing)
		}
	}
	for _, r := range cov.Runners {
		states := map[string]string{}
		for i, h := range cov.Hosts {
			states[h.Host] = r.States[i]
		}
		switch r.RunnerID {
		case f.id:
			if states["direct"] != hostkeys.StateApproved || states["fronted"] != hostkeys.StateInFile || states["norecord"] != "" || r.Missing != 1 {
				t.Errorf("bound runner states = %v missing %d", states, r.Missing)
			}
		case "runner-gone":
			if r.Registered || r.Missing != 3 {
				t.Errorf("a deregistered bound runner should trust nothing here: %+v", r)
			}
		}
	}
	if missing, total, err := hostkeys.Missing(context.Background(), f.svc.db, "dmz", f.id); err != nil || missing != 1 || total != 3 {
		t.Fatalf("Missing = %d of %d (%v); want 1 of 3", missing, total, err)
	}
}

// Carrying is a reviewed copy: the dry run shows what would come over, and only
// the commit writes.
func TestCarryKeysFromAnotherRunner(t *testing.T) {
	f := newHKFixture(t, "new")
	old := &hkFixture{t: t, svc: f.svc, as: f.as, id: "runner-old", tok: "crn_run_old"}
	insertRunner(t, f.svc, old.id, "old", "online", []string{"bash"})
	bindRunnerToken(t, f.svc, old.tok, old.id)
	shared, onlyOld := testHostKey(t), testHostKey(t)
	old.resolve([]string{old.scan("web01", shared), old.scan("web02", onlyOld)}, nil)
	f.resolve([]string{f.scan("web01", shared)}, nil) // the new runner already trusts web01

	post := func(body any) (*httptest.ResponseRecorder, candidatesResponse) {
		b, _ := json.Marshal(body)
		req := operator(http.MethodPost, "/x?from="+old.id, string(b))
		req.SetPathValue("id", f.id)
		rec := httptest.NewRecorder()
		f.svc.HandleCarryHostKeys(rec, req)
		var out candidatesResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec, out
	}
	carry := func(dryRun bool) (*httptest.ResponseRecorder, candidatesResponse) {
		rec, out := post(carryRequest{DryRun: true})
		if dryRun {
			return rec, out
		}
		sel := []keySelection{}
		for _, c := range out.Candidates {
			sel = append(sel, keySelection{Host: c.Host, KeyType: c.KeyType, Fingerprint: c.Fingerprint, Status: c.Status})
		}
		return post(carryRequest{Select: &sel})
	}
	// A commit that names nothing reviewed is refused outright.
	if rec, _ := post(carryRequest{}); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a carry with no selection answered %d; want 422", rec.Code)
	}
	rec, out := carry(true)
	if rec.Code != http.StatusOK || len(out.Candidates) != 1 || out.Candidates[0].Host != "web02" {
		t.Fatalf("dry run: %d %+v; want only web02 (web01 is already trusted)", rec.Code, out)
	}
	if n := f.count(`SELECT COUNT(*) FROM host_key_ledger WHERE runner_id = ? AND source = 'carried'`, f.id); n != 0 {
		t.Fatalf("a dry run carried %d key(s)", n)
	}
	if rec, _ := carry(false); rec.Code != http.StatusOK {
		t.Fatalf("carry: %d %s", rec.Code, rec.Body.String())
	}
	if n := f.count(`SELECT COUNT(*) FROM host_key_ledger WHERE runner_id = ? AND source = 'carried' AND host = 'web02' AND superseded_at IS NULL`, f.id); n != 1 {
		t.Fatalf("carried rows = %d; want 1", n)
	}
	// The source runner's record is untouched.
	if n := f.count(`SELECT COUNT(*) FROM host_key_ledger WHERE runner_id = ? AND superseded_at IS NULL`, old.id); n != 2 {
		t.Fatalf("carrying changed the source runner's keys: %d in force", n)
	}
}

// The record outlives the runner; the report does not.
func TestLedgerOutlivesItsRunner(t *testing.T) {
	f := newHKFixture(t, "del")
	f.resolve([]string{f.scan("web01", testHostKey(t))}, nil)
	f.report(knownHostsEntry{Line: 1, Hosts: "web01", KeyType: "ssh-ed25519", Fingerprint: "SHA256:x"})
	if _, err := f.svc.db.Exec(`DELETE FROM runners WHERE id = ?`, f.id); err != nil {
		t.Fatalf("delete runner: %v", err)
	}
	if n := f.count(`SELECT COUNT(*) FROM host_key_ledger WHERE runner_id = ? AND runner_name = 'del'`, f.id); n != 1 {
		t.Fatalf("ledger rows after deleting the runner = %d; want 1, still named", n)
	}
	if n := f.count(`SELECT COUNT(*) FROM runner_known_hosts WHERE runner_id = ?`, f.id); n != 0 {
		t.Fatalf("the deleted runner's file report was kept (%d rows)", n)
	}
}

// The review screen's ticks decide what a commit writes: an unticked candidate
// is left out, an empty selection writes nothing, and a tick is bound to the
// FINGERPRINT that was on screen — not just to the host it sat beside.
func TestProvideCommitsOnlyTheSelection(t *testing.T) {
	f := newHKFixture(t, "sel")
	keyA, keyB := testHostKey(t), testHostKey(t)
	a, b := renderKnownHostsLine("web01", keyA), renderKnownHostsLine("web02", keyB)
	commit := func(sel ...keySelection) *httptest.ResponseRecorder {
		if sel == nil {
			sel = []keySelection{}
		}
		body, _ := json.Marshal(provideRequest{Lines: []string{a, b}, Select: &sel})
		return f.do(f.svc.HandleProvideHostKeys, http.MethodPost, "/x", string(body))
	}
	result := func(rec *httptest.ResponseRecorder) batchResult {
		t.Helper()
		if rec.Code != http.StatusOK {
			t.Fatalf("provide: %d %s", rec.Code, rec.Body.String())
		}
		var res batchResult
		_ = json.Unmarshal(rec.Body.Bytes(), &res)
		return res
	}
	// No selection at all is not "everything".
	body, _ := json.Marshal(provideRequest{Lines: []string{a, b}})
	if rec := f.do(f.svc.HandleProvideHostKeys, http.MethodPost, "/x", string(body)); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a commit with no selection answered %d; want 422", rec.Code)
	}
	if res := result(commit()); res.Approved != 0 || res.BatchID != "" {
		t.Fatalf("an empty selection wrote %+v", res)
	}
	// The right host with a fingerprint that was never on screen: stale.
	if rec := commit(keySelection{Host: "web02", KeyType: keyB.Type(), Fingerprint: ssh.FingerprintSHA256(keyA), Status: keyStatusNew}); rec.Code != http.StatusConflict {
		t.Fatalf("a selection naming another key answered %d; want 409", rec.Code)
	}
	if n := f.count(`SELECT COUNT(*) FROM host_key_ledger WHERE runner_id = ?`, f.id); n != 0 {
		t.Fatalf("%d ledger row(s) written before any valid commit", n)
	}
	if res := result(commit(keySelection{Host: "web02", KeyType: keyB.Type(), Fingerprint: ssh.FingerprintSHA256(keyB), Status: keyStatusNew})); res.Approved != 1 {
		t.Fatalf("selection of one wrote %+v", res)
	}
	var host string
	_ = f.svc.db.QueryRow(`SELECT host FROM host_key_ledger WHERE runner_id = ?`, f.id).Scan(&host)
	if n := f.count(`SELECT COUNT(*) FROM host_key_ledger WHERE runner_id = ?`, f.id); n != 1 || host != "web02" {
		t.Fatalf("ledger holds %d row(s), host %q; want only web02", n, host)
	}

	// A key reviewed as NEW that would now replace a trusted one is refused:
	// someone approved a different key for web01 while this review was open.
	f.resolve([]string{f.scan("web01", testHostKey(t))}, nil)
	if rec := commit(keySelection{Host: "web01", KeyType: keyA.Type(), Fingerprint: ssh.FingerprintSHA256(keyA), Status: keyStatusNew}); rec.Code != http.StatusConflict {
		t.Fatalf("a new-turned-changed key answered %d; want 409", rec.Code)
	}
	if res := result(commit(keySelection{Host: "web01", KeyType: keyA.Type(), Fingerprint: ssh.FingerprintSHA256(keyA), Status: keyStatusChanged})); res.Approved != 1 {
		t.Fatalf("an acknowledged change wrote %+v", res)
	}
}

// What can make a runner's whole known_hosts unloadable is refused before it is
// stored, from every direction it could arrive.
func TestNothingThatBreaksTheFileIsAccepted(t *testing.T) {
	f := newHKFixture(t, "junk")
	key := testHostKey(t)
	for _, host := range []string{"[]", ":22", "[x", "[", "[a:b", "[x]y", "|1|abc", "|1|", "|1|a|b|c"} {
		_, out := f.provide(true, host+" "+strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))))
		if len(out.Candidates) != 1 || out.Candidates[0].Error == "" || out.Acceptable {
			t.Errorf("pasted host %q was accepted: %+v", host, out)
		}
		b, _ := json.Marshal(map[string]any{"hosts": []string{host}})
		if rec := f.do(f.svc.HandleKeyscan, http.MethodPost, "/x", string(b)); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("scan target %q answered %d; want 422", host, rec.Code)
		}
		if code := uploadKeys(t, f.svc, f.as, f.id, f.tok, scanBody(host, renderKnownHostsLine(host, key))); code != http.StatusOK {
			t.Errorf("upload answered %d", code)
		}
	}
	if n := f.count(`SELECT COUNT(*) FROM pending_host_keys`) + f.count(`SELECT COUNT(*) FROM host_key_ledger`); n != 0 {
		t.Fatalf("%d row(s) stored for hosts no runner could load", n)
	}
	// A comment of several words is not a reason to refuse a line.
	_, out := f.provide(true, renderKnownHostsLine("web01", key)+" added by bob on tuesday")
	if !out.Acceptable || len(out.Candidates) != 1 || out.Candidates[0].Host != "web01" {
		t.Fatalf("a line with a long comment was refused: %+v", out)
	}
}

// "Send again" clears the delivery stamp. A key removed before the runner next
// polls must still be taken out of its file — the stamp is a to-do marker, not
// a record of whether the line ever got there.
func TestRemovalIsSentEvenAfterAResend(t *testing.T) {
	f := newHKFixture(t, "resend")
	key := testHostKey(t)
	f.resolve([]string{f.scan("web01", key)}, nil)
	f.control() // delivered: the line is in the runner's file
	id := fmt.Sprint(f.view().InForce[0].ID)
	if rec := f.do(f.svc.HandleResendHostKey, http.MethodPost, "/x", "", "ledgerId", id); rec.Code != http.StatusAccepted {
		t.Fatalf("resend: %d", rec.Code)
	}
	if rec := f.do(f.svc.HandleRemoveHostKey, http.MethodPost, "/x", "", "ledgerId", id); rec.Code != http.StatusOK {
		t.Fatalf("remove: %d", rec.Code)
	}
	ctl := f.control()
	if got := ctl["untrust-hosts"].Entries; len(got) != 1 || got[0] != scanLine("web01", key) {
		t.Fatalf("untrust-hosts = %v; the removed key is still in the runner's file", got)
	}
	if got := ctl["trust-hosts"].Entries; len(got) != 0 {
		t.Fatalf("a removed key was delivered: %v", got)
	}
}

// The runner's report is believed over the ledger's own stamps.
func TestReportReconcilesTheLedger(t *testing.T) {
	f := newHKFixture(t, "recon")
	key, other := testHostKey(t), testHostKey(t)
	fp := ssh.FingerprintSHA256
	f.resolve([]string{f.scan("web01", key)}, nil)
	f.control()
	here := knownHostsEntry{Line: 1, Hosts: "web01", KeyType: key.Type(), Fingerprint: fp(key)}

	// The same key under ANOTHER host is not this approval: no confirmation.
	f.report(knownHostsEntry{Line: 1, Hosts: "somewhere-else", KeyType: key.Type(), Fingerprint: fp(key)})
	v := f.view()
	if v.InForce[0].ConfirmedAt != nil || *v.InForce[0].PresentInFile || v.KnownHosts.Entries[0].ApprovedHere {
		t.Fatalf("a key under another host name was taken for the approved one: %+v / %+v", v.InForce[0], v.KnownHosts.Entries[0])
	}
	f.report(here)
	if v := f.view(); v.InForce[0].ConfirmedAt == nil || !*v.InForce[0].PresentInFile || !v.KnownHosts.Entries[0].ApprovedHere {
		t.Fatalf("the approved key in the file was not recognised: %+v", v.InForce[0])
	}

	// The file is reset. The key is no longer confirmed, so approving the same
	// key again (a rescan of the scope) sends it again instead of answering
	// "already trusted" while every run fails.
	f.report()
	if v := f.view(); v.InForce[0].ConfirmedAt != nil {
		t.Fatalf("a key missing from the file stayed confirmed")
	}
	rec := f.resolve([]string{f.scan("web01", key)}, nil)
	var res batchResult
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if res.Unchanged != 1 {
		t.Fatalf("re-approval = %+v; want unchanged", res)
	}
	if got := f.control()["trust-hosts"].Entries; len(got) != 1 {
		t.Fatalf("the key was not sent again after the file lost it: %v", got)
	}
	// A truncated report proves no absence.
	f.report(here)
	b, _ := json.Marshal(knownHostsReport{Entries: []knownHostsEntry{}, Truncated: true})
	h := f.as.RequireRunner(http.HandlerFunc(f.svc.HandleUploadKnownHosts))
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(string(b)))
	req.SetPathValue("id", f.id)
	req.Header.Set("Authorization", "Bearer "+f.tok)
	h.ServeHTTP(httptest.NewRecorder(), req)
	if v := f.view(); v.InForce[0].ConfirmedAt == nil {
		t.Fatalf("a truncated report un-confirmed a key")
	}

	// A replaced key whose removal was sent but which the file STILL holds is
	// queued for removal again — once the first attempt is old enough.
	f.report(here)
	changed := f.scan("web01", other)
	if rec := f.resolve([]string{changed}, nil, changed); rec.Code != http.StatusOK {
		t.Fatalf("replace: %d %s", rec.Code, rec.Body.String())
	}
	f.control() // untrust(old) + trust(new) sent
	stillThere := []knownHostsEntry{here, {Line: 2, Hosts: "web01", KeyType: other.Type(), Fingerprint: fp(other)}}
	f.report(stillThere...)
	if got := f.control()["untrust-hosts"].Entries; len(got) != 0 {
		t.Fatalf("a removal sent a moment ago was re-sent at once: %v", got)
	}
	if _, err := f.svc.db.Exec(`UPDATE host_key_ledger SET untrusted_at = '2020-01-01T00:00:00Z' WHERE untrusted_at IS NOT NULL`); err != nil {
		t.Fatal(err)
	}
	f.report(stillThere...)
	if got := f.control()["untrust-hosts"].Entries; len(got) != 1 || got[0] != scanLine("web01", key) {
		t.Fatalf("a retired key still in the file was not queued for removal again: %v", got)
	}
}

// The long-poll asks "is there host-key work for this runner" twice a second
// per connected runner. Both ledger probes must be answered from the partial
// indexes of migration 1200 — a scan of the runner's keys there would be paid
// by every idle runner, forever.
func TestHostKeyWorkProbeUsesThePartialIndexes(t *testing.T) {
	svc := newTestService(t)
	rows, err := svc.db.Query(`EXPLAIN QUERY PLAN `+hostKeyWorkProbe, "r1")
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		plan.WriteString(detail + "\n")
	}
	for _, idx := range []string{"idx_host_key_ledger_to_trust", "idx_host_key_ledger_to_untrust"} {
		if !strings.Contains(plan.String(), idx) {
			t.Errorf("the probe does not use %s:\n%s", idx, plan.String())
		}
	}
}

func TestHostKeyWorkProbe(t *testing.T) {
	f := newHKFixture(t, "probe")
	work := func() bool {
		t.Helper()
		var status string
		var resync int
		var w bool
		if err := f.svc.db.QueryRow(hostKeyWorkProbe, f.id).Scan(&status, &resync, &w); err != nil {
			t.Fatalf("probe: %v", err)
		}
		return w
	}
	if work() {
		t.Fatalf("an idle runner reads as having host-key work")
	}
	id := f.scan("web01", testHostKey(t))
	if work() {
		t.Fatalf("a key awaiting REVIEW is not work for the runner")
	}
	f.resolve([]string{id}, nil)
	if !work() {
		t.Fatalf("an approved, undelivered key is work")
	}
	f.control()
	if work() {
		t.Fatalf("a delivered key in force is not work")
	}
	v := f.view()
	f.do(f.svc.HandleRemoveHostKey, http.MethodPost, "/x", "", "ledgerId", fmt.Sprint(v.InForce[0].ID))
	if !work() {
		t.Fatalf("a removed key still on the runner is work")
	}
	f.control()
	if work() {
		t.Fatalf("work remains after the removal was sent")
	}
	f.do(f.svc.HandleRequestKnownHosts, http.MethodPost, "/x", "")
	if !work() {
		t.Fatalf("a requested report is work")
	}
}

// An approval that lands while the runner is already inside a long-poll is
// delivered on that poll, not after its timeout: an operator is watching.
func TestApprovalIsDeliveredDuringALongPoll(t *testing.T) {
	f := newHKFixture(t, "prompt")
	id := f.scan("web01", testHostKey(t))
	type result struct {
		ops  map[string]runnerproto.PollControl
		took time.Duration
	}
	done := make(chan result, 1)
	go func() {
		start := time.Now()
		pr, _ := pollDecode(t, f.svc, f.as, f.id, f.tok, "settingsVersion=0") // nothing queued: this blocks
		ops := map[string]runnerproto.PollControl{}
		for _, c := range pr.Control {
			ops[c.Op] = c
		}
		done <- result{ops, time.Since(start)}
	}()
	time.Sleep(300 * time.Millisecond)
	if rec := f.resolve([]string{id}, nil); rec.Code != http.StatusOK {
		t.Fatalf("approve: %d", rec.Code)
	}
	select {
	case r := <-done:
		if len(r.ops["trust-hosts"].Entries) != 1 {
			t.Fatalf("the poll returned without the approved key: %+v", r.ops)
		}
		if _, ok := r.ops["known-hosts-report"]; !ok {
			t.Fatalf("no report requested with the delivery: %+v", r.ops)
		}
	case <-time.After(8 * time.Second):
		t.Fatalf("the approval was not delivered during the in-flight poll")
	}
}

// serverTrusts puts a key in force for the local runner, creating its row if
// the test has not: what the server trusts for a host since 2.3.0. scopeID is
// the scope the decision was made for ("" for none) and actor who made it.
func serverTrusts(t *testing.T, svc *Service, pattern string, key ssh.PublicKey, scopeID, actor string) {
	t.Helper()
	var localID string
	_ = svc.db.QueryRow(`SELECT id FROM runners WHERE kind = 'server'`).Scan(&localID)
	if localID == "" {
		localID = "local-runner"
		if _, err := svc.db.Exec(`INSERT INTO runners (id, name, kind, status, registered_at, created_at)
		                          VALUES (?, 'Local runner', 'server', 'online', ?, ?)`, localID, now(), now()); err != nil {
			t.Fatalf("the local runner's row: %v", err)
		}
	}
	if _, err := svc.db.Exec(`
		INSERT INTO host_key_ledger (batch_id, runner_id, runner_name, scope_id, host, key_type, fingerprint, known_hosts_line,
		                             decision, source, actor, decided_at, delivered_at, confirmed_at)
		VALUES ('test', ?, 'Local runner', NULLIF(?, ''), ?, ?, ?, ?, 'approved', 'pasted', ?, ?, ?, ?)`,
		localID, scopeID, pattern, key.Type(), ssh.FingerprintSHA256(key), renderKnownHostsLine(pattern, key),
		actor, now(), now(), now()); err != nil {
		t.Fatalf("server trust for %s: %v", pattern, err)
	}
}

// The server's key for a host is a second opinion on the review screen: it can
// reassure a reviewer ("matches the key the server trusts") and it can mark a
// key "changed", which a guest may not approve. So it must be an opinion the
// reviewing runner's own side could have formed (LR-69). The local runner
// serves several agencies, an administrator of any of them may approve a first
// key on it for a host of their own scope (LR-63), and two agencies can list
// the same address — so a decision made for ANOTHER agency's scope, or carried
// by the upgrade from a record another agency had written, is not shown.
func TestServerPinIsADecisionTheReviewersSideCouldHaveMade(t *testing.T) {
	f := newHKFixture(t, "pinowner")
	ctx := context.Background()
	exec := func(q string, a ...any) {
		t.Helper()
		if _, err := f.svc.db.Exec(q, a...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	realKey, planted := testHostKey(t), testHostKey(t)
	fp := func(k ssh.PublicKey) string { return ssh.FingerprintSHA256(k) }
	const pattern = "10.0.0.9"

	exec(`INSERT INTO agencies (id, name, created_at) VALUES ('ag-fin', 'Finance', ?), ('ag-tax', 'Tax', ?)`, now(), now())
	for _, sc := range [][2]string{{"sc-fin", "ag-fin"}, {"sc-tax", "ag-tax"}} {
		exec(`INSERT INTO scopes (id, name, source, created_at) VALUES (?, ?, 'cronomicon', ?)`, sc[0], sc[0], now())
		exec(`DELETE FROM scope_agencies WHERE scope_id = ?`, sc[0])
		exec(`INSERT INTO scope_agencies (scope_id, agency_id) VALUES (?, ?)`, sc[0], sc[1])
	}
	// The runner under review is Finance's.
	exec(`DELETE FROM runner_agencies WHERE runner_id = ?`, f.id)
	exec(`INSERT INTO runner_agencies (runner_id, agency_id) VALUES (?, 'ag-fin')`, f.id)
	pin := func() string { return serverPin(ctx, f.svc.db, f.id, pattern, realKey.Type()) }
	reset := func() { exec(`DELETE FROM host_key_ledger`) }

	// A key approved on the local runner for TAX's scope: not Finance's opinion.
	serverTrusts(t, f.svc, pattern, planted, "sc-tax", "tax-admin@example.com")
	if got := pin(); got != "" {
		t.Errorf("a Finance runner was shown a key approved for Tax's scope as the server's: %s", got)
	}
	if c := classifyKey(ctx, f.svc.db, f.id, pattern, "db01", "sc-fin", planted.Type(), fp(planted)); c.Status == keyStatusMatch || c.MatchedServerPin {
		t.Errorf("a key trusted only for ANOTHER agency's scope classifies %q (matched=%v)", c.Status, c.MatchedServerPin)
	}
	// One approved for Finance's own scope is.
	reset()
	serverTrusts(t, f.svc, pattern, realKey, "sc-fin", "fin-admin@example.com")
	if got := pin(); got != fp(realKey) {
		t.Errorf("the server's key for a host of Finance's scope = %q, want %s", got, fp(realKey))
	}
	if c := classifyKey(ctx, f.svc.db, f.id, pattern, "db01", "sc-fin", realKey.Type(), fp(realKey)); c.Status != keyStatusMatch {
		t.Errorf("the key the server trusts classifies %q, want a match", c.Status)
	}
	if c := classifyKey(ctx, f.svc.db, f.id, pattern, "db01", "sc-fin", planted.Type(), fp(planted)); c.Status != keyStatusChanged || c.PreviousSource != "server" {
		t.Errorf("a key that differs from the server's classifies %q (%s), want changed against the server's", c.Status, c.PreviousSource)
	}
	// One approved for no scope at all (typed or pasted: the local runner's
	// owner, a global administrator) answers for everyone.
	reset()
	serverTrusts(t, f.svc, pattern, realKey, "", "root@example.com")
	if got := pin(); got != fp(realKey) {
		t.Errorf("a global administrator's approval was not read as the server's key: %q", got)
	}
	// A key the upgrade carried from a record TAX had written by hand is Tax's
	// opinion; from one Finance had written, or Global's, it counts.
	for actor, want := range map[string]string{
		carriedByUpgradeFor("ag-tax"): "",
		carriedByUpgradeFor("ag-fin"): fp(realKey),
		carriedByUpgradeFor("global"): fp(realKey),
	} {
		reset()
		serverTrusts(t, f.svc, pattern, realKey, "", actor)
		if got := pin(); got != want {
			t.Errorf("a key carried as %q is read as the server's key %q, want %q", actor, got, want)
		}
	}
}

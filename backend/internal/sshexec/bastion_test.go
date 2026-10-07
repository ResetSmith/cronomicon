package sshexec

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func insertBastion(t *testing.T, svc *Service, id, name, authKeyEnvVar string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := svc.db.Exec(`
		INSERT INTO bastions(id, name, hostname, address, port, username, auth_key_env_var, created_at)
		VALUES (?, ?, ?, '127.0.0.1', 2222, 'jump', ?, ?)`,
		id, name, name, authKeyEnvVar, now); err != nil {
		t.Fatalf("insert bastion: %v", err)
	}
}

// TestBastionAddr_ReturnsAuthKeyEnvVar (PP-H4 H4-1): bastionAddr surfaces the
// bastion's own auth_key_env_var (and empty string when NULL).
func TestBastionAddr_ReturnsAuthKeyEnvVar(t *testing.T) {
	svc, _ := newReaperService(t)

	insertBastion(t, svc, "b1", "jump-a", "JUMP_KEY")
	b, err := svc.bastionAddr(context.Background(), "jump-a", nil)
	if err != nil {
		t.Fatalf("bastionAddr: %v", err)
	}
	if b.AuthKeyEnvVar != "JUMP_KEY" {
		t.Errorf("authKeyEnvVar = %q, want JUMP_KEY", b.AuthKeyEnvVar)
	}

	insertBastion(t, svc, "b2", "jump-b", "") // NULL/empty
	b2, err := svc.bastionAddr(context.Background(), "jump-b", nil)
	if err != nil {
		t.Fatalf("bastionAddr: %v", err)
	}
	if b2.AuthKeyEnvVar != "" {
		t.Errorf("authKeyEnvVar = %q, want empty for unset key", b2.AuthKeyEnvVar)
	}
}

// TestDial_BastionKeyLoadError (PP-H4 H4-2): when the bastion has its own
// auth_key_env_var but no key material is resolvable, dial fails with a wrapped
// "dial bastion" error (→ cred_error via classifyDialErr) BEFORE any network —
// proving dial now loads the BASTION's key, not the target's.
func TestDial_BastionKeyLoadError(t *testing.T) {
	svc, _ := newReaperService(t)
	insertBastion(t, svc, "b1", "jump", "MISSING_BASTION_KEY")

	// A valid target signer that must NOT be used for the bastion hop.
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)

	tgt := target{Name: "web01", Address: "127.0.0.1", Port: 2222, User: "deploy", Via: "jump"}
	_, _, err := svc.dial(context.Background(), tgt, signer)
	if err == nil {
		t.Fatal("dial succeeded; want a bastion key-load error")
	}
	if !strings.Contains(err.Error(), "dial bastion") {
		t.Errorf("error = %q, want it to wrap 'dial bastion ...' (bastion key load failed)", err.Error())
	}
	// And it must classify as a credential error (not a generic conn_error), so
	// the operator gets the actionable key hint (PP-H4 review).
	if status, _ := classifyDialErr(err); status != StatusCredError {
		t.Errorf("classifyDialErr = %q, want %q (bastion key-load is a credential failure)", status, StatusCredError)
	}
}

// TestBastionHostKeyCallback (SU-4): the bastion callback strict-compares a pinned
// key (mismatch → MITM error), refuses an unparseable stored key, and TOFU-captures
// the first-seen key into the bastion row when unpinned.
func TestBastionHostKeyCallback(t *testing.T) {
	svc, _ := newReaperService(t)
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)
	pub := signer.PublicKey()
	authLine := string(ssh.MarshalAuthorizedKey(pub))
	_, priv2, _ := ed25519.GenerateKey(rand.Reader)
	signer2, _ := ssh.NewSignerFromKey(priv2)

	// Strict: pinned key matches → nil; a different key → MITM error.
	strict := svc.bastionHostKeyCallback("b1", "jump", authLine)
	if err := strict("", nil, pub); err != nil {
		t.Errorf("matching pinned bastion key rejected: %v", err)
	}
	if err := strict("", nil, signer2.PublicKey()); err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Errorf("mismatched bastion key must be rejected as MITM, got %v", err)
	}

	// Unparseable stored key → refuse (fail-closed, no accept-any fallback).
	bad := svc.bastionHostKeyCallback("b1", "jump", "not-a-valid-key")
	if err := bad("", nil, pub); err == nil {
		t.Error("unparseable stored bastion key must refuse")
	}

	// TOFU: empty stored key → capture the first-seen key into the bastion row.
	insertBastion(t, svc, "b-tofu", "jump-tofu", "")
	tofu := svc.bastionHostKeyCallback("b-tofu", "jump-tofu", "")
	if err := tofu("", nil, pub); err != nil {
		t.Errorf("TOFU capture errored: %v", err)
	}
	var stored string
	_ = svc.db.QueryRow(`SELECT COALESCE(host_key,'') FROM bastions WHERE id='b-tofu'`).Scan(&stored)
	if strings.TrimSpace(stored) != strings.TrimSpace(authLine) {
		t.Errorf("TOFU did not persist the bastion key: got %q want %q", stored, authLine)
	}
	// After capture, a subsequent changed key strict-fails.
	if err := svc.bastionHostKeyCallback("b-tofu", "jump-tofu", stored)("", nil, signer2.PublicKey()); err == nil {
		t.Error("after TOFU, a changed bastion key must be rejected")
	}
}

// LR-70 — a host routes only through a bastion that its own agency owns, or
// Global. A bastion of another agency does not exist for the lookup, so a host
// record cannot be sent through another agency's jump host by naming it.
func TestBastionAddr_OnlyTheRecordsAgencysBastionsAndGlobals(t *testing.T) {
	svc, _, _ := guardFixture(t)
	ctx := context.Background()
	now := time.Now().UTC().Format(time.RFC3339)
	for _, b := range [][3]string{
		{"b-fin", "jump-fin", "ag:fin"}, {"b-tax", "jump-tax", "ag:tax"}, {"b-glob", "jump-shared", "global"},
		// One name, an agency's and Global's: the agency's own is the one meant.
		{"b-fin2", "edge", "ag:fin"}, {"b-glob2", "edge", "global"},
	} {
		if _, err := svc.db.Exec(`
			INSERT INTO bastions(id, name, hostname, address, port, username, created_at, owner_agency)
			VALUES (?, ?, ?, '127.0.0.1', 2222, 'jump', ?, ?)`, b[0], b[1], b[1], now, b[2]); err != nil {
			t.Fatalf("insert bastion: %v", err)
		}
	}
	for _, c := range []struct {
		ref    string
		owners []string
		want   string // bastion id, "" = not found
	}{
		{"jump-fin", []string{"ag:fin"}, "b-fin"},
		{"jump-fin", []string{"ag:tax"}, ""},
		{"jump-fin", nil, ""},
		{"b-fin", []string{"ag:tax"}, ""}, // by id is no way round it
		{"jump-shared", []string{"ag:fin"}, "b-glob"},
		{"jump-shared", []string{"ag:tax"}, "b-glob"},
		{"jump-shared", nil, "b-glob"},
		{"edge", []string{"ag:fin"}, "b-fin2"},
		{"edge", []string{"ag:tax"}, "b-glob2"},
		{"edge", []string{"global"}, "b-glob2"},
	} {
		b, err := svc.bastionAddr(ctx, c.ref, c.owners)
		got := ""
		if err == nil && b != nil {
			got = b.ID
		}
		if got != c.want {
			t.Errorf("bastion %q for a record of %v = %q (err %v), want %q", c.ref, c.owners, got, err, c.want)
		}
	}
	// The bastion says whose it is, so its own key can be judged against that.
	if b, err := svc.bastionAddr(ctx, "jump-fin", []string{"ag:fin"}); err != nil || len(b.Owners) != 1 || b.Owners[0] != "ag:fin" {
		t.Errorf("FIN's bastion answers to %+v (%v), want [ag:fin]", b, err)
	}
}

// LR-72 at connect — a bastion authenticates with a key its owner may use: the
// owner's own or Global's. A bastion was unowned until 2.3.0 and its key was
// loaded without a question.
func TestDial_ABastionsKeyMustBeItsOwners(t *testing.T) {
	svc, ids, _ := guardFixture(t)
	ctx := context.Background()
	now := time.Now().UTC().Format(time.RFC3339)
	insert := func(id, name, owner, credID string) {
		t.Helper()
		if _, err := svc.db.Exec(`
			INSERT INTO bastions(id, name, hostname, address, port, username, auth_credential_id, created_at, owner_agency)
			VALUES (?, ?, ?, '127.0.0.1', 1, 'jump', ?, ?, ?)`, id, name, name, credID, now, owner); err != nil {
			t.Fatalf("insert bastion: %v", err)
		}
	}
	insert("b-bad", "fin-with-tax-key", "ag:fin", ids["tax"])
	insert("b-own", "fin-with-own-key", "ag:fin", ids["fin"])
	insert("b-shared", "fin-with-global-key", "ag:fin", ids["shared"])
	insert("b-glob-bad", "global-with-fin-key", "global", ids["fin"])

	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)
	dialVia := func(via string, owners []string) error {
		dctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		_, _, err := svc.dial(dctx, target{Name: "t", Address: "127.0.0.1", Port: 1, Via: via, Owners: owners}, signer)
		return err
	}
	for _, c := range []struct {
		via     string
		owners  []string
		refused bool
	}{
		{"fin-with-tax-key", []string{"ag:fin"}, true},
		{"global-with-fin-key", []string{"ag:fin"}, true}, // a Global bastion names a Global key
		{"fin-with-own-key", []string{"ag:fin"}, false},
		{"fin-with-global-key", []string{"ag:fin"}, false},
	} {
		err := dialVia(c.via, c.owners)
		gotRefused := errors.Is(err, errKeyNotUsable)
		if gotRefused != c.refused {
			t.Errorf("dial via %s: key refused = %v (err %v), want %v", c.via, gotRefused, err, c.refused)
		}
		if c.refused {
			if status, msg := classifyDialErr(err); status != StatusCredError || !strings.Contains(msg, "not one its agency may use") {
				t.Errorf("dial via %s classified %q / %q, want a credential error that says why", c.via, status, msg)
			}
		}
	}
}

// A bastion from before 2.3.0 may name its key by NAME, and that name was
// looked up across every agency and scope. It is loaded for the bastion's owner
// now, so one that names a department's or a scoped row fails every run routed
// through it. The inbox says so, by the same rule the connect applies.
func TestBastionKeyNameFindingsReportsWhatTheConnectWouldRefuse(t *testing.T) {
	svc, _, _ := guardFixture(t)
	ctx := context.Background()
	now := time.Now().UTC().Format(time.RFC3339)
	exec := func(q string, a ...any) {
		t.Helper()
		if _, err := svc.db.Exec(q, a...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	// Secrets a bastion might name: Global's with no scope, Global's in a scope,
	// and FIN's.
	exec(`INSERT INTO secrets (id, key, scope, source, created_at) VALUES ('s-glob', 'JUMP_KEY', NULL, 'stored', ?), ('s-scoped', 'SCOPED_KEY', 'fin-prod', 'stored', ?)`, now, now)
	exec(`INSERT INTO secrets (id, key, scope, source, created_at, owner_agency) VALUES ('s-fin', 'FIN_JUMP_KEY', NULL, 'stored', ?, 'ag:fin')`, now)
	exec(`INSERT INTO secret_agencies (secret_id, agency_id) VALUES ('s-fin', 'ag:fin')`)
	bastion := func(id, owner, keyName string) {
		exec(`INSERT INTO bastions(id, name, hostname, address, port, username, auth_key_env_var, created_at, owner_agency)
		      VALUES (?, ?, ?, '127.0.0.1', 1, 'jump', ?, ?, ?)`, id, id, id, keyName, now, owner)
	}
	bastion("b-ok-global", "global", "JUMP_KEY")
	bastion("b-ok-fin-own", "ag:fin", "FIN_JUMP_KEY")
	bastion("b-ok-fin-global", "ag:fin", "JUMP_KEY")
	bastion("b-ok-label", "ag:fin", "CRONOMICON_KEY_finkey")
	bastion("b-bad-dept", "global", "FIN_JUMP_KEY")           // a Global bastion naming FIN's secret
	bastion("b-bad-scoped", "global", "SCOPED_KEY")           // ...or a scoped one
	bastion("b-bad-label", "ag:fin", "CRONOMICON_KEY_taxkey") // FIN's bastion naming TAX's key
	bastion("b-bad-missing", "ag:tax", "NO_SUCH_KEY")
	exec(`INSERT INTO bastions(id, name, hostname, address, port, created_at) VALUES ('b-keyless', 'b-keyless', 'b-keyless', '127.0.0.1', 1, ?)`, now)

	found, err := BastionKeyNameFindings(ctx, svc.db)
	if err != nil {
		t.Fatalf("BastionKeyNameFindings: %v", err)
	}
	got := map[string]string{}
	for _, f := range found {
		got[f.Subject] = f.AgencyID
	}
	want := map[string]string{
		"bastion:b-bad-dept": "global", "bastion:b-bad-scoped": "global",
		"bastion:b-bad-label": "ag:fin", "bastion:b-bad-missing": "ag:tax",
	}
	if len(got) != len(want) {
		t.Errorf("findings = %v, want exactly %v", got, want)
	}
	for subject, agency := range want {
		if got[subject] != agency {
			t.Errorf("%s is filed under %q, want %q (its owner)", subject, got[subject], agency)
		}
	}

	// The finding and the connect agree, bastion by bastion.
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)
	for _, id := range []string{"b-ok-fin-own", "b-ok-label", "b-bad-dept", "b-bad-scoped", "b-bad-label", "b-bad-missing"} {
		var owner string
		_ = svc.db.QueryRow(`SELECT owner_agency FROM bastions WHERE id = ?`, id).Scan(&owner)
		dctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		_, _, derr := svc.dial(dctx, target{Name: "t", Address: "127.0.0.1", Port: 1, Via: id, Owners: []string{owner}}, signer)
		cancel()
		_, reported := want["bastion:"+id]
		if refused := errors.Is(derr, errKeyNotUsable); refused != reported {
			t.Errorf("bastion %s: the connect refused its key = %v (err %v), the check reported it = %v", id, refused, derr, reported)
		}
	}
}

package settings

import (
	"errors"
	"net/url"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/secrets"
	"github.com/ResetSmith/cronomicon/internal/secretstore/aws"
	"github.com/ResetSmith/cronomicon/internal/secretstore/aws/fakeaws"
)

// connectAWS gives the agency ag-aws (made here) a connection to a fake AWS
// Secrets Manager holding cronomicon/db, and a secret of its own naming it.
func (f *agencyVaultFix) connectAWS(t *testing.T) (*fakeaws.Server, string) {
	t.Helper()
	fake := fakeaws.New("AKIDTEST", map[string]string{"cronomicon/db": `{"password":"from-aws"}`})
	t.Cleanup(fake.Close)
	if _, err := f.pool.ExecContext(f.ctx, `INSERT INTO agencies(id, name, created_at) VALUES('ag-aws', 'AWSAG', 't')`); err != nil {
		t.Fatal(err)
	}
	ou, _ := url.Parse(f.own.URL)
	fu, _ := url.Parse(fake.URL)
	if err := writeVaultAllowedHosts(f.ctx, f.pool, []string{ou.Host, fu.Host}, "t", "t"); err != nil {
		t.Fatal(err)
	}
	rt := AgencyVaultRuntime{
		Agency: "ag-aws", Provider: aws.ID,
		Settings:   map[string]string{aws.SettingRegion: "eu-west-1", aws.SettingEndpoint: fake.URL, aws.SettingCAPEM: fake.CAPEM()},
		Credential: map[string]string{aws.CredAccessKeyID: "AKIDTEST", aws.CredSecretAccessKey: "s"},
	}
	if _, err := PutAgencyVault(f.ctx, f.pool, f.cfg, "ag-aws", rt, "t"); err != nil {
		t.Fatal(err)
	}
	sec, err := f.sec.Create(f.ctx, secrets.CreateInput{Key: "AWS_DB", Source: "vault", VaultPath: "cronomicon/db#password", OwnerAgency: "ag-aws"}, "t")
	if err != nil {
		t.Fatal(err)
	}
	return fake, sec.ID
}

// SM3: an agency's AWS connection is sealed, read back and used for its rows.
func TestSM3_AnAgencyReadsItsAWSStore(t *testing.T) {
	f := newAgencyVaultFix(t)
	_, id := f.connectAWS(t)
	if v, err := f.reveal(t, id); err != nil || v != "from-aws" {
		t.Fatalf("AWSAG's secret = %q, %v; want from-aws", v, err)
	}
	// FIN, with no connection, still reads the installation's Vault.
	if v, err := f.reveal(t, f.finSecret); err != nil || v != "installation" {
		t.Fatalf("FIN's secret = %q, %v; want the installation's", v, err)
	}
	var cred string
	_ = f.pool.QueryRowContext(f.ctx, `SELECT credential_enc FROM agency_vaults WHERE agency_id = 'ag-aws'`).Scan(&cred)
	open, err := openStoreCredential(f.cfg, "ag-aws", cred)
	if err != nil || open[aws.CredAccessKeyID] != "AKIDTEST" {
		t.Fatalf("stored credential = %v, %v", open, err)
	}
	vals, err := secrets.RedactionValues(f.ctx, f.pool, f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	masked := map[string]bool{}
	for _, v := range vals {
		masked[v] = true
	}
	if !masked["AKIDTEST"] || !masked["s"] {
		t.Error("the AWS credential's values are not in the redaction dictionary one by one")
	}
}

// SM-6: a refused credential raises the agency's notice though the endpoint
// answers its handshake, and a read that works clears it.
func TestSM3_ARefusedCredentialRaisesTheNotice(t *testing.T) {
	f := newAgencyVaultFix(t)
	fake, id := f.connectAWS(t)
	open := func() int {
		var n int
		if err := f.pool.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM notices WHERE kind = ? AND agency_id = 'ag-aws' AND resolved_at IS NULL`,
			KindAgencyVaultUnreachable).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	check := func() {
		t.Helper()
		if err := CheckAgencyVaults(f.ctx, f.pool, f.cfg); err != nil {
			t.Fatal(err)
		}
	}
	check()
	if n := open(); n != 0 {
		var d string
		_ = f.pool.QueryRowContext(f.ctx, `SELECT detail FROM notices WHERE kind = ? AND agency_id = 'ag-aws'`, KindAgencyVaultUnreachable).Scan(&d)
		t.Fatalf("open notices while AWS answers = %d: %s", n, d)
	}
	fake.SetKeyID("SOMEONE-ELSE")
	_, err := f.reveal(t, id)
	if !errors.Is(err, secrets.ErrVaultUnavailable) || !errors.Is(err, aws.ErrCredential) {
		t.Fatalf("a read with a refused credential = %v; want ErrVaultUnavailable and ErrCredential", err)
	}
	check()
	if n := open(); n != 1 {
		t.Fatalf("open notices after a refused credential = %d, want 1", n)
	}
	check() // the handshake still works; the read's failure stands
	if n := open(); n != 1 {
		t.Fatalf("a passing handshake cleared the read's notice")
	}
	fake.SetKeyID("AKIDTEST")
	if _, err := f.reveal(t, id); err != nil {
		t.Fatal(err)
	}
	check()
	if n := open(); n != 0 {
		t.Fatalf("open notices after a read that works = %d, want 0", n)
	}
}

// SM-9: an external row does not move to an agency whose store is another
// provider; a stored one does.
func TestSM3_AMoveAcrossProvidersIsRefused(t *testing.T) {
	f := newAgencyVaultFix(t)
	f.connectAWS(t)
	err := SetAgencyMembership(f.ctx, f.pool, MemberSecret, []AgencyMembership{{ID: f.finSecret, AgencyIDs: []string{"ag-aws"}}}, "t",
		MoveOptions{ConfirmVaultChange: true})
	if !errors.Is(err, ErrProviderMismatch) {
		t.Fatalf("moving FIN's Vault-backed secret to AWSAG = %v, want ErrProviderMismatch", err)
	}
	stored, err := f.sec.Create(f.ctx, secrets.CreateInput{Key: "PLAIN", Source: "stored", Value: "v", OwnerAgency: "ag-fin"}, "t")
	if err != nil {
		t.Fatal(err)
	}
	if err := SetAgencyMembership(f.ctx, f.pool, MemberSecret, []AgencyMembership{{ID: stored.ID, AgencyIDs: []string{"ag-aws"}}}, "t"); err != nil {
		t.Fatalf("moving a stored secret to AWSAG = %v, want it moved", err)
	}
}

// The refusal names the kind of store once, and says why without the
// sentinels' own prefixes (found on the screenshots, 2026-11-06).
func TestSM5_ARefusalReadsAsOneSentence(t *testing.T) {
	f := newAgencyVaultFix(t)
	f.connectAWS(t)
	v, err := JudgeStoreRef(f.ctx, f.pool, "ag-aws", "cronomicon//db", "", "secret")
	if err != nil {
		t.Fatal(err)
	}
	want := `Not a valid AWS Secrets Manager reference: "cronomicon//db" has an empty segment (a slash at either end, or a doubled slash)`
	if v.OK || v.Code != "validation_failed" || v.Reason != want {
		t.Fatalf("verdict = %+v\nwant reason %s", v, want)
	}
}

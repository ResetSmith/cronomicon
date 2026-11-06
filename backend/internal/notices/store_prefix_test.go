package notices_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/notices"
)

// 2.5.5 SM-1/SM3: an agency's external references are judged by its own
// store's provider. With AWS Secrets Manager, "cronomicon/tax-audit/db" is
// outside the prefix "cronomicon/tax" (whole segments, as in Vault), an ARN
// is judged by its name, and the notice quotes the secret id.
func TestSM3_ThePrefixNoticeUsesTheOwnersProvider(t *testing.T) {
	pool := open(t)
	mustExec(t, pool, `INSERT INTO agencies (id, name, created_at) VALUES ('ag-tax', 'Tax', 't')`)
	mustExec(t, pool, `INSERT INTO agency_vaults (agency_id, provider, addr, role_id_enc, secret_id_enc, credential_enc, settings_json,
	                   created_by, created_at, last_modified_by, last_modified_at)
	                   VALUES ('ag-tax', 'aws', '', '', '', 'x', '{"region":"eu-west-1"}', 't', 't', 't', 't')`)
	mustExec(t, pool, `INSERT INTO agency_vault_prefixes (agency_id, prefix, created_at) VALUES ('ag-tax', 'cronomicon/tax', 't')`)
	for _, s := range [][2]string{
		{"s-in", "cronomicon/tax/db#password"},
		{"s-arn", "arn:aws:secretsmanager:eu-west-1:123456789012:secret:cronomicon/tax/api-AbCdEf#token"},
		{"s-out", "cronomicon/tax-audit/db#password"},
	} {
		mustExec(t, pool, `INSERT INTO secrets (id, key, source, vault_ref, owner_agency, created_by, created_at, last_modified_by, last_modified_at)
		                   VALUES (?, ?, 'vault', ?, 'ag-tax', 't', 't', 't', 't')`, s[0], strings.ToUpper(s[0]), s[1])
	}
	if err := notices.RunChecks(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	got := openOf(t, pool, notices.KindVaultPathOutsidePrefix)
	if len(got) != 1 {
		t.Fatalf("vault_path_outside_prefix = %v, want the tax-audit secret alone", got)
	}
	n, ok := got["secret:s-out"]
	if !ok || !strings.Contains(n.Detail, "cronomicon/tax-audit/db") || strings.Contains(n.Detail, "#password") {
		t.Errorf("the tax-audit secret: %+v", n)
	}
}

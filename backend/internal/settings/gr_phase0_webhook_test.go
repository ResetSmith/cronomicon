package settings

// Phase R0 of 2.4.0 read these two about the webhook secret and did not run
// them. They are reproduced here before Phase R2 changes anything: each test
// passes on the code as it is. No production code changes.

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/secrets"
)

func grWebhookCfg() *config.Config {
	kek := make([]byte, 32)
	_, _ = rand.Read(kek)
	return &config.Config{SecretKEKEnv: base64.StdEncoding.EncodeToString(kek), SecretKEKVersion: 1}
}

// Present defect 16. An installation configured by environment alone has no
// row for its connection, and the webhook policy reads "no row" as enabled.
// The first rotation of the webhook secret writes the row, naming only the
// secret columns: the webhook flags take their column default, 0. From that
// moment every delivery is refused as "webhook disabled", by a control nobody
// touched.
func TestGR0_TheFirstRotationTurnsTheWebhookOff(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	cfg := grWebhookCfg()

	if p := GetWebhookPolicy(ctx, pool); !p.Enabled || !p.Accepts("Push Hook") {
		t.Fatalf("with no connection saved the webhook is expected to be on, got %+v", p)
	}
	if _, _, _, err := RotateWebhookSecret(ctx, pool, cfg, false, 10, "tester"); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	switch p := GetWebhookPolicy(ctx, pool); {
	case !p.Enabled && !p.Accepts("Push Hook"):
		// Today: rotating the secret switched the webhook off.
	case p.Enabled && p.Accepts("Push Hook"):
		t.Errorf("the webhook is still on after the first rotation: this is fixed, and the test is to be inverted (Phase R2)")
	default:
		t.Errorf("after the first rotation the policy is %+v", p)
	}
}

// Present defect 18. A rotation keeps the previous webhook secret for an
// overlap, during which it is still accepted. It is encrypted and stored, and
// it is in neither the redaction dictionary nor `rewrap-secrets`: both read
// one list of encrypted settings columns, and the previous secret's column is
// not on it. So a secret that still opens the webhook is printed in clear
// wherever it is echoed, and a KEK rotation leaves it under the old key.
func TestGR0_ThePreviousWebhookSecretIsNotMasked(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	cfg := grWebhookCfg()

	first, _, _, err := RotateWebhookSecret(ctx, pool, cfg, false, 10, "tester")
	if err != nil {
		t.Fatalf("first rotation: %v", err)
	}
	second, _, _, err := RotateWebhookSecret(ctx, pool, cfg, false, 10, "tester")
	if err != nil {
		t.Fatalf("second rotation: %v", err)
	}
	values, _, err := secrets.RedactionReport(ctx, pool, cfg)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(values, "\n")
	if !strings.Contains(joined, second) {
		t.Fatalf("the current webhook secret is not in the redaction dictionary")
	}
	if strings.Contains(joined, first) {
		t.Errorf("the previous webhook secret is in the redaction dictionary: this is fixed, and the test is to be inverted (Phase R2)")
	}
	for _, sc := range secrets.EncryptedSettingsColumns {
		if strings.Contains(sc.Column, "webhook_secret_prev") {
			t.Errorf("the list of encrypted settings columns names %s.%s: this is fixed, and the test is to be inverted (Phase R2)", sc.Table, sc.Column)
		}
	}
}

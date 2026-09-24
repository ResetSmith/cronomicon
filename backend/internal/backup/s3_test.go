package backup

import (
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/config"
)

// TestResolveS3Creds_Static verifies that when both the access key and secret
// key are configured, resolveS3Creds picks static V4 credentials. (V1.1-11)
func TestResolveS3Creds_Static(t *testing.T) {
	cfg := &config.Config{
		BackupS3AccessKey: "AKIAEXAMPLE",
		BackupS3SecretKey: "secretexample",
	}

	creds, mode := resolveS3Creds(cfg)
	if creds == nil {
		t.Fatal("expected non-nil credentials")
	}
	if mode != "static" {
		t.Fatalf("expected mode %q, got %q", "static", mode)
	}
}

// TestResolveS3Creds_AmbientWhenMissing verifies that when either key is
// missing, resolveS3Creds falls back to the ambient AWS credential chain. The
// test stays offline: it only asserts the selection branch and that the
// credentials object is non-nil — it never calls AWS/IMDS. (V1.1-11)
func TestResolveS3Creds_AmbientWhenMissing(t *testing.T) {
	cases := []struct {
		name      string
		accessKey string
		secretKey string
	}{
		{name: "both empty", accessKey: "", secretKey: ""},
		{name: "only access key", accessKey: "AKIAEXAMPLE", secretKey: ""},
		{name: "only secret key", accessKey: "", secretKey: "secretexample"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{
				BackupS3AccessKey: tc.accessKey,
				BackupS3SecretKey: tc.secretKey,
			}

			creds, mode := resolveS3Creds(cfg)
			if creds == nil {
				t.Fatal("expected non-nil credentials")
			}
			if !strings.Contains(mode, "ambient") || !strings.Contains(mode, "chain") {
				t.Fatalf("expected ambient chain mode, got %q", mode)
			}
		})
	}
}

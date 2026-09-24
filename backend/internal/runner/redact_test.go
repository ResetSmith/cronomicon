package runner

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/redactdict"
)

func TestRedactSkip(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"short literal true", "true", true},                // 4 chars, also common
		{"short numeric port", "80", true},                  // 2 chars
		{"allow-listed production", "production", true},     // long but common
		{"allow-listed false", "false", true},               // 5 chars but common
		{"real secret", "s3cr3t-token-9f2a", false},         // long, not common
		{"case-insensitive Production", "Production", true}, // common, mixed case
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := redactSkip(tt.in); got != tt.want {
				t.Errorf("redactSkip(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestRedactMasksRealSecrets(t *testing.T) {
	const secret = "s3cr3t-token-9f2a"
	r := &Redactor{dict: redactdict.FromValues([]string{secret})}

	t.Run("standalone occurrence", func(t *testing.T) {
		out := string(r.Redact([]byte("line with " + secret + " inside")))
		if !strings.Contains(out, redactMask) {
			t.Errorf("expected output to contain %q, got %q", redactMask, out)
		}
		if strings.Contains(out, secret) {
			t.Errorf("expected secret %q to be masked, got %q", secret, out)
		}
	})

	t.Run("embedded occurrence", func(t *testing.T) {
		out := string(r.Redact([]byte("x-" + secret + "-y")))
		if !strings.Contains(out, redactMask) {
			t.Errorf("expected output to contain %q, got %q", redactMask, out)
		}
		if strings.Contains(out, secret) {
			t.Errorf("expected embedded secret %q to be masked, got %q", secret, out)
		}
	})

	t.Run("empty dictionary passthrough", func(t *testing.T) {
		r2 := &Redactor{dict: redactdict.FromValues([]string{})}
		in := "nothing to redact here"
		out := string(r2.Redact([]byte(in)))
		if out != in {
			t.Errorf("expected passthrough %q, got %q", in, out)
		}
	})
}

// TestRedactMultiLineSecret is the PP-B4a regression: a multi-line secret (e.g. a
// PEM private key) is redacted even though the redactor sees the persisted log one
// line at a time. addRedactionValue must contribute each discriminating line to
// the dictionary so a job echoing the key cannot leak it.
func TestRedactMultiLineSecret(t *testing.T) {
	const pem = "-----BEGIN OPENSSH PRIVATE KEY-----\n" +
		"b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAAB\n" +
		"AAAAMwAAAAtzc2gtZWQyNTUxOQAAACDxyz1234567890abcdEFGH\n" +
		"-----END OPENSSH PRIVATE KEY-----"

	r := &Redactor{dict: redactdict.FromValues(addRedactionValue(nil, pem))}

	// Each non-empty line, fed alone as the ingest scanner would, must be masked.
	for line := range strings.SplitSeq(pem, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		out := string(r.Redact([]byte(line)))
		if out != redactMask {
			t.Errorf("multi-line secret line not fully masked:\n  in:  %q\n  out: %q", line, out)
		}
	}

	// A non-secret line is untouched.
	if got := string(r.Redact([]byte("just a normal log line"))); got != "just a normal log line" {
		t.Errorf("normal line was altered: %q", got)
	}
}

// TestNewRedactorVariablesVisible: single-line env_vars values (the Variables
// tab) are NOT in the dictionary — variables are plaintext, log-safe values
// (D7) and masking them confused users — while a MULTI-LINE env_vars value (an
// SSH private key a host references by name, sshexec.signerFromEnvVar) is
// still masked line by line.
func TestNewRedactorVariablesVisible(t *testing.T) {
	pool, err := db.Open(filepath.Join(t.TempDir(), "redact.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	const plain = "plain-visible-value-123"
	const pem = "-----BEGIN OPENSSH PRIVATE KEY-----\n" +
		"b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAAB\n" +
		"-----END OPENSSH PRIVATE KEY-----"
	for _, row := range []struct{ id, key, val string }{
		{"ev-plain", "APP_URL", plain},
		{"ev-key", "SSH_KEY", pem},
	} {
		if _, err := pool.Exec(
			`INSERT INTO env_vars(id, key, scope, value, created_at) VALUES(?,?,?,?,?)`,
			row.id, row.key, "prod", row.val, "2026-01-01T00:00:00Z"); err != nil {
			t.Fatalf("seed %s: %v", row.id, err)
		}
	}

	r, err := NewRedactor(context.Background(), pool, nil, "prod")
	if err != nil {
		t.Fatalf("NewRedactor: %v", err)
	}

	if got := string(r.Redact([]byte("url is " + plain))); got != "url is "+plain {
		t.Errorf("single-line variable was masked, want it visible: %q", got)
	}
	for line := range strings.SplitSeq(pem, "\n") {
		if out := string(r.Redact([]byte(line))); strings.Contains(out, line) && out != redactMask {
			t.Errorf("multi-line key material leaked: %q -> %q", line, out)
		}
	}
}

func TestRedactLongestFirst(t *testing.T) {
	// Mirror what NewRedactor produces: values ordered longest-first so greedy
	// replacement masks the superstring before its substring. With this order
	// "SECRETLONG" is replaced first, yielding a single mask rather than
	// "[REDACTED]LONG".
	r := &Redactor{dict: redactdict.FromValues([]string{"SECRETLONG", "SECRET"})}
	out := string(r.Redact([]byte("SECRETLONG")))
	if out != redactMask {
		t.Errorf("expected single %q for longest-first replacement, got %q", redactMask, out)
	}
}

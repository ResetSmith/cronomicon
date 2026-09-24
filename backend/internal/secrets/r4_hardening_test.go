package secrets

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/config"
)

// selfSignedCAPEM returns a minimal valid self-signed CA certificate in PEM form,
// enough for x509.CertPool.AppendCertsFromPEM to accept.
func selfSignedCAPEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// TestRevealMissingFailsClosed (L3): Reveal on a non-existent secret id returns
// ErrSecretNotFound, not ("", nil) — so the dispatch resolver fails closed and the
// reveal API returns 404 instead of injecting/returning an empty value.
func TestRevealMissingFailsClosed(t *testing.T) {
	pool, cleanup := openTestDB(t)
	defer cleanup()
	kek, _ := randomBytes(32)
	cfg := &config.Config{SecretKEKEnv: base64.StdEncoding.EncodeToString(kek)}
	svc := New(pool, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))

	val, err := svc.Reveal(context.Background(), "does-not-exist")
	if !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("Reveal(missing) err = %v, want ErrSecretNotFound", err)
	}
	if val != "" {
		t.Errorf("Reveal(missing) value = %q, want empty", val)
	}
}

// TestVaultConfigured (L7): the stub client reports NOT configured; a real wired
// client reports configured — the "actually usable" signal the capabilities
// endpoint needs so a bad-CA stub doesn't advertise vault=true.
func TestVaultConfigured(t *testing.T) {
	pool, cleanup := openTestDB(t)
	defer cleanup()
	svc := New(pool, &config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if svc.VaultConfigured() {
		t.Error("fresh Service (stub vault) reports VaultConfigured=true")
	}
	vc, err := NewVaultClientWithOptions("https://vault.example:8200", "role", "secret", VaultOptions{})
	if err != nil {
		t.Fatalf("NewVaultClientWithOptions: %v", err)
	}
	svc.WithVaultClient(vc)
	if !svc.VaultConfigured() {
		t.Error("Service with a real wired client reports VaultConfigured=false")
	}
}

// TestCATransportKeepsDefaults (L6): the private-CA transport clones
// http.DefaultTransport, so proxy support / keep-alives / HTTP2 survive — a bare
// &http.Transport{} silently dropped ProxyFromEnvironment (bypassing an egress
// proxy). We assert the cloned transport keeps a non-nil Proxy func and applies the
// custom RootCAs.
func TestCATransportKeepsDefaults(t *testing.T) {
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, selfSignedCAPEM(t), 0o600); err != nil {
		t.Fatal(err)
	}
	tr, err := CATransport(caFile)
	if err != nil {
		t.Fatalf("CATransport: %v", err)
	}
	if tr.Proxy == nil {
		t.Error("cloned transport lost Proxy (ProxyFromEnvironment) — egress proxy would be bypassed")
	}
	// http.DefaultTransport's Proxy is ProxyFromEnvironment; sanity-check identity is
	// preserved by comparing behavior on a proxy-less env (returns nil, nil).
	if req, _ := http.NewRequest("GET", "https://vault.example:8200", nil); func() bool {
		u, perr := tr.Proxy(req)
		return perr == nil && u == nil
	}() == false {
		t.Error("cloned transport Proxy does not behave like ProxyFromEnvironment")
	}
	if tr.TLSClientConfig == nil || tr.TLSClientConfig.RootCAs == nil {
		t.Error("custom RootCAs not applied to the cloned transport")
	}
}

package sshkeys

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// pkcs8PEM marshals any private key to an unencrypted PKCS#8 PEM, which
// ssh.ParsePrivateKey reads for every key type — a uniform test fixture.
func pkcs8PEM(t *testing.T, key crypto.PrivateKey) string {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal pkcs8: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

func TestParse_DerivesMetadata(t *testing.T) {
	_, ed, _ := ed25519.GenerateKey(rand.Reader)
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	ecKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)

	cases := []struct {
		name     string
		material string
		wantType string
	}{
		{"ed25519", pkcs8PEM(t, ed), "ssh-ed25519"},
		{"rsa", pkcs8PEM(t, rsaKey), "ssh-rsa"},
		{"ecdsa", pkcs8PEM(t, ecKey), "ecdsa-sha2-nistp256"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			keyType, fp, pub, err := Parse(tc.material)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if keyType != tc.wantType {
				t.Errorf("keyType = %q, want %q", keyType, tc.wantType)
			}
			if !strings.HasPrefix(fp, "SHA256:") {
				t.Errorf("fingerprint = %q, want SHA256: prefix", fp)
			}
			if !strings.HasPrefix(pub, tc.wantType+" ") {
				t.Errorf("publicKey = %q, want %q prefix", pub, tc.wantType)
			}
		})
	}
}

func TestParse_RejectsGarbage(t *testing.T) {
	_, _, _, err := Parse("-----BEGIN nonsense-----\nnope\n-----END nonsense-----")
	if err == nil {
		t.Fatal("want error for unparseable material")
	}
	if errors.Is(err, ErrPassphraseProtected) {
		t.Errorf("garbage misclassified as passphrase-protected")
	}
}

func TestParse_RejectsPassphraseProtected(t *testing.T) {
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	blk, err := ssh.MarshalPrivateKeyWithPassphrase(rsaKey, "", []byte("hunter2"))
	if err != nil {
		t.Fatalf("marshal encrypted: %v", err)
	}
	_, _, _, err = Parse(string(pem.EncodeToMemory(blk)))
	if !errors.Is(err, ErrPassphraseProtected) {
		t.Fatalf("err = %v, want ErrPassphraseProtected", err)
	}
}

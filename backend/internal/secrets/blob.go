package secrets

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"

	"github.com/ResetSmith/cronomicon/internal/config"
)

// EncryptString envelope-encrypts a short secret value (e.g. the SMTP password)
// and returns a single self-describing token safe to store in a config column.
// It uses the same KEK + AES-256-GCM envelope scheme as the secrets table (S14),
// so credentials never sit in plaintext config. Returns ErrNoKEK if no KEK is
// configured.
//
// Token format (current): "v2:" + <kekVersion> + ":" + base64(ciphertext) + ":" +
// base64(nonce) + ":" + base64(wrappedDEK). The embedded KEK version lets
// DecryptString pick the right key after a KEK rotation (PP-L5).
//
// Legacy "v1:"-prefixed tokens (no version field) are still decryptable and are
// treated as KEK version 1; they migrate to v2 on next write.
func EncryptString(cfg *config.Config, plaintext string) (string, error) {
	kek, err := loadKEK(cfg)
	if err != nil {
		return "", err
	}
	defer zero(kek) // SU-10: wipe the loaded KEK after the envelope op (best-effort)
	ct, nonce, wrapped, err := envelopeEncrypt(kek, []byte(plaintext))
	if err != nil {
		return "", fmt.Errorf("encrypt: %w", err)
	}
	enc := base64.StdEncoding.EncodeToString
	return fmt.Sprintf("v2:%d:%s:%s:%s", cfg.SecretKEKVersion, enc(ct), enc(nonce), enc(wrapped)), nil
}

// TokenKEKVersion reports which KEK version sealed a token produced by
// EncryptString, WITHOUT decrypting it — so a rotation audit can count what is
// outstanding even for values whose historical key is no longer configured.
// ok is false for anything that is not a recognisable token (including the empty
// string, which is how an unset config credential is stored).
func TokenKEKVersion(token string) (version int, ok bool) {
	parts := strings.Split(token, ":")
	switch {
	case len(parts) == 5 && parts[0] == "v2":
		v, err := strconv.Atoi(parts[1])
		if err != nil {
			return 0, false
		}
		return v, true
	case len(parts) == 4 && parts[0] == "v1":
		// Legacy tokens carry no version field and are defined as version 1.
		return 1, true
	}
	return 0, false
}

// DecryptString reverses EncryptString. It accepts both the current "v2:<ver>:…"
// format and the legacy "v1:…" format (treated as KEK version 1).
func DecryptString(cfg *config.Config, token string) (string, error) {
	parts := strings.Split(token, ":")
	var kekVer int
	var ctB64, nonceB64, wrappedB64 string
	switch {
	case len(parts) == 5 && parts[0] == "v2":
		v, err := strconv.Atoi(parts[1])
		if err != nil {
			return "", fmt.Errorf("bad secret token version: %w", err)
		}
		kekVer, ctB64, nonceB64, wrappedB64 = v, parts[2], parts[3], parts[4]
	case len(parts) == 4 && parts[0] == "v1":
		kekVer, ctB64, nonceB64, wrappedB64 = 1, parts[1], parts[2], parts[3]
	default:
		return "", fmt.Errorf("bad secret token format")
	}
	kek, err := loadKEKForVersion(cfg, kekVer)
	if err != nil {
		return "", err
	}
	defer zero(kek) // SU-10: wipe the loaded KEK after the envelope op (best-effort)
	dec := base64.StdEncoding.DecodeString
	ct, err := dec(ctB64)
	if err != nil {
		return "", fmt.Errorf("decode ciphertext: %w", err)
	}
	nonce, err := dec(nonceB64)
	if err != nil {
		return "", fmt.Errorf("decode nonce: %w", err)
	}
	wrapped, err := dec(wrappedB64)
	if err != nil {
		return "", fmt.Errorf("decode wrapped dek: %w", err)
	}
	plain, err := envelopeDecrypt(kek, ct, nonce, wrapped)
	if err != nil {
		return "", fmt.Errorf("decrypt: %w", err)
	}
	return string(plain), nil
}

// ErrNoKEK is the exported sentinel for "no KEK configured", so callers (e.g.
// notify) can distinguish a missing KEK from a real crypto failure.
var ErrNoKEK = errNoKEK

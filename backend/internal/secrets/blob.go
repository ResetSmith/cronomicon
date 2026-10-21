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
// Token formats: "v3:" + <kekVersion> + ":" + base64(ciphertext) + ":" +
// base64(nonce) + ":" + base64(wrappedDEK) is the bound form (2.5.1 SG2.10,
// EncryptSetting); "v2:" the same, unbound, which every token was until 2.5.1
// and which EncryptString still writes for BindSeals' reading of old values
// and for tests. The embedded KEK version lets the decoder pick the right key
// after a KEK rotation (PP-L5). Legacy "v1:"-prefixed tokens (no version
// field) are still decryptable and are treated as KEK version 1.
func EncryptString(cfg *config.Config, plaintext string) (string, error) {
	return encryptToken(cfg, plaintext, "")
}

// EncryptSetting seals a settings column's value bound to its row (2.5.1
// SG2.10): the token is "v3:…" and opens only under b (SettingsBinding).
// Every production write of a sealed setting goes through this.
func EncryptSetting(cfg *config.Config, b Binding, plaintext string) (string, error) {
	if b == "" {
		return "", fmt.Errorf("encrypt setting: no binding")
	}
	return encryptToken(cfg, plaintext, b)
}

func encryptToken(cfg *config.Config, plaintext string, b Binding) (string, error) {
	kek, err := loadKEK(cfg)
	if err != nil {
		return "", err
	}
	defer zero(kek) // SU-10: wipe the loaded KEK after the envelope op (best-effort)
	var ad []byte
	format := "v2"
	if b != "" {
		ad, format = []byte(b), "v3"
	}
	ct, nonce, wrapped, err := envelopeEncryptAD(kek, []byte(plaintext), ad)
	if err != nil {
		return "", fmt.Errorf("encrypt: %w", err)
	}
	enc := base64.StdEncoding.EncodeToString
	return fmt.Sprintf("%s:%d:%s:%s:%s", format, cfg.SecretKEKVersion, enc(ct), enc(nonce), enc(wrapped)), nil
}

// TokenKEKVersion reports which KEK version sealed a token produced by
// EncryptString or EncryptSetting, WITHOUT decrypting it — so a rotation audit
// can count what is outstanding even for values whose historical key is no
// longer configured. ok is false for anything that is not a recognisable token
// (including the empty string, which is how an unset config credential is
// stored).
func TokenKEKVersion(token string) (version int, ok bool) {
	t, err := parseToken(token)
	if err != nil {
		return 0, false
	}
	return t.kekVer, true
}

// TokenBound reports whether a token is the bound (v3) form.
func TokenBound(token string) bool {
	t, err := parseToken(token)
	return err == nil && t.bound
}

type parsedToken struct {
	bound                         bool
	kekVer                        int
	ctB64, nonceB64, wrappedB64 string
}

func parseToken(token string) (parsedToken, error) {
	parts := strings.Split(token, ":")
	switch {
	case len(parts) == 5 && (parts[0] == "v2" || parts[0] == "v3"):
		v, err := strconv.Atoi(parts[1])
		if err != nil {
			return parsedToken{}, fmt.Errorf("bad secret token version: %w", err)
		}
		return parsedToken{bound: parts[0] == "v3", kekVer: v, ctB64: parts[2], nonceB64: parts[3], wrappedB64: parts[4]}, nil
	case len(parts) == 4 && parts[0] == "v1":
		// Legacy tokens carry no version field and are defined as version 1.
		return parsedToken{kekVer: 1, ctB64: parts[1], nonceB64: parts[2], wrappedB64: parts[3]}, nil
	}
	return parsedToken{}, fmt.Errorf("bad secret token format")
}

// DecryptString opens a token with no binding: an unbound (v1, v2) one, until
// BindSeals has bound every value. It cannot open a bound (v3) token, which
// takes DecryptSetting and its binding.
func DecryptString(cfg *config.Config, token string) (string, error) {
	return decryptToken(cfg, token, "")
}

// DecryptSetting opens a settings column's token under its binding: a v3
// token only under b; an unbound v1/v2 token from before 2.5.1 with no
// binding, until BindSeals has bound every value (ErrSealUnbound after).
func DecryptSetting(cfg *config.Config, b Binding, token string) (string, error) {
	return decryptToken(cfg, token, b)
}

func decryptToken(cfg *config.Config, token string, b Binding) (string, error) {
	t, err := parseToken(token)
	if err != nil {
		return "", err
	}
	var ad []byte
	switch {
	case t.bound:
		if b == "" {
			return "", fmt.Errorf("decrypt: a bound token needs its binding")
		}
		ad = []byte(b)
	case UnboundSealsRefused():
		return "", ErrSealUnbound
	}
	kek, err := loadKEKForVersion(cfg, t.kekVer)
	if err != nil {
		return "", err
	}
	defer zero(kek) // SU-10: wipe the loaded KEK after the envelope op (best-effort)
	dec := base64.StdEncoding.DecodeString
	ct, err := dec(t.ctB64)
	if err != nil {
		return "", fmt.Errorf("decode ciphertext: %w", err)
	}
	nonce, err := dec(t.nonceB64)
	if err != nil {
		return "", fmt.Errorf("decode nonce: %w", err)
	}
	wrapped, err := dec(t.wrappedB64)
	if err != nil {
		return "", fmt.Errorf("decode wrapped dek: %w", err)
	}
	plain, err := envelopeDecryptAD(kek, ct, nonce, wrapped, ad)
	if err != nil {
		return "", fmt.Errorf("decrypt: %w", err)
	}
	return string(plain), nil
}

// OpenSetting is a stored setting's plaintext under its binding: the decrypted
// value of a token, or the value itself when it is not one (written before the
// column was sealed, or with no KEK configured). For a column that is sealed
// from some release on and may still hold plain rows (2.5.1: push-target URLs).
func OpenSetting(cfg *config.Config, b Binding, v string) (string, error) {
	if _, ok := TokenKEKVersion(v); !ok {
		return v, nil
	}
	return DecryptSetting(cfg, b, v)
}

// ErrNoKEK is the exported sentinel for "no KEK configured", so callers (e.g.
// notify) can distinguish a missing KEK from a real crypto failure.
var ErrNoKEK = errNoKEK

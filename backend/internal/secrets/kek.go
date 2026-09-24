// Package secrets implements AES-256-GCM envelope encryption for stored secrets
// (S14) and CRUD operations on the `secrets` DB table.
//
// Envelope encryption:
//
//	KEK (32 bytes, base64 from file or env) wraps a per-secret random DEK (32 bytes).
//	The secret plaintext is encrypted by the DEK using AES-256-GCM with a random
//	12-byte nonce.  Stored on the row: ciphertext, nonce, wrapped_dek, kek_version.
//
// KEK precedence (S14): file at cfg.SecretKEKFile (if set and readable) >
// cfg.SecretKEKEnv > neither → stored-source operations return errNoKEK.
//
// Vault path: a small VaultClient interface (vault.go) is backed by a real
// AppRole/KV-v2 HTTP client (vault_http.go, no Vault SDK dep by design), wired
// via settings.WireVaultClient when Vault is configured; unconfigured falls back
// to the no-op stub.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"

	"github.com/ResetSmith/cronomicon/internal/config"
)

// errNoKEK is returned when no KEK is configured and a stored-secret operation
// is attempted.
var errNoKEK = errors.New("no KEK configured: set CRONOMICON_KEK_FILE or CRONOMICON_KEK")

// evaluateKEKFileMode applies the DR-Q4 two-tier permission policy to a
// file-sourced KEK. It is the pure, testable half of VerifyKEKFileMode.
//
//   - any "other" bit set (e.g. 0644) is FATAL: a key readable by every account
//     on the host decrypts every stored secret, and starting anyway normalises it;
//   - any "group" bit set (e.g. 0640) warns and continues, because a group-owned
//     secret mount is a defensible deployment choice that an operator may not
//     fully control.
//
// A Stat failure is NOT an error here: loadKEK reports an unreadable KEK file with
// a far better message, and this check must not pre-empt it.
func evaluateKEKFileMode(path string) (warn bool, err error) {
	fi, statErr := os.Stat(path)
	if statErr != nil {
		return false, nil
	}
	mode := fi.Mode().Perm()
	if mode&0o007 != 0 {
		return false, fmt.Errorf(
			"KEK file %q has mode %04o — it is readable by every account on this host, "+
				"and it decrypts every stored secret. Fix with: chmod 0400 %s "+
				"(and chown it to the account amadeus runs as)", path, mode, path)
	}
	return mode&0o070 != 0, nil
}

// kekModeOnce caches the permission verdict for the process. loadKEK runs on every
// seal and every open, so evaluating per call would re-stat the file thousands of
// times a day and flood the log with the same warning.
var (
	kekModeOnce sync.Once
	kekModeErr  error
)

// VerifyKEKFileMode enforces the DR-Q4 policy, once per process. The server calls
// it at startup so a fatal verdict surfaces when the container boots rather than
// hours later on the first secret operation; loadKEK calls it too, so the refusal
// cannot be bypassed by a path that skips startup.
//
// It is a no-op for an env-supplied KEK (CRONOMICON_KEK), which has no file and no
// mode. That posture is discouraged for other reasons — see the administrator
// manual §6.6 — but it is not something a startup refusal can express.
func VerifyKEKFileMode(cfg *config.Config) error {
	if cfg == nil || cfg.SecretKEKFile == "" {
		return nil
	}
	kekModeOnce.Do(func() {
		warn, err := evaluateKEKFileMode(cfg.SecretKEKFile)
		kekModeErr = err
		if warn {
			slog.Warn("KEK file is group-readable; 0400 is expected",
				"path", cfg.SecretKEKFile)
		}
	})
	return kekModeErr
}

// loadKEK reads the 256-bit (32-byte) Key Encryption Key following the
// precedence rule from S14: KEK file > KEK env > error.
//
// The KEK must be base64-encoded (standard or URL encoding tolerated).
func loadKEK(cfg *config.Config) ([]byte, error) {
	var raw string

	if cfg.SecretKEKFile != "" {
		data, err := os.ReadFile(cfg.SecretKEKFile)
		if err != nil {
			// Hard failure: an explicitly configured KEK file that cannot be read
			// means we would silently decrypt with the wrong key (PP-L4). Fail loudly.
			return nil, fmt.Errorf("read KEK file %q: %w", cfg.SecretKEKFile, err)
		}
		raw = string(data)
		// DR-6: refuse a world-readable key before it is used. Checked after the
		// read so an unreadable-file error still wins (it names a better fix), and
		// memoised so this costs one Stat per process rather than one per envelope op.
		if err := VerifyKEKFileMode(cfg); err != nil {
			return nil, err
		}
	}

	if raw == "" && cfg.SecretKEKEnv != "" {
		raw = cfg.SecretKEKEnv
	}

	if raw == "" {
		return nil, errNoKEK
	}

	// Strip trailing whitespace/newlines from file reads.
	for len(raw) > 0 && (raw[len(raw)-1] == '\n' || raw[len(raw)-1] == '\r' || raw[len(raw)-1] == ' ') {
		raw = raw[:len(raw)-1]
	}

	kek, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		// Try URL encoding (no padding) as a fallback.
		kek, err = base64.RawURLEncoding.DecodeString(raw)
		if err != nil {
			return nil, fmt.Errorf("KEK is not valid base64: %w", err)
		}
	}
	if len(kek) != 32 {
		return nil, fmt.Errorf("KEK must be 32 bytes (AES-256); got %d bytes", len(kek))
	}
	return kek, nil
}

// loadKEKForVersion loads the KEK for a specific version number.
// For the active version (cfg.SecretKEKVersion) it delegates to loadKEK.
// For historical versions it reads CRONOMICON_KEK_<N>_FILE then CRONOMICON_KEK_<N>.
// This enables zero-downtime KEK rotation: set CRONOMICON_KEK_VERSION=2, supply the new
// KEK via the standard vars, and keep the old key at CRONOMICON_KEK_1 / CRONOMICON_KEK_1_FILE.
func loadKEKForVersion(cfg *config.Config, version int) ([]byte, error) {
	if version == cfg.SecretKEKVersion {
		return loadKEK(cfg)
	}
	fileEnv := fmt.Sprintf("CRONOMICON_KEK_%d_FILE", version)
	valEnv := fmt.Sprintf("CRONOMICON_KEK_%d", version)
	var raw string
	path := os.Getenv(fileEnv)
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read KEK v%d file %q: %w", version, path, err)
		}
		raw = string(data)
	}
	if raw == "" {
		raw = os.Getenv(valEnv)
	}
	if raw == "" {
		return nil, fmt.Errorf("no KEK configured for version %d (set %s or %s)", version, fileEnv, valEnv)
	}
	for len(raw) > 0 && (raw[len(raw)-1] == '\n' || raw[len(raw)-1] == '\r' || raw[len(raw)-1] == ' ') {
		raw = raw[:len(raw)-1]
	}
	kek, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		kek, err = base64.RawURLEncoding.DecodeString(raw)
		if err != nil {
			return nil, fmt.Errorf("KEK v%d is not valid base64: %w", version, err)
		}
	}
	if len(kek) != 32 {
		return nil, fmt.Errorf("KEK v%d must be 32 bytes (AES-256); got %d bytes", version, len(kek))
	}
	return kek, nil
}

// encryptWithKey encrypts plaintext using AES-256-GCM and the provided 32-byte key.
// Returns (ciphertext, nonce) where nonce is a fresh random 12-byte value.
func encryptWithKey(key, plaintext []byte) (ciphertext, nonce []byte, err error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, nil, fmt.Errorf("new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, fmt.Errorf("new GCM: %w", err)
	}
	nonce = make([]byte, gcm.NonceSize())
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, fmt.Errorf("random nonce: %w", err)
	}
	ciphertext = gcm.Seal(nil, nonce, plaintext, nil)
	return ciphertext, nonce, nil
}

// decryptWithKey decrypts AES-256-GCM ciphertext using key and nonce.
func decryptWithKey(key, ciphertext, nonce []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("new GCM: %w", err)
	}
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("GCM open: %w", err)
	}
	return plaintext, nil
}

// randomBytes returns n cryptographically random bytes.
func randomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return nil, fmt.Errorf("random bytes: %w", err)
	}
	return b, nil
}

// zero best-effort wipes a key buffer. Go offers no guaranteed secure erase — GC
// copies and heap growth may leave stale copies elsewhere — so this NARROWS, not
// eliminates, the window a KEK/DEK sits recoverable in memory (SU-10). It does not
// change the encryption construction. nil/empty is a no-op.
func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// envelopeEncrypt encrypts plaintext using envelope encryption:
//  1. Generate a random 256-bit DEK.
//  2. Encrypt the plaintext with the DEK (AES-256-GCM, random nonce).
//  3. Encrypt (wrap) the DEK with the KEK (AES-256-GCM, fresh nonce stored in wrappedDEK prefix).
//
// Returns (ciphertext, nonce, wrappedDEK) suitable for storing in the DB.
func envelopeEncrypt(kek []byte, plaintext []byte) (ciphertext, nonce, wrappedDEK []byte, err error) {
	dek, err := randomBytes(32)
	if err != nil {
		return nil, nil, nil, err
	}
	defer zero(dek) // SU-10: narrow the window the DEK sits in memory (best-effort)
	ciphertext, nonce, err = encryptWithKey(dek, plaintext)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("encrypt with DEK: %w", err)
	}
	// Wrap the DEK: the nonce for DEK-wrap is prepended to the wrapped ciphertext
	// so the DB stores a single blob: [12-byte nonce | wrapped DEK ciphertext].
	block, err := aes.NewCipher(kek)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("kek cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("kek GCM: %w", err)
	}
	dekNonce := make([]byte, gcm.NonceSize())
	if _, err = io.ReadFull(rand.Reader, dekNonce); err != nil {
		return nil, nil, nil, fmt.Errorf("random DEK nonce: %w", err)
	}
	wrappedDEKCiphertext := gcm.Seal(nil, dekNonce, dek, nil)
	// Store as: dekNonce || wrappedDEKCiphertext
	wrappedDEK = make([]byte, len(dekNonce)+len(wrappedDEKCiphertext))
	copy(wrappedDEK, dekNonce)
	copy(wrappedDEK[len(dekNonce):], wrappedDEKCiphertext)

	return ciphertext, nonce, wrappedDEK, nil
}

// envelopeDecrypt reverses envelopeEncrypt.
func envelopeDecrypt(kek, ciphertext, nonce, wrappedDEK []byte) ([]byte, error) {
	// Unwrap the DEK.
	block, err := aes.NewCipher(kek)
	if err != nil {
		return nil, fmt.Errorf("kek cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("kek GCM: %w", err)
	}
	if len(wrappedDEK) < gcm.NonceSize() {
		return nil, errors.New("wrapped_dek too short")
	}
	dekNonce := wrappedDEK[:gcm.NonceSize()]
	wrappedDEKBody := wrappedDEK[gcm.NonceSize():]
	dek, err := gcm.Open(nil, dekNonce, wrappedDEKBody, nil)
	if err != nil {
		return nil, fmt.Errorf("unwrap DEK: %w", err)
	}
	defer zero(dek) // SU-10: wipe the unwrapped DEK on return (best-effort)
	// Decrypt the plaintext with the DEK.
	plaintext, err := decryptWithKey(dek, ciphertext, nonce)
	if err != nil {
		return nil, fmt.Errorf("decrypt with DEK: %w", err)
	}
	return plaintext, nil
}

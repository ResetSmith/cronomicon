package secrets

import (
	"fmt"

	"github.com/ResetSmith/cronomicon/internal/config"
)

// Sealer is the single, externally-callable implementation of the column-based
// envelope scheme used by the secrets table — the (ciphertext, nonce, wrapped_dek,
// kek_version) layout, as opposed to the single self-describing token EncryptString
// produces for config columns. Any store that keeps its own envelope columns
// (e.g. ssh_credentials, ssh-keys-update.md SK.2/SK.3) seals and opens through this
// type so there is one KEK + AES-256-GCM code path and KEK rotation applies to every
// store uniformly — no second crypto implementation.
type Sealer struct{ cfg *config.Config }

// NewSealer returns a Sealer bound to the process KEK configuration.
func NewSealer(cfg *config.Config) *Sealer { return &Sealer{cfg: cfg} }

// Sealed is the envelope output: the four column values a caller persists alongside
// its row. KEKVersion records which KEK wrapped the DEK so Open can pick the right
// key after a rotation (PP-L5).
type Sealed struct {
	Ciphertext []byte
	Nonce      []byte
	WrappedDEK []byte
	KEKVersion int
}

// Seal envelope-encrypts plaintext under the current KEK and returns the four
// column values to persist. Returns ErrNoKEK when no KEK is configured, so callers
// can distinguish a missing KEK from a real crypto failure.
func (s *Sealer) Seal(plaintext []byte) (Sealed, error) {
	kek, err := loadKEK(s.cfg)
	if err != nil {
		return Sealed{}, err
	}
	defer zero(kek) // SU-10: wipe the loaded KEK after the envelope op (best-effort)
	ct, nonce, wrapped, err := envelopeEncrypt(kek, plaintext)
	if err != nil {
		return Sealed{}, fmt.Errorf("seal: %w", err)
	}
	return Sealed{Ciphertext: ct, Nonce: nonce, WrappedDEK: wrapped, KEKVersion: s.cfg.SecretKEKVersion}, nil
}

// Open reverses Seal, decrypting the persisted column values with the KEK named by
// kekVersion (so a value sealed before a KEK rotation still opens). kekVersion is
// passed through to the KEK loader unchanged — it equals cfg.SecretKEKVersion for
// the current key, which the loader maps to the active KEK. Callers reading a
// nullable kek_version column should resolve NULL themselves (the secrets table
// uses COALESCE(kek_version, 1)) before calling Open.
func (s *Sealer) Open(ciphertext, nonce, wrappedDEK []byte, kekVersion int) ([]byte, error) {
	kek, err := loadKEKForVersion(s.cfg, kekVersion)
	if err != nil {
		return nil, err
	}
	defer zero(kek) // SU-10: wipe the loaded KEK after the envelope op (best-effort)
	plain, err := envelopeDecrypt(kek, ciphertext, nonce, wrappedDEK)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	return plain, nil
}

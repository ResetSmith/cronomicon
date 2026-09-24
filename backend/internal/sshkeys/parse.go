// Package sshkeys is the first-class store for the system SSH key credentials
// Cronomicon uses for automated SSH (ssh-keys-update.md, SK.3/SK.4). Private key
// material is envelope-encrypted through the shared secrets.Sealer (one crypto
// path, KEK rotation applies uniformly) and validated on save; hosts and bastions
// reference a credential by id rather than the legacy auth_key_env_var name.
package sshkeys

import (
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/ssh"
)

// ErrPassphraseProtected is returned by Parse when the supplied private key is
// encrypted with a passphrase. v1 supports unencrypted stored keys only (SK-D8);
// the API surfaces this as a clear 422 ("passphrase-protected keys are not
// supported") rather than a cryptic parse error.
var ErrPassphraseProtected = errors.New("passphrase-protected keys are not supported — provide an unencrypted key")

// ErrInvalidKey wraps an unparseable-material failure so the API can map it to a
// 422 (operator input error) rather than a 500.
var ErrInvalidKey = errors.New("not a parseable private key")

// Parse validates SSH private-key material and derives its public metadata
// without retaining the private key. It is the single validate-on-save entry
// point (SK.4): a typo'd or truncated paste is rejected here, before the key can
// fail at dial time. Returns (keyType, fingerprint, publicKeyLine) — the
// authorized_keys public line is safe to display and copy into a target's
// authorized_keys.
func Parse(material string) (keyType, fingerprint, publicKey string, err error) {
	signer, err := parseSigner(material)
	if err != nil {
		return "", "", "", err
	}
	pub := signer.PublicKey()
	return pub.Type(),
		ssh.FingerprintSHA256(pub),
		strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))),
		nil
}

// parseSigner parses unencrypted private-key material into a signer, mapping the
// passphrase-protected case to ErrPassphraseProtected so callers can distinguish
// "needs a passphrase" (unsupported in v1) from "not a parseable key".
func parseSigner(material string) (ssh.Signer, error) {
	signer, err := ssh.ParsePrivateKey([]byte(material))
	if err != nil {
		if _, ok := errors.AsType[*ssh.PassphraseMissingError](err); ok {
			return nil, ErrPassphraseProtected
		}
		return nil, fmt.Errorf("%w: %v", ErrInvalidKey, err)
	}
	return signer, nil
}

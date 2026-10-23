package backup

// Encrypted backups (2.5.1 SG1, SG-1..SG-8). A snapshot is encrypted with age
// to the X25519 public keys ("recipients") a global administrator pasted into
// Settings → Backups. The server holds public keys only: it can seal a backup
// and can never open one. Opening takes a private key ("identity"), which is
// given to `cronomicon restore --identity` or to the age CLI, never to the app.
//
// Only X25519 recipients (`age1…`) are accepted. age refuses to mix them with
// its post-quantum hybrid recipients in one file, and an SSH key or a
// passphrase would put the key's custody somewhere else than the card says.

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"filippo.io/age"
)

// ErrInvalidRecipient is wrapped by every refusal of a pasted recipient, so a
// caller can map it to 422.
var ErrInvalidRecipient = errors.New("invalid backup key")

// RecipientsSuffix is appended to a sealed file's name for the list of the
// fingerprints it was sealed to, one per line (SG-7).
const RecipientsSuffix = ".recipients"

// SealedSuffix ends the name of an encrypted snapshot.
const SealedSuffix = ".db.age"

// ParseRecipient validates one pasted public key and returns it trimmed. The
// refusals say what kind of thing was pasted, because the common mistakes are
// an SSH public key, a private key and a hybrid key, and each needs a different
// fix. They never repeat the input or age's error, which quotes it (2.5.1
// SG6.3, the review's A8): a private key pasted with quotes, in lower case or
// inside the age-keygen file came back in the 422, on the card and, from
// CRONOMICON_BACKUP_RECIPIENTS, in the boot log. So any input holding a
// private key, anywhere and in any case, is refused as one.
func ParseRecipient(s string) (string, error) {
	s = strings.TrimSpace(s)
	switch {
	case s == "":
		return "", fmt.Errorf("%w: empty", ErrInvalidRecipient)
	case strings.Contains(strings.ToUpper(s), "AGE-SECRET-KEY-"):
		return "", fmt.Errorf("%w: that is a PRIVATE key; paste the public line (age1…) that age-keygen printed, and keep the private key off this server", ErrInvalidRecipient)
	case strings.HasPrefix(s, "ssh-") || strings.HasPrefix(s, "ecdsa-"):
		return "", fmt.Errorf("%w: SSH keys are not accepted; generate an age key with age-keygen and paste its public line (age1…)", ErrInvalidRecipient)
	case strings.HasPrefix(s, "age1pq1"):
		return "", fmt.Errorf("%w: post-quantum (age1pq1…) keys are not accepted; generate an X25519 key with age-keygen (without -pq)", ErrInvalidRecipient)
	}
	if _, err := age.ParseX25519Recipient(s); err != nil {
		return "", fmt.Errorf("%w: not an age public key: paste the whole public line (age1…) that age-keygen printed, with nothing around it", ErrInvalidRecipient)
	}
	return s, nil
}

// Fingerprint is how a recipient is named on the card, in the change log and
// beside each backup: the first 16 hex characters of the SHA-256 of its text.
func Fingerprint(recipient string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(recipient)))
	return hex.EncodeToString(sum[:8])
}

// EncryptFile seals src to the recipients and writes dst (mode 0600), then
// dst+RecipientsSuffix with their fingerprints. On any error it removes both,
// so a failed seal leaves nothing that looks like a backup. It returns the
// fingerprints it sealed to.
func EncryptFile(src, dst string, recipients []string) (fingerprints []string, err error) {
	if len(recipients) == 0 {
		return nil, errors.New("no backup keys")
	}
	rs := make([]age.Recipient, 0, len(recipients))
	for _, r := range recipients {
		x, perr := age.ParseX25519Recipient(strings.TrimSpace(r))
		if perr != nil {
			// Named by its fingerprint, never by its text (SG6.3).
			return nil, fmt.Errorf("%w: the stored key %s is not an age public key", ErrInvalidRecipient, Fingerprint(r))
		}
		rs = append(rs, x)
		fingerprints = append(fingerprints, Fingerprint(r))
	}
	defer func() {
		if err != nil {
			_ = os.Remove(dst)
			_ = os.Remove(dst + RecipientsSuffix)
		}
	}()
	in, err := os.Open(src)
	if err != nil {
		return nil, err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	bw := bufio.NewWriterSize(out, 1<<20)
	w, err := age.Encrypt(bw, rs...)
	if err == nil {
		if _, err = io.Copy(w, in); err == nil {
			err = w.Close()
		}
	}
	if err == nil {
		err = bw.Flush()
	}
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, fmt.Errorf("encrypt %s: %w", src, err)
	}
	if err = os.WriteFile(dst+RecipientsSuffix, []byte(strings.Join(fingerprints, "\n")+"\n"), 0o600); err != nil {
		return nil, err
	}
	return fingerprints, nil
}

// ageHeader opens every age file (the binary format; the uploader never armors).
const ageHeader = "age-encryption.org/v1\n"

// IsEncrypted reports whether the file at path is an age file, by its header
// rather than its name, so a renamed copy is still recognised.
func IsEncrypted(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	buf := make([]byte, len(ageHeader))
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return false, err
	}
	return string(buf[:n]) == ageHeader, nil
}

// ReadIdentities reads an age identity file (as age-keygen writes it; several
// keys may be in one file, and comment lines are skipped).
func ReadIdentities(path string) ([]age.Identity, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	ids, err := age.ParseIdentities(f)
	if err != nil {
		return nil, fmt.Errorf("read identity file %s: %w", path, err)
	}
	return ids, nil
}

// DecryptFile opens an encrypted snapshot with any of the identities and
// writes the plaintext to dst (mode 0600). A wrong key is refused before dst
// holds anything; dst is removed on any error.
func DecryptFile(src, dst string, identities []age.Identity) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	r, err := age.Decrypt(bufio.NewReaderSize(in, 1<<20), identities...)
	if err != nil {
		return fmt.Errorf("open %s: %w (is this the private key of one of the backup keys it was sealed to?)", src, err)
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			_ = os.Remove(dst)
		}
	}()
	if _, err = io.Copy(out, r); err != nil {
		return fmt.Errorf("decrypt %s: %w", src, err)
	}
	return out.Sync()
}

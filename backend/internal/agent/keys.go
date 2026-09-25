package agent

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"

	"github.com/ResetSmith/cronomicon/internal/runnerproto"
)

// materializeKeys writes each delivered SSH-key material (manifest.Keys, D8) to a
// 0600 file in a per-run directory OFF the run tree, and returns:
//
//   - keyMap:  bare credential NAME → file path, for the agent's key resolver
//     (resolveKeyPath / loadSigner) so a delivered key takes precedence over the
//     runner's own key-map / key-dir (which remains the fallback).
//   - keyEnv:  CRONOMICON_KEY_<name> reference → file path, injected into the run env
//     so a job body / inventory that references the derived form resolves to the
//     delivered path directly.
//   - cleanup: best-effort zeroes each file then removes the directory. ALWAYS safe
//     to call (a no-op when nothing was written), and the caller MUST defer it so
//     the material never outlives the run — including on an early error.
//
// The material lives ONLY in these 0600 files (mode-restricted, off the working
// tree so a checkout/archive can't sweep it up); nothing but the PATH ever enters
// the run env. The server redacts the material in logs (resolved.Redact), so a job
// that cats the file has its bytes masked at ingest.
func materializeKeys(keys []runnerproto.ManifestKey) (keyMap, keyEnv map[string]string, cleanup func(), err error) {
	cleanup = func() {}
	if len(keys) == 0 {
		return nil, nil, cleanup, nil
	}

	dir, err := os.MkdirTemp(keyBaseDir(), "cronomicon-keys-")
	if err != nil {
		return nil, nil, cleanup, fmt.Errorf("create key dir: %w", err)
	}
	// From here on the directory exists — the caller's deferred cleanup wipes it even
	// if a later write fails mid-way.
	written := make([]string, 0, len(keys))
	cleanup = func() {
		for _, p := range written {
			wipeFile(p)
		}
		_ = os.RemoveAll(dir)
	}

	keyMap = make(map[string]string, len(keys))
	keyEnv = make(map[string]string, len(keys))
	for i, k := range keys {
		// Index-based filename so an unusual credential name can never influence the
		// path; the NAME/REFERENCE keys of the returned maps carry the identity.
		path := filepath.Join(dir, fmt.Sprintf("key-%d", i))
		f, oerr := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if oerr != nil {
			return nil, nil, cleanup, fmt.Errorf("write key %q: %w", k.Name, oerr)
		}
		if _, werr := f.WriteString(normalizeKeyMaterial(k.Material)); werr != nil {
			_ = f.Close()
			return nil, nil, cleanup, fmt.Errorf("write key %q: %w", k.Name, werr)
		}
		if cerr := f.Close(); cerr != nil {
			return nil, nil, cleanup, fmt.Errorf("write key %q: %w", k.Name, cerr)
		}
		written = append(written, path)
		if k.Name != "" {
			keyMap[k.Name] = path
		}
		if k.Reference != "" {
			keyEnv[k.Reference] = path
		}
	}
	return keyMap, keyEnv, cleanup, nil
}

// normalizeKeyMaterial canonicalizes delivered key bytes on their way to disk: CRLF
// (or bare CR) line endings become LF, and the file is guaranteed a trailing
// newline. Both artifacts survive the server's validate-on-save because that check
// is Go's ssh.ParsePrivateKey, which accepts them — OpenSSH does NOT, so a key
// pasted from a Windows editor or without a final newline stores clean, delivers
// clean, and then dies at dial time with a bare `Load key "<path>": invalid format`
// (or "error in libcrypto"; the wording varies by OpenSSH version) that names
// nothing an operator can act on. Since ansible shells out to the system ssh, the
// STRICTER parser is the one that matters at the delivery seam. Well-formed
// material is returned byte-identical, and empty material stays empty (a key file
// containing a lone newline would be a worse error than an empty one).
func normalizeKeyMaterial(m string) string {
	if m == "" {
		return m
	}
	m = strings.ReplaceAll(m, "\r\n", "\n")
	m = strings.ReplaceAll(m, "\r", "\n")
	if !strings.HasSuffix(m, "\n") {
		m += "\n"
	}
	return m
}

// keyBaseDir prefers a tmpfs (/dev/shm) so key material never touches disk; it
// falls back to the OS temp dir when /dev/shm is absent or not writable (non-Linux,
// or a hardened mount). MkdirTemp("") uses os.TempDir().
func keyBaseDir() string {
	const shm = "/dev/shm"
	if fi, err := os.Stat(shm); err == nil && fi.IsDir() {
		// Confirm writability rather than trusting the mode bits.
		if probe, err := os.MkdirTemp(shm, ".cronomicon-probe-"); err == nil {
			_ = os.Remove(probe)
			return shm
		}
	}
	return "" // os.MkdirTemp("") → os.TempDir()
}

// wipeFile best-effort overwrites a file's bytes with zeroes before removal, so a
// deleted-but-not-yet-reclaimed inode doesn't retain key material. On tmpfs this is
// belt-and-suspenders; on a disk fallback it reduces the residual window.
func wipeFile(path string) {
	fi, err := os.Stat(path)
	if err != nil {
		return
	}
	if f, err := os.OpenFile(path, os.O_WRONLY, 0o600); err == nil {
		zeros := make([]byte, fi.Size())
		_, _ = f.WriteAt(zeros, 0)
		_ = f.Sync()
		_ = f.Close()
	}
	_ = os.Remove(path)
}

// mergeKeyMap returns base with delivered overlaid (delivered WINS). The result is
// a fresh map so callers never mutate the shared agent key-map. nil-safe.
func mergeKeyMap(base, delivered map[string]string) map[string]string {
	if len(delivered) == 0 {
		return base
	}
	out := make(map[string]string, len(base)+len(delivered))
	maps.Copy(out, base)
	// delivered takes precedence over the runner's own key custody
	maps.Copy(out, delivered)
	return out
}

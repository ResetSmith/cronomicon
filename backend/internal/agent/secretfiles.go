package agent

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/ResetSmith/cronomicon/internal/runnerproto"
)

// materializeSecretFiles writes each file-delivered secret (manifest.SecretFiles,
// RA-12) to a 0600 file in a per-run directory OFF the run tree, and returns:
//
//   - fileEnv:  CRONOMICON_SECRET_<name> reference → file path, injected into the run
//     env so a job body / inventory that references the derived form resolves to
//     the delivered path rather than to the value.
//   - cleanup:  best-effort zeroes each file then removes the directory. ALWAYS safe
//     to call (a no-op when nothing was written), and the caller MUST defer it so
//     the value never outlives the run — including on an early error.
//
// This is materializeKeys' twin and deliberately so: the delivery problem is
// identical (secret bytes that must reach a local toolchain as a PATH, live only
// as long as the run, and never touch the working tree), and having two mechanisms
// for it would mean two places to get the mode bits, the tmpfs preference and the
// wipe wrong. The value lives ONLY in these files — nothing but the path enters the
// run env — and the server holds it in the run's redaction dictionary, so a job that
// cats the file has its bytes masked at ingest.
//
// The whole point over an environment variable: an env var is readable from /proc by
// anything that can see the process, and an operator who pipes one into `sudo -S`
// puts it in the target's process table, where redaction cannot reach.
func materializeSecretFiles(files []runnerproto.ManifestSecretFile) (fileEnv map[string]string, cleanup func(), err error) {
	cleanup = func() {}
	if len(files) == 0 {
		return nil, cleanup, nil
	}

	dir, err := os.MkdirTemp(keyBaseDir(), "cronomicon-secretfiles-")
	if err != nil {
		return nil, cleanup, fmt.Errorf("create secret-file dir: %w", err)
	}
	written := make([]string, 0, len(files))
	cleanup = func() {
		for _, p := range written {
			wipeFile(p)
		}
		_ = os.RemoveAll(dir)
	}

	fileEnv = make(map[string]string, len(files))
	for i, f := range files {
		// Index-based filename so an unusual row name can never influence the path;
		// the REFERENCE key of the returned map carries the identity.
		path := filepath.Join(dir, fmt.Sprintf("secret-%d", i))
		fh, oerr := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if oerr != nil {
			return nil, cleanup, fmt.Errorf("write secret file %q: %w", f.Name, oerr)
		}
		// Written VERBATIM — no normalization, unlike key material. A become password
		// is an opaque byte string: appending a newline the way materializeKeys does
		// for PEM would change the password ansible sends, and trimming one would
		// break a password that legitimately ends in whitespace. Whatever the operator
		// stored is what gets used.
		if _, werr := fh.WriteString(f.Value); werr != nil {
			_ = fh.Close()
			return nil, cleanup, fmt.Errorf("write secret file %q: %w", f.Name, werr)
		}
		if cerr := fh.Close(); cerr != nil {
			return nil, cleanup, fmt.Errorf("write secret file %q: %w", f.Name, cerr)
		}
		written = append(written, path)
		if f.Reference != "" {
			fileEnv[f.Reference] = path
		}
	}
	return fileEnv, cleanup, nil
}

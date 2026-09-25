package gitlab

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/ResetSmith/cronomicon/internal/ansiblereq"
	"github.com/ResetSmith/cronomicon/internal/inventory"
)

// LintProject runs the Phase 3 project checks against a checkout project rooted
// at projectRoot (repo-relative, e.g. "scripts/vmware-patch") under repoDir:
//
//   - requirements.yml pinning lint (RX.5): every collection has an exact
//     version; every git role is SHA-pinned. Unpinned = "same SHA, different
//     Tuesday".
//   - tree secret-scan (§6.2): a vars/main.yml or .j2 in the project must not
//     smuggle a plaintext secret the value-based log redactor will never see.
//     Vault-encrypted files ($ANSIBLE_VAULT) are exempt (RX.13).
//
// Findings are returned as ValidationError so the caller decides severity:
// `cronomicon validate` / CI treats them as errors (fail), git sync surfaces them
// as never-blocking warnings (SyncLintProjectWarnings).
func LintProject(repoDir, projectRoot string) []ValidationError {
	var errs []ValidationError
	rootAbs := filepath.Join(repoDir, filepath.FromSlash(projectRoot))

	// requirements.yml pinning lint.
	reqAbs := filepath.Join(rootAbs, "requirements.yml")
	if data, err := os.ReadFile(reqAbs); err == nil {
		reqRel := filepath.ToSlash(filepath.Join(projectRoot, "requirements.yml"))
		reqs, perr := ansiblereq.Parse(data)
		if perr != nil {
			errs = append(errs, ValidationError{File: reqRel, Field: "requirements",
				Message: "cannot parse requirements.yml: " + perr.Error()})
		} else {
			for _, f := range ansiblereq.Lint(reqs) {
				errs = append(errs, ValidationError{File: reqRel, Field: "requirements",
					Message: f.String()})
			}
		}
	}

	// Tree secret-scan.
	errs = append(errs, scanTreeForSecrets(repoDir, projectRoot)...)
	return errs
}

// scanTreeForSecrets walks a project tree looking for inline plaintext secret
// values (the inventory.SecretBearingVar set) in YAML/INI/template files. A file
// whose content is (or contains) an Ansible Vault payload is exempt — its
// secrets are encrypted. Env-lookup indirection is allowed (the same allowed
// form the inventory guard accepts).
func scanTreeForSecrets(repoDir, projectRoot string) []ValidationError {
	var errs []ValidationError
	rootAbs := filepath.Join(repoDir, filepath.FromSlash(projectRoot))
	_ = filepath.WalkDir(rootAbs, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return nil
		}
		switch strings.ToLower(filepath.Ext(path)) {
		case ".yml", ".yaml", ".ini", ".j2", ".cfg":
		default:
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		// A vault-encrypted file is opaque ciphertext — exempt (RX.13).
		if strings.Contains(string(data), "$ANSIBLE_VAULT") {
			return nil
		}
		rel, _ := filepath.Rel(repoDir, path)
		for _, hit := range inventory.ScanSecretAssignments(string(data)) {
			errs = append(errs, ValidationError{File: filepath.ToSlash(rel), Field: hit.Var,
				Message: fmt.Sprintf("line %d: %s", hit.Line, hit.Message)})
		}
		return nil
	})
	return errs
}

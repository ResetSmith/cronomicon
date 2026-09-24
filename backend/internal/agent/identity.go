package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Identity is the persisted {id, apiKey} pair that lets a restarted agent
// resume its runners row instead of orphaning it on every restart (R2.4). The
// apiKey is the per-runner long-lived bearer (amt_run_*).
type Identity struct {
	ID     string `json:"id"`
	APIKey string `json:"apiKey"`
}

// loadIdentity reads the identity file. Returns (nil, nil) when the file does
// not exist (first run → register). A present-but-corrupt file is an error so
// we don't silently re-register and orphan the existing row.
func loadIdentity(path string) (*Identity, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var id Identity
	if err := json.Unmarshal(data, &id); err != nil {
		return nil, fmt.Errorf("identity file %q is corrupt: %w", path, err)
	}
	if id.ID == "" || id.APIKey == "" {
		return nil, fmt.Errorf("identity file %q missing id/apiKey", path)
	}
	return &id, nil
}

// saveIdentity atomically writes the identity to disk (write-temp-then-rename)
// with 0600 perms — it holds the runner's bearer key.
func saveIdentity(path string, id Identity) error {
	data, err := json.MarshalIndent(id, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	tmp, err := os.CreateTemp(dir, ".identity-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// discardIdentity removes the identity file so the agent re-registers (used on
// a 404 poll — the server reaped/deregistered this runner, D4).
func discardIdentity(path string) error {
	err := os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

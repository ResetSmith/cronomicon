package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/backup"
	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/httpx"
	"github.com/ResetSmith/cronomicon/internal/settings"
)

// Settings → Backups (2.5.1 SG1.3): the keys every nightly snapshot is
// encrypted to, and what the backups have been. A global administrator's,
// the GET too: it names the bucket and the local directory.
//
//	GET /api/v1/settings/backup  the keys, the last backup, the local copies, the target
//	PUT /api/v1/settings/backup  {recipients, acknowledgedKeyCustody, confirmUnencrypted}
//
// Encryption is optional (SG-4). Turning it on needs acknowledgedKeyCustody
// (SG-8: a lost private key makes every encrypted backup unreadable); turning
// it off, confirmUnencrypted. There is no proof-of-possession step (question
// 125): the card shows each key's fingerprint for the administrator to compare.
func (s *Server) mountBackupSettings(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/settings/backup", s.requireGlobal(auth.PermConfigureApp)(http.HandlerFunc(s.getBackupSettings)))
	mux.Handle("PUT /api/v1/settings/backup", s.requireGlobal(auth.PermConfigureApp)(http.HandlerFunc(s.putBackupSettings)))
}

// backupSettings is the card's state.
type backupSettings struct {
	settings.BackupKeys
	Encrypted     bool               `json:"encrypted"`
	LastBackup    *db.LastSnapshot   `json:"lastBackup"`
	LastSuccessAt string             `json:"lastSuccessAt,omitempty"`
	LocalBackups  []db.LocalSnapshot `json:"localBackups"`
	LocalDir      string             `json:"localDirectory"`
	LocalKeepDays int                `json:"localKeepDays"`
	Bucket        string             `json:"bucket"`
	Schedule      string             `json:"schedule"`
}

func (s *Server) backupState(r *http.Request) (backupSettings, error) {
	keys, err := settings.GetBackupKeys(r.Context(), s.db)
	if err != nil {
		return backupSettings{}, err
	}
	last, err := db.ReadLastSnapshot(r.Context(), s.db)
	if err != nil {
		return backupSettings{}, err
	}
	local, err := db.ListLocalSnapshots(db.DefaultBackupDir)
	if err != nil {
		s.log.Warn("settings/backup: could not list the local backups", "dir", db.DefaultBackupDir, "error", err)
		local = []db.LocalSnapshot{}
	}
	var lastAt string
	_ = s.db.QueryRowContext(r.Context(), `SELECT COALESCE(value, '') FROM settings WHERE key = 'backupLastSuccessAt'`).Scan(&lastAt)
	out := backupSettings{
		BackupKeys:    keys,
		Encrypted:     len(keys.Recipients) > 0,
		LastBackup:    last,
		LastSuccessAt: lastAt,
		LocalBackups:  local,
		LocalDir:      db.DefaultBackupDir,
		LocalKeepDays: db.SnapshotKeepDays,
		Schedule:      "02:00",
	}
	if s.cfg != nil {
		out.Bucket = s.cfg.BackupS3Bucket
		if s.cfg.BackupAt != "" {
			out.Schedule = s.cfg.BackupAt
		}
	}
	return out, nil
}

func (s *Server) getBackupSettings(w http.ResponseWriter, r *http.Request) {
	st, err := s.backupState(r)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	httpx.JSON(w, http.StatusOK, st)
}

func (s *Server) putBackupSettings(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.IdentityFrom(r.Context())
	var in struct {
		Recipients             *[]string `json:"recipients"`
		AcknowledgedKeyCustody bool      `json:"acknowledgedKeyCustody"`
		ConfirmUnencrypted     bool      `json:"confirmUnencrypted"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", "invalid request body")
		return
	}
	// The route replaces the list, so the field is required: a non-strict
	// decode would read {} as "remove every key".
	if in.Recipients == nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", "recipients is required (an empty list turns encryption off)")
		return
	}
	_, err := settings.SetBackupRecipients(r.Context(), s.db, *in.Recipients, in.AcknowledgedKeyCustody, in.ConfirmUnencrypted, id.Email)
	switch {
	case errors.Is(err, backup.ErrInvalidRecipient):
		httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_recipient", err.Error())
		return
	case errors.Is(err, settings.ErrBackupKeyCustody):
		httpx.Fail(w, http.StatusUnprocessableEntity, "acknowledgement_required", err.Error())
		return
	case errors.Is(err, settings.ErrBackupUnencryptedConfirm):
		httpx.Fail(w, http.StatusUnprocessableEntity, "confirmation_required", err.Error())
		return
	case err != nil:
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	st, err := s.backupState(r)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	// SG-7: someone who can change the keys can point later backups at a key
	// of their own, so Global's alert rules hear of every change.
	summary := "Backups are no longer encrypted."
	if len(st.Recipients) > 0 {
		fps := make([]string, 0, len(st.Recipients))
		for _, k := range st.Recipients {
			fps = append(fps, k.Fingerprint)
		}
		summary = "Backups are encrypted to: " + strings.Join(fps, ", ") + "."
	}
	s.raiseBackupKeyAlert(r, summary)
	httpx.JSON(w, http.StatusOK, st)
}

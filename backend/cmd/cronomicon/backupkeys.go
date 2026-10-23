package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"

	"github.com/ResetSmith/cronomicon/internal/settings"
)

// seedBackupKeys seeds the backup keys from CRONOMICON_BACKUP_RECIPIENTS once
// (2.5.1 SG-3). A variable that is set but holds no usable key, with no list
// stored, refuses the start (SG6.3, the review's A7): the server used to log
// it and start, and its boot backup was written and uploaded unencrypted.
// Beside a stored list it is logged, and the stored list applies.
func seedBackupKeys(ctx context.Context, pool *sql.DB, raw string, logger *slog.Logger) error {
	if seeded, serr := settings.SeedBackupRecipientsFromEnv(ctx, pool, raw); errors.Is(serr, settings.ErrBackupRecipientsEnvUnusable) {
		return fmt.Errorf("backup keys: %w", serr)
	} else if serr != nil {
		logger.Error("could not seed the backup keys from CRONOMICON_BACKUP_RECIPIENTS; backups stay as Settings → Backups has them", "error", serr)
	} else if seeded {
		logger.Info("backup keys seeded from CRONOMICON_BACKUP_RECIPIENTS")
	}
	return nil
}

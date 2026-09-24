package db

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"

	"github.com/golang-migrate/migrate/v4"
	sqlite3 "github.com/golang-migrate/migrate/v4/database/sqlite3"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

// migrationsFS holds the numbered NNN_name.{up,down}.sql files (T4).
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrate applies all pending up-migrations. Idempotent: a no-op if current.
func Migrate(pool *sql.DB) error {
	m, err := migrator(pool)
	if err != nil {
		return err
	}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}

// Status returns the current migration version and whether the schema is in a
// dirty (failed-mid-migration) state. Used by the /readyz check (T13).
func Status(pool *sql.DB) (version uint, dirty bool, err error) {
	m, err := migrator(pool)
	if err != nil {
		return 0, false, err
	}
	version, dirty, err = m.Version()
	if errors.Is(err, migrate.ErrNilVersion) {
		return 0, false, nil
	}
	return version, dirty, err
}

func migrator(pool *sql.DB) (*migrate.Migrate, error) {
	src, err := iofs.New(migrationsFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("load embedded migrations: %w", err)
	}
	drv, err := sqlite3.WithInstance(pool, &sqlite3.Config{})
	if err != nil {
		return nil, fmt.Errorf("migrate driver: %w", err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "sqlite3", drv)
	if err != nil {
		return nil, fmt.Errorf("init migrator: %w", err)
	}
	return m, nil
}

// ReadyCheck returns a probe that fails if the DB is unreachable, the schema
// is dirty, or the DB is not writable (e.g. volume full / read-only mount).
// Wired into /readyz (T13). A read-only DB passes ping but silently fails all
// mutations — the write probe catches that before traffic is routed in (PP-M3).
func ReadyCheck(pool *sql.DB) func(context.Context) error {
	return func(ctx context.Context) error {
		if err := pool.PingContext(ctx); err != nil {
			return fmt.Errorf("db unreachable: %w", err)
		}
		_, dirty, err := Status(pool)
		if err != nil {
			return fmt.Errorf("migration status: %w", err)
		}
		if dirty {
			return errors.New("schema is dirty (failed migration)")
		}
		// Write probe: verify the volume is writable and SQLite can commit.
		if _, err := pool.ExecContext(ctx,
			`INSERT INTO settings(key,value) VALUES('_readyz','1')
			 ON CONFLICT(key) DO UPDATE SET value=excluded.value`); err != nil {
			return fmt.Errorf("db not writable: %w", err)
		}
		return nil
	}
}

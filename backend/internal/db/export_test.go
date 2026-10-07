package db

import (
	"database/sql"
	"errors"

	"github.com/golang-migrate/migrate/v4"
)

// MigrateTo moves a database to one schema version. Test-only (this file is
// compiled into the test binary alone): it is how an external test package
// seeds a database at the PREVIOUS schema and then reads it through the
// packages that import this one, which an internal test cannot import.
func MigrateTo(pool *sql.DB, version uint) error {
	m, err := migrator(pool)
	if err != nil {
		return err
	}
	if err := m.Migrate(version); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return err
	}
	return nil
}

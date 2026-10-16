package execspec

import (
	"context"
	"database/sql"
	"testing"
)

// mustOwner is the agency the one scope of this name belongs to: what a test
// that names a scope asks as (I2: a scope's name is unique within an agency).
func mustOwner(t *testing.T, pool *sql.DB, scope string) string {
	t.Helper()
	a, err := ScopeOwnerByName(context.Background(), pool, scope)
	if err != nil {
		t.Fatalf("owner of scope %q: %v", scope, err)
	}
	return a
}

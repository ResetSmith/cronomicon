package agencyid_test

import (
	"path/filepath"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/agencyid"
	"github.com/ResetSmith/cronomicon/internal/db"
)

// The no-owner rule (2.4.4): NULL, the empty string and 'global' are all Global's, and an
// agency's id is that agency's.
func TestOwnerReadsEveryWayOfSayingNoOwnerAsGlobal(t *testing.T) {
	pool, err := db.Open(filepath.Join(t.TempDir(), "o.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	for _, c := range []struct {
		in   any
		want string
	}{{nil, "global"}, {"", "global"}, {"global", "global"}, {"ag:FIN", "ag:FIN"}} {
		var got string
		if err := pool.QueryRow(`SELECT `+agencyid.Owner("?1"), c.in).Scan(&got); err != nil {
			t.Fatalf("%v: %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("Owner(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

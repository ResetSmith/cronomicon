package runref

import (
	"context"
	"database/sql"
	"testing"
)

// R2F-1 — reference bindings follow the owner's IDENTITY.
//
// Since R2-5 two departments may own a job of the same name. The binding store
// read and wrote by (owner_kind, owner_source, owner_name), so the twins shared
// one namespace: a full replace on one deleted the other's rows, and a list
// returned the union. Secret RESOLUTION was never affected (it stays scope-gated,
// with RA-17's fail-closed ambiguity rule), but editing one twin's bindings
// destroyed the other's — cross-agency data loss on an ordinary save.

// seedTwinJobs inserts two amadeus jobs sharing a name, each with its own uid,
// and returns the two owners. The uids follow the band's 'uid-'||name convention
// with a suffix, because the whole point is that the NAMES are identical.
func seedTwinJobs(t *testing.T, pool *sql.DB, name string) (a, b Owner) {
	t.Helper()
	const now = "2026-01-01T00:00:00Z"
	for _, uid := range []string{"uid-" + name + "-a", "uid-" + name + "-b"} {
		if _, err := pool.Exec(`INSERT INTO jobs(uid, name, source, run_type, command, content_hash, synced_at)
			VALUES(?,?,'amadeus','bash','echo','sha256:x',?)`, uid, name, now); err != nil {
			t.Fatalf("seed twin %s: %v", uid, err)
		}
	}
	return Owner{Kind: "job", Source: "amadeus", Name: name, UID: "uid-" + name + "-a"},
		Owner{Kind: "job", Source: "amadeus", Name: name, UID: "uid-" + name + "-b"}
}

func bindingNames(bs []Binding) []string {
	out := make([]string, 0, len(bs))
	for _, b := range bs {
		out = append(out, b.Name)
	}
	return out
}

// The defect itself: saving one twin's set must leave the other's alone. Asserted
// in BOTH directions, because a delete keyed on the wrong column is symmetric and
// a one-way test would pass on half a fix.
func TestTwinBindingsSurviveEachOthersSave(t *testing.T) {
	pool := openDB(t)
	ctx := context.Background()
	a, b := seedTwinJobs(t, pool, "deploy")

	if err := ReplaceBindings(ctx, pool, a, []Binding{{Kind: KindSecret, Name: "A_PASS"}}, "alice"); err != nil {
		t.Fatalf("save A: %v", err)
	}
	if err := ReplaceBindings(ctx, pool, b, []Binding{{Kind: KindSecret, Name: "B_PASS"}}, "bob"); err != nil {
		t.Fatalf("save B: %v", err)
	}

	// B's save must not have taken A's rows with it.
	got, err := ListBindings(ctx, pool, a)
	if err != nil {
		t.Fatalf("list A: %v", err)
	}
	if len(got) != 1 || got[0].Name != "A_PASS" {
		t.Errorf("A's bindings after B saved = %v, want [A_PASS] only", bindingNames(got))
	}

	// And the reverse: re-saving A must not disturb B.
	if err := ReplaceBindings(ctx, pool, a, []Binding{{Kind: KindSecret, Name: "A_PASS2"}}, "alice"); err != nil {
		t.Fatalf("re-save A: %v", err)
	}
	got, err = ListBindings(ctx, pool, b)
	if err != nil {
		t.Fatalf("list B: %v", err)
	}
	if len(got) != 1 || got[0].Name != "B_PASS" {
		t.Errorf("B's bindings after A re-saved = %v, want [B_PASS] only", bindingNames(got))
	}
}

// The read half: neither twin may see the other's declared references. This is
// what the enforcement paths (RA-24's unbound probe, the claim gate, dispatch's
// resolve) all sit on, so a union here is a union everywhere.
func TestTwinBindingsAreNotUnioned(t *testing.T) {
	pool := openDB(t)
	ctx := context.Background()
	a, b := seedTwinJobs(t, pool, "report")

	if err := ReplaceBindings(ctx, pool, a, []Binding{{Kind: KindVar, Name: "REGION"}}, "alice"); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceBindings(ctx, pool, b, []Binding{{Kind: KindVar, Name: "TENANT"}}, "bob"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		owner Owner
		want  string
	}{{a, "REGION"}, {b, "TENANT"}} {
		got, err := ListBindings(ctx, pool, tc.owner)
		if err != nil {
			t.Fatalf("list %s: %v", tc.owner.UID, err)
		}
		if len(got) != 1 || got[0].Name != tc.want {
			t.Errorf("%s sees %v, want [%s] only", tc.owner.UID, bindingNames(got), tc.want)
		}
	}
}

// Scripts keep name identity permanently (they stayed outside AF-4b), so the name
// arm must still serve them — including when a JOB of the same name exists, whose
// uid must not be borrowed for the script's rows.
func TestScriptBindingsKeepNameIdentity(t *testing.T) {
	pool := openDB(t)
	ctx := context.Background()
	const now = "2026-01-01T00:00:00Z"
	if _, err := pool.Exec(`INSERT INTO scripts(name, run_type, command, content_hash, synced_at)
		VALUES('build','bash','echo','sha256:y',?)`, now); err != nil {
		t.Fatalf("seed script: %v", err)
	}
	seedTwinJobs(t, pool, "build") // same name, different catalog

	// A uid on a script owner is a caller error and must be ignored, not written:
	// filed under a job's identity, these rows would never be read back.
	script := Owner{Kind: "script", Name: "build", UID: "uid-build-a"}
	if err := ReplaceBindings(ctx, pool, script, []Binding{{Kind: KindSecret, Name: "SIGNING_KEY"}}, "alice"); err != nil {
		t.Fatalf("save script bindings: %v", err)
	}
	got, err := ListBindings(ctx, pool, Owner{Kind: "script", Name: "build"})
	if err != nil {
		t.Fatalf("list script: %v", err)
	}
	if len(got) != 1 || got[0].Name != "SIGNING_KEY" {
		t.Errorf("script bindings = %v, want [SIGNING_KEY]", bindingNames(got))
	}
	var uid sql.NullString
	if err := pool.QueryRow(`SELECT owner_uid FROM reference_bindings WHERE owner_kind='script'`).Scan(&uid); err != nil {
		t.Fatalf("read owner_uid: %v", err)
	}
	if uid.Valid {
		t.Errorf("script binding stamped owner_uid = %q, want NULL", uid.String)
	}
	// The job twins must be unaffected by the script's name collision.
	if got, _ := ListBindings(ctx, pool, Owner{Kind: "job", Source: "amadeus", Name: "build", UID: "uid-build-a"}); len(got) != 0 {
		t.Errorf("job twin sees the script's bindings: %v", bindingNames(got))
	}
}

// A uid-less job owner keeps the legacy shared name-keyed namespace it always
// had — the store must not guess which twin it meant. But when the name resolves
// to exactly ONE job, the insert still stamps that uid: the delete trigger (1050)
// cascades by owner_uid alone, so an unstamped row would outlive its owner and a
// later same-named job would silently inherit the grant.
func TestUIDLessJobOwnerStampsOnlyWhenUnambiguous(t *testing.T) {
	pool := openDB(t)
	ctx := context.Background()
	const now = "2026-01-01T00:00:00Z"
	if _, err := pool.Exec(`INSERT INTO jobs(uid, name, source, run_type, command, content_hash, synced_at)
		VALUES('uid-solo','solo','amadeus','bash','echo','sha256:x',?)`, now); err != nil {
		t.Fatalf("seed job: %v", err)
	}
	solo := Owner{Kind: "job", Source: "amadeus", Name: "solo"} // no UID
	if err := ReplaceBindings(ctx, pool, solo, []Binding{{Kind: KindVar, Name: "REGION"}}, "alice"); err != nil {
		t.Fatal(err)
	}
	var uid sql.NullString
	if err := pool.QueryRow(`SELECT owner_uid FROM reference_bindings WHERE owner_name='solo'`).Scan(&uid); err != nil {
		t.Fatalf("read owner_uid: %v", err)
	}
	if uid.String != "uid-solo" {
		t.Errorf("unambiguous name stamped %q, want uid-solo (the cascade depends on it)", uid.String)
	}
	// Reading by uid finds it, and so does reading by name — the same row.
	byUID, _ := ListBindings(ctx, pool, Owner{Kind: "job", Source: "amadeus", Name: "solo", UID: "uid-solo"})
	if len(byUID) != 1 {
		t.Errorf("uid read of a name-written binding = %v, want it found", bindingNames(byUID))
	}

	// Ambiguous: no stamp, because there is no non-guessing answer.
	twinA, _ := seedTwinJobs(t, pool, "twinned")
	nameOnly := Owner{Kind: "job", Source: "amadeus", Name: "twinned"}
	if err := ReplaceBindings(ctx, pool, nameOnly, []Binding{{Kind: KindVar, Name: "TENANT"}}, "alice"); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(`SELECT owner_uid FROM reference_bindings WHERE owner_name='twinned'`).Scan(&uid); err != nil {
		t.Fatalf("read owner_uid: %v", err)
	}
	if uid.Valid {
		t.Errorf("ambiguous name stamped %q, want NULL (no guess)", uid.String)
	}
	// And neither twin's identity-keyed read picks it up: the legacy row belongs
	// to the name, which is exactly what a caller that could not name a twin got.
	if got, _ := ListBindings(ctx, pool, twinA); len(got) != 0 {
		t.Errorf("twin A inherited a name-keyed legacy binding: %v", bindingNames(got))
	}
}

// The cascade, stated for the identity path: deleting one twin clears ITS rows
// and only its rows. TestBindingsClearedOnOwnerDelete covers the single-job case.
func TestTwinDeleteCascadesOnlyItsOwn(t *testing.T) {
	pool := openDB(t)
	ctx := context.Background()
	a, b := seedTwinJobs(t, pool, "nightly")
	if err := ReplaceBindings(ctx, pool, a, []Binding{{Kind: KindSecret, Name: "A_PASS"}}, "alice"); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceBindings(ctx, pool, b, []Binding{{Kind: KindSecret, Name: "B_PASS"}}, "bob"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(`DELETE FROM jobs WHERE uid = ?`, a.UID); err != nil {
		t.Fatal(err)
	}
	if got, _ := ListBindings(ctx, pool, a); len(got) != 0 {
		t.Errorf("deleted twin's bindings survived: %v", bindingNames(got))
	}
	if got, _ := ListBindings(ctx, pool, b); len(got) != 1 || got[0].Name != "B_PASS" {
		t.Errorf("surviving twin's bindings = %v, want [B_PASS]", bindingNames(got))
	}
}

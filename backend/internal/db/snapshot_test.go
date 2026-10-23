package db

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"
)

// fixedDay pins nowUTC for one test.
func fixedDay(t *testing.T, day time.Time) {
	t.Helper()
	orig := nowUTC
	var mu sync.Mutex
	nowUTC = func() time.Time { mu.Lock(); defer mu.Unlock(); return day }
	t.Cleanup(func() { nowUTC = orig })
}

// fakeSeal stands in for internal/backup: it copies the plaintext to dst with a
// marker in front (so a test can tell it was "sealed") and writes the
// recipients file, as backup.EncryptFile does.
func fakeSeal(fps ...string) func(context.Context, string, string) ([]string, error) {
	return func(_ context.Context, src, dst string) ([]string, error) {
		b, err := os.ReadFile(src)
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(dst, append([]byte("SEALED:"), b...), 0o600); err != nil {
			return nil, err
		}
		if err := os.WriteFile(dst+".recipients", []byte("x\n"), 0o600); err != nil {
			return nil, err
		}
		return fps, nil
	}
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// SG-6: with a key set, the snapshot is kept and shipped sealed, the staging
// directory is empty afterwards, and no plaintext is in the backup directory.
func TestSnapshotSealedWhenAKeyIsSet(t *testing.T) {
	pool := migratedPool(t)
	dir := t.TempDir()
	fixedDay(t, time.Date(2026, 10, 21, 2, 0, 0, 0, time.UTC))
	var uploaded string
	p := RetentionPolicy{BackupDir: dir, Seal: fakeSeal("fp1", "fp2"),
		Upload: func(_ context.Context, path string) error { uploaded = path; return nil }}
	if err := runSweep(context.Background(), pool, p, testLogger()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	want := filepath.Join(dir, "cronomicon-20261021.db.age")
	if uploaded != want {
		t.Fatalf("uploaded %q, want %q", uploaded, want)
	}
	if got := dirNames(t, dir); !slices.Equal(got, []string{".staging", "cronomicon-20261021.db.age", "cronomicon-20261021.db.age.recipients"}) {
		t.Fatalf("backup dir holds %v", got)
	}
	if got := dirNames(t, filepath.Join(dir, ".staging")); len(got) != 0 {
		t.Fatalf("staging not emptied: %v", got)
	}
	st, err := os.Stat(filepath.Join(dir, ".staging"))
	if err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("staging mode %v (%v), want 0700", st.Mode().Perm(), err)
	}
	last, err := ReadLastSnapshot(context.Background(), pool)
	if err != nil || last == nil || last.File != "cronomicon-20261021.db.age" || !slices.Equal(last.Recipients, []string{"fp1", "fp2"}) {
		t.Fatalf("last snapshot %+v (%v)", last, err)
	}
}

// SG-4: encryption is optional. A Seal that reports no key leaves the snapshot
// unencrypted, as before 2.5.1, in the backup directory with mode 0600.
func TestSnapshotUnencryptedWithoutAKey(t *testing.T) {
	pool := migratedPool(t)
	dir := t.TempDir()
	fixedDay(t, time.Date(2026, 10, 21, 2, 0, 0, 0, time.UTC))
	p := RetentionPolicy{BackupDir: dir, Seal: fakeSeal()}
	if err := runSweep(context.Background(), pool, p, testLogger()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	snap := filepath.Join(dir, "cronomicon-20261021.db")
	st, err := os.Stat(snap)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("snapshot %v mode %v, want 0600", err, st.Mode().Perm())
	}
	if _, err := Open(snap); err != nil {
		t.Fatalf("unencrypted snapshot not a database: %v", err)
	}
	last, _ := ReadLastSnapshot(context.Background(), pool)
	if last == nil || len(last.Recipients) != 0 {
		t.Fatalf("last snapshot %+v, want unencrypted", last)
	}
}

// SG-5: an encryption failure fails the backup and leaves nothing behind: no
// plaintext, no partial sealed file, nothing uploaded, no last-success.
func TestSnapshotSealFailureKeepsNothing(t *testing.T) {
	pool := migratedPool(t)
	dir := t.TempDir()
	fixedDay(t, time.Date(2026, 10, 21, 2, 0, 0, 0, time.UTC))
	uploaded := false
	p := RetentionPolicy{BackupDir: dir,
		Seal: func(_ context.Context, _, dst string) ([]string, error) {
			_ = os.WriteFile(dst, []byte("partial"), 0o600)
			return nil, errors.New("disk full")
		},
		Upload: func(context.Context, string) error { uploaded = true; return nil }}
	if err := runSweep(context.Background(), pool, p, testLogger()); err == nil {
		t.Fatal("sweep succeeded with a failing seal")
	}
	if uploaded {
		t.Fatal("a failed seal uploaded something")
	}
	if got := dirNames(t, dir); !slices.Equal(got, []string{".staging"}) {
		t.Fatalf("backup dir holds %v after a failed seal", got)
	}
	if got := dirNames(t, filepath.Join(dir, ".staging")); len(got) != 0 {
		t.Fatalf("plaintext left in staging: %v", got)
	}
	if !backupOverdue(context.Background(), pool, backupInterval) {
		t.Fatal("a failed backup recorded a success")
	}
}

// An upload failure after a successful seal still leaves no plaintext.
func TestSnapshotUploadFailureLeavesNoPlaintext(t *testing.T) {
	pool := migratedPool(t)
	dir := t.TempDir()
	fixedDay(t, time.Date(2026, 10, 21, 2, 0, 0, 0, time.UTC))
	p := RetentionPolicy{BackupDir: dir, Seal: fakeSeal("fp"),
		Upload: func(context.Context, string) error { return errors.New("bucket gone") }}
	if err := runSweep(context.Background(), pool, p, testLogger()); err == nil {
		t.Fatal("sweep succeeded with a failing upload")
	}
	if got := dirNames(t, filepath.Join(dir, ".staging")); len(got) != 0 {
		t.Fatalf("plaintext left in staging: %v", got)
	}
}

// A plaintext a crash left in staging is removed when retention starts.
func TestStagingClearedAtStart(t *testing.T) {
	dir := t.TempDir()
	staging := filepath.Join(dir, ".staging")
	if err := os.MkdirAll(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "cronomicon-20261020.db"), []byte("plaintext"), 0o600); err != nil {
		t.Fatal(err)
	}
	clearStaging(dir, testLogger())
	if got := dirNames(t, staging); len(got) != 0 {
		t.Fatalf("staging holds %v", got)
	}
}

// A same-day re-run replaces the day's snapshot in whichever form it was, so a
// day never holds both an unencrypted and an encrypted copy.
func TestSameDayRerunReplacesTheOtherForm(t *testing.T) {
	pool := migratedPool(t)
	dir := t.TempDir()
	fixedDay(t, time.Date(2026, 10, 21, 2, 0, 0, 0, time.UTC))
	if err := runSweep(context.Background(), pool, RetentionPolicy{BackupDir: dir}, testLogger()); err != nil {
		t.Fatal(err)
	}
	if err := runSweep(context.Background(), pool, RetentionPolicy{BackupDir: dir, Seal: fakeSeal("fp")}, testLogger()); err != nil {
		t.Fatal(err)
	}
	if got := dirNames(t, dir); !slices.Equal(got, []string{".staging", "cronomicon-20261021.db.age", "cronomicon-20261021.db.age.recipients"}) {
		t.Fatalf("backup dir holds %v", got)
	}
}

// PP-M5 in both forms: files older than 7 days go, today's and the week's stay,
// and anything that is not a snapshot's name is never touched.
func TestPruneSnapshotsBothForms(t *testing.T) {
	dir := t.TempDir()
	fixedDay(t, time.Date(2026, 10, 21, 2, 0, 0, 0, time.UTC))
	names := []string{
		"cronomicon-20261010.db", "cronomicon-20261010.db.age", "cronomicon-20261010.db.age.recipients",
		"cronomicon-20261014.db", "cronomicon-20261015.db.age", "cronomicon-20261015.db.age.recipients",
		"cronomicon-20261021.db.age", "cronomicon-20261001.db.bak", "notes.txt", "cronomicon-latest.db",
	}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	pruneSnapshots(dir, testLogger())
	want := []string{
		"cronomicon-20261001.db.bak", "cronomicon-20261014.db", "cronomicon-20261015.db.age", "cronomicon-20261015.db.age.recipients",
		"cronomicon-20261021.db.age", "cronomicon-latest.db", "notes.txt",
	}
	if got := dirNames(t, dir); !slices.Equal(got, want) {
		t.Fatalf("after prune %v, want %v", got, want)
	}
}

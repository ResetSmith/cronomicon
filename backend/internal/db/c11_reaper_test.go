package db

import (
	"context"
	"sort"
	"strings"
	"testing"
)

// 2.5.0 C11, defect 78: the recycle-bin reaper hands its purger each binned
// definition's uid. By name alone, a name two agencies had binned was refused
// as ambiguous every night and never purged.
func TestC11_TheReaperNamesEachBinnedDefinitionByItsUID(t *testing.T) {
	pool := migratedPool(t)
	exec := func(q string) {
		t.Helper()
		if _, err := pool.Exec(q); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	exec(`INSERT INTO agencies (id, name, created_at) VALUES ('ag-f', 'Fin', 't'), ('ag-t', 'Tax', 't')`)
	exec(`INSERT INTO jobs (uid, name, source, run_type, owner_agency, command, created_at, deleted_at) VALUES
	        ('j-f', 'cleanup', 'cronomicon', 'bash', 'ag-f', 'true', 't', '2020-01-01T00:00:00Z'),
	        ('j-t', 'cleanup', 'cronomicon', 'bash', 'ag-t', 'true', 't', '2020-01-01T00:00:00Z')`)
	var got []string
	p := RetentionPolicy{RecycleBinDays: 30, PurgeDefinition: func(_ context.Context, kind, name, uid string) (bool, error) {
		got = append(got, kind+":"+name+":"+uid)
		return true, nil
	}}
	if err := purgeExpiredDefinitions(context.Background(), pool, p, testLogger()); err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != "job:cleanup:j-f,job:cleanup:j-t" {
		t.Errorf("the reaper asked to purge %v; want each binned cleanup by its uid", got)
	}
}

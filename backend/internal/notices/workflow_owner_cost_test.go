package notices_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/notices"
)

// SpanningWorkflows reads the workflow graph once (2.4.0 Phase R7; found by
// the review of R6's second part and R7).
//
// Every save of a job or a workflow by anybody but a global administrator runs
// it twice inside its write transaction (GR-7 after a save), so its cost is a
// lock that every other writer waits on, five seconds at most before a writer
// gives up. Its first form asked the per-step rows of each workflow
// separately: 200 workflows of eight steps and a sub-workflow each took five
// seconds, 300 more than ten, 2,000 eight minutes. Read once, 300 take tens of
// milliseconds; the bound here is wide enough for the race detector and a
// slow machine, and far below the first form.
func TestSpanningWorkflowsReadsTheGraphOnce(t *testing.T) {
	pool := open(t)
	ctx := context.Background()
	mustExec(t, pool, `INSERT INTO agencies (id, name, created_at) VALUES ('ag-fin', 'Finance', 't'), ('ag-tax', 'Tax', 't')`)
	for _, s := range [][3]string{{"sc-fin", "fin-hosts", "ag-fin"}, {"sc-tax", "tax-hosts", "ag-tax"}} {
		mustExec(t, pool, `INSERT INTO scopes (id, name, source, created_at) VALUES (?, ?, 'cronomicon', 't')`, s[0], s[1])
		mustExec(t, pool, `INSERT INTO scope_agencies (scope_id, agency_id) VALUES (?, ?)`, s[0], s[2])
	}
	const n = 300
	tx, err := pool.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2*n; i++ {
		scope := "fin-hosts"
		if i == 2*n-1 {
			scope = "tax-hosts" // the one TAX job, run by the last workflow alone
		}
		if _, err := tx.Exec(`INSERT INTO jobs (uid, name, source, run_type, scope, synced_at) VALUES (?, ?, 'cronomicon', 'bash', ?, 't')`,
			fmt.Sprintf("j%d", i), fmt.Sprintf("job-%d", i), scope); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < n; i++ {
		steps := "["
		for k := 0; k < 8; k++ {
			if k > 0 {
				steps += ","
			}
			steps += fmt.Sprintf(`{"type":"job","name":"job-%d"}`, (i*8+k)%(2*n-1))
		}
		if i > 0 {
			steps += fmt.Sprintf(`,{"type":"workflow","name":"go","workflow":"wf-%d"}`, i-1)
		}
		if i == n-1 {
			steps += fmt.Sprintf(`,{"type":"job","name":"job-%d"}`, 2*n-1)
		}
		steps += "]"
		if _, err := tx.Exec(`INSERT INTO workflows (uid, name, source, steps, enabled, synced_at) VALUES (?, ?, 'cronomicon', ?, 1, 't')`,
			fmt.Sprintf("w%d", i), fmt.Sprintf("wf-%d", i), steps); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	spanning, err := notices.SpanningWorkflows(ctx, pool)
	took := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if took > 4*time.Second {
		t.Errorf("SpanningWorkflows over %d workflows took %v: it reads the graph per workflow again", n, took)
	}
	// And it is still right: the last workflow runs TAX's job beside FIN's.
	if len(spanning) != 1 || spanning[0].UID != fmt.Sprintf("w%d", n-1) || spanning[0].Agencies != "Finance, Tax" {
		t.Errorf("spanning = %+v, want the last workflow alone, of Finance and Tax", spanning)
	}
	t.Logf("%d workflows: %v", n, took)
}

package preflight_test

import (
	"bytes"
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/preflight"
)

// at2210 is a database on the last 2.2 schema, which is what the 2.3.0 section
// is run against: no Global agency row, "no agency" as the absence of
// membership rows, host keys stored on the records.
func at2210(t *testing.T) *sql.DB {
	t.Helper()
	pool, err := db.Open(filepath.Join(t.TempDir(), "pf230.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	if err := db.MigrateTo(pool, 1210); err != nil {
		t.Fatal(err)
	}
	return pool
}

func TestThe230SectionSaysWhatTheUpgradeWillFind(t *testing.T) {
	pool := at2210(t)
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	exec(`INSERT INTO agencies (id, name, created_at) VALUES ('ag-fin', 'Finance', 't'), ('ag-tax', 'Tax', 't')`)
	exec(`INSERT INTO scopes (id, name, source, created_at) VALUES
	        ('sc-fin', 'fin-prod', 'cronomicon', 't'), ('sc-two', 'shared', 'cronomicon', 't'),
	        ('sc-tax', 'tax-prod', 'cronomicon', 't'), ('sc-pool', 'nobody', 'cronomicon', 't'),
	        ('sc-bound', 'fin-bound', 'cronomicon', 't')`)
	exec(`INSERT INTO scope_agencies VALUES ('sc-fin', 'ag-fin'), ('sc-two', 'ag-fin'), ('sc-two', 'ag-tax'),
	        ('sc-tax', 'ag-tax'), ('sc-bound', 'ag-fin')`)
	exec(`INSERT INTO scope_hosts (scope_id, host) VALUES ('sc-fin', 'web01')`)

	// Runners: one agency each, one in both (a legacy placement to be), one in none.
	runner := func(id, name, caps string, agencies ...string) {
		exec(`INSERT INTO runners (id, name, status, capabilities, registered_at, created_at) VALUES (?, ?, 'online', ?, 't', 't')`, id, name, caps)
		for _, a := range agencies {
			exec(`INSERT INTO runner_agencies VALUES (?, ?)`, id, a)
		}
	}
	runner("r-fin", "fin-agent", `["bash","ansible"]`, "ag-fin")
	runner("r-tax", "tax-ansible", `["ansible"]`, "ag-tax")
	runner("r-both", "both-agent", `["python"]`, "ag-fin", "ag-tax")
	runner("r-pool", "pool-agent", `["bash"]`)
	exec(`INSERT INTO scope_runners (scope_id, runner_id, runner_name, bound_by, bound_at) VALUES ('sc-bound', 'r-fin', 'fin-agent', 'x', 't')`)

	job := func(name, runType, scope, executor, requires, target string) {
		exec(`INSERT INTO jobs (uid, name, source, run_type, scope, executor, requires_json, target_host, concurrency_policy, enabled, synced_at)
		      VALUES (?, ?, 'git', ?, NULLIF(?, ''), NULLIF(?, ''), COALESCE(NULLIF(?, ''), '[]'), NULLIF(?, ''), 'Allow', 1, 't')`,
			"u-"+name, name, runType, scope, executor, requires, target)
	}
	job("fin-default", "bash", "fin-prod", "", "", "web01")         // resolved to ssh; Finance has agents with a shell type
	job("fin-off-scope", "bash", "fin-prod", "ssh", "", "db99")     // its target is not a host of the scope
	job("tax-runner", "python", "tax-prod", "runner", `["jq"]`, "") // ran on agents, where requires was already read
	job("fin-needs", "bash", "fin-prod", "", `["jq"]`, "")          // ran from the server, which ignored requires
	job("bound-needs", "bash", "fin-bound", "ssh", `["jq"]`, "")    // a bound scope's jobs ran on its runners
	job("tax-default", "bash", "tax-prod", "", "", "")              // …and one that ran from the server: the scope is both
	job("pooled", "perl", "nobody", "", "", "")                     // a scope in no agency
	job("unscoped", "bash", "", "", "", "")                         // no scope at all
	job("bound", "bash", "fin-bound", "ssh", "", "")                // a bound scope is not the pass's business
	job("play", "ansible", "fin-prod", "", `["vault"]`, "")         // not a shell job
	exec(`INSERT INTO scripts (name, run_type, executor, content_hash, synced_at) VALUES ('s1', 'bash', 'runner', 'h', 't')`)

	exec(`INSERT INTO secrets (id, key, scope, source, created_at) VALUES
	        ('s-two', 'DB_PASS', '', 'stored', 't'), ('s-one', 'API_KEY', 'fin-prod', 'stored', 't'),
	        ('s-vault', 'VAULTED', '', 'vault', 't')`)
	exec(`INSERT INTO secret_agencies VALUES ('s-two', 'ag-fin'), ('s-two', 'ag-tax'), ('s-one', 'ag-fin'), ('s-vault', 'ag-tax')`)
	exec(`INSERT INTO ssh_credentials (id, label, source, created_at) VALUES ('k-v', 'vault_key', 'vault', 't')`)
	exec(`INSERT INTO ssh_credential_agencies VALUES ('k-v', 'ag-fin')`)

	exec(`INSERT INTO ssh_hosts (id, hostname, address, port, host_key, created_at) VALUES
	        ('h1', 'web01', '10.0.0.5', 22, 'ssh-ed25519 AAAAone', 't'),
	        ('h2', 'web01-alias', '10.0.0.5', 22, 'ssh-ed25519 AAAAtwo', 't'),
	        ('h3', 'fresh', '10.0.0.7', 22, NULL, 't')`)
	exec(`INSERT INTO bastions (id, hostname, name, address, port, created_at) VALUES ('b1', 'jump.example', 'jump', '10.0.0.9', 22, 't')`)

	rep, err := preflight.Build230(ctx, pool)
	if err != nil {
		t.Fatalf("Build230 against the 2.2 schema: %v", err)
	}

	shared := map[string]string{}
	for _, s := range rep.Shared {
		shared[s.Kind+" "+s.Name] = strings.Join(s.Agencies, ",")
	}
	if len(shared) != 2 || shared["scope shared"] != "Finance,Tax" || shared["secret DB_PASS"] != "Finance,Tax" {
		t.Errorf("rows in several agencies = %v, want the scope and the secret, each with both", shared)
	}
	if len(rep.LegacyRunners) != 1 || rep.LegacyRunners[0].Name != "both-agent" || strings.Join(rep.LegacyRunners[0].Agencies, ",") != "Finance,Tax" {
		t.Errorf("legacy placements = %+v, want both-agent in Finance and Tax", rep.LegacyRunners)
	}
	if strings.Join(rep.PoolRunners, ",") != "pool-agent" {
		t.Errorf("runners in no agency = %v, want pool-agent", rep.PoolRunners)
	}
	if rep.UnownedSecrets != 2 || rep.UnownedKeys != 1 {
		t.Errorf("unowned rows in one agency: %d secret(s), %d key(s); want 2 and 1", rep.UnownedSecrets, rep.UnownedKeys)
	}

	by := map[string]preflight.ShellScope{}
	for _, s := range rep.ShellScopes {
		by[s.Scope] = s
	}
	if _, bound := by["fin-bound"]; bound || len(by) != 4 {
		t.Fatalf("shell scopes = %+v, want fin-prod, tax-prod, nobody and the no-scope jobs — never a bound scope", by)
	}
	if s := by["fin-prod"]; !s.FromServer || s.OnAgents || s.Jobs != 3 || strings.Join(s.Agents, ",") != "both-agent,fin-agent" {
		t.Errorf("fin-prod = %+v, want from the server, 3 jobs, and both agents that serve Finance with a shell type", s)
	}
	if s := by["tax-prod"]; !s.FromServer || !s.OnAgents || strings.Join(s.Agents, ",") != "both-agent" {
		t.Errorf("tax-prod = %+v, want both places, and only the agent with a shell type (not the ansible-only one)", s)
	}
	if s := by["nobody"]; strings.Join(s.Agencies, ",") != "Global" || strings.Join(s.Agents, ",") != "pool-agent" {
		t.Errorf("a scope in no agency = %+v, want Global's, takeable by the runner in no agency", s)
	}
	if s := by[""]; strings.Join(s.Agencies, ",") != "Global" || s.Jobs != 1 {
		t.Errorf("the jobs with no scope = %+v, want Global's", s)
	}
	if got := strings.Join(rep.Placed(), ","); got != "Finance,Global,Tax" {
		t.Errorf("agencies the local runner will serve = %s, want Finance, Global and Tax", got)
	}

	// Only a shell job the SERVER ran: an agent already read requires on every
	// claim, for any run type, and a bound scope's jobs ran on its runners.
	if len(rep.RequiresJobs) != 1 || rep.RequiresJobs[0].Job != "fin-needs" {
		t.Errorf("shell jobs whose requires start to matter = %+v, want fin-needs only", rep.RequiresJobs)
	}
	if len(rep.TargetsOutsideScope) != 1 || rep.TargetsOutsideScope[0].Job != "fin-off-scope" {
		t.Errorf("targets outside their scope = %+v, want fin-off-scope", rep.TargetsOutsideScope)
	}
	if strings.Join(rep.HostsWithoutKey, ",") != "fresh (10.0.0.7:22)" || strings.Join(rep.BastionsWithoutKey, ",") != "jump (10.0.0.9:22)" {
		t.Errorf("records with no key = %v / %v", rep.HostsWithoutKey, rep.BastionsWithoutKey)
	}
	if len(rep.KeyConflicts) != 1 || rep.KeyConflicts[0].Address != "10.0.0.5:22" || len(rep.KeyConflicts[0].Records) != 2 {
		t.Errorf("key conflicts = %+v, want the two records at 10.0.0.5:22", rep.KeyConflicts)
	}
	if rep.ExecutorJobs != 4 || rep.ExecutorScripts != 1 {
		t.Errorf("executor keys: %d job(s), %d script(s); want 4 and 1", rep.ExecutorJobs, rep.ExecutorScripts)
	}
	if len(rep.VaultSecrets) != 1 || rep.VaultSecrets[0].Key != "VAULTED" || len(rep.VaultKeys) != 1 || rep.VaultKeys[0].Name != "vault_key" {
		t.Errorf("vault-backed rows = %+v / %+v", rep.VaultSecrets, rep.VaultKeys)
	}

	// The text: what the switch says changes what is promised, and only that.
	render := func(on *bool) string {
		var b bytes.Buffer
		rep.Write(&b, on)
		return b.String()
	}
	yes, no := true, false
	on, off, unknown := render(&yes), render(&no), render(nil)
	for _, want := range []string{
		"signs every user out, once",
		"scope shared — Finance, Tax",
		"both-agent — Finance, Tax",
		"scope fin-prod (Finance), 3 shell job(s) — both-agent, fin-agent",
		"fin-needs (scope fin-prod) requires [\"jq\"]",
		"The SSH executor",
		"!! Jobs whose fixed target host is not in their scope (1). From 2.3.0 a job runs",
		"4 job(s) and 1 script(s) synced from Git declare `executor`",
		"fresh (10.0.0.7:22)",
		"bastion jump (10.0.0.9:22)",
		"10.0.0.5:22 — host web01; host web01-alias",
		"secret VAULTED — Tax",
		"fin-off-scope — db99 is not a host of scope fin-prod",
	} {
		if !strings.Contains(on, want) {
			t.Errorf("the report does not say %q:\n%s", want, on)
		}
	}
	if !strings.Contains(on, "It will serve: Finance, Global, Tax") ||
		!strings.Contains(on, "may now be taken by the SERVER") {
		t.Errorf("with the SSH executor on, the report must say where the local runner is placed and what it may take:\n%s", on)
	}
	if strings.Contains(off, "It will serve") || strings.Contains(off, "may now be taken by the SERVER") ||
		!strings.Contains(off, "the local runner starts off") {
		t.Errorf("with the SSH executor off, nothing is placed and the server takes nothing:\n%s", off)
	}
	// Off or on, an agent may take what the server ran (or never ran).
	if !strings.Contains(off, "may now be taken by an AGENT (4") {
		t.Errorf("the agent line must not depend on the switch:\n%s", off)
	}
	if !strings.Contains(unknown, "could not be read here") || !strings.Contains(unknown, "It will serve") {
		t.Errorf("when the switch cannot be read the report says so and shows the on case:\n%s", unknown)
	}
}

// The local runner serves Global from the moment its row exists. Where Global's
// shell jobs all ran on agents, the server may take them once it is on, though
// the upgrade pass places it nowhere: the report must say so, as the notice
// will afterwards.
func TestThe230SectionKnowsTheLocalRunnerServesGlobal(t *testing.T) {
	pool := at2210(t)
	for _, q := range []string{
		`INSERT INTO runners (id, name, status, capabilities, registered_at, created_at) VALUES ('r', 'pool-agent', 'online', '["bash"]', 't', 't')`,
		`INSERT INTO jobs (uid, name, source, run_type, executor, requires_json, concurrency_policy, enabled, synced_at)
		 VALUES ('u', 'unscoped', 'git', 'bash', 'runner', '[]', 'Allow', 1, 't')`,
	} {
		if _, err := pool.Exec(q); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	rep, err := preflight.Build230(context.Background(), pool)
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	on := true
	rep.Write(&b, &on)
	if got := strings.Join(rep.Placed(), ","); got != "Global" {
		t.Errorf("Placed = %s, want Global", got)
	}
	if !strings.Contains(b.String(), "may now be taken by the SERVER (1 place(s))") || !strings.Contains(b.String(), "jobs with no scope (Global), 1 shell job(s)") {
		t.Errorf("Global's shell jobs that ran on agents are not reported as the server's to take:\n%s", b.String())
	}
}

// An agency already called Global is renamed by the upgrade, and the upgrade
// refuses to run when the new name is taken: both must be said beforehand, and
// the agency must not be confused with the built-in one in the rest of the
// report.
func TestThe230SectionWarnsAboutAnAgencyCalledGlobal(t *testing.T) {
	pool := at2210(t)
	for _, q := range []string{
		`INSERT INTO agencies (id, name, created_at) VALUES ('ag-g', 'GLOBAL', 't'), ('ag-r', 'GLOBAL (renamed)', 't')`,
		`INSERT INTO scopes (id, name, source, created_at) VALUES ('sc', 'ops', 'cronomicon', 't')`,
		`INSERT INTO scope_agencies VALUES ('sc', 'ag-g')`,
		`INSERT INTO jobs (uid, name, source, run_type, scope, requires_json, concurrency_policy, enabled, synced_at)
		 VALUES ('u', 'j', 'git', 'bash', 'ops', '[]', 'Allow', 1, 't')`,
	} {
		if _, err := pool.Exec(q); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	rep, err := preflight.Build230(context.Background(), pool)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(rep.NamedGlobal, ",") != "GLOBAL" || strings.Join(rep.RenameBlocked, ",") != "GLOBAL" {
		t.Fatalf("agencies called Global = %v, blocked = %v; want GLOBAL in both", rep.NamedGlobal, rep.RenameBlocked)
	}
	if len(rep.ShellScopes) != 1 || strings.Join(rep.ShellScopes[0].Agencies, ",") != "GLOBAL (renamed)" {
		t.Errorf("the scope's agency = %+v, want it shown under the name the upgrade gives it", rep.ShellScopes)
	}
	var b bytes.Buffer
	rep.Write(&b, nil)
	for _, want := range []string{`The agency "GLOBAL" cannot be renamed`, "REFUSES TO RUN", "An agency is already called Global: GLOBAL"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("the report does not say %q:\n%s", want, b.String())
		}
	}
}

// Two records for one address conflict only when they hold different keys of
// the SAME type; a record with no key is not listed when another record's key
// covers its address; and a port of 0 is 22.
func TestThe230SectionJudgesHostKeysByAddressAndType(t *testing.T) {
	pool := at2210(t)
	for _, q := range []string{
		`INSERT INTO ssh_hosts (id, hostname, address, port, host_key, created_at) VALUES
		   ('h1', 'web', '10.0.0.5', 22, 'ssh-ed25519 AAAAone', 't'),
		   ('h2', 'web-rsa', '10.0.0.5', 0, 'ssh-rsa AAAAtwo', 't'),
		   ('h3', 'web-alias', '10.0.0.5', 22, NULL, 't'),
		   ('h4', 'db', '10.0.0.6', 22, 'ssh-ed25519 AAAAdb1', 't'),
		   ('h5', 'db-old', '10.0.0.6', 22, 'ssh-ed25519 AAAAdb2', 't'),
		   ('h6', 'fresh', '10.0.0.7', 22, '  ', 't')`,
	} {
		if _, err := pool.Exec(q); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	rep, err := preflight.Build230(context.Background(), pool)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.KeyConflicts) != 1 || rep.KeyConflicts[0].Address != "10.0.0.6:22" {
		t.Errorf("conflicts = %+v, want only 10.0.0.6:22 (two ed25519 keys); an ed25519 and an rsa key for one address are both carried", rep.KeyConflicts)
	}
	if strings.Join(rep.HostsWithoutKey, ",") != "fresh (10.0.0.7:22)" {
		t.Errorf("records with no key = %v, want fresh only: web-alias's address has a key on another record", rep.HostsWithoutKey)
	}
}

// An installation with nothing for the upgrade to find is told so, plainly.
func TestThe230SectionOnAnEmptyInstallation(t *testing.T) {
	pool := at2210(t)
	rep, err := preflight.Build230(context.Background(), pool)
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	off := false
	rep.Write(&b, &off)
	for _, want := range []string{
		"No scope, secret, variable or SSH key is in several agencies.",
		"No runner serves more than one agency.",
		"No shell job that ran from the server can be taken by an agent.",
		"Every host record and bastion has a stored key.",
		"No agency holds a Vault-backed secret or SSH key.",
	} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("an empty installation's report does not say %q:\n%s", want, b.String())
		}
	}
}

// The sections for 2.2.2 and 2.2.3 are run by the same command against the
// same 2.2 database, so they must read it too.
func TestTheEarlierSectionsReadThe22Schema(t *testing.T) {
	if _, err := preflight.Build(context.Background(), at2210(t)); err != nil {
		t.Fatalf("Build against the 2.2 schema: %v", err)
	}
}

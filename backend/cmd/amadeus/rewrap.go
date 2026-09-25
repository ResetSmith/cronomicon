package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"sort"
	"text/tabwriter"

	_ "github.com/mattn/go-sqlite3"

	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/secrets"
)

// runRewrapSecrets implements `cronomicon rewrap-secrets` (DR-5): re-wrap every
// stored credential under the active KEK so an old key can actually be retired.
//
//	cronomicon rewrap-secrets --dry-run   # what is outstanding, per store and version
//	cronomicon rewrap-secrets             # re-wrap everything to CRONOMICON_KEK_VERSION
//
// KEK rotation is zero-downtime but LAZY: a row moves to the new version only
// when it is rewritten, so without this an operator must hand-touch every secret,
// every SSH credential and every settings integration before dropping the old key
// — and until they do, the old key still decrypts everything untouched.
//
// Runs ONLINE, against the live database, with the server up (DR-Q3). Unlike
// `restore` there is no in-use refusal: restore needs one because it swaps the
// file, whereas this is row-by-row transactional UPDATEs. A concurrent re-save by
// the server seals at the active version, which is the version being rotated to,
// so the two cannot fight; each write is still guarded on the value it read.
//
// IMPORTANT, and repeated in --help because it is the thing most likely to be
// misunderstood: re-wrapping does NOT undo exposure. If the old KEK leaked, the
// attacker held the plaintexts, and the remediation is rotating the underlying
// credentials — new SSH keys on the estate, a new GitLab token, a new Vault
// AppRole. Re-wrap is the hygiene that follows; it is not the fix.
func runRewrapSecrets(args []string) int {
	fs := flag.NewFlagSet("rewrap-secrets", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "report what is outstanding per store and KEK version, change nothing")
	dbPath := fs.String("db", "", "database path (default: CRONOMICON_DB_PATH from config)")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `Usage: cronomicon rewrap-secrets [--dry-run] [--db <path>]

Re-wraps every stored credential under the active KEK (CRONOMICON_KEK_VERSION) so a
superseded key can be retired. Covers three stores: stored secrets, SSH
credentials, and the encrypted settings integrations (GitLab token + webhook
secret, S3 log-storage key, Vault credentials, SMTP password, observability
bearer token).

Safe to run with the server up, and safe to re-run: it is idempotent, and
re-running is the recovery for a partial pass. Start with --dry-run.

Both the old and the new KEK must be configured — the new one as CRONOMICON_KEK /
CRONOMICON_KEK_FILE with CRONOMICON_KEK_VERSION set, the old one as CRONOMICON_KEK_<N> /
CRONOMICON_KEK_<N>_FILE.

NOTE: re-wrapping does not undo exposure. If the old key leaked, whoever held it
also held the plaintexts; the remediation is rotating the underlying credentials
(new SSH keys, new tokens), and re-wrapping is the hygiene that follows.

`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "rewrap-secrets: load config:", err)
		return 1
	}
	target := *dbPath
	if target == "" {
		target = cfg.DBPath
	}
	if target == "" {
		fmt.Fprintln(os.Stderr, "rewrap-secrets: no DB path (set CRONOMICON_DB_PATH or pass --db)")
		return 1
	}

	pool, err := db.Open(target)
	if err != nil {
		fmt.Fprintln(os.Stderr, "rewrap-secrets: open db:", err)
		return 1
	}
	defer pool.Close()

	ctx := context.Background()
	active := cfg.SecretKEKVersion

	// Refuse up front when the ACTIVE key is unusable. Proceeding would write rows
	// nothing can decrypt, which is the one failure this command must never cause.
	sealer := secrets.NewSealer(cfg)
	if _, err := sealer.Seal([]byte("rewrap-preflight")); err != nil {
		fmt.Fprintf(os.Stderr, "rewrap-secrets: the active KEK (version %d) is not usable: %v\n", active, err)
		return 1
	}

	report, err := survey(ctx, pool, active)
	if err != nil {
		fmt.Fprintln(os.Stderr, "rewrap-secrets:", err)
		return 1
	}
	report.print(os.Stdout, active)

	if *dryRun {
		if report.outstanding() == 0 {
			fmt.Printf("\nRotation is COMPLETE — every stored credential is at version %d.\n", active)
			fmt.Printf("It is safe to drop the superseded CRONOMICON_KEK_<N> entries.\n")
		} else {
			fmt.Printf("\n%d item(s) still sealed under a superseded KEK. Re-run without --dry-run to move them.\n", report.outstanding())
		}
		return 0
	}
	if report.outstanding() == 0 {
		fmt.Printf("\nNothing to do — every stored credential is already at version %d.\n", active)
		return 0
	}

	moved, failed := rewrapAll(ctx, pool, cfg, sealer, active)
	fmt.Printf("\nRe-wrapped %d item(s) to version %d.\n", moved, active)
	if failed > 0 {
		// A failure here is almost always a missing historical key. Report it and
		// exit non-zero: the operator must NOT read a partial pass as complete and
		// go on to drop the old KEK.
		fmt.Fprintf(os.Stderr, "%d item(s) could not be re-wrapped — most likely the KEK that sealed them is not configured.\n", failed)
		fmt.Fprintf(os.Stderr, "Supply it as CRONOMICON_KEK_<N> / CRONOMICON_KEK_<N>_FILE and re-run; do NOT drop a superseded key until --dry-run reports rotation complete.\n")
		return 1
	}
	fmt.Printf("Re-run with --dry-run to confirm before dropping the superseded key.\n")
	return 0
}

// tokenColumns is the complete set of encrypted settings columns —
// secrets.EncryptedSettingsColumns, shared with the redaction dictionary so a
// column added for one cannot be missed by the other.
type tokenColumn = secrets.SettingsColumn

var tokenColumns = secrets.EncryptedSettingsColumns

// envelopeStores are the tables carrying the four-column envelope layout. Only
// source='stored' rows hold a value; a vault-sourced row holds a reference.
var envelopeStores = []string{"secrets", "ssh_credentials"}

type storeCount struct {
	store    string
	byVer    map[int]int
	unparsed int
}

type surveyReport struct {
	counts []storeCount
	active int
}

func (r *surveyReport) outstanding() int {
	n := 0
	for _, c := range r.counts {
		for v, k := range c.byVer {
			if v != r.active {
				n += k
			}
		}
	}
	return n
}

func (r *surveyReport) print(w *os.File, active int) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "STORE\tKEK VERSION\tITEMS\n")
	for _, c := range r.counts {
		vers := make([]int, 0, len(c.byVer))
		for v := range c.byVer {
			vers = append(vers, v)
		}
		sort.Ints(vers)
		if len(vers) == 0 {
			fmt.Fprintf(tw, "%s\t-\t0\n", c.store)
			continue
		}
		for _, v := range vers {
			mark := ""
			if v != active {
				mark = "  <- outstanding"
			}
			fmt.Fprintf(tw, "%s\tv%d\t%d%s\n", c.store, v, c.byVer[v], mark)
		}
	}
	tw.Flush()
}

// survey counts what is sealed under which KEK version, without decrypting
// anything — so it still answers "is rotation complete?" when a historical key is
// no longer configured.
func survey(ctx context.Context, pool *sql.DB, active int) (*surveyReport, error) {
	rep := &surveyReport{active: active}

	for _, table := range envelopeStores {
		c := storeCount{store: table, byVer: map[int]int{}}
		rows, err := pool.QueryContext(ctx, fmt.Sprintf(
			`SELECT COALESCE(kek_version,1), COUNT(*) FROM %s
			  WHERE source='stored' AND ciphertext IS NOT NULL GROUP BY 1`, table))
		if err != nil {
			return nil, fmt.Errorf("survey %s: %w", table, err)
		}
		for rows.Next() {
			var v, n int
			if err := rows.Scan(&v, &n); err != nil {
				rows.Close()
				return nil, fmt.Errorf("survey %s: %w", table, err)
			}
			c.byVer[v] = n
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("survey %s: %w", table, err)
		}
		rows.Close()
		rep.counts = append(rep.counts, c)
	}

	settings := storeCount{store: "settings integrations", byVer: map[int]int{}}
	for _, tc := range tokenColumns {
		tok, err := readToken(ctx, pool, tc)
		if err != nil {
			return nil, err
		}
		if tok == "" {
			continue // unset credential; nothing sealed
		}
		if v, ok := secrets.TokenKEKVersion(tok); ok {
			settings.byVer[v]++
		} else {
			settings.unparsed++
		}
	}
	rep.counts = append(rep.counts, settings)
	return rep, nil
}

// readToken returns "" when the column is absent, NULL or empty — several of
// these default to ” rather than NULL when the credential is unset.
func readToken(ctx context.Context, pool *sql.DB, tc tokenColumn) (string, error) {
	return secrets.ReadSettingsToken(ctx, pool, tc)
}

// rewrapAll moves every outstanding item to the active version, returning how
// many moved and how many could not be. It never aborts on a single failure: one
// row whose historical key is missing must not block the rest, and the non-zero
// exit plus the residual --dry-run count is what stops a partial pass reading as
// complete.
func rewrapAll(ctx context.Context, pool *sql.DB, cfg *config.Config, sealer *secrets.Sealer, active int) (moved, failed int) {
	for _, table := range envelopeStores {
		m, f := rewrapEnvelopeTable(ctx, pool, sealer, table, active)
		moved += m
		failed += f
	}
	for _, tc := range tokenColumns {
		switch rewrapTokenColumn(ctx, pool, cfg, tc, active) {
		case rewrapMoved:
			moved++
		case rewrapFailed:
			failed++
		}
	}
	return moved, failed
}

type rewrapOutcome int

const (
	rewrapSkipped rewrapOutcome = iota
	rewrapMoved
	rewrapFailed
)

func rewrapEnvelopeTable(ctx context.Context, pool *sql.DB, sealer *secrets.Sealer, table string, active int) (moved, failed int) {
	type row struct {
		id         string
		ct, nonce  []byte
		wrapped    []byte
		kekVersion int
	}
	var todo []row
	rows, err := pool.QueryContext(ctx, fmt.Sprintf(
		`SELECT id, ciphertext, nonce, wrapped_dek, COALESCE(kek_version,1) FROM %s
		  WHERE source='stored' AND ciphertext IS NOT NULL AND COALESCE(kek_version,1) <> ?`, table), active)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rewrap %s: %v\n", table, err)
		return 0, 1
	}
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.ct, &r.nonce, &r.wrapped, &r.kekVersion); err != nil {
			rows.Close()
			fmt.Fprintf(os.Stderr, "rewrap %s: %v\n", table, err)
			return moved, failed + 1
		}
		todo = append(todo, r)
	}
	rows.Close()

	for _, r := range todo {
		plain, err := sealer.Open(r.ct, r.nonce, r.wrapped, r.kekVersion)
		if err != nil {
			fmt.Fprintf(os.Stderr, "rewrap %s %s: cannot open under KEK v%d: %v\n", table, r.id, r.kekVersion, err)
			failed++
			continue
		}
		sealed, err := sealer.Seal(plain)
		zeroBytes(plain)
		if err != nil {
			fmt.Fprintf(os.Stderr, "rewrap %s %s: %v\n", table, r.id, err)
			failed++
			continue
		}
		// Guarded on the version we read: if the server re-saved this row while we
		// worked, it is already at the active version and our write must not clobber
		// the newer ciphertext. RowsAffected 0 means exactly that — not an error.
		res, err := pool.ExecContext(ctx, fmt.Sprintf(
			`UPDATE %s SET ciphertext=?, nonce=?, wrapped_dek=?, kek_version=?
			  WHERE id=? AND COALESCE(kek_version,1)=?`, table),
			sealed.Ciphertext, sealed.Nonce, sealed.WrappedDEK, sealed.KEKVersion, r.id, r.kekVersion)
		if err != nil {
			fmt.Fprintf(os.Stderr, "rewrap %s %s: %v\n", table, r.id, err)
			failed++
			continue
		}
		if n, _ := res.RowsAffected(); n > 0 {
			moved++
		}
	}
	return moved, failed
}

func rewrapTokenColumn(ctx context.Context, pool *sql.DB, cfg *config.Config, tc tokenColumn, active int) rewrapOutcome {
	tok, err := readToken(ctx, pool, tc)
	if err != nil || tok == "" {
		if err != nil {
			fmt.Fprintf(os.Stderr, "rewrap %s: %v\n", tc.Label, err)
			return rewrapFailed
		}
		return rewrapSkipped
	}
	if v, ok := secrets.TokenKEKVersion(tok); !ok || v == active {
		// Not a token (an un-encrypted legacy value someone set by hand), or
		// already current. Neither is this command's business to rewrite.
		return rewrapSkipped
	}
	plain, err := secrets.DecryptString(cfg, tok)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rewrap %s: cannot decrypt: %v\n", tc.Label, err)
		return rewrapFailed
	}
	next, err := secrets.EncryptString(cfg, plain)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rewrap %s: %v\n", tc.Label, err)
		return rewrapFailed
	}
	// Guarded on the exact token we read, for the same reason as the envelope
	// path: a concurrent settings save wins.
	res, err := pool.ExecContext(ctx, fmt.Sprintf(
		"UPDATE %s SET %s=? WHERE %s AND %s=?", tc.Table, tc.Column, tc.Where, tc.Column), next, tok)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rewrap %s: %v\n", tc.Label, err)
		return rewrapFailed
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return rewrapMoved
	}
	return rewrapSkipped
}

// zeroBytes wipes a decrypted plaintext once it has been re-sealed (SU-10's
// best-effort posture, applied to the copies this command holds).
func zeroBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

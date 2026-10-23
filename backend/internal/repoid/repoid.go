// Package repoid names the one Git repository every installation has.
//
// Until v2.4.0 there was a single repository of definitions and nothing said
// so: a Git row was a Git row. From 2.4.0 an agency may connect its own
// (GR-1), and every row that comes from Git records which repository it came
// from (GR-3). Global's repository is the one that was always there, connected
// by the global administrators. Since §0a (IN-3) it is Global's own and not a
// library: nothing in it is resolved for another agency (from GR-16 until then
// it was read by every agency's definitions).
//
// Its id is fixed, for the reason agencyid.Global is: migrations, triggers and
// code name it, and a row written before the repositories table existed
// (migration 1290 writes scripts.repo_id) has to mean the same repository
// afterwards. Every other repository's id is generated.
//
// The package is a leaf so that sync, the composer, the API and the
// migrations' tests spell the id the same way without importing one another.
package repoid

// Global is the id of Global's repository.
const Global = "global"

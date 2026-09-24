package agent

import (
	"os"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/runnerproto"
)

// Phase B (RA-12, the runas-update plan) — secret-as-file delivery.
//
// The value of file delivery over an env var is entirely in the handling: 0600, off
// the run tree, wiped at run end, and byte-exact. Each of those is asserted here
// because each of them failing is silent — a become password that gained a trailing
// newline simply fails to authenticate, and a file left behind is a credential on
// disk nobody knows about.

func TestMaterializeSecretFilesWritesLockedDownFiles(t *testing.T) {
	fileEnv, cleanup, err := materializeSecretFiles([]runnerproto.ManifestSecretFile{
		{Name: "BECOME_PASSWORD", Reference: "AMADEUS_SECRET_BECOME_PASSWORD", Value: "s3cr3t"},
	})
	if err != nil {
		t.Fatalf("materializeSecretFiles: %v", err)
	}
	defer cleanup()

	path := fileEnv["AMADEUS_SECRET_BECOME_PASSWORD"]
	if path == "" {
		t.Fatal("no path exposed under the derived reference")
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("file mode = %o, want 600", perm)
	}
	// The run tree is the working directory; the file must not be under it, or a
	// checkout/archive step could sweep the password up with the source.
	if wd, _ := os.Getwd(); strings.HasPrefix(path, wd) {
		t.Errorf("secret file %q is under the working tree %q", path, wd)
	}

	// Byte-exact. materializeKeys normalizes line endings for PEM; doing that here
	// would CHANGE the password ansible sends, and trimming would break one that
	// legitimately ends in whitespace.
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "s3cr3t" {
		t.Errorf("file contents = %q, want %q (no normalization)", got, "s3cr3t")
	}
}

// TestMaterializeSecretFilesPreservesAwkwardValues — a password is an opaque byte
// string. Trailing whitespace, embedded newlines and a missing final newline must
// all survive: every one of them is a legal password and every one of them would be
// silently corrupted by the key-material normalizer.
func TestMaterializeSecretFilesPreservesAwkwardValues(t *testing.T) {
	for _, want := range []string{"trailing-space ", "has\nnewline", "no-trailing-newline", " "} {
		fileEnv, cleanup, err := materializeSecretFiles([]runnerproto.ManifestSecretFile{
			{Name: "P", Reference: "AMADEUS_SECRET_P", Value: want},
		})
		if err != nil {
			t.Fatalf("materializeSecretFiles(%q): %v", want, err)
		}
		got, rerr := os.ReadFile(fileEnv["AMADEUS_SECRET_P"])
		cleanup()
		if rerr != nil {
			t.Fatalf("read: %v", rerr)
		}
		if string(got) != want {
			t.Errorf("value %q round-tripped as %q", want, got)
		}
	}
}

// TestMaterializeSecretFilesCleanupWipes — the file must not outlive the run. A
// credential left on a runner's tmpfs after the run that needed it is exactly the
// exposure file delivery is supposed to shrink.
func TestMaterializeSecretFilesCleanupWipes(t *testing.T) {
	fileEnv, cleanup, err := materializeSecretFiles([]runnerproto.ManifestSecretFile{
		{Name: "P", Reference: "AMADEUS_SECRET_P", Value: "gone-after-cleanup"},
	})
	if err != nil {
		t.Fatalf("materializeSecretFiles: %v", err)
	}
	path := fileEnv["AMADEUS_SECRET_P"]
	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("secret file still present after cleanup (stat err = %v)", err)
	}
}

// TestMaterializeSecretFilesNoFilesIsANoOp — the overwhelmingly common case. No
// directory is created and the returned cleanup is safe to defer unconditionally.
func TestMaterializeSecretFilesNoFilesIsANoOp(t *testing.T) {
	fileEnv, cleanup, err := materializeSecretFiles(nil)
	if err != nil {
		t.Fatalf("materializeSecretFiles(nil): %v", err)
	}
	if fileEnv != nil {
		t.Errorf("expected no env for no files, got %v", fileEnv)
	}
	cleanup() // must not panic
}

// TestCoreAtLeast is RA-20a's gate: which runners advertise `become-file`.
// --become-password-file landed in ansible-core 2.12, and an UNPARSEABLE version
// must report false — an unknown toolchain advertising a capability it may not have
// turns a queued run into a broken escalation.
func TestCoreAtLeast(t *testing.T) {
	cases := []struct {
		version string
		want    bool
	}{
		{"2.16.3", true},   // the pinned RHEL8-capable core
		{"2.12.0", true},   // exactly the floor
		{"2.11.12", false}, // one minor below
		{"2.9.27", false},  // the long-lived old core
		{"3.0.0", true},    // a future major
		{"1.9.6", false},
		{"2.16.3rc1", true}, // a pre-release of a capable core
		{"2.16rc1", true},   // two-component with a suffix
		{"", false},         // no ansible detected
		{"garbage", false},
		{"2", false}, // too few components to judge
	}
	for _, tc := range cases {
		if got := coreAtLeast(tc.version, 2, 12); got != tc.want {
			t.Errorf("coreAtLeast(%q, 2, 12) = %v, want %v", tc.version, got, tc.want)
		}
	}
}

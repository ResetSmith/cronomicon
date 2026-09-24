package config

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestNoConfigNameUnderReservedPrefix locks the namespace contract (§2, W4): no
// server/runner CONFIG env name may live under a reserved reference prefix
// (CRONOMICON_VAR_/SECRET_/KEY_/RUN_). It scans every CRONOMICON_-identifier literal in
// config.go — the source of truth for config reads. There are no exceptions:
// the evicted KEK family's CRONOMICON_SECRET_KEK* aliases were the last one and
// were removed in v1.5.41. A reserved-prefixed config read must fail this test.
func TestNoConfigNameUnderReservedPrefix(t *testing.T) {
	src, err := os.ReadFile("config.go")
	if err != nil {
		t.Fatalf("read config.go: %v", err)
	}
	reserved := []string{"CRONOMICON_VAR_", "CRONOMICON_SECRET_", "CRONOMICON_KEY_", "CRONOMICON_RUN_"}
	allowed := map[string]bool{}
	lit := regexp.MustCompile(`"(CRONOMICON_[A-Za-z0-9_]+)"`)
	for _, m := range lit.FindAllStringSubmatch(string(src), -1) {
		name := m[1]
		for _, p := range reserved {
			if strings.HasPrefix(name, p) && !allowed[name] {
				t.Errorf("config env name %q is under reserved reference prefix %q — config must not squat the reference namespace (§2). If this is a new knob, respell it outside the reserved prefixes.", name, p)
			}
		}
	}
}

// loadWith runs Load with CRONOMICON_DEV_AUTH set so the trusted-header boot guard
// (which requires CRONOMICON_TRUSTED_PROXIES) does not interfere with KEK tests.
func loadWith(t *testing.T, env map[string]string) *Config {
	t.Helper()
	t.Setenv("CRONOMICON_DEV_AUTH", "true")
	for k, v := range env {
		t.Setenv(k, v)
	}
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return c
}

// TestKEKNewNames verifies the CRONOMICON_KEK* names are read and produce no
// deprecation warning.
func TestKEKNewNames(t *testing.T) {
	c := loadWith(t, map[string]string{
		"CRONOMICON_KEK_FILE":    "/run/secrets/amadeus_kek",
		"CRONOMICON_KEK":         "inline-kek",
		"CRONOMICON_KEK_VERSION": "3",
	})
	if c.SecretKEKFile != "/run/secrets/amadeus_kek" {
		t.Errorf("SecretKEKFile = %q, want /run/secrets/amadeus_kek", c.SecretKEKFile)
	}
	if c.SecretKEKEnv != "inline-kek" {
		t.Errorf("SecretKEKEnv = %q, want inline-kek", c.SecretKEKEnv)
	}
	if c.SecretKEKVersion != 3 {
		t.Errorf("SecretKEKVersion = %d, want 3", c.SecretKEKVersion)
	}
}

// TestKEKDefaults verifies the version defaults to 1 when nothing is set.
func TestKEKDefaults(t *testing.T) {
	c := loadWith(t, nil)
	if c.SecretKEKVersion != 1 {
		t.Errorf("SecretKEKVersion default = %d, want 1", c.SecretKEKVersion)
	}
}

// ── Process log file (LU-4) ───────────────────────────────────────────────────
//
// Why these are validated at Load rather than tolerated at runtime. The process
// log is the only durable record of what the server did, and every one of these
// knobs fails SILENTLY if it's wrong: a relative path resolves against whatever
// working directory systemd/docker happened to give the process (so the file
// exists, just nowhere the operator will ever look), and a nonsensical size or
// keep count either rotates on every line or is quietly coerced to a default the
// operator did not ask for. A boot-time error is the only feedback that reaches
// them, because the thing that would otherwise report the problem is the logger
// itself.

// loadErrWith is loadWith's negative twin: it asserts Load REJECTS the given
// environment and returns the error for message assertions.
func loadErrWith(t *testing.T, env map[string]string) error {
	t.Helper()
	t.Setenv("CRONOMICON_DEV_AUTH", "true")
	for k, v := range env {
		t.Setenv(k, v)
	}
	c, err := Load()
	if err == nil {
		t.Fatalf("Load() accepted %v, want a validation error (config = %+v)", env, c)
	}
	return err
}

// TestLogFileDefaults pins the shipped defaults: file logging on, a bounded
// (Keep+1)*MaxMB footprint, and an empty path meaning "derive it from the run-log
// directory". Turning this on by default is only defensible because the footprint
// is bounded, so the two numbers are part of the contract.
func TestLogFileDefaults(t *testing.T) {
	c := loadWith(t, nil)
	if !c.LogFileEnabled {
		t.Error("LogFileEnabled default = false, want true")
	}
	if c.LogFilePath != "" {
		t.Errorf("LogFilePath default = %q, want empty (derived from the run-log dir)", c.LogFilePath)
	}
	if c.LogFileMaxMB != 64 {
		t.Errorf("LogFileMaxMB default = %d, want 64", c.LogFileMaxMB)
	}
	if c.LogFileKeep != 5 {
		t.Errorf("LogFileKeep default = %d, want 5", c.LogFileKeep)
	}
}

// TestLogFilePathMustBeAbsolute: a relative path is the failure that hides
// itself. The process starts, the file is created, everything looks healthy —
// and the log is sitting in whatever directory the supervisor chose, which for a
// container is usually "/" and for a systemd unit is whatever WorkingDirectory
// happens to be. Nobody discovers this until they need the log.
func TestLogFilePathMustBeAbsolute(t *testing.T) {
	err := loadErrWith(t, map[string]string{"CRONOMICON_LOG_FILE": "logs/amadeus.log"})
	if !strings.Contains(err.Error(), "CRONOMICON_LOG_FILE") {
		t.Errorf("error %q should name CRONOMICON_LOG_FILE so the operator knows which knob to fix", err)
	}

	c := loadWith(t, map[string]string{"CRONOMICON_LOG_FILE": "/var/log/amadeus/amadeus.log"})
	if c.LogFilePath != "/var/log/amadeus/amadeus.log" {
		t.Errorf("LogFilePath = %q, want the absolute path to be accepted verbatim", c.LogFilePath)
	}
}

// TestLogFileMaxMBMustBeAtLeastOne: zero (or negative) would mean "rotate before
// every write", which produces a directory of empty generations and loses the
// log entirely. Coercing it to the default instead of erroring would hide an
// operator's typo.
func TestLogFileMaxMBMustBeAtLeastOne(t *testing.T) {
	for _, v := range []string{"0", "-1"} {
		err := loadErrWith(t, map[string]string{"CRONOMICON_LOG_FILE_MAX_MB": v})
		if !strings.Contains(err.Error(), "CRONOMICON_LOG_FILE_MAX_MB") {
			t.Errorf("MAX_MB=%s: error %q should name the knob", v, err)
		}
	}
	if c := loadWith(t, map[string]string{"CRONOMICON_LOG_FILE_MAX_MB": "1"}); c.LogFileMaxMB != 1 {
		t.Errorf("LogFileMaxMB = %d, want 1 to be the accepted minimum", c.LogFileMaxMB)
	}
}

// TestLogFileKeepAllowsZeroButNotNegative: zero generations is a legitimate
// choice — "cap the file, keep no history" — on a box that ships logs elsewhere,
// so it must not be rejected or silently upgraded to the default. A negative
// count is meaningless and must be an error rather than being clamped, since a
// clamp would leave the operator believing they had configured something.
func TestLogFileKeepAllowsZeroButNotNegative(t *testing.T) {
	err := loadErrWith(t, map[string]string{"CRONOMICON_LOG_FILE_KEEP": "-1"})
	if !strings.Contains(err.Error(), "CRONOMICON_LOG_FILE_KEEP") {
		t.Errorf("error %q should name CRONOMICON_LOG_FILE_KEEP", err)
	}

	c := loadWith(t, map[string]string{"CRONOMICON_LOG_FILE_KEEP": "0"})
	if c.LogFileKeep != 0 {
		t.Errorf("LogFileKeep = %d, want 0 to survive as an explicit choice", c.LogFileKeep)
	}
}

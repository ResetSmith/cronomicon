package redactdict

import (
	"strings"
	"testing"
)

func TestSkipRules(t *testing.T) {
	for _, tt := range []struct {
		in   string
		want bool
	}{
		{"", true}, {"abcd", true}, {"abcde", false},
		{"true", true}, {"PRODUCTION", true}, {" staging ", true},
		{"hunter2!", false}, {"localhost", true}, {"localhost1", false},
	} {
		if got := Skip(tt.in); got != tt.want {
			t.Errorf("Skip(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestAddValueContributesEachLineOfAMultiLineValue(t *testing.T) {
	const pem = "-----BEGIN OPENSSH PRIVATE KEY-----\n" +
		"b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAAB\n" +
		"ok\n" + // too short: dropped
		"-----END OPENSSH PRIVATE KEY-----"
	vals := AddValue(nil, pem)
	want := []string{pem, "-----BEGIN OPENSSH PRIVATE KEY-----", "b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAAB", "-----END OPENSSH PRIVATE KEY-----"}
	if strings.Join(vals, "|") != strings.Join(want, "|") {
		t.Errorf("AddValue = %q, want %q", vals, want)
	}
}

func TestRedactLongestFirstAndNilSafe(t *testing.T) {
	d := New("SECRET", "SECRETLONG")
	if got := d.RedactString("x SECRETLONG y"); got != "x "+Mask+" y" {
		t.Errorf("longest-first: got %q", got)
	}
	var nilDict *Dictionary
	if got := nilDict.RedactString("SECRET"); got != "SECRET" {
		t.Errorf("nil dictionary must mask nothing, got %q", got)
	}
	if nilDict.Len() != 0 || len(nilDict.Values()) != 0 {
		t.Error("nil dictionary must report empty")
	}
	if got := New("tiny", "prod").Len(); got != 0 {
		t.Errorf("skipped values must not enter: Len = %d", got)
	}
}

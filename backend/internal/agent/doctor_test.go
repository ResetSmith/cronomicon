package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCheckHealth pins the three failure modes the doctor must distinguish:
// healthy, an SSO-proxy HTML login page, and a plain non-200.
func TestCheckHealth(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
		wantErr string // substring; "" = expect nil
	}{
		{"healthy", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200); _, _ = w.Write([]byte("ok")) }, ""},
		{"sso html intercept", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<!doctype html><html>login</html>"))
		}, "reverse proxy"},
		{"server error", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500); _, _ = w.Write([]byte("boom")) }, "HTTP 500"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()
			c, err := NewClient(srv.URL, "")
			if err != nil {
				t.Fatalf("NewClient: %v", err)
			}
			err = c.CheckHealth(context.Background())
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("want nil, got %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

// TestDoctorQuickPassesAgainstLiveServer checks the aggregate PASS path and the
// missing-token FAIL path, in quick mode (no toolchain/sandbox probes).
func TestDoctorQuickPassesAgainstLiveServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()
	dir := t.TempDir()

	cfg, err := Resolve([]string{"-server", srv.URL, "-name", "d", "-capabilities", "bash",
		"-registration-token", "crn_reg_x", "-identity-file", dir + "/id.json"}, noEnv)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	checks, ok := Doctor(context.Background(), cfg, true)
	if !ok {
		t.Errorf("expected all quick checks to pass, got %+v", checks)
	}
	if s := statusOf(checks, "server-reachable"); s != CheckPass {
		t.Errorf("server-reachable = %s, want PASS", s)
	}

	// Drop the token and remove the (absent) identity → registration FAIL.
	cfg.RegistrationToken = ""
	checks, ok = Doctor(context.Background(), cfg, true)
	if ok {
		t.Error("expected FAIL with no token and no identity")
	}
	if s := statusOf(checks, "registration"); s != CheckFail {
		t.Errorf("registration = %s, want FAIL", s)
	}
}

func statusOf(checks []Check, name string) CheckStatus {
	for _, c := range checks {
		if c.Name == name {
			return c.Status
		}
	}
	return "MISSING"
}

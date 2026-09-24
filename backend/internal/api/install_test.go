package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/web"
)

const validInstallToken = "amt_reg_" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestPersonalizeInstallScript(t *testing.T) {
	// The DOWNLOAD=1 in the arg-parse arm must NOT be touched — only the
	// header default lines are baked. This mirrors the real script, where
	// `--download) DOWNLOAD=1` coexists with the `DOWNLOAD=0` default.
	published := "#!/bin/bash\nSERVER_URL=\"\"\nREG_TOKEN=\"\"\nDOWNLOAD=0\n--download) DOWNLOAD=1 ;;\n"
	want := "#!/bin/bash\nSERVER_URL=\"https://amadeus.example.com\"\nREG_TOKEN=\"" +
		validInstallToken + "\"\nDOWNLOAD=1\n--download) DOWNLOAD=1 ;;\n"
	out := personalizeInstallScript(published, "https://amadeus.example.com", validInstallToken)
	if out != want {
		t.Errorf("personalize baked the wrong bytes:\ngot  %q\nwant %q", out, want)
	}
	// Belt and suspenders: no empty default survives the bake.
	if strings.Contains(out, `SERVER_URL=""`) || strings.Contains(out, `REG_TOKEN=""`) {
		t.Errorf("baked script still carries an empty default:\n%s", out)
	}
}

// serveInstall calls the /install/{token} handler directly (the {token} path
// value set as the mux would) so the unit test needs no DB — buildMux wires the
// git/settings slices which require one. Route existence is covered separately
// by TestSpecRouteConformance.
func serveInstall(t *testing.T, token string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	srv := New(Options{WebFS: web.DistFS()})
	req := httptest.NewRequest(http.MethodGet, "/install/x", nil)
	req.SetPathValue("token", token)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	srv.handleInstallScript(rec, req)
	return rec
}

func TestInstallEndpointServesBakedScript(t *testing.T) {
	rec := serveInstall(t, validInstallToken, map[string]string{
		"X-Forwarded-Proto": "https",
		"X-Forwarded-Host":  "amadeus.example.com",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/x-shellscript") {
		t.Errorf("Content-Type = %q, want text/x-shellscript", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store (single-use token must not cache)", cc)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`SERVER_URL="https://amadeus.example.com"`,
		`REG_TOKEN="` + validInstallToken + `"`,
		"DOWNLOAD=1",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("served script missing %q", want)
		}
	}
}

func TestInstallEndpointAcceptsIPv6AndContainerHosts(t *testing.T) {
	// IPv6 literals ([..]) and container-DNS hostnames (underscore) must bake a
	// valid SERVER_URL rather than 400 (they're inert inside double quotes).
	for _, host := range []string{"[2001:db8::1]:8443", "amadeus_backend:8080"} {
		rec := serveInstall(t, validInstallToken, map[string]string{
			"X-Forwarded-Proto": "https",
			"X-Forwarded-Host":  host,
		})
		if rec.Code != http.StatusOK {
			t.Errorf("host %q: status = %d, want 200", host, rec.Code)
			continue
		}
		if !strings.Contains(rec.Body.String(), `SERVER_URL="https://`+host+`"`) {
			t.Errorf("host %q: baked SERVER_URL missing or wrong", host)
		}
	}
}

func TestInstallEndpointRejectsNonToken(t *testing.T) {
	for _, bad := range []string{
		"not-a-token",
		"amt_run_" + strings.Repeat("a", 64), // wrong prefix (runner key, not reg token)
		"amt_reg_" + strings.Repeat("a", 63), // too short
		"amt_reg_" + strings.Repeat("Z", 64), // non-hex
		"amt_reg_" + strings.Repeat("a", 64) + "; rm -rf /", // shell-meta
	} {
		rec := serveInstall(t, bad, nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("token %q: status = %d, want 404", bad, rec.Code)
		}
		// No oracle: the 404 body must not distinguish "used/expired" from
		// "malformed" — it's the same generic message.
		if strings.Contains(strings.ToLower(rec.Body.String()), "expired") ||
			strings.Contains(strings.ToLower(rec.Body.String()), "used") {
			t.Errorf("token %q: 404 body leaks token-validity state", bad)
		}
	}
}

func TestInstallEndpointServedBytesMatchPublished(t *testing.T) {
	// The served script must be exactly the published /runner-install.sh with
	// personalizeInstallScript applied — nothing else. (TestPersonalizeInstall
	// pins that personalize only touches the three header defaults.)
	published, err := io.ReadAll(mustOpen(t, "runner-install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	want := personalizeInstallScript(string(published), "https://amadeus.example.com", validInstallToken)
	rec := serveInstall(t, validInstallToken, map[string]string{
		"X-Forwarded-Proto": "https",
		"X-Forwarded-Host":  "amadeus.example.com",
	})
	if rec.Body.String() != want {
		t.Error("served /install script diverges from personalize(published /runner-install.sh)")
	}
}

func mustOpen(t *testing.T, name string) io.Reader {
	t.Helper()
	f, err := web.DistFS().Open(name)
	if err != nil {
		t.Fatalf("open %s from dist: %v", name, err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

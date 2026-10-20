package api_test

import (
	"net/http"
	"strings"
	"testing"
)

// 2.5.0 C11, defect 87 (question 108): an access grant and a service account
// are made in the agency the request works in, as every creation. A grant on
// another agency, or on every agency (none named: Global's), was made from
// Finance and then never listed, the list being narrowed to the agency in use.
func TestC11_AGrantIsMadeInTheAgencyInUse(t *testing.T) {
	h, _ := gateServer(t)
	in := func(agency, method, path, body string) (int, string) {
		t.Helper()
		rec := gateReqWithHeader(t, h, method, path, gRoot, body, "X-Cronomicon-Agency", agency)
		return rec.Code, rec.Body.String()
	}
	for _, c := range []struct {
		what, agency, path, body string
		want                     int
	}{
		{"a grant on TAX, from FIN", "ag:FIN", "/api/v1/access-grants", `{"adGroup":"g-1","role":"viewer","agencyId":"ag:TAX"}`, http.StatusUnprocessableEntity},
		{"a grant on every agency, from FIN", "ag:FIN", "/api/v1/access-grants", `{"adGroup":"g-2","role":"viewer","allScopes":true}`, http.StatusUnprocessableEntity},
		{"a service account of TAX, from FIN", "ag:FIN", "/api/v1/service-accounts", `{"name":"sa-1","role":"viewer","agencyId":"ag:TAX"}`, http.StatusUnprocessableEntity},
		{"a service account of every agency, from FIN", "ag:FIN", "/api/v1/service-accounts", `{"name":"sa-2","role":"viewer","allScopes":true}`, http.StatusUnprocessableEntity},
		{"a grant on FIN, from FIN", "ag:FIN", "/api/v1/access-grants", `{"adGroup":"g-3","role":"viewer","agencyId":"ag:FIN"}`, http.StatusCreated},
		{"a service account of FIN, from FIN", "ag:FIN", "/api/v1/service-accounts", `{"name":"sa-3","role":"viewer","agencyId":"ag:FIN"}`, http.StatusCreated},
		{"a grant on every agency, from Global", "global", "/api/v1/access-grants", `{"adGroup":"g-4","role":"viewer","allScopes":true}`, http.StatusCreated},
		{"a service account of every agency, from Global", "global", "/api/v1/service-accounts", `{"name":"sa-4","role":"viewer","allScopes":true}`, http.StatusCreated},
	} {
		code, body := in(c.agency, http.MethodPost, c.path, c.body)
		if code != c.want {
			t.Errorf("%s = %d, want %d (%s)", c.what, code, c.want, body)
		}
		if c.want == http.StatusUnprocessableEntity && !strings.Contains(body, "agency_context_mismatch") {
			t.Errorf("%s: the refusal does not say why: %s", c.what, body)
		}
	}
}

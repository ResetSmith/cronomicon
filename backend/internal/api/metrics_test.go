package api_test

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestMetricsEndpoint verifies /metrics returns 200 and registers the expected
// metric families: Go runtime, process, and Cronomicon HTTP/domain metrics (C.2).
func TestMetricsEndpoint(t *testing.T) {
	ts, _ := newTestServer(t)

	// Generate one HTTP observation so the http_requests family has a sample.
	if resp, err := http.Get(ts.URL + "/healthz"); err == nil {
		resp.Body.Close()
	}

	resp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/metrics status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	text := string(body)

	// Note: label-less gauges and any vec WITH a sample are exported; a CounterVec
	// with zero samples is not printed until first incremented, so we assert on
	// families guaranteed to have a value here.
	for _, want := range []string{
		"go_goroutines",               // Go runtime collector
		"process_",                    // process collector (prefix)
		"amadeus_http_requests_total", // HTTP counter (sampled by /healthz above)
		"amadeus_active_runners",      // gauge func (always emitted)
	} {
		if !strings.Contains(text, want) {
			t.Errorf("/metrics output missing %q", want)
		}
	}
}

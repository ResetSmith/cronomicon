package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"
)

// runHealthcheck implements `amadeus healthcheck` — the container HEALTHCHECK
// probe. The distroless final image has no shell, curl, or wget, so the binary
// probes itself over HTTP (B.5). It hits /healthz (liveness) by default; pass
// -ready to probe /readyz instead (dependency + migration readiness).
//
// Exit 0 = healthy (2xx), non-zero = unhealthy, so Docker/compose can gate on it.
func runHealthcheck(args []string) int {
	fs := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	addr := fs.String("addr", envOr("CRONOMICON_ADDR", ":8080"), "listen address to probe")
	ready := fs.Bool("ready", false, "probe /readyz instead of /healthz")
	timeout := fs.Duration("timeout", 3*time.Second, "request timeout")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	path := "/healthz"
	if *ready {
		path = "/readyz"
	}
	// The probe runs inside the container, so it always targets loopback; only
	// the port from CRONOMICON_ADDR matters.
	url := "http://127.0.0.1" + portOf(*addr) + path

	client := &http.Client{Timeout: *timeout}
	resp, err := client.Get(url)
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		fmt.Fprintf(os.Stderr, "healthcheck: %s returned %d\n", path, resp.StatusCode)
		return 1
	}
	return 0
}

// portOf extracts the ":port" suffix from a listen address like ":8080" or
// "0.0.0.0:8080", defaulting to ":8080".
func portOf(addr string) string {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return addr[i:]
		}
	}
	return ":8080"
}

func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The owner's answer to question 73 (2.5.0 C2): /readyz is unauthenticated, so
// it says which check failed and not why; the reason (a path, an address) is
// logged.
func TestReadyzNamesAFailedCheckWithoutItsText(t *testing.T) {
	s := &Server{
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		readyChecks: []ReadyCheck{{Name: "db", Check: func(context.Context) error {
			return errors.New("open /var/lib/cronomicon/secret-path.db: permission denied")
		}}},
	}
	rec := httptest.NewRecorder()
	s.readyz(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz with a failed check = %d, want 503", rec.Code)
	}
	if body := rec.Body.String(); strings.Contains(body, "secret-path") || !strings.Contains(body, `"db":"error"`) {
		t.Errorf("readyz answered %s; want the check named, its text not", body)
	}
}

package auth

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/ResetSmith/cronomicon/internal/httpx"
)

// HashToken returns the hex SHA-256 of a runner token. Only the hash is stored
// (runner_tokens.token_hash); the plaintext is shown once at generation (A6.1).
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// RequireRunner authenticates a runner via `Authorization: Bearer <token>` (T9).
// No cookies, no CSRF. A token is valid if its hash matches a non-revoked,
// non-expired row in runner_tokens. The token's owning runner_id (R1.4) is
// stashed in the request context so downstream handlers can authorize by run
// ownership (run.runner_id == caller.runnerId).
func (s *Service) RequireRunner(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r)
		if !ok {
			httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "missing bearer token")
			return
		}
		runnerID, valid, err := s.runnerTokenIdentity(r.Context(), token)
		if err != nil {
			s.log.Error("runner token check failed", "error", err)
			httpx.Fail(w, http.StatusServiceUnavailable, "unavailable", "auth backend error")
			return
		}
		if !valid {
			httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "invalid or expired runner token")
			return
		}
		ctx := context.WithValue(r.Context(), runnerIDKey, runnerID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// runnerTokenIdentity validates a runner bearer token and returns its owning
// runner_id (R1.4 token→runner binding). runnerID is empty for legacy/unbound
// token rows (pre-migration-130), which still authenticate but own no run.
func (s *Service) runnerTokenIdentity(ctx context.Context, token string) (runnerID string, valid bool, err error) {
	now := time.Now().UTC().Format(time.RFC3339)
	var rid sql.NullString
	err = s.db.QueryRowContext(ctx, `
		SELECT runner_id FROM runner_tokens
		WHERE token_hash = ? AND revoked_at IS NULL AND expires_at > ?
		LIMIT 1`,
		HashToken(token), now).Scan(&rid)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return rid.String, true, nil
}

// RunnerIDFrom extracts the authenticated runner's id from the request context
// (set by RequireRunner). ok is false when the request was not runner-authed or
// the token predates the R1.4 token→runner binding.
func RunnerIDFrom(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(runnerIDKey).(string)
	if !ok || id == "" {
		return "", false
	}
	return id, true
}

func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	return strings.TrimSpace(h[len(prefix):]), true
}

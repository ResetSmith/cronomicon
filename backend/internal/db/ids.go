package db

import "github.com/google/uuid"

// NewTraceID mints a backend-assigned, time-sortable, unguessable run/trace ID
// (S11). Clients treat these as opaque — they never mint their own. UUIDv7
// embeds a millisecond timestamp so IDs sort chronologically, which keeps
// run-history queries index-friendly.
func NewTraceID() string {
	return uuid.Must(uuid.NewV7()).String()
}

// NewID mints a generic backend-assigned identifier (scopes, runners, env vars,
// secrets, …). Also UUIDv7 for the same time-ordering benefit.
func NewID() string {
	return uuid.Must(uuid.NewV7()).String()
}

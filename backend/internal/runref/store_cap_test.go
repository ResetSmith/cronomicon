package runref

import (
	"context"
	"errors"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/envref"
)

// TestReplaceBindingsRowCap (L9): a PUT exceeding MaxBindingsPerOwner is rejected
// with an *envref.Error (→ 422) before any DB work, bounding a runaway insert.
func TestReplaceBindingsRowCap(t *testing.T) {
	pool := openDB(t)
	over := make([]Binding, MaxBindingsPerOwner+1)
	for i := range over {
		over[i] = Binding{Kind: KindVar, Name: "V"}
	}
	err := ReplaceBindings(context.Background(), pool, Owner{Kind: "job", Source: "amadeus", Name: "j"}, over, "tester")
	if _, ok := errors.AsType[*envref.Error](err); !ok {
		t.Fatalf("expected *envref.Error for an oversized set, got %v", err)
	}

	// The cap boundary itself is accepted (all dedupe to 1 row, but validation runs).
	atCap := make([]Binding, MaxBindingsPerOwner)
	for i := range atCap {
		atCap[i] = Binding{Kind: KindVar, Name: "OK"}
	}
	if err := ReplaceBindings(context.Background(), pool, Owner{Kind: "job", Source: "amadeus", Name: "j"}, atCap, "tester"); err != nil {
		t.Fatalf("at-cap set rejected: %v", err)
	}
}

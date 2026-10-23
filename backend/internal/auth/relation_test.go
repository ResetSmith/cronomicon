package auth

import (
	"context"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/auditlog"
)

// 2.5.1 SG-11: how an actor stands to an agency, as a change_log row records it.
func TestRelationTo(t *testing.T) {
	root := Identity{Grants: []RoleGrant{{Role: "admin", Scopes: []string{AllScopes}}}}
	finAdmin := Identity{Grants: []RoleGrant{{Role: "admin", Agency: "ag-fin", Scopes: []string{"fin"}}}}
	viewerAll := Identity{Grants: []RoleGrant{{Role: "viewer", Scopes: []string{AllScopes}}}}
	rootAndFin := Identity{Grants: []RoleGrant{
		{Role: "admin", Scopes: []string{AllScopes}},
		{Role: "operator", Agency: "ag-fin", Scopes: []string{"fin"}},
	}}
	for _, c := range []struct {
		name   string
		id     Identity
		agency string
		want   string
	}{
		{"a global administrator in an agency", root, "ag-fin", auditlog.RelationGlobalAdmin},
		{"a global administrator on Global's row", root, "global", auditlog.RelationMember},
		{"an owner that names no agency is Global's", root, "", auditlog.RelationMember},
		{"an agency's own administrator", finAdmin, "ag-fin", auditlog.RelationMember},
		{"an administrator of another agency", finAdmin, "ag-tax", auditlog.RelationOutside},
		{"an agency's administrator on Global's row", finAdmin, "global", auditlog.RelationOutside},
		{"a viewer of everything is not a global administrator", viewerAll, "ag-fin", auditlog.RelationOutside},
		{"a global administrator who also holds a grant there is a member", rootAndFin, "ag-fin", auditlog.RelationMember},
	} {
		if got := c.id.RelationTo(c.agency); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	// Every attached identity carries the judge; a context without one is a
	// background writer's.
	ctx := withIdentity(context.Background(), finAdmin)
	if got := auditlog.ActorRelation(ctx, "ag-fin"); got != auditlog.RelationMember {
		t.Errorf("through the context: %q", got)
	}
	if got := auditlog.ActorRelation(context.Background(), "ag-fin"); got != auditlog.RelationSystem {
		t.Errorf("no identity: %q, want system", got)
	}
}

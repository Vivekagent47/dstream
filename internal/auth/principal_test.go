package auth

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

func TestRoleLessThan(t *testing.T) {
	cases := []struct {
		a, b Role
		want bool
	}{
		{RoleMember, RoleAdmin, true},
		{RoleAdmin, RoleOwner, true},
		{RoleMember, RoleOwner, true},
		{RoleAdmin, RoleMember, false},
		{RoleOwner, RoleAdmin, false},
		{RoleOwner, RoleMember, false},
		{RoleMember, RoleMember, false},
		{RoleAdmin, RoleAdmin, false},
		{RoleOwner, RoleOwner, false},
		// Unknown role ranks at 0 and is below all named roles.
		{Role("nope"), RoleMember, true},
		{RoleMember, Role("nope"), false},
	}
	for _, c := range cases {
		got := c.a.LessThan(c.b)
		if got != c.want {
			t.Errorf("Role(%q).LessThan(%q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestFromContext_Empty(t *testing.T) {
	if _, err := FromContext(context.Background()); err == nil {
		t.Fatal("FromContext(empty) returned nil error; want failure")
	}
}

func TestWithPrincipal_Roundtrip(t *testing.T) {
	uid := uuid.New()
	oid := uuid.New()
	kid := uuid.New()
	in := Principal{
		Source:   SourceSession,
		UserID:   uid,
		OrgID:    oid,
		APIKeyID: kid,
		Role:     RoleOwner,
	}
	ctx := WithPrincipal(context.Background(), in)
	got, err := FromContext(ctx)
	if err != nil {
		t.Fatalf("FromContext: %v", err)
	}
	if got != in {
		t.Fatalf("roundtrip mismatch: got %+v want %+v", got, in)
	}
}

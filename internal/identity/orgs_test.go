package identity_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/identity"
)

func TestOrg_MarshalsAsTheOrgObject(t *testing.T) {
	t.Parallel()
	created := time.Date(2026, 1, 2, 3, 4, 5, 600, time.UTC)
	updated := created.Add(time.Hour)
	for _, c := range []struct {
		name  string
		owner string
		want  any
	}{
		{"with an owner", "u1", "u1"},
		{"without an owner", "", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			data, err := json.Marshal(identity.Org{ID: "o1", Name: "Acme Inc", Slug: "acme", URL: "https://acme.example",
				OwnerID: c.owner, Status: identity.StatusActive, CreatedAt: created, UpdatedAt: updated})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var got map[string]any
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("unmarshal %s: %v", data, err)
			}
			want := map[string]any{"id": "o1", "name": "Acme Inc", "slug": "acme", "url": "https://acme.example",
				"owner_id": c.want, "status": "active", "created_at": "2026-01-02T03:04:05Z", "updated_at": "2026-01-02T04:04:05Z"}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("Org object = %s, want %v", data, want)
			}
		})
	}
}

func TestOrgLookups_FindAnOrgByItsIDOrExactSlug(t *testing.T) {
	t.Parallel()
	h := newTeamHub(t)
	ctx, db := context.Background(), h.st.Main()
	o, err := identity.OrgBySlug(ctx, db, "alice")
	if err != nil {
		t.Fatalf("OrgBySlug(alice): %v", err)
	}
	if o.Name != "alice" || o.OwnerID != h.alice.ID || o.Status != identity.StatusActive || !o.CreatedAt.Equal(h.now()) {
		t.Errorf("OrgBySlug(alice) = %+v, want alice's personal org", o)
	}
	byID, err := identity.OrgByID(ctx, db, o.ID)
	if err != nil || !reflect.DeepEqual(byID, o) {
		t.Errorf("OrgByID(%s) = %+v, %v; want %+v", o.ID, byID, err, o)
	}
	for _, slug := range []string{"ALICE", "alic", "nope"} {
		if _, err := identity.OrgBySlug(ctx, db, slug); !errors.Is(err, identity.ErrNotFound) {
			t.Errorf("OrgBySlug(%s) error = %v, want ErrNotFound", slug, err)
		}
	}
	if _, err := identity.OrgByID(ctx, db, h.alice.ID); !errors.Is(err, identity.ErrNotFound) {
		t.Errorf("OrgByID(a user id) error = %v, want ErrNotFound", err)
	}
}

func TestMemberships_ListsTheMembersOfOneOrg(t *testing.T) {
	t.Parallel()
	h := newTeamHub(t)
	acme := h.seedOrg("Acme Inc", "acme")
	if got := h.membershipsOf(acme); len(got) != 0 {
		t.Errorf("memberships of a new org = %v, want none", got)
	}
	h.addMember(acme, h.alice.ID)
	h.advance(time.Minute)
	h.addMember(acme, h.root.ID)
	want := []identity.Membership{
		{OrgID: acme, UserID: h.alice.ID, CreatedAt: h.now().Add(-time.Minute)},
		{OrgID: acme, UserID: h.root.ID, CreatedAt: h.now()},
	}
	if got := h.membershipsOf(acme); !reflect.DeepEqual(got, want) {
		t.Errorf("memberships of acme = %+v, want %+v", got, want)
	}
}

package identity_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"
	"uuid"

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

func TestOrgBySelector_LooksAValueUpByItsShapeOnly(t *testing.T) {
	t.Parallel()
	h := newTestHub(t)
	acme := h.seedOrg("Acme Inc", "acme")
	// A slug in UUID form is read as an id, so its org is not found by it.
	uuidSlug := uuid.NewV4().String()
	uuidShaped := h.seedOrg("Shaped", uuidSlug)
	for _, c := range []struct {
		name, selector string
		want           string // "": no org
	}{
		{"the slug", "acme", acme},
		{"the id", acme, acme},
		{"the slug in other case", "ACME", ""},
		{"the name", "Acme Inc", ""},
		{"a slug in UUID form", uuidSlug, ""},
		{"the id of that org", uuidShaped, uuidShaped},
		{"a random UUID", uuid.NewV4().String(), ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			o, err := identity.OrgBySelector(context.Background(), h.st.Main(), c.selector)
			if c.want == "" {
				if !errors.Is(err, identity.ErrNotFound) {
					t.Errorf("OrgBySelector(%q) = %+v, %v, want ErrNotFound", c.selector, o, err)
				}
				return
			}
			if err != nil || o.ID != c.want {
				t.Errorf("OrgBySelector(%q) = %+v, %v, want the org %s", c.selector, o, err, c.want)
			}
		})
	}
}

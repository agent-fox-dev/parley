package identity_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
	"uuid"

	"github.com/agent-fox-dev/parley/internal/identity"
)

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
		{"a prefix of the slug", "acm", ""},
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

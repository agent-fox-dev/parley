package identity_test

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"
	"uuid"

	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/store"
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

func TestActiveOrgIDs_ListsOnlyTheActiveOrgsOfWhichTheUserIsAMember(t *testing.T) {
	t.Parallel()
	h := newTestHub(t)
	alice, bob := h.seedUser("alice"), h.seedUser("bob")
	acme, beta, gone, other := h.seedOrg("Acme", "acme"), h.seedOrg("Beta", "beta"), h.seedOrg("Gone", "gone"), h.seedOrg("Other", "other")
	for _, org := range []string{acme, beta, gone} {
		h.addMember(org, alice.ID)
	}
	h.addMember(other, bob.ID)
	h.exec(`UPDATE orgs SET status = ? WHERE id = ?`, identity.StatusBlocked, beta)
	h.exec(`DELETE FROM orgs WHERE id = ?`, gone)
	for _, c := range []struct {
		name, userID string
		want         []string
	}{
		{"a member of an active, a blocked and a deleted org", alice.ID, []string{acme}},
		{"a member of another org", bob.ID, []string{other}},
		{"a user of no org", "nobody", nil},
	} {
		if got, err := identity.ActiveOrgIDs(context.Background(), h.st.Main(), c.userID); err != nil || !slices.Equal(got, c.want) {
			t.Errorf("%s: ActiveOrgIDs = %v, %v, want %v", c.name, got, err, c.want)
		}
	}
}

func TestPersonalOrgStep_MarksTheOrgEachUserOwnsUnderTheirUsernameInAnExistingStore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	opts := store.Options{DatabasePath: filepath.Join(dir, "parley.db"), WorkspaceRoot: filepath.Join(dir, "workspaces")}
	st, err := store.Open(ctx, opts, []store.Step{identity.SchemaStep})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Main().ExecContext(ctx, `
INSERT INTO users (id, username, email, role, status, provider, provider_id, created_at, updated_at)
VALUES ('u1', 'alice', 'a@x', 'user', 'active', 'github', '1', 0, 0);
INSERT INTO orgs (id, name, slug, owner_id, status, created_at, updated_at) VALUES
	('o1', 'Acme Inc', 'acme', 'u1', 'active', 0, 0),
	('o2', 'alice', 'alice', 'u1', 'active', 0, 0);`); err != nil {
		t.Fatal(err)
	}
	st.Close()

	st, err = store.Open(ctx, opts, []store.Step{identity.SchemaStep, identity.PersonalOrgStep})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.Main().ExecContext(ctx, `UPDATE orgs SET name = 'Alice Ltd' WHERE id = 'o2'`); err != nil {
		t.Fatal(err)
	}
	if o, err := identity.PersonalOrg(ctx, st.Main(), "u1"); err != nil || o.ID != "o2" {
		t.Errorf("PersonalOrg = %q, %v; want o2", o.ID, err)
	}
}

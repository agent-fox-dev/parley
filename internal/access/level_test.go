package access_test

import (
	"context"
	"errors"
	"testing"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/store"
)

// The callers of the level tables, as identity's authenticator reports
// them: the credential type and the user's role are in the caller, and the
// rule must not read them.
var adminToken = server.Caller{CredentialID: "admin", Admin: true,
	Detail: &identity.Principal{Type: identity.TypeAdmin, CredentialID: "admin"}}

func byCredential(typ, userID, role string) server.Caller {
	return server.Caller{CredentialID: typ + "-" + userID, UserID: userID, Detail: &identity.Principal{
		Type: typ, CredentialID: typ + "-" + userID, User: &identity.User{ID: userID, Role: role}}}
}

func byKey(userID string) server.Caller {
	return byCredential(identity.TypeAPIKey, userID, identity.RoleUser)
}

// levelWorld is a store and a service over it: repositories acme (o1), solo
// (o2), held (o3, blocked) and orphan (o9, no such org), all owned by
// u-owner; forks of acme owned by u-member, u-owner and u-left, a fork of
// held owned by u-blocked-org-member, and lost--f, whose parent lost is
// gone. u-owner, u-member and u-other are members of o1,
// u-blocked-org-member of o3 and u-ghost of o9; only o1 and o2 are active.
func levelWorld(t *testing.T) (*store.Store, *access.Service) {
	t.Helper()
	st := openStore(t, t.TempDir())
	seed(t, st,
		repo("acme", "u-owner", "o1", access.StatusReady),
		repo("solo", "u-owner", "o2", access.StatusReady),
		repo("held", "u-owner", "o3", access.StatusReady),
		repo("orphan", "u-owner", "o9", access.StatusReady),
		repo("lost", "u-owner", "o1", access.StatusReady),
		fork("acme--m", "u-member", "o1", "acme"),
		fork("acme--o", "u-owner", "o1", "acme"),
		fork("acme--gone", "u-left", "o1", "acme"),
		fork("held--m", "u-blocked-org-member", "o3", "held"),
		fork("lost--f", "u-member", "o1", "lost"),
	)
	if _, err := st.Main().ExecContext(context.Background(), `DELETE FROM workspaces WHERE slug = 'lost'`); err != nil {
		t.Fatalf("remove lost: %v", err)
	}
	return st, newService(access.StoreRecords(st.Main()), memberships{
		of: map[string][]string{
			"u-owner": {"o1"}, "u-member": {"o1"}, "u-other": {"o1"},
			"u-blocked-org-member": {"o3"}, "u-ghost": {"o9"},
		},
		active: []string{"o1", "o2"},
	})
}

type levelCase struct {
	name   string
	caller server.Caller
	slug   string
	want   access.Level
}

// resolve runs c through svc and checks the level, and that the record of
// an existing slug comes with it.
func resolve(t *testing.T, svc *access.Service, c levelCase) (access.Level, access.Record) {
	t.Helper()
	got, rec, found, err := svc.Level(context.Background(), access.CallerFrom(c.caller), c.slug)
	if err != nil || !found || rec.Slug != c.slug || got != c.want {
		t.Errorf("%s: Level on %s = %q, %+v, %t, %v, want %q with its record", c.name, c.slug, got, rec, found, err, c.want)
	}
	return got, rec
}

func TestLevel_OnARepositoryComesFromTheOwnerAndTheActiveOrgsNeverTheStatus(t *testing.T) {
	t.Parallel()
	st, svc := levelWorld(t)
	cases := []levelCase{
		{"the admin token", adminToken, "acme", access.LevelAdmin},
		{"the admin token in a blocked org", adminToken, "held", access.LevelAdmin},
		{"the owner, also a member", byKey("u-owner"), "acme", access.LevelOwner},
		{"the owner, no member", byKey("u-owner"), "solo", access.LevelOwner},
		{"the owner in a blocked org", byKey("u-owner"), "held", access.LevelOwner},
		{"the owner in a deleted org", byKey("u-owner"), "orphan", access.LevelOwner},
		{"a member", byKey("u-member"), "acme", access.LevelMember},
		{"a member of another org", byKey("u-member"), "solo", access.LevelNone},
		{"a member of the blocked org", byKey("u-blocked-org-member"), "held", access.LevelNone},
		{"a member of the deleted org", byKey("u-ghost"), "orphan", access.LevelNone},
		{"a stranger", byKey("u-stranger"), "acme", access.LevelNone},
		{"a user with role admin by API key", byCredential(identity.TypeAPIKey, "u-admin-role", identity.RoleAdmin), "acme", access.LevelNone},
		{"a user with role admin by PAT", byCredential(identity.TypePAT, "u-admin-role", identity.RoleAdmin), "acme", access.LevelNone},
		{"a member by PAT", byCredential(identity.TypePAT, "u-member", identity.RoleUser), "acme", access.LevelMember},
	}
	for _, status := range []string{access.StatusPending, access.StatusCloning, access.StatusReady, access.StatusFailed, access.StatusArchived} {
		slug := "acme-" + status
		seed(t, st, repo(slug, "u-owner", "o1", status))
		cases = append(cases,
			levelCase{status + ": the admin token", adminToken, slug, access.LevelAdmin},
			levelCase{status + ": the owner", byKey("u-owner"), slug, access.LevelOwner},
			levelCase{status + ": a member", byKey("u-member"), slug, access.LevelMember},
			levelCase{status + ": a stranger", byKey("u-stranger"), slug, access.LevelNone})
	}
	for _, c := range cases {
		resolve(t, svc, c)
	}
	for _, caller := range []server.Caller{adminToken, byKey("u-owner")} {
		if got, rec, found, err := svc.Level(context.Background(), access.CallerFrom(caller), "nope"); got != access.LevelNone || found || rec != (access.Record{}) || err != nil {
			t.Errorf("Level on a slug without a record = %q, %+v, %t, %v, want none, not found, no error", got, rec, found, err)
		}
	}
}

func TestLevel_OnAForkNeverExceedsTheLevelOnItsParent(t *testing.T) {
	t.Parallel()
	_, svc := levelWorld(t)
	for _, c := range []levelCase{
		{"the fork's owner, a member", byKey("u-member"), "acme--m", access.LevelOwner},
		{"the fork's owner, the parent's owner", byKey("u-owner"), "acme--o", access.LevelOwner},
		{"the parent's owner", byKey("u-owner"), "acme--m", access.LevelOwner},
		{"another member", byKey("u-other"), "acme--m", access.LevelMember},
		{"a member on the parent owner's fork", byKey("u-member"), "acme--o", access.LevelMember},
		{"the fork's owner who left the org", byKey("u-left"), "acme--gone", access.LevelNone},
		{"the fork's owner in a blocked org", byKey("u-blocked-org-member"), "held--m", access.LevelNone},
		{"the parent's owner in a blocked org", byKey("u-owner"), "held--m", access.LevelOwner},
		{"a stranger", byKey("u-stranger"), "acme--m", access.LevelNone},
		{"the admin token", adminToken, "acme--m", access.LevelAdmin},
		{"the admin token on a lost member's fork", adminToken, "acme--gone", access.LevelAdmin},
		{"the fork's owner, parent gone", byKey("u-member"), "lost--f", access.LevelNone},
		{"the admin token, parent gone", adminToken, "lost--f", access.LevelAdmin},
	} {
		got, rec := resolve(t, svc, c)
		if c.caller.Admin {
			continue
		}
		onParent, _, _, err := svc.Level(context.Background(), access.CallerFrom(c.caller), rec.ParentSlug)
		if err == nil && onParent == access.LevelNone && got != access.LevelNone {
			t.Errorf("%s: level %q on %s although none on its parent %q", c.name, got, c.slug, rec.ParentSlug)
		}
	}
}

func TestLevel_AFailedReadIsAnErrorWithLevelNone(t *testing.T) {
	t.Parallel()
	st, _ := levelWorld(t)
	reader := access.StoreRecords(st.Main())
	members := memberships{of: map[string][]string{"u-member": {"o1"}}, active: []string{"o1"}}
	for _, c := range []struct {
		name      string
		records   access.RecordReader
		members   access.Memberships
		caller    server.Caller
		slug      string
		wantErr   bool
		wantLevel access.Level
	}{
		{"the record", failingRecords{reader, []string{"acme"}}, members, byKey("u-owner"), "acme", true, access.LevelNone},
		{"the record, for the admin token", failingRecords{reader, []string{"acme"}}, members, adminToken, "acme", true, access.LevelNone},
		{"the fork's parent", failingRecords{reader, []string{"acme"}}, members, byKey("u-member"), "acme--m", true, access.LevelNone},
		{"the memberships", reader, failingMemberships, byKey("u-member"), "acme", true, access.LevelNone},
		{"the memberships, on a fork", reader, failingMemberships, byKey("u-member"), "acme--m", true, access.LevelNone},
		// The owner is decided before any membership read.
		{"the memberships, for the owner", reader, failingMemberships, byKey("u-owner"), "acme", false, access.LevelOwner},
		{"the memberships, for the admin token", reader, failingMemberships, adminToken, "acme", false, access.LevelAdmin},
	} {
		got, rec, found, err := newService(c.records, c.members).Level(context.Background(), access.CallerFrom(c.caller), c.slug)
		if c.wantErr {
			if !errors.Is(err, errRead) || got != access.LevelNone || found || rec != (access.Record{}) {
				t.Errorf("a failed read of %s = %q, %+v, %t, %v, want none, no record and the read's error", c.name, got, rec, found, err)
			}
			continue
		}
		if err != nil || got != c.wantLevel {
			t.Errorf("a failed read of %s = %q, %v, want %q and no error", c.name, got, err, c.wantLevel)
		}
	}
}

func TestCanWrite_AnswersYesExactlyForAnOwnerOfTheWorkspace(t *testing.T) {
	t.Parallel()
	st, svc := levelWorld(t)
	for _, c := range []struct {
		user, slug string
		want       bool
	}{
		{"u-owner", "acme", true},
		{"u-member", "acme", false},
		{"u-stranger", "acme", false},
		{"u-member", "acme--m", true},
		{"u-owner", "acme--m", true},
		{"u-owner", "nope", false},
	} {
		if got, err := svc.CanWrite(context.Background(), c.user, c.slug); got != c.want || err != nil {
			t.Errorf("CanWrite(%s, %s) = %t, %v, want %t and no error", c.user, c.slug, got, err, c.want)
		}
	}
	failing := newService(failingRecords{access.StoreRecords(st.Main()), []string{"acme"}}, memberships{})
	if got, err := failing.CanWrite(context.Background(), "u-owner", "acme"); got || !errors.Is(err, errRead) {
		t.Errorf("CanWrite with a failed read = %t, %v, want false and the read's error", got, err)
	}
}
